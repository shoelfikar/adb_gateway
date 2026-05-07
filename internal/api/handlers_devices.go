package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// serialPattern validates device serial numbers. Per T-05-02: alphanumeric
// plus dashes, colons, and dots (common in Android serials including WiFi ADB
// serials like "adb-R9CXA0460JZ-QBVjw8._adb-tls-connect._tcp"), to prevent injection.
var serialPattern = regexp.MustCompile(`^[a-zA-Z0-9:._-]+$`)

// deviceResponse represents a device in the API response.
type deviceResponse struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
}

// sessionResponse represents a session in the API response.
type sessionResponse struct {
	ID     string `json:"id"`
	Serial string `json:"serial"`
	State  string `json:"state"`
}

// ListDevices returns an HTTP handler that lists all tracked devices with their
// serial numbers and session states.
func ListDevices(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries := registry.List()
		devices := make([]deviceResponse, 0, len(entries))

		for _, entry := range entries {
			// Skip failed devices -- they're not reachable and should
			// not appear in the device list. Clients can retry by
			// creating a new session once the device recovers.
			if entry.GetState() == session.StateFailed {
				continue
			}
			devices = append(devices, deviceResponse{
				Serial: entry.Serial,
				State:  entry.GetState().String(),
			})
		}

		writeJSON(w, http.StatusOK, devices)
	}
}

// CreateSession returns an HTTP handler that creates a scrcpy session for a device.
// Per DEV-03: if a session already exists and is active, returns the existing session
// with 200 OK instead of creating a new one.
//
// The per-device mutex is released during the long-running Launch operation to
// prevent blocking other requests for the same device. The entry state is set to
// StateStarting before releasing the lock, so concurrent requests receive 409 Conflict.
func CreateSession(registry *session.Registry, adbClient *adb.Client, hostServices *adb.HostServices) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate serial per T-05-02: alphanumeric-only to prevent injection.
		if !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry := registry.GetOrCreate(serial)
		entry.Lock()

		// Idempotent: return existing session if active (DEV-03).
		// Note: IsSessionActive reads fields directly since we hold the lock.
		if session.IsSessionActive(entry) {
			sess := entry.Session
			entry.Unlock()
			writeJSON(w, http.StatusOK, sessionResponse{
				ID:     sess.ID,
				Serial: serial,
				State:  session.StateActive.String(),
			})
			return
		}

		// Cannot start if the device is in a non-idle, non-failed state.
		// StateStarting means another request is currently launching; return 409.
		if entry.State != session.StateIdle && entry.State != session.StateFailed {
			entry.Unlock()
			writeError(w, ErrSessionConflict)
			return
		}

		// Transition to starting and release the lock BEFORE the long-running Launch.
		// This prevents blocking other requests for the same device during launch
		// (which can take 30-60 seconds for push, tunnel, accept, and metadata reads).
		entry.State = session.StateStarting
		entry.Unlock()

		// Use a dedicated timeout context for the launch operation.
		// context.Background() ensures the launch continues even if the client
		// disconnects, so the device state is always updated to a terminal state.
		launchCtx, launchCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer launchCancel()

		// Create launcher and session. The launcher is stateless and safe to
		// create per request; it delegates to adbClient and hostServices.
		launcher := scrcpy.NewLauncher(adbClient, hostServices)
		sess := session.NewDeviceSession(serial, adbClient, launcher)

		if err := sess.Start(launchCtx); err != nil {
			entry.Lock()
			entry.State = session.StateFailed
			entry.Session = nil
			entry.Unlock()

			// Map the launch error to a domain error per D-08.
			category := session.GetLaunchErrorCategory(err)
			switch category {
			case "PUSH_FAILED":
				writeError(w, ErrPushFailed)
			case "REVERSE_FORWARD_FAILED":
				writeError(w, ErrReverseForwardFailed)
			case "ADB_UNAVAILABLE":
				writeError(w, ErrADBUnavailable)
			default:
				writeError(w, ErrScrcpyLaunchFailed)
			}
			return
		}

		entry.Lock()
		// Re-validate state: ADB disconnect may have transitioned it to StateFailed
		// during the lock-free launch window. Discard the launch result if so.
		if entry.State != session.StateStarting {
			entry.Unlock()
			sess.Close(context.Background())
			writeError(w, ErrADBUnavailable)
			return
		}
		entry.Session = sess
		entry.State = session.StateActive
		entry.Unlock()

		writeJSON(w, http.StatusCreated, sessionResponse{
			ID:     sess.ID,
			Serial: serial,
			State:  session.StateActive.String(),
		})
	}
}

// DeleteSession returns an HTTP handler that ends a session for a device.
// Verifies the session ID matches before closing. Per DEV-04: cancels context,
// removes reverse forwards, kills app_process.
func DeleteSession(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		sessionID := chi.URLParam(r, "sessionID")

		if serial == "" {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate serial per T-05-02.
		if !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrSessionNotFound)
			return
		}

		sess := entry.GetSession()
		if sess == nil {
			writeError(w, ErrSessionNotFound)
			return
		}

		// Verify session ID matches to prevent accidental deletion.
		if sess.ID != sessionID {
			writeError(w, ErrSessionNotFound)
			return
		}

		if err := sess.Close(r.Context()); err != nil {
			// Log the error but still clear the session reference.
			// The session resources are cleaned up regardless.
			slog.Error("error closing session", "device", serial, "error", err)
		}

		entry.SetSession(nil)
		entry.SetState(session.StateIdle)

		w.WriteHeader(http.StatusNoContent)
	}
}