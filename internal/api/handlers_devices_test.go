package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// testConfig returns a Config with reasonable test defaults.
func testConfig() *config.Config {
	return &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:0",
		Stream: config.StreamConfig{
			ViewerBufferFrames:  60,
			MaxConsecutiveDrops: 120,
			AudioEnabled:        true,
		},
		Control: config.ControlConfig{
			LeaseTTLSeconds: 60,
		},
		WS: config.WSConfig{
			PingIntervalSeconds: 25,
			IdleTimeoutSeconds:  90,
			ReadLimitBytes:      4194304,
		},
	}
}

// setupTestRouter creates a chi router with the device handlers for testing.
func setupTestRouter(registry *session.Registry) *chi.Mux {
	cfg := testConfig()
	r := chi.NewRouter()
	r.Get("/devices", ListDevices(registry))
	r.Route("/devices/{serial}", func(r chi.Router) {
		r.Post("/sessions", CreateSession(registry, nil, nil, cfg))
		r.Delete("/sessions/{sessionID}", DeleteSession(registry))
	})
	return r
}

// mockLauncherForAPI implements session.Launcher for API-level tests.
// Returns a configurable result or error.
type mockLauncherForAPI struct {
	result *scrcpy.LaunchResult
	err    error
	calls  int
}

func (m *mockLauncherForAPI) LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

// createTestLaunchResult creates a LaunchResult with fake resources that can be closed.
func createTestLaunchResult() *scrcpy.LaunchResult {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	return &scrcpy.LaunchResult{
		VideoLn:    ln,
		DeviceName: "test-device",
		CodecMeta:  [12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20},
		ReverseMap: &adb.ReverseMapping{DeviceSpec: "localabstract:scrcpy_test", HostSpec: "tcp:0"},
		SCID:       "deadbeef",
		Cleanup:    func() { ln.Close() },
	}
}

func TestListDevicesEmpty(t *testing.T) {
	registry := session.NewRegistry()
	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var devices []deviceResponse
	err := json.Unmarshal(w.Body.Bytes(), &devices)
	require.NoError(t, err)
	assert.Empty(t, devices)
}

func TestListDevicesWithDevices(t *testing.T) {
	registry := session.NewRegistry()
	entry1 := registry.GetOrCreate("ABC123")
	entry1.SetState(session.StateIdle)
	entry2 := registry.GetOrCreate("DEF456")
	entry2.SetState(session.StateActive)

	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var devices []deviceResponse
	err := json.Unmarshal(w.Body.Bytes(), &devices)
	require.NoError(t, err)

	// Build a map for order-independent comparison
	deviceMap := make(map[string]string)
	for _, d := range devices {
		deviceMap[d.Serial] = d.State
	}
	assert.Equal(t, "idle", deviceMap["ABC123"])
	assert.Equal(t, "active", deviceMap["DEF456"])
}

func TestListDevicesExcludesFailed(t *testing.T) {
	registry := session.NewRegistry()
	entry1 := registry.GetOrCreate("AVAILABLE1")
	entry1.SetState(session.StateIdle)
	entry2 := registry.GetOrCreate("AVAILABLE2")
	entry2.SetState(session.StateActive)
	entry3 := registry.GetOrCreate("FAILED1")
	entry3.SetState(session.StateFailed)

	router := setupTestRouter(registry)
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var devices []deviceResponse
	err := json.Unmarshal(w.Body.Bytes(), &devices)
	require.NoError(t, err)

	// Only available1 and available2 should appear, not failed1
	assert.Len(t, devices, 2)
	serials := make(map[string]bool)
	for _, d := range devices {
		serials[d.Serial] = true
		assert.NotEqual(t, "failed", d.State)
	}
	assert.True(t, serials["AVAILABLE1"])
	assert.True(t, serials["AVAILABLE2"])
}

func TestCreateSessionInvalidSerial(t *testing.T) {
	registry := session.NewRegistry()
	router := setupTestRouter(registry)

	// Serial with special characters should be rejected per T-05-02
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC@123/sessions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should get 404 (ErrDeviceNotFound)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteSessionNoEntry(t *testing.T) {
	registry := session.NewRegistry()
	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/sessions/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteSessionInvalidSerial(t *testing.T) {
	registry := session.NewRegistry()
	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC%20123/sessions/some-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSerialPatternValidation(t *testing.T) {
	tests := []struct {
		name   string
		serial string
		valid  bool
	}{
		{"alphanumeric", "ABC123", true},
		{"with dashes", "ABC-123", true},
		{"with colons", "AA:BB:CC:DD:EE:FF", true},
		{"with dots", "ABC.123", true},
		{"wifi adb serial", "adb-R9CXA0460JZ-QBVjw8._adb-tls-connect._tcp", true},
		{"with spaces", "ABC 123", false},
		{"with slashes", "ABC/123", false},
		{"with special chars", "ABC@123", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.valid, serialPattern.MatchString(tt.serial))
		})
	}
}

func TestListDevicesRequiresAuth(t *testing.T) {
	registry := session.NewRegistry()
	r := chi.NewRouter()
	r.Use(APIKeyAuth("test-key", "secondary-key"))
	r.Get("/devices", ListDevices(registry))

	// Without API key - should get 401
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// With valid API key - should get 200
	req = httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateSessionIdempotent(t *testing.T) {
	// Test the idempotent logic at the handler level by manually
	// setting up an active entry.
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Manually set up an active session (simulating a completed start)
	result := createTestLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher, session.DefaultSessionOpts())
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupTestRouter(registry)

	// Second request for same device should return 200 with existing session
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/sessions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp sessionResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, resp.ID)
	assert.Equal(t, "ABC123", resp.Serial)
	assert.Equal(t, "active", resp.State)

	sess.Close(context.Background())
	client.Close()
}

func TestDeleteSessionSuccess(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create a session using mock launcher
	result := createTestLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher, session.DefaultSessionOpts())
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupTestRouter(registry)

	// Delete session
	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/sessions/"+sess.ID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)

	// Verify session is cleared
	assert.Nil(t, entry.GetSession())
	assert.Equal(t, session.StateIdle, entry.GetState())

	client.Close()
}

func TestDeleteSessionIDMismatch(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create a session
	result := createTestLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher, session.DefaultSessionOpts())
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupTestRouter(registry)

	// Delete with wrong session ID
	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/sessions/wrong-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	// Session should still exist
	assert.NotNil(t, entry.GetSession())

	sess.Close(context.Background())
	client.Close()
}

func TestCreateSessionStartingConflict(t *testing.T) {
	// When a device is in StateStarting (launch in progress),
	// a second CreateSession request should return 409 Conflict.
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Simulate a device in the "starting" state (launch in progress).
	entry.Lock()
	entry.State = session.StateStarting
	entry.Unlock()

	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/sessions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should get 409 Conflict because device is in starting state.
	assert.Equal(t, http.StatusConflict, w.Code)

	var errResp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "SESSION_CONFLICT", errResp.Error.Code)
}

func TestCreateSessionActiveConflict(t *testing.T) {
	// When a device already has an active session, CreateSession should
	// return the existing session (200 OK, not 409 Conflict).
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	result := createTestLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher, session.DefaultSessionOpts())
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.Lock()
	entry.Session = sess
	entry.State = session.StateActive
	entry.Unlock()

	router := setupTestRouter(registry)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/sessions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 200 OK with existing session (idempotent per DEV-03).
	assert.Equal(t, http.StatusOK, w.Code)

	var resp sessionResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, resp.ID)

	sess.Close(context.Background())
	client.Close()
}