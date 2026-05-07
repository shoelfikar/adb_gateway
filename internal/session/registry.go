// Package session manages device registry and session lifecycle state.
package session

import (
	"context"
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
	Session *DeviceSession // nil when no active session; defined in supervisor.go
	mu      sync.Mutex
}

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

// MarkAllDisconnected handles registry cleanup after ADB server disconnect.
// It iterates all entries and handles two cases:
//   - Entries with StateIdle (and no session, or session also idle): REMOVED from the
//     registry. These are stale — the ADB server can't confirm the device is present.
//   - Entries with non-idle state (StateActive, StateStarting, StateStopping): transitioned
//     to StateFailed. These are KEPT so the reconnect loop knows which sessions need
//     reverse forwards re-issued.
//
// StateIdle -> StateFailed is NOT a valid FSM transition, so idle entries are removed
// rather than transitioned. This also ensures GET /devices does not return stale
// available entries after ADB disconnect.
func (r *Registry) MarkAllDisconnected() {
	var toRemove []string

	r.devices.Range(func(key, val any) bool {
		entry := val.(*DeviceEntry)
		entry.mu.Lock()
		state := entry.State
		session := entry.Session
		entry.mu.Unlock()

		switch state {
		case StateIdle:
			// Idle entries are stale after ADB disconnect. Remove them.
			// Even if they have a session (unlikely), it's not in a recoverable state.
			toRemove = append(toRemove, entry.Serial)
			slog.Info("marking idle device disconnected (removing from registry)",
				"device", entry.Serial,
			)
		case StateActive, StateStarting, StateStopping:
			// Active sessions are transitioned to StateFailed so the reconnect
			// loop can identify which sessions need reverse forwards re-issued.
			if newState, err := TransitionTo(state, StateFailed); err == nil {
				entry.mu.Lock()
				entry.State = newState
				entry.mu.Unlock()
				slog.Info("marking active device disconnected (transitioned to failed)",
					"device", entry.Serial,
					"previous_state", state,
				)
			} else {
				// Should not happen: Active/Starting/Stopping -> Failed is valid.
				slog.Error("failed to transition device to failed state",
					"device", entry.Serial,
					"current_state", state,
					"error", err,
				)
			}
		case StateFailed:
			// Already failed, nothing to do.
			slog.Debug("device already in failed state, skipping",
				"device", entry.Serial,
			)
		default:
			// Unknown state; log but don't crash.
			slog.Warn("unknown device state during disconnect marking",
				"device", entry.Serial,
				"state", state,
			)
		}

		_ = session // suppress unused warning
		return true
	})

	// Remove idle entries after iteration (can't delete during Range).
	for _, serial := range toRemove {
		r.devices.Delete(serial)
		slog.Info("removed idle device from registry after ADB disconnect",
			"device", serial,
		)
	}
}

// ActiveSessionSpecs returns the reverse mapping specs for all entries that have
// an active session. This MUST be called BEFORE MarkAllDisconnected, since it
// queries StateActive entries which are transitioned to StateFailed by that method.
// Returns a map keyed by device serial, where each value is a slice of specs
// that can be used with Reconnector.ReissueReverseForwards.
func (r *Registry) ActiveSessionSpecs() map[string][]adb.ReverseMappingSpec {
	specs := make(map[string][]adb.ReverseMappingSpec)

	r.devices.Range(func(key, val any) bool {
		entry := val.(*DeviceEntry)
		entry.mu.Lock()
		state := entry.State
		session := entry.Session
		entry.mu.Unlock()

		if state != StateActive || session == nil {
			return true
		}

		// Get the session's reverse mapping to capture its specs.
		rm := session.ReverseMap()
		if rm == nil {
			return true
		}

		specs[entry.Serial] = []adb.ReverseMappingSpec{
			{
				DeviceSpec: rm.DeviceSpec,
				HostSpec:   rm.HostSpec,
			},
		}

		return true
	})

	return specs
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