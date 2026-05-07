// Package session manages device registry and session lifecycle state.
// supervisor.go defines DeviceSession, which orchestrates the scrcpy server
// lifecycle for a single device: push jar, set up reverse tunnels, launch
// app_process, and track session state through the FSM.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/scrcpy"
)

// Launcher is the interface for launching a scrcpy server on a device.
// *scrcpy.Launcher satisfies this interface. Defined here so tests can
// provide a mock without importing the concrete type.
type Launcher interface {
	Launch(ctx context.Context, serial string) (*scrcpy.LaunchResult, error)
}

// DeviceSession manages the full lifecycle of a scrcpy session for one device.
// It tracks state transitions via the FSM (idle -> starting -> active -> stopping -> idle)
// and holds resources (video connection, reverse mapping, listener) acquired during startup.
//
// All public methods are thread-safe; the per-device mutex (ADB-07) prevents one hung
// device from blocking operations on others.
type DeviceSession struct {
	ID     string
	Serial string

	state SessionState
	mu    sync.Mutex
	log   *slog.Logger

	// Resources acquired during scrcpy launch; set in Start, cleaned up in Close.
	videoConn  net.Conn
	videoLn    net.Listener
	reverseMap *adb.ReverseMapping
	codecMeta  [12]byte
	deviceName string
	scid       string
	cleanup    func()

	// Dependencies injected at creation.
	adbClient *adb.Client
	launcher  Launcher
}

// NewDeviceSession creates a DeviceSession for the given device serial.
// It generates a UUID for the session ID and creates a per-device sublogger
// per OBS-03 (structured fields: device serial, session ID).
func NewDeviceSession(serial string, adbClient *adb.Client, launcher Launcher) *DeviceSession {
	id := uuid.New().String()
	return &DeviceSession{
		ID:        id,
		Serial:    serial,
		state:     StateIdle,
		log:       slog.With("device", serial, "session", id),
		adbClient: adbClient,
		launcher:  launcher,
	}
}

// Start acquires the device mutex, validates the idle->starting transition,
// calls launcher.Launch to execute the 8-step scrcpy startup sequence,
// stores acquired resources, and transitions to StateActive.
//
// On failure, transitions to StateFailed and returns an error describing the failure.
// Per D-05: startup failure at step N cleans up steps 1..N-1.
func (s *DeviceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newState, err := TransitionTo(s.state, StateStarting)
	if err != nil {
		return fmt.Errorf("cannot start session: %w", err)
	}
	s.state = newState
	s.log.Info("session starting")

	result, err := s.launcher.Launch(ctx, s.Serial)
	if err != nil {
		s.state = StateFailed
		s.log.Error("scrcpy launch failed", "error", err)
		return fmt.Errorf("launch: %w", err)
	}

	// Store resources from the successful launch.
	s.videoConn = result.VideoConn
	s.videoLn = result.VideoLn
	s.reverseMap = result.ReverseMap
	s.codecMeta = result.CodecMeta
	s.deviceName = result.DeviceName
	s.scid = result.SCID
	s.cleanup = result.Cleanup

	s.state = StateActive
	s.log.Info("session active",
		"codec", string(result.CodecMeta[:4]),
		"device_name", result.DeviceName,
		"scid", result.SCID,
	)
	return nil
}

// State returns the current session state. Thread-safe via mutex.
func (s *DeviceSession) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close transitions to StateStopping, cleans up all resources (video connection,
// reverse mapping, listener), and transitions to StateIdle.
// Per D-05: cleanup happens on both normal shutdown and error paths.
func (s *DeviceSession) Close(ctx context.Context) error {
	s.mu.Lock()

	newState, err := TransitionTo(s.state, StateStopping)
	if err != nil {
		// If we can't transition to stopping (e.g., already idle), just return.
		s.mu.Unlock()
		return fmt.Errorf("cannot close session in state %s: %w", s.state, err)
	}
	s.state = newState
	s.mu.Unlock()

	s.log.Info("session stopping")
	s.cleanupResources()

	s.mu.Lock()
	s.state = StateIdle
	s.mu.Unlock()

	s.log.Info("session closed")
	return nil
}

// cleanupResources closes all resources acquired during scrcpy launch.
// Safe to call multiple times (idempotent).
func (s *DeviceSession) cleanupResources() {
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
	// Also nil out references to prevent accidental use after cleanup.
	s.videoConn = nil
	s.videoLn = nil
	s.reverseMap = nil
}

// Run executes the video reader loop in an errgroup with a closer goroutine.
// The closer goroutine waits for context cancellation, then calls cleanup to
// unblock any pending reads on the video connection.
// Returns the first error from the errgroup.
func (s *DeviceSession) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	// Closer goroutine: close sockets when context cancels to unblock reads.
	g.Go(func() error {
		<-ctx.Done()
		s.cleanupResources()
		return ctx.Err()
	})

	return g.Wait()
}

// CodecMeta returns the 12-byte codec metadata from the scrcpy video stream.
// This is sent as the first binary WebSocket message in the video relay.
func (s *DeviceSession) CodecMeta() [12]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codecMeta
}

// VideoConn returns the video connection for reading frames.
// Used by the WebSocket relay to read scrcpy video frames.
func (s *DeviceSession) VideoConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoConn
}

// DeviceName returns the device model name read from scrcpy device metadata.
func (s *DeviceSession) DeviceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceName
}

// SCID returns the scrcpy session ID used for the device-side socket name.
func (s *DeviceSession) SCID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scid
}

// mapLaunchError maps launcher errors to domain error categories.
// This function lives in the session package to avoid circular imports with
// the api package. Handlers in the api package use this to determine which
// DomainError to return to the client per D-08.
func mapLaunchError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "push"):
		return "PUSH_FAILED"
	case strings.Contains(msg, "reverse forward"):
		return "REVERSE_FORWARD_FAILED"
	case strings.Contains(msg, "dial adb"):
		return "ADB_UNAVAILABLE"
	default:
		return "SCRCPY_LAUNCH_FAILED"
	}
}

// IsLaunchError checks if an error from DeviceSession.Start maps to a specific
// domain error category. Used by handlers to determine the correct HTTP status.
func IsLaunchError(err error, category string) bool {
	return mapLaunchError(err) == category
}

// IsSessionActive checks if a session is in the active state.
// The caller must hold entry.Lock() before calling this method.
// Useful for idempotent session creation (DEV-03).
func IsSessionActive(entry *DeviceEntry) bool {
	return entry.State == StateActive && entry.Session != nil
}

// wrapLaunchError wraps a launch error with its domain category for use by the API layer.
type launchError struct {
	err      error
	category string
}

func (e *launchError) Error() string {
	return e.err.Error()
}

func (e *launchError) Unwrap() error {
	return e.err
}

// Category returns the domain error category for this launch error.
func (e *launchError) Category() string {
	return e.category
}

// WrapLaunchError wraps an error from the launcher with a domain category.
func WrapLaunchError(err error) error {
	if err == nil {
		return nil
	}
	return &launchError{err: err, category: mapLaunchError(err)}
}

// GetLaunchErrorCategory extracts the domain category from a wrapped launch error.
// Returns empty string if the error is not a launch error.
func GetLaunchErrorCategory(err error) string {
	var le *launchError
	if errors.As(err, &le) {
		return le.Category()
	}
	return mapLaunchError(err)
}