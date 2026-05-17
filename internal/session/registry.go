// Package session manages device registry and session lifecycle state.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/adb"
)

// DeviceEntry represents a tracked Android device in the registry.
// Each entry has its own mutex (ADB-07: per-device, never global)
// to prevent one hung device from blocking operations on others.
type DeviceEntry struct {
	Serial         string
	State          SessionState
	Session        *DeviceSession // nil when no active session; defined in supervisor.go
	LeaseManager   *LeaseManager  // per-device lease manager (plan 02-04, wired in 02-05)
	AudioAvailable bool           // set by launcher probe (plan 02-05 D-12)
	mu             sync.Mutex

	// InstallInFlight is the lock-free admission gate for APK install
	// (Phase 3 Plan 03-04). Concurrent POST /apks on the same device must
	// see CAS(false,true) fail and return 503 DEVICE_BUSY. The defer that
	// resets it must use Store(false). Independent of mu (Pitfall 9 — never
	// hold the device mutex during a long ADB call).
	InstallInFlight atomic.Bool

	// WriteInFlight is the lock-free admission gate for Phase 03.1 write
	// ops that must single-flight per device (backup, uninstall,
	// recursive-delete, upload-folder). Independent of InstallInFlight so
	// an APK install does not block an in-flight backup. CAS-only access
	// (Pitfall 9 — never hold DeviceEntry.mu while a long ADB call runs).
	WriteInFlight atomic.Bool

	// Recordings holds active screen recordings keyed by recording_id.
	// Phase 3 Plan 03-04 limits one active recording per device (a
	// concurrent POST returns 503 DEVICE_BUSY); the map shape allows
	// future relaxation. Allocated lazily by AddRecording.
	Recordings map[uuid.UUID]*recordingHandle
}

// recordingHandle pairs an active *Recording with its CancelFunc.
type recordingHandle struct {
	Rec    *Recording
	Cancel context.CancelFunc
}

// AddRecording stores a recording handle on the entry. Returns ErrRecordingBusy
// if another recording is already active.
func (e *DeviceEntry) AddRecording(rec *Recording, cancel context.CancelFunc) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Recordings == nil {
		e.Recordings = make(map[uuid.UUID]*recordingHandle)
	}
	if len(e.Recordings) > 0 {
		return ErrRecordingBusy
	}
	e.Recordings[rec.ID()] = &recordingHandle{Rec: rec, Cancel: cancel}
	return nil
}

// GetRecording returns the handle for a recording_id, or nil if not found.
func (e *DeviceEntry) GetRecording(id uuid.UUID) *recordingHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Recordings[id]
}

// RemoveRecording deletes a recording handle. Idempotent.
func (e *DeviceEntry) RemoveRecording(id uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.Recordings, id)
}

// ListRecordings returns a snapshot of active recordings on this entry.
func (e *DeviceEntry) ListRecordings() []*Recording {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Recording, 0, len(e.Recordings))
	for _, h := range e.Recordings {
		out = append(out, h.Rec)
	}
	return out
}

// ErrRecordingBusy means a recording is already active on this device.
var ErrRecordingBusy = errors.New("recording already active for this device")

// Lock acquires the per-device mutex. Used by external packages (e.g., api handlers)
// that need to read/modify DeviceEntry fields atomically.
func (e *DeviceEntry) Lock() { e.mu.Lock() }

// Unlock releases the per-device mutex.
func (e *DeviceEntry) Unlock() { e.mu.Unlock() }

// GetState returns the current session state. Thread-safe.
func (e *DeviceEntry) GetState() SessionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State
}

// SetState sets the session state. Thread-safe.
func (e *DeviceEntry) SetState(s SessionState) {
	e.mu.Lock()
	e.State = s
	e.mu.Unlock()
}

// GetSession returns the current session, if any. Thread-safe.
func (e *DeviceEntry) GetSession() *DeviceSession {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Session
}

// SetSession sets the session pointer. Thread-safe.
func (e *DeviceEntry) SetSession(s *DeviceSession) {
	e.mu.Lock()
	e.Session = s
	e.mu.Unlock()
}

// GetLeaseManager returns the device's lease manager (allocated lazily on first GetOrCreate).
func (e *DeviceEntry) GetLeaseManager() *LeaseManager {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.LeaseManager
}

// GetAudioAvailable returns whether audio is available for this device. Thread-safe.
func (e *DeviceEntry) GetAudioAvailable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.AudioAvailable
}

// SetAudioAvailable sets whether audio is available for this device. Thread-safe.
func (e *DeviceEntry) SetAudioAvailable(v bool) {
	e.mu.Lock()
	e.AudioAvailable = v
	e.mu.Unlock()
}

// RegistryOpts configures Registry creation.
type RegistryOpts struct {
	LeaseTTL time.Duration // = cfg.Control.LeaseTTLSeconds * time.Second
}

// Registry tracks connected devices using a thread-safe sync.Map.
// Devices are keyed by serial string. The registry is fed by
// host:track-devices events and updated in real time.
type Registry struct {
	devices  sync.Map        // serial string -> *DeviceEntry
	leaseTTL time.Duration
}

// NewRegistry creates an empty device registry with default lease TTL (60s).
// Use NewRegistryWithOpts for explicit TTL.
func NewRegistry() *Registry {
	return NewRegistryWithOpts(RegistryOpts{LeaseTTL: 60 * time.Second})
}

// NewRegistryWithOpts creates a registry with explicit configuration.
func NewRegistryWithOpts(opts RegistryOpts) *Registry {
	return &Registry{leaseTTL: opts.LeaseTTL}
}

// GetOrCreate returns the existing DeviceEntry for the given serial,
// or creates and stores a new one with StateIdle if none exists.
// Uses sync.Map.LoadOrStore for thread-safe idempotent creation.
func (r *Registry) GetOrCreate(serial string) *DeviceEntry {
	newEntry := &DeviceEntry{
		Serial:       serial,
		State:        StateIdle,
		LeaseManager: NewLeaseManager(r.leaseTTL, slog.With("device", serial)),
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

// MarkAllDisconnected handles registry cleanup after ADB server disconnect.
// It releases all session resources (video listeners, reverse mappings, device-side
// app_process cleanup) and removes ALL entries from the registry. When ADB reconnects,
// WatchDevices will re-populate the registry via GetOrCreate.
func (r *Registry) MarkAllDisconnected() {
	var toRemove []string

	r.devices.Range(func(key, val any) bool {
		entry := val.(*DeviceEntry)
		entry.mu.Lock()
		serial := entry.Serial
		state := entry.State
		sess := entry.Session
		entry.Session = nil // Clear session reference before releasing
		entry.mu.Unlock()

		// Release session resources (video listener, connection, reverse
		// mapping, app_process cleanup). The session is no longer viable
		// after ADB disconnects.
		if sess != nil {
			sess.ReleaseResources()
		}

		toRemove = append(toRemove, serial)
		slog.Info("removing device from registry after ADB disconnect",
			"device", serial,
			"previous_state", state,
		)
		return true
	})

	// Remove all entries after iteration (can't delete during Range).
	for _, serial := range toRemove {
		r.devices.Delete(serial)
		slog.Info("removed device from registry after ADB disconnect",
			"device", serial,
		)
	}
}

// WatchDevices reads from the ADB device event channel and updates the registry.
// On device connect: GetOrCreate (register the device with StateIdle).
// On device disconnect: Remove (remove from registry).
// Returns true if the event channel was closed (ADB disconnect), false if
// the context was cancelled (graceful shutdown).
// When a device reconnect event is received for an entry in StateFailed,
// its state is updated to StateIdle (post-reconnect state recovery).
func (r *Registry) WatchDevices(ctx context.Context, events <-chan adb.DeviceEvent) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case event, ok := <-events:
			if !ok {
				// Channel closed; ADB disconnect.
				return true
			}
			switch event.State {
			case "StateOnline", "StateOffline", "StateUnauthorized", "StateAuthorizing":
				// Device is present. goadb DeviceState.String() returns
				// "StateOnline" (not "device"), "StateOffline", etc.
				// Register it so session manager can attempt connection.
				entry := r.GetOrCreate(event.Serial)

				// If the entry was in StateFailed (from a previous ADB disconnect),
				// transition it back to StateIdle so it can be used again.
				entry.mu.Lock()
				if entry.State == StateFailed {
					entry.State = StateIdle
					slog.Info("device event: failed device recovered to idle",
						"device", event.Serial,
					)
				}
				entry.mu.Unlock()

				slog.Info("device event: device connected",
					"device", event.Serial,
					"state", event.State,
				)
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

// DeviceSession is defined in supervisor.go.