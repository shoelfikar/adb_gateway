package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// serialPattern validates device serial numbers. Per T-05-02: alphanumeric
// plus dashes, colons, and dots (common in Android serials including WiFi ADB
// serials like "adb-R9CXA0460JZ-QBVjw8._adb-tls-connect._tcp"), to prevent injection.
var serialPattern = regexp.MustCompile(`^[a-zA-Z0-9:._-]+$`)

// deviceResponse represents a device in the API response.
type deviceResponse struct {
	Serial    string  `json:"serial"`
	State     string  `json:"state"`
	SessionID *string `json:"session_id,omitempty"`
}

// sessionResponse represents a session in the API response.
type sessionResponse struct {
	ID     string `json:"id"`
	Serial string `json:"serial"`
	State  string `json:"state"`
}

// sessionResponseWithConfig extends sessionResponse with the effective scrcpy
// config used for this session, so callers can verify their overrides took effect.
type sessionResponseWithConfig struct {
	sessionResponse
	Scrcpy config.ScrcpyConfig `json:"scrcpy"`
}

// createSessionRequest holds optional per-request scrcpy tunables.
// Zero values mean "use config default". This allows callers (e.g. a
// monitoring dashboard) to request lower quality without changing global config.
type createSessionRequest struct {
	MaxFPS      *int    `json:"max_fps,omitempty"`       // 0 = unlimited (server default)
	BitRate     *int    `json:"bit_rate,omitempty"`       // bps, 0 = server default
	MaxSize     *int    `json:"max_size,omitempty"`       // px, 0 = device default
	Codec       *string `json:"codec,omitempty"`          // h264 | h265 | av1
	AudioCodec  *string `json:"audio_codec,omitempty"`    // opus | aac | raw | flac
	AudioSource *string `json:"audio_source,omitempty"`   // output | mic | playback
}

// mergeWithConfig builds a session.ScrcpyConfig from per-request overrides
// falling back to cfg defaults for any field the caller omitted.
func (r *createSessionRequest) mergeWithConfig(cfg config.ScrcpyConfig) config.ScrcpyConfig {
	result := cfg
	if r.MaxFPS != nil {
		result.MaxFPS = *r.MaxFPS
	}
	if r.BitRate != nil {
		result.BitRate = *r.BitRate
	}
	if r.MaxSize != nil {
		result.MaxSize = *r.MaxSize
	}
	if r.Codec != nil {
		result.Codec = *r.Codec
	}
	if r.AudioCodec != nil {
		result.AudioCodec = *r.AudioCodec
	}
	if r.AudioSource != nil {
		result.AudioSource = *r.AudioSource
	}
	return result
}

// connectRequest is the body for POST /devices/connect.
type connectRequest struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// connectResponse is returned by POST /devices/connect. Status holds the raw
// human-readable ADB reply (e.g. "connected to 192.168.1.10:5555") so callers
// can distinguish "already connected" from "newly connected".
type connectResponse struct {
	Serial string `json:"serial"`
	Status string `json:"status"`
}

// connectHostPattern accepts hostnames, IPv4, and bracketed/unbracketed IPv6 —
// but rejects whitespace, colons inside the host part, and any character that
// could break the ADB wire format (`host:connect:<host>:<port>` is colon-
// delimited).
var connectHostPattern = regexp.MustCompile(`^[a-zA-Z0-9._\-\[\]]+$`)

// ConnectDevice returns an HTTP handler that asks the local ADB server to
// open a TCP/IP connection to a network-attached Android device via
// `host:connect:<host>:<port>`. NOTE: PROJECT.md / CLAUDE.md declares
// "Local ADB server only (no remote ADB) — devices are USB-attached". This
// endpoint is opt-in to that constraint and was added by user request.
func ConnectDevice(hostServices *adb.HostServices) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req connectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, ErrInvalidConnectTarget)
			return
		}
		if req.Host == "" || req.Port <= 0 || req.Port > 65535 {
			writeError(w, ErrInvalidConnectTarget)
			return
		}
		if !connectHostPattern.MatchString(req.Host) {
			writeError(w, ErrInvalidConnectTarget)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		status, err := hostServices.Connect(ctx, req.Host, req.Port)
		if err != nil {
			slog.Error("adb connect failed", "host", req.Host, "port", req.Port, "error", err)
			writeError(w, ErrAdbConnectFailed)
			return
		}

		// ADB returns OKAY at the wire layer even for "failed to connect to
		// host:port: ..." results — sniff the status text to map the outcome.
		if len(status) >= 6 && status[:6] == "failed" {
			slog.Warn("adb connect rejected by daemon", "host", req.Host, "port", req.Port, "status", status)
			writeError(w, ErrAdbConnectFailed)
			return
		}

		serial := req.Host + ":" + strconv.Itoa(req.Port)
		slog.Info("adb connect", "serial", serial, "status", status)
		writeJSON(w, http.StatusOK, connectResponse{Serial: serial, Status: status})
	}
}

// DisconnectDevice returns an HTTP handler that issues
// `host:disconnect:<host>:<port>` against the local ADB server. The serial
// path parameter must be a TCP/IP-style `host:port` value previously
// returned by ConnectDevice.
func DisconnectDevice(hostServices *adb.HostServices) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		status, err := hostServices.Disconnect(ctx, serial)
		if err != nil {
			slog.Error("adb disconnect failed", "serial", serial, "error", err)
			writeError(w, ErrAdbDisconnectFailed)
			return
		}
		slog.Info("adb disconnect", "serial", serial, "status", status)
		writeJSON(w, http.StatusOK, connectResponse{Serial: serial, Status: status})
	}
}

// ListDevices returns an HTTP handler that lists all tracked devices with their
// serial numbers and session states.
func ListDevices(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries := registry.List()
		devices := make([]deviceResponse, 0, len(entries))

		for _, entry := range entries {
			// Include all devices including Failed — the frontend needs
			// to show failed devices so operators can trigger restart.
			// Previously failed devices were hidden, making the restart
			// button unreachable (catch-22).
			d := deviceResponse{
				Serial: entry.Serial,
				State:  entry.GetState().String(),
			}
			if sess := entry.GetSession(); sess != nil {
				id := sess.ID
				d.SessionID = &id
			}
			devices = append(devices, d)
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
func CreateSession(registry *session.Registry, adbClient *adb.Client, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
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

		// Parse optional per-request scrcpy overrides. Empty body is fine —
		// all fields are optional and fall back to config defaults.
		var req createSessionRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				// Permissive: ignore decode errors so callers that send no body
				// or wrong Content-Type still work. Zero values → config defaults.
				req = createSessionRequest{}
			}
		}
		scrpyCfg := req.mergeWithConfig(cfg.Scrcpy)

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
		sess := session.NewDeviceSession(serial, adbClient, launcher, session.SessionOpts{
		BufFrames:      cfg.Stream.ViewerBufferFrames,
		MaxConsecDrops: cfg.Stream.MaxConsecutiveDrops,
		AudioEnabled:   cfg.Stream.AudioEnabled,
		ScrcpyCodec:       scrpyCfg.Codec,
		ScrcpyMaxSize:     scrpyCfg.MaxSize,
		ScrcpyBitRate:     scrpyCfg.BitRate,
		ScrcpyMaxFPS:      scrpyCfg.MaxFPS,
		ScrcpyAudioCodec:  scrpyCfg.AudioCodec,
		ScrcpyAudioSource: scrpyCfg.AudioSource,
	})

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

		// Start the Run() goroutines (video/audio hubs, readers, control writer)
		// in the background. Run() creates the hubs and relay loops that WS
		// handlers depend on. It runs until the session is closed (which calls
		// runCancel) or a fatal error occurs.
		runCtx, runCancel := context.WithCancel(context.Background())
		sess.SetRunCancel(runCancel)
		go func() {
			if err := sess.Run(runCtx); err != nil && runCtx.Err() == nil {
				slog.Error("session run failed", "device", serial, "session", sess.ID, "error", err)
				entry.Lock()
				if entry.State == session.StateActive {
					entry.State = session.StateFailed
				}
				entry.Unlock()
			}
		}()

		// Wait for Run() to instantiate hubs + controlWriter before returning 201.
		// Without this, a client that opens /control immediately after the response
		// can race the goroutine and observe a nil ControlWriter, causing the WS
		// handler to short-circuit with ErrDeviceOffline.
		select {
		case <-sess.Ready():
		case <-time.After(2 * time.Second):
			slog.Error("session run did not become ready in time", "device", serial, "session", sess.ID)
			runCancel()
			sess.Close(context.Background())
			entry.Lock()
			entry.State = session.StateFailed
			entry.Session = nil
			entry.Unlock()
			writeError(w, ErrScrcpyLaunchFailed)
			return
		}

		writeJSON(w, http.StatusCreated, sessionResponseWithConfig{
			sessionResponse: sessionResponse{
				ID:     sess.ID,
				Serial: serial,
				State:  session.StateActive.String(),
			},
			Scrcpy: scrpyCfg,
		})
	}
}

// LauncherFactory produces a fresh session.Launcher per request. The
// production wiring binds this to scrcpy.NewLauncher(adbClient, hostServices);
// tests bind it to a stub. Introduced in Plan 03-02 so RestartSession (and
// future Plan 03-03 endpoints that re-launch scrcpy) can be tested without
// touching real ADB.
type LauncherFactory func() session.Launcher

// RestartSession returns an HTTP handler that recovers a sticky-Failed
// device by transitioning Failed -> Idle -> Starting -> Active via a fresh
// scrcpy launch. This is the manual reverse of recovery exhaustion.
//
// Pre-conditions:
//   - The device entry must exist and be in StateFailed.
//   - Any other state returns 409 Conflict (callers should DELETE the
//     existing session first).
//
// Lock discipline (Pitfall 9): transition under entry.Lock() -> release ->
// run launch -> re-acquire to commit. Mirrors CreateSession exactly.
//
// 03-03 handoff: this handler is exported but the route registration in
// router.go is owned by Plan 03-03 (which also touches router.go for
// logcat/screenshot/files). 03-03 must add:
//
//	r.Post("/restart", api.RestartSession(registry, cfg, launcherFactory))
//
// inside the /devices/{serial} route group.
func RestartSession(registry *session.Registry, cfg *config.Config, factory LauncherFactory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Parse optional per-request scrcpy overrides (same as CreateSession).
		var req createSessionRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				req = createSessionRequest{}
			}
		}
		scrpyCfg := req.mergeWithConfig(cfg.Scrcpy)

		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Pre-flight: only Failed devices may be restarted. Any other
		// state means a session is in flight or already active — caller
		// should DELETE first.
		entry.Lock()
		if entry.State != session.StateFailed {
			entry.Unlock()
			writeError(w, ErrSessionConflict)
			return
		}
		// Failed -> Starting (manual recovery from sticky failed). The
		// new session starts a fresh internal FSM at StateIdle so its
		// Idle -> Starting -> Active chain is observable independently.
		entry.State = session.StateStarting
		entry.Unlock()

		launchCtx, launchCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer launchCancel()

		sess := session.NewDeviceSession(serial, nil, factory(), session.SessionOpts{
			BufFrames:      cfg.Stream.ViewerBufferFrames,
			MaxConsecDrops: cfg.Stream.MaxConsecutiveDrops,
			AudioEnabled:   cfg.Stream.AudioEnabled,
			ScrcpyCodec:       scrpyCfg.Codec,
			ScrcpyMaxSize:     scrpyCfg.MaxSize,
			ScrcpyBitRate:     scrpyCfg.BitRate,
			ScrcpyMaxFPS:      scrpyCfg.MaxFPS,
			ScrcpyAudioCodec:  scrpyCfg.AudioCodec,
			ScrcpyAudioSource: scrpyCfg.AudioSource,
		})

		if err := sess.Start(launchCtx); err != nil {
			entry.Lock()
			entry.State = session.StateFailed
			entry.Session = nil
			entry.Unlock()

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
		// Re-validate: ADB disconnect or external state change may have
		// re-failed us during the lock-free launch.
		if entry.State != session.StateStarting {
			entry.Unlock()
			sess.Close(context.Background())
			writeError(w, ErrADBUnavailable)
			return
		}
		entry.Session = sess
		entry.State = session.StateActive
		entry.Unlock()

		writeJSON(w, http.StatusCreated, sessionResponseWithConfig{
			sessionResponse: sessionResponse{
				ID:     sess.ID,
				Serial: serial,
				State:  session.StateActive.String(),
			},
			Scrcpy: scrpyCfg,
		})
	}
}

// deviceShellFn is the minimal callback for sending a shell command to a device.
// Production wiring binds this to hostServices.RunShellCommand via a closure.
type deviceShellFn func(ctx context.Context, cmd string) (string, error)

// RebootDevice returns an HTTP handler that reboots the Android device.
// It closes any active scrcpy session before sending the reboot command.
// The device will disconnect and reconnect via WatchDevices naturally.
func RebootDevice(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return hostServices.RunShellCommand(ctx, "target-device", cmd)
	}
	return rebootDeviceImpl(registry, shellFn)
}

// RebootDeviceForTest builds the reboot handler with an injectable shell function.
func RebootDeviceForTest(registry *session.Registry, shellFn deviceShellFn) http.HandlerFunc {
	return rebootDeviceImpl(registry, shellFn)
}

func rebootDeviceImpl(registry *session.Registry, shellFn deviceShellFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Close any active session before rebooting. The device will
		// disconnect from ADB shortly after this command completes.
		entry.Lock()
		sess := entry.Session
		entry.Session = nil
		entry.State = session.StateIdle
		entry.Unlock()

		if sess != nil {
			if err := sess.Close(r.Context()); err != nil {
				slog.Error("error closing session before reboot", "device", serial, "error", err)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if _, err := shellFn(ctx, "reboot"); err != nil {
			slog.Error("reboot command failed", "device", serial, "error", err)
			writeError(w, ErrRebootFailed)
			return
		}

		slog.Info("device reboot initiated", "device", serial)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"serial":  serial,
			"message": "Device reboot initiated",
		})
	}
}

// ShutdownDevice returns an HTTP handler that powers off the Android device.
// It closes any active scrcpy session before sending the shutdown command.
func ShutdownDevice(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return hostServices.RunShellCommand(ctx, "target-device", cmd)
	}
	return shutdownDeviceImpl(registry, shellFn)
}

// ShutdownDeviceForTest builds the shutdown handler with an injectable shell function.
func ShutdownDeviceForTest(registry *session.Registry, shellFn deviceShellFn) http.HandlerFunc {
	return shutdownDeviceImpl(registry, shellFn)
}

func shutdownDeviceImpl(registry *session.Registry, shellFn deviceShellFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Close any active session before shutting down.
		entry.Lock()
		sess := entry.Session
		entry.Session = nil
		entry.State = session.StateIdle
		entry.Unlock()

		if sess != nil {
			if err := sess.Close(r.Context()); err != nil {
				slog.Error("error closing session before shutdown", "device", serial, "error", err)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if _, err := shellFn(ctx, "reboot -p"); err != nil {
			slog.Error("shutdown command failed", "device", serial, "error", err)
			writeError(w, ErrShutdownFailed)
			return
		}

		slog.Info("device shutdown initiated", "device", serial)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"serial":  serial,
			"message": "Device shutdown initiated",
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