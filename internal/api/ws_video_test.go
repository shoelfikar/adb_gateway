package api

import (
	"context"
	"errors"
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
	"github.com/pelni/adb-gateway/internal/session"
)

// setupWSVideoRouter creates a test router with the video WebSocket endpoint.
func setupWSVideoRouter(registry *session.Registry, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	origins := cfg.ParseAllowedOrigins()
	r.Get("/devices/{serial}/video", StreamVideo(registry, origins, cfg))
	return r
}

// setupAuthWSVideoRouter creates a test router with auth middleware and video WebSocket.
func setupAuthWSVideoRouter(registry *session.Registry, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	origins := cfg.ParseAllowedOrigins()
	r.Use(APIKeyAuth(cfg.APIKeyPrimary, cfg.APIKeySecondary))
	r.Get("/devices/{serial}/video", StreamVideo(registry, origins, cfg))
	return r
}

// newActiveSessionWithHub creates a DeviceSession in StateActive with a running
// Hub for integration testing of WS handlers.
func newActiveSessionWithHub(t *testing.T, serial string) (*session.DeviceSession, *session.Hub, context.CancelFunc) {
	t.Helper()
	hub := session.NewHub(session.HubOpts{
		Stream:              "video",
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hub.SetCodecMeta([12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = hub.Run(ctx)
	}()

	sess := session.NewActiveSessionForTest(serial, hub)
	return sess, hub, cancel
}

func TestStreamVideoNoSession(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	router := setupWSVideoRouter(registry, cfg)

	// Device exists but no session
	registry.GetOrCreate("ABC123")

	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoNoDevice(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	router := setupWSVideoRouter(registry, cfg)

	req := httptest.NewRequest(http.MethodGet, "/devices/NONEXISTENT/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoAuthRequired(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	router := setupAuthWSVideoRouter(registry, cfg)

	// WebSocket upgrade without X-API-Key should return 401
	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestStreamVideoInvalidSerial(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	router := setupWSVideoRouter(registry, cfg)

	req := httptest.NewRequest(http.MethodGet, "/devices/ABC@123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should get 404 because invalid serial is rejected
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoMultiViewer(t *testing.T) {
	// STR-04 e2e: Two viewers receive identical bytes for the same published frame.
	sess, hub, cancel := newActiveSessionWithHub(t, "ABC123")
	defer cancel()

	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	// Viewer 1
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn1.CloseNow()

	// Viewer 2
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn2.CloseNow()

	// Give subscriptions time to register
	time.Sleep(50 * time.Millisecond)

	// Publish a frame to the Hub
	hub.Publish(&session.Frame{
		Header:   [12]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		Payload:  []byte("test-frame-data"),
		KeyFrame: false,
	})

	// Both viewers should receive the frame (after codec meta and possibly keyframe)
	readWithTimeout := func(conn *websocket.Conn) ([]byte, error) {
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		defer readCancel()
		_, data, err := conn.Read(readCtx)
		return data, err
	}

	// Read codec meta from both viewers
	_, err = readWithTimeout(conn1)
	require.NoError(t, err, "viewer 1 should receive codec meta")
	_, err = readWithTimeout(conn2)
	require.NoError(t, err, "viewer 2 should receive codec meta")

	// Read the published frame from both viewers
	data1, err := readWithTimeout(conn1)
	require.NoError(t, err, "viewer 1 should receive frame")
	data2, err := readWithTimeout(conn2)
	require.NoError(t, err, "viewer 2 should receive frame")

	// Both should have received the same data
	assert.Equal(t, data1, data2, "both viewers should receive identical frame data")
}

func TestStreamVideoLateJoinerReceivesKeyframe(t *testing.T) {
	// STR-07 e2e: Late joiner receives cached keyframe.
	sess, hub, cancel := newActiveSessionWithHub(t, "ABC123")
	defer cancel()

	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	// Viewer 1 connects first
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn1.CloseNow()

	time.Sleep(30 * time.Millisecond)

	// Publish a keyframe
	hub.Publish(&session.Frame{
		Header:   [12]byte{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, // keyframe bit set
		Payload:  []byte("keyframe-data"),
		KeyFrame: true,
	})

	time.Sleep(30 * time.Millisecond)

	// Publish a non-keyframe
	hub.Publish(&session.Frame{
		Header:   [12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4},
		Payload:  []byte("pframe-data"),
		KeyFrame: false,
	})

	// Viewer 1 reads codec meta + keyframe + pframe
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err = conn1.Read(readCtx) // codec meta
	require.NoError(t, err)

	// Viewer 2 connects AFTER keyframe was published (late joiner)
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn2.CloseNow()

	// Viewer 2 should receive: codec meta + cached keyframe
	readCtx2, readCancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel2()
	msgType, data, err := conn2.Read(readCtx2)
	require.NoError(t, err, "late joiner should receive codec meta")
	assert.Equal(t, websocket.MessageBinary, msgType)
	assert.Len(t, data, 12, "first message should be codec meta")

	// Next message should be the cached keyframe
	readCtx3, readCancel3 := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel3()
	_, data2, err := conn2.Read(readCtx3)
	require.NoError(t, err, "late joiner should receive cached keyframe")
	// Keyframe data includes the header + payload
	assert.Contains(t, string(data2), "keyframe-data", "late joiner should receive the cached keyframe")
}

func TestStreamVideoPingPongCycle(t *testing.T) {
	// STR-08: With CloseRead now active, pings from the server are answered by
	// the client's auto-pong (coder/websocket processes pings during Read).
	// The connection stays alive as long as pongs arrive. We verify that after
	// multiple ping intervals the connection is still alive by writing a frame
	// from the server and reading it on the client.
	sess, hub, cancel := newActiveSessionWithHub(t, "ABC123")
	defer cancel()

	registry := session.NewRegistry()
	cfg := testConfig()
	// Loosened from 1s/3s/4s sleep to 2s/6s/8s sleep so CI runners (which can
	// stall a goroutine for several hundred ms under contention) no longer
	// flake. We still cross >3 ping intervals and sleep > idle_timeout, so
	// the test still proves that auto-pong is keeping the connection alive.
	cfg.WS.PingIntervalSeconds = 2 // ping every 2s
	cfg.WS.IdleTimeoutSeconds = 6   // 3 consecutive misses = idle

	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Read codec meta
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err = conn.Read(readCtx)
	require.NoError(t, err, "should receive codec meta")

	// Wait through 3+ ping intervals (>idle_timeout). With CloseRead on the
	// server side, pong responses are processed and the idle counter resets.
	// Connection should still be alive.
	time.Sleep(8 * time.Second)

	// Publish a frame from the hub; if the connection is alive the client
	// will receive it.
	hub.Publish(&session.Frame{
		Header:   [12]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		Payload:  []byte("after-pings"),
		KeyFrame: false,
	})

	readCtx2, readCancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel2()
	_, data, err := conn.Read(readCtx2)
	require.NoError(t, err, "connection should still be alive after pings")
	assert.Contains(t, string(data), "after-pings", "should receive frame published after ping cycles")
}

func TestStreamVideoReadLimitApplied(t *testing.T) {
	// STR-09: Server-side ReadLimit enforcement. With CloseRead active, the
	// server now reads inbound frames. An oversized frame triggers
	// StatusMessageTooBig and the connection is closed by the server.
	cfg := testConfig()
	cfg.WS.ReadLimitBytes = 256

	sess, _, cancel := newActiveSessionWithHub(t, "ABC123")
	defer cancel()

	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Read codec meta first
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err = conn.Read(readCtx)
	require.NoError(t, err, "should receive codec meta")

	// Send an oversized message (>256 bytes) from the client.
	// The server's CloseRead goroutine will read it, detect the ReadLimit
	// violation, and close the connection with StatusMessageTooBig.
	largePayload := make([]byte, 512)
	writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer writeCancel()
	err = conn.Write(writeCtx, websocket.MessageText, largePayload)
	require.NoError(t, err, "client should be able to write the oversized message")

	// The server closes the connection. The client's next read should return
	// an error containing StatusMessageTooBig (1009).
	readCtx2, readCancel2 := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel2()
	_, _, err = conn.Read(readCtx2)
	assert.Error(t, err, "connection should be closed after read limit violation")

	// Verify the close status is StatusMessageTooBig.
	var wsErr *websocket.CloseError
	if errors.As(err, &wsErr) {
		assert.Equal(t, websocket.StatusMessageTooBig, wsErr.Code,
			"close code should be StatusMessageTooBig (1009)")
	}
}

func TestStreamVideoCloseFrameProcessed(t *testing.T) {
	// Client-initiated close frames are now processed by CloseRead.
	// When a client sends a normal close, the server should exit cleanly
	// (no code 1006 abnormal closure).
	sess, _, cancel := newActiveSessionWithHub(t, "ABC123")
	defer cancel()

	registry := session.NewRegistry()
	cfg := testConfig()

	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)

	// Read codec meta
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err = conn.Read(readCtx)
	require.NoError(t, err, "should receive codec meta")

	// Client sends a normal close frame.
	closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer closeCancel()
	err = conn.Close(websocket.StatusNormalClosure, "bye")
	require.NoError(t, err, "client should send close frame")

	// Give the server's CloseRead goroutine time to process the close frame.
	_ = closeCtx

	// The connection should be cleanly shut down (not code 1006).
	// Verify by attempting to read; we should get a close error with
	// a normal closure code, not an abnormal closure.
	readCtx2, readCancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel2()
	_, _, err = conn.Read(readCtx2)
	// The read should return an error (connection closed), but it should
	// NOT be StatusAbnormalClosure (1006).
	if err != nil {
		var wsErr *websocket.CloseError
		if errors.As(err, &wsErr) {
			assert.NotEqual(t, websocket.StatusAbnormalClosure, wsErr.Code,
				"should not receive abnormal closure (1006); close frames should be processed cleanly")
		}
		// Any non-1006 error is acceptable: normal closure, going away, etc.
	}
}

func TestBuildAcceptOptions(t *testing.T) {
	t.Run("empty origins allows all", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/devices/abc/video", nil)
		opts := buildAcceptOptions(nil, r)
		assert.Equal(t, []string{"*"}, opts.OriginPatterns)
		assert.Equal(t, websocket.CompressionDisabled, opts.CompressionMode)
	})

	t.Run("specified origins", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/devices/abc/video", nil)
		opts := buildAcceptOptions([]string{"https://example.com", "https://other.com"}, r)
		assert.Equal(t, []string{"https://example.com", "https://other.com"}, opts.OriginPatterns)
	})

	t.Run("64-char subprotocol echo", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/devices/abc/video", nil)
		key := strings.Repeat("a", 64)
		r.Header.Set("Sec-WebSocket-Protocol", key)
		opts := buildAcceptOptions(nil, r)
		assert.Equal(t, []string{key}, opts.Subprotocols)
	})

	t.Run("lease subprotocol", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/devices/abc/video", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "lease.abc-123")
		opts := buildAcceptOptions(nil, r)
		// lease.<id> subprotocol is echoed back for WebSocket handshake
		assert.Equal(t, []string{"lease.abc-123"}, opts.Subprotocols)
	})

	t.Run("api subprotocol", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/devices/abc/video", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "api.my-secret-key")
		opts := buildAcceptOptions(nil, r)
		// api.<key> subprotocol is echoed back for WebSocket handshake
		assert.Equal(t, []string{"api.my-secret-key"}, opts.Subprotocols)
	})
}

func TestExtractAPIKeyFromSubprotocol(t *testing.T) {
	t.Run("no subprotocol", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		assert.Equal(t, "", extractAPIKeyFromSubprotocol(r))
	})

	t.Run("api subprotocol", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "api.my-secret-key")
		assert.Equal(t, "my-secret-key", extractAPIKeyFromSubprotocol(r))
	})

	t.Run("mixed subprotocols", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "lease.abc, api.my-key, other")
		assert.Equal(t, "my-key", extractAPIKeyFromSubprotocol(r))
	})
}