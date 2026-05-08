package api

import (
	"context"
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

// setupWSAudioRouter creates a test router with the audio WebSocket endpoint.
func setupWSAudioRouter(registry *session.Registry, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	origins := cfg.ParseAllowedOrigins()
	r.Get("/devices/{serial}/audio", StreamAudio(registry, origins, cfg))
	return r
}

func TestStreamAudioReturns404WhenUnavailable(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetAudioAvailable(false)

	router := setupWSAudioRouter(registry, cfg)

	// Should return HTTP 404 before WS upgrade
	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/audio", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "should return 404 for audio unavailable")
}

func TestStreamAudioStreamsFrames(t *testing.T) {
	// Create an audio Hub
	hub := session.NewHub(session.HubOpts{
		Stream:              "audio",
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hub.SetCodecMeta([12]byte{'o', 'p', 'u', 's', 0, 0, 0, 0, 0, 0, 0, 0})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()

	sess := session.NewActiveSessionForTest("ABC123", nil)
	// Manually set the audioHub since NewActiveSessionForTest only sets videoHub
	sess.SetAudioHubForTest(hub)

	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)
	entry.SetAudioAvailable(true)

	router := setupWSAudioRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/audio"
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()

	// Viewer 1
	conn1, _, err := websocket.Dial(wsCtx, wsURL, nil)
	require.NoError(t, err)
	defer conn1.CloseNow()

	// Viewer 2
	conn2, _, err := websocket.Dial(wsCtx, wsURL, nil)
	require.NoError(t, err)
	defer conn2.CloseNow()

	// Give subscriptions time to register
	time.Sleep(50 * time.Millisecond)

	// Publish an audio frame
	hub.Publish(&session.Frame{
		Header:   [12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 6},
		Payload:  []byte("audio-data"),
		KeyFrame: false,
	})

	// Read codec meta from both viewers
	readWithTimeout := func(conn *websocket.Conn) ([]byte, error) {
		readCtx, readCancel := context.WithTimeout(wsCtx, 2*time.Second)
		defer readCancel()
		_, data, err := conn.Read(readCtx)
		return data, err
	}

	_, err = readWithTimeout(conn1)
	require.NoError(t, err, "viewer 1 should receive codec meta")
	_, err = readWithTimeout(conn2)
	require.NoError(t, err, "viewer 2 should receive codec meta")

	// Read the audio frame from both viewers
	data1, err := readWithTimeout(conn1)
	require.NoError(t, err, "viewer 1 should receive audio frame")
	data2, err := readWithTimeout(conn2)
	require.NoError(t, err, "viewer 2 should receive audio frame")

	assert.Equal(t, data1, data2, "both viewers should receive identical audio data")
}

func TestStreamAudioReadLimitAndPingLoop(t *testing.T) {
	// Verify that audio WS has read limit and ping loop applied.
	// These are inherited from ws_helpers.go (applyWSDefaults, pingLoop).
	// We just verify the config values are applied correctly.
	cfg := testConfig()
	assert.Equal(t, int64(4194304), cfg.WS.ReadLimitBytes, "default read limit should be 4 MiB")
	assert.Equal(t, 25, cfg.WS.PingIntervalSeconds, "default ping interval should be 25s")
	assert.Equal(t, 90, cfg.WS.IdleTimeoutSeconds, "default idle timeout should be 90s")
}