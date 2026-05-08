package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// setupWSControlRouter creates a test router with the control WebSocket endpoint.
func setupWSControlRouter(registry *session.Registry, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	origins := cfg.ParseAllowedOrigins()
	r.Get("/devices/{serial}/control", StreamControl(registry, origins, cfg))
	return r
}

func newControlSessionWithLease(t *testing.T, serial string) (*session.DeviceSession, *session.Hub, string, context.CancelFunc) {
	t.Helper()
	hub := session.NewHub(session.HubOpts{
		Stream:              "video",
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hub.SetCodecMeta([12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Run(ctx) }()

	sess := session.NewActiveSessionForTest(serial, hub)

	// Create a control writer
	cw := scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
		Conn:       nil, // nil is fine for tests that don't write
		Log:        slog.Default(),
		BufferSize: 64,
	})
	sess.SetControlWriterForTest(cw)

	return sess, hub, "", cancel
}

func TestControlWSRejectsWithoutLease(t *testing.T) {
	// Connect without X-Lease-ID -> should get 403 LEASE_REQUIRED before WS upgrade
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	router := setupWSControlRouter(registry, cfg)

	registry.GetOrCreate("ABC123")

	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/control", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "should return 403 without lease header")
}

func TestControlWSRejectsWithBadLease(t *testing.T) {
	// Connect with wrong lease ID -> should get 403 LEASE_INVALID
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	router := setupWSControlRouter(registry, cfg)

	entry := registry.GetOrCreate("ABC123")

	// Acquire a lease with key-1
	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	// Try to connect with wrong lease ID
	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/control", nil)
	req.Header.Set("X-Lease-ID", "wrong-lease-id")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "should return 403 with wrong lease ID")
	_ = lease // suppress unused warning
}

func TestControlWSAcceptsValidLease(t *testing.T) {
	sess, _, _, cancel := newControlSessionWithLease(t, "ABC123")
	defer cancel()

	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	// Acquire a lease
	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	router := setupWSControlRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/control"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ctxCancel()

	// Connect with valid lease ID
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Lease-ID": []string{lease.ID}},
	})
	require.NoError(t, err, "WS upgrade should succeed with valid lease")
	conn.CloseNow()
}

func TestControlWSJSONToScrcpyBytes(t *testing.T) {
	// Holding the lease, send a JSON message and verify ControlWriter receives it
	sess, _, _, cancel := newControlSessionWithLease(t, "ABC123")
	defer cancel()

	// Create a real ControlWriter with a buffered channel
	cw := scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
		Conn:       nil,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BufferSize: 64,
	})
	sess.SetControlWriterForTest(cw)

	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	// Acquire a lease
	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	router := setupWSControlRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/control"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Lease-ID": []string{lease.ID}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send a BACK_OR_SCREEN_ON control message
	msg := map[string]any{"type": "BACK_OR_SCREEN_ON", "action": 0}
	msgBytes, _ := json.Marshal(msg)
	writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer writeCancel()
	err = conn.Write(writeCtx, websocket.MessageText, msgBytes)
	require.NoError(t, err)

	// Verify the message arrives at ControlWriter.in
	select {
	case cmsg := <-cw.InChanForTest():
		assert.Equal(t, scrcpy.TypeBackOrScreenOn, cmsg.Type)
		assert.Equal(t, byte(0), cmsg.BackOrScreenOn.Action)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control message")
	}
}

func TestControlWSValidatesType(t *testing.T) {
	// Send an unknown control type -> should get error text frame, NOT close
	sess, _, _, cancel := newControlSessionWithLease(t, "ABC123")
	defer cancel()

	cw := scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
		Conn:       nil,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BufferSize: 64,
	})
	sess.SetControlWriterForTest(cw)

	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	router := setupWSControlRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/control"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Lease-ID": []string{lease.ID}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send an unknown type
	msg := map[string]any{"type": "INJECT_NUKE"}
	msgBytes, _ := json.Marshal(msg)
	writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer writeCancel()
	err = conn.Write(writeCtx, websocket.MessageText, msgBytes)
	require.NoError(t, err)

	// Read the error response
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	msgType, data, err := conn.Read(readCtx)
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, msgType, "error should be text frame")

	var errResp map[string]any
	json.Unmarshal(data, &errResp)
	errBody := errResp["error"].(map[string]any)
	assert.Equal(t, "UNKNOWN_CONTROL_TYPE", errBody["code"])
}

func TestControlWSEnforcesLengthLimits(t *testing.T) {
	// Send INJECT_TEXT with 301-char string -> should get CONTROL_PAYLOAD_TOO_LARGE error
	sess, _, _, cancel := newControlSessionWithLease(t, "ABC123")
	defer cancel()

	cw := scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
		Conn:       nil,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BufferSize: 64,
	})
	sess.SetControlWriterForTest(cw)

	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	router := setupWSControlRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/control"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Lease-ID": []string{lease.ID}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send INJECT_TEXT with 301-char string (exceeds 300 byte limit)
	longText := strings.Repeat("x", 301)
	msg := map[string]any{"type": "INJECT_TEXT", "text": longText}
	msgBytes, _ := json.Marshal(msg)
	writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer writeCancel()
	err = conn.Write(writeCtx, websocket.MessageText, msgBytes)
	require.NoError(t, err)

	// Read the error response
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	msgType, data, err := conn.Read(readCtx)
	require.NoError(t, err)

	var errResp map[string]any
	json.Unmarshal(data, &errResp)
	errBody := errResp["error"].(map[string]any)
	assert.Equal(t, "CONTROL_PAYLOAD_TOO_LARGE", errBody["code"])
	_ = msgType
}

func TestControlWSForceReleaseDeliversEvent(t *testing.T) {
	// Hold lease, force-release -> client receives lease_released event
	sess, _, _, cancel := newControlSessionWithLease(t, "ABC123")
	defer cancel()

	cw := scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
		Conn:       nil,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		BufferSize: 64,
	})
	sess.SetControlWriterForTest(cw)

	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	mgr := entry.GetLeaseManager()
	lease, err := mgr.Acquire(sha256Hex("key-1"))
	require.NoError(t, err)

	router := setupWSControlRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/control"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Lease-ID": []string{lease.ID}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Give the WS handler time to start listening for release events
	time.Sleep(50 * time.Millisecond)

	// Force release the lease
	mgr.ForceRelease(session.ReasonAdminRevoked)

	// Read the force-release event
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	msgType, data, err := conn.Read(readCtx)
	require.NoError(t, err, "should receive force-release event")
	assert.Equal(t, websocket.MessageText, msgType)

	var event map[string]any
	json.Unmarshal(data, &event)
	assert.Equal(t, "lease_released", event["type"])
	assert.Equal(t, "admin_revoked", event["reason"])
}

func TestDecodeControlEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    scrcpy.ControlType
		wantErr bool
	}{
		{"INJECT_KEYCODE", `{"type":"INJECT_KEYCODE","action":0,"keycode":66,"repeat":0,"meta_state":0}`, scrcpy.TypeInjectKeycode, false},
		{"INJECT_TEXT", `{"type":"INJECT_TEXT","text":"hello"}`, scrcpy.TypeInjectText, false},
		{"BACK_OR_SCREEN_ON", `{"type":"BACK_OR_SCREEN_ON","action":0}`, scrcpy.TypeBackOrScreenOn, false},
		{"EXPAND_NOTIFICATION_PANEL", `{"type":"EXPAND_NOTIFICATION_PANEL"}`, scrcpy.TypeExpandNotificationPanel, false},
		{"ROTATE_DEVICE", `{"type":"ROTATE_DEVICE"}`, scrcpy.TypeRotateDevice, false},
		{"RESET_VIDEO", `{"type":"RESET_VIDEO"}`, scrcpy.TypeResetVideo, false},
		{"unknown type", `{"type":"INJECT_NUKE"}`, 0, true},
		{"missing type", `{}`, 0, true},
		{"empty type", `{"type":""}`, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := decodeControlEnvelope([]byte(tt.json))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, msg.Type)
		})
	}
}

// sha256Hex is a test helper for computing owner key fingerprint.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}