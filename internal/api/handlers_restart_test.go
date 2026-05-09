package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// stubLauncherFactory returns a LauncherFactory that yields a fresh
// mockLauncherForAPI per call, with a working LaunchResult.
func stubLauncherFactory() LauncherFactory {
	return func() session.Launcher {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		return &mockLauncherForAPI{result: &scrcpy.LaunchResult{
			VideoLn:    ln,
			DeviceName: "test-device",
			CodecMeta:  [12]byte{'h', '2', '6', '4'},
			SCID:       "deadbeef",
			Cleanup:    func() { ln.Close() },
		}}
	}
}

// TestRestartSessionFromFailed verifies POST /devices/{serial}/restart
// recovers a sticky-Failed device by transitioning Failed -> Idle and
// re-launching via the standard Create path.
func TestRestartSessionFromFailed(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("device-X")
	// Drive the entry into StateFailed.
	entry.Lock()
	entry.State = session.StateFailed
	entry.Unlock()

	cfg := testConfig()

	r := chi.NewRouter()
	r.Route("/devices/{serial}", func(r chi.Router) {
		// Bind RestartSession with a stub launcher factory so the test can
		// drive it without the real scrcpy CLI.
		r.Post("/restart", RestartSession(registry, cfg, stubLauncherFactory()))
	})

	req := httptest.NewRequest(http.MethodPost, "/devices/device-X/restart", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	var resp sessionResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "device-X", resp.Serial)
	assert.Equal(t, session.StateActive.String(), resp.State)
	assert.NotEmpty(t, resp.ID, "restart must return a fresh session id")

	// Final entry state must be StateActive with a new session attached.
	assert.Equal(t, session.StateActive, entry.GetState())
	require.NotNil(t, entry.GetSession())
	assert.Equal(t, resp.ID, entry.GetSession().ID)
}

// TestRestartSessionRejectsNonFailed verifies that calling /restart on a
// device that is NOT in StateFailed returns 409 Conflict (sessions in
// active/starting/etc. should be DELETE'd first, not restarted).
func TestRestartSessionRejectsNonFailed(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("device-Y")
	// Default state is StateIdle.
	_ = entry

	cfg := testConfig()

	r := chi.NewRouter()
	r.Route("/devices/{serial}", func(r chi.Router) {
		r.Post("/restart", RestartSession(registry, cfg, stubLauncherFactory()))
	})

	req := httptest.NewRequest(http.MethodPost, "/devices/device-Y/restart", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code, "non-Failed devices must not accept /restart; body=%s", w.Body.String())
}
