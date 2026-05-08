// Package session manages device registry and session lifecycle state.
// supervisor.go defines DeviceSession, which orchestrates the scrcpy server
// lifecycle for a single device: push jar, set up reverse tunnels, launch
// app_process, and track session state through the FSM.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error)
}

// SessionOpts configures DeviceSession creation. Phase 1 callers can use
// defaults; Phase 2 wiring passes values from config.
type SessionOpts struct {
	BufFrames      int // = cfg.Stream.ViewerBufferFrames
	MaxConsecDrops int // = cfg.Stream.MaxConsecutiveDrops
	AudioEnabled   bool
}

// DefaultSessionOpts returns reasonable defaults for Phase 1 compatibility.
func DefaultSessionOpts() SessionOpts {
	return SessionOpts{BufFrames: 60, MaxConsecDrops: 120, AudioEnabled: false}
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

	// Phase 2: audio + control resources from LaunchWithOptions.
	audioConn      net.Conn
	controlConn    net.Conn
	audioAvailable bool
	audioCodec     scrcpy.AudioCodec

	// Phase 2: goroutine-managed components.
	videoHub      *Hub
	audioHub      *Hub
	controlWriter *scrcpy.ControlWriter
	deviceMsgs    chan scrcpy.DeviceMessage

	// Phase 2: config snapshot.
	bufFrames      int
	maxConsecDrops int
	audioEnabled   bool

	// Dependencies injected at creation.
	adbClient *adb.Client
	launcher  Launcher
}

// NewDeviceSession creates a DeviceSession for the given device serial.
// It generates a UUID for the session ID and creates a per-device sublogger
// per OBS-03 (structured fields: device serial, session ID).
func NewDeviceSession(serial string, adbClient *adb.Client, launcher Launcher, opts SessionOpts) *DeviceSession {
	id := uuid.New().String()
	return &DeviceSession{
		ID:             id,
		Serial:         serial,
		state:          StateIdle,
		log:            slog.With("device", serial, "session", id),
		adbClient:      adbClient,
		launcher:       launcher,
		bufFrames:      opts.BufFrames,
		maxConsecDrops: opts.MaxConsecDrops,
		audioEnabled:   opts.AudioEnabled,
	}
}

// Start acquires the device mutex, validates the idle->starting transition,
// calls launcher.LaunchWithOptions, stores acquired resources, and transitions
// to StateActive.
func (s *DeviceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newState, err := TransitionTo(s.state, StateStarting)
	if err != nil {
		return fmt.Errorf("cannot start session: %w", err)
	}
	s.state = newState
	s.log.Info("session starting")

	result, err := s.launcher.LaunchWithOptions(ctx, s.Serial, scrcpy.LaunchOptions{
		AudioEnabled:   s.audioEnabled,
		ControlEnabled: true,
	})
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

	// Phase 2 resources.
	s.audioConn = result.AudioConn
	s.controlConn = result.ControlConn
	s.audioAvailable = result.AudioAvailable
	s.audioCodec = result.AudioCodec

	s.state = StateActive
	s.log.Info("session active",
		"codec", string(result.CodecMeta[:4]),
		"device_name", result.DeviceName,
		"scid", result.SCID,
		"audio_available", result.AudioAvailable,
		"audio_codec", result.AudioCodec,
		"control_enabled", result.ControlConn != nil,
	)
	return nil
}

// State returns the current session state. Thread-safe via mutex.
func (s *DeviceSession) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close transitions to StateStopping, cleans up all resources, and transitions to StateIdle.
func (s *DeviceSession) Close(ctx context.Context) error {
	s.mu.Lock()
	newState, err := TransitionTo(s.state, StateStopping)
	if err != nil {
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

// ReleaseResources releases all resources held by the session without
// transitioning FSM states. Used when ADB disconnects.
func (s *DeviceSession) ReleaseResources() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
	if s.videoLn != nil {
		s.videoLn.Close()
		s.videoLn = nil
	}
	if s.videoConn != nil {
		s.videoConn.Close()
		s.videoConn = nil
	}
	if s.reverseMap != nil {
		s.reverseMap.Close()
		s.reverseMap = nil
	}
}

// cleanupResources closes all resources acquired during scrcpy launch.
func (s *DeviceSession) cleanupResources() {
	if s.cleanup != nil {
		s.cleanup()
		s.mu.Lock()
		s.cleanup = nil
		s.videoConn = nil
		s.videoLn = nil
		s.reverseMap = nil
		s.audioConn = nil
		s.controlConn = nil
		s.mu.Unlock()
	}
}

// Run spawns 4-6 goroutines under errgroup: video reader → videoHub,
// audio reader → audioHub (if available), control writer, device message reader,
// and a closer goroutine for context cancellation cleanup.
// Returns the first error from the errgroup.
func (s *DeviceSession) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	s.deviceMsgs = make(chan scrcpy.DeviceMessage, 32)

	// Capture conn pointers into locals before starting goroutines to avoid
	// data race: the closer goroutine writes nil to s.controlConn etc via
	// cleanupResources, while the reader/writer goroutines read them.
	audioConn := s.audioConn
	controlConn := s.controlConn

	// Closer goroutine: close sockets when context cancels.
	g.Go(func() error {
		<-ctx.Done()
		s.cleanupResources()
		return ctx.Err()
	})

	// 1. Video Hub + Video Reader.
	s.videoHub = NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           s.bufFrames,
		MaxConsecutiveDrops: s.maxConsecDrops,
		Log:                 s.log,
	})
	s.videoHub.SetCodecMeta(s.codecMeta)
	g.Go(func() error { return s.videoHub.Run(ctx) })
	g.Go(func() error { return s.videoReaderLoop(ctx) })

	// 2. Audio Hub + Audio Reader (only if audio is available).
	if s.audioAvailable && audioConn != nil {
		s.audioHub = NewHub(HubOpts{
			Stream:              "audio",
			BufFrames:           s.bufFrames,
			MaxConsecutiveDrops: s.maxConsecDrops,
			Log:                 s.log,
		})
		g.Go(func() error { return s.audioHub.Run(ctx) })
		g.Go(func() error { return s.audioReaderLoop(ctx, audioConn) })
	}

	// 3. Control Writer + Device Message Reader (on the same conn).
	if controlConn != nil {
		s.controlWriter = scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
			Conn:       controlConn,
			Log:        s.log,
			BufferSize: 64,
		})
		g.Go(func() error { return s.controlWriter.Run(ctx) })
		g.Go(func() error { return s.deviceMessageReaderLoop(ctx, controlConn) })
	}

	return g.Wait()
}

// videoReaderLoop reads frames from the video connection and publishes to videoHub.
func (s *DeviceSession) videoReaderLoop(ctx context.Context) error {
	conn := s.VideoConn()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, payload, err := scrcpy.ReadVideoFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("video read: %w", err)
		}
		s.videoHub.Publish(&Frame{
			Header:   hdr.RawHeader(),
			Payload:  payload,
			KeyFrame: hdr.KeyFrame,
		})
	}
}

// audioReaderLoop reads frames from the audio connection and publishes to audioHub.
// Codec ID was already consumed by the launcher's probe.
// The conn parameter is passed explicitly to avoid racing with cleanupResources nil-assignment.
func (s *DeviceSession) audioReaderLoop(ctx context.Context, conn net.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, payload, err := scrcpy.ReadAudioFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("audio read: %w", err)
		}
		s.audioHub.Publish(&Frame{
			Header:   hdr.RawHeader(),
			Payload:  payload,
			KeyFrame: hdr.KeyFrame,
		})
	}
}

// deviceMessageReaderLoop reads DeviceMessages from the control connection
// and sends them on the deviceMsgs channel.
// The conn parameter is passed explicitly to avoid racing with cleanupResources nil-assignment.
func (s *DeviceSession) deviceMessageReaderLoop(ctx context.Context, conn net.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := scrcpy.ReadDeviceMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			if errors.Is(err, scrcpy.ErrUnknownDeviceMessage) {
				s.log.Warn("unknown device message; reader exiting", "error", err)
				return err
			}
			return fmt.Errorf("device message read: %w", err)
		}
		select {
		case s.deviceMsgs <- msg:
		case <-ctx.Done():
			return ctx.Err()
		default:
			s.log.Warn("device message dropped: consumer slow", "type", fmt.Sprintf("0x%02x", byte(msg.Type)))
		}
	}
}

// Accessor methods for Phase 2 WS handlers (plan 02-06).

func (s *DeviceSession) VideoHub() *Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoHub
}

func (s *DeviceSession) AudioHub() *Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioHub
}

func (s *DeviceSession) ControlWriter() *scrcpy.ControlWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlWriter
}

func (s *DeviceSession) DeviceMessages() <-chan scrcpy.DeviceMessage {
	return s.deviceMsgs
}

func (s *DeviceSession) AudioAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioAvailable
}

func (s *DeviceSession) AudioCodec() scrcpy.AudioCodec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioCodec
}

// Existing Phase 1 accessors.

func (s *DeviceSession) CodecMeta() [12]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codecMeta
}

func (s *DeviceSession) VideoConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoConn
}

func (s *DeviceSession) DeviceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceName
}

func (s *DeviceSession) SCID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scid
}

func (s *DeviceSession) ReverseMap() *adb.ReverseMapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reverseMap
}

func (s *DeviceSession) SetReverseMap(rm *adb.ReverseMapping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reverseMap = rm
}

func (s *DeviceSession) VideoLn() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoLn
}

// mapLaunchError maps launcher errors to domain error categories.
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

// IsLaunchError checks if an error from DeviceSession.Start maps to a specific category.
func IsLaunchError(err error, category string) bool {
	return mapLaunchError(err) == category
}

// IsSessionActive checks if a session is in the active state.
// The caller must hold entry.Lock() before calling this method.
func IsSessionActive(entry *DeviceEntry) bool {
	return entry.State == StateActive && entry.Session != nil
}

type launchError struct {
	err      error
	category string
}

func (e *launchError) Error() string { return e.err.Error() }
func (e *launchError) Unwrap() error { return e.err }

func (e *launchError) Category() string { return e.category }

func WrapLaunchError(err error) error {
	if err == nil {
		return nil
	}
	return &launchError{err: err, category: mapLaunchError(err)}
}

func GetLaunchErrorCategory(err error) string {
	var le *launchError
	if errors.As(err, &le) {
		return le.Category()
	}
	return mapLaunchError(err)
}