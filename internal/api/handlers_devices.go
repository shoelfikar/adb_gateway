package api

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// serialPattern validates device serial numbers. Per T-05-02: alphanumeric
// plus dashes and colons (common in Android serials), to prevent injection.
var serialPattern = regexp.MustCompile(`^[a-zA-Z0-9:-]+$`)

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
		defer entry.Unlock()

		// Idempotent: return existing session if active (DEV-03).
		// Note: IsSessionActive reads fields directly since we hold the lock.
		if session.IsSessionActive(entry) {
			sess := entry.Session
			writeJSON(w, http.StatusOK, sessionResponse{
				ID:     sess.ID,
				Serial: serial,
				State:  session.StateActive.String(),
			})
			return
		}

		// Cannot start if the device is in a non-idle, non-failed state.
		if entry.State != session.StateIdle && entry.State != session.StateFailed {
			writeError(w, ErrSessionConflict)
			return
		}

		// Create launcher and session. The launcher is stateless and safe to
		// create per request; it delegates to adbClient and hostServices.
		launcher := scrcpy.NewLauncher(adbClient, hostServices)
		sess := session.NewDeviceSession(serial, adbClient, launcher)

		if err := sess.Start(r.Context()); err != nil {
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

		entry.SetSession(sess)
		entry.SetState(session.StateActive)

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