// Package session manages device registry and session lifecycle state.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pelni/adb-gateway/internal/adb"
)

// DeviceEntry represents a tracked Android device in the registry.
// Each entry has its own mutex (ADB-07: per-device, never global)
// to prevent one hung device from blocking operations on others.
type DeviceEntry struct {
	Serial  string
	State   SessionState
	Session *DeviceSession // nil when no active session; type defined later (Plan 05)
	mu      sync.Mutex
}

// Registry tracks connected devices using a thread-safe sync.Map.
// Devices are keyed by serial string. The registry is fed by
// host:track-devices events and updated in real time.
type Registry struct {
	devices sync.Map // serial string -> *DeviceEntry
}

// NewRegistry creates an empty device registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// GetOrCreate returns the existing DeviceEntry for the given serial,
// or creates and stores a new one with StateIdle if none exists.
// Uses sync.Map.LoadOrStore for thread-safe idempotent creation.
func (r *Registry) GetOrCreate(serial string) *DeviceEntry {
	newEntry := &DeviceEntry{
		Serial: serial,
		State:  StateIdle,
	}
	actual, _ := r.devices.LoadOrStore(serial, newEntry)
	return actual.(*DeviceEntry)
}

// Get returns the DeviceEntry for the given serial, or nil if not found.
func (r *Registry) Get(serial string) (*DeviceEntry, bool) {
	val, ok := r.devices.Load(serial)
	if !ok {
		return nil, false
	}
	return val.(*DeviceEntry), true
}

// List returns all device entries currently tracked in the registry.
func (r *Registry) List() []*DeviceEntry {
	var entries []*DeviceEntry
	r.devices.Range(func(key, val any) bool {
		entries = append(entries, val.(*DeviceEntry))
		return true
	})
	return entries
}

// Remove removes a device entry from the registry (e.g., on device disconnect).
func (r *Registry) Remove(serial string) {
	r.devices.Delete(serial)
}

// CloseAllSessions iterates all registry entries and closes any active sessions.
// Used during graceful shutdown drain.
func (r *Registry) CloseAllSessions(ctx context.Context) {
	r.devices.Range(func(key, val any) bool {
		entry := val.(*DeviceEntry)
		entry.mu.Lock()
		session := entry.Session
		entry.mu.Unlock()

		if session != nil {
			if err := session.Close(ctx); err != nil {
				slog.Error("error closing session",
					"device", entry.Serial,
					"error", err,
				)
			}
		}
		return true
	})
}

// WatchDevices reads from the ADB device event channel and updates the registry.
// On device connect: GetOrCreate (register the device with StateIdle).
// On device disconnect: Remove (remove from registry).
// This method blocks until the context is cancelled or the event channel is closed.
func (r *Registry) WatchDevices(ctx context.Context, events <-chan adb.DeviceEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				// Channel closed; stop watching.
				return
			}
			switch event.State {
			case "device", "recovery", "offline":
				// Device is present (may be usable or in recovery/offline).
				// Register it so session manager can attempt connection.
				entry := r.GetOrCreate(event.Serial)
				slog.Info("device event: device connected",
					"device", event.Serial,
					"state", event.State,
				)
				// If the device was already tracked, GetOrCreate returns
				// the existing entry. We still log the state change.
				_ = entry
			default:
				// Device disconnected or unknown state.
				// Remove from registry so no new sessions can start.
				r.Remove(event.Serial)
				slog.Info("device event: device disconnected",
					"device", event.Serial,
					"state", event.State,
				)
			}
		}
	}
}

// DeviceSession is a placeholder type for the session supervisor that will
// be defined in Plan 05. For now, it's a minimal stub so that the registry
// can reference it. The Close method is required by CloseAllSessions.
type DeviceSession struct {
	ID     string
	Serial string
}

// Close gracefully shuts down the session. This is a placeholder
// implementation; the real version will be defined in Plan 05.
func (s *DeviceSession) Close(ctx context.Context) error {
	return fmt.Errorf("DeviceSession.Close not yet implemented")
}