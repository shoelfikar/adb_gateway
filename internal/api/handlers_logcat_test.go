package api

import (
	"context"
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

func setupWSLogcatRouter(registry *session.Registry, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	origins := cfg.ParseAllowedOrigins()
	r.Get("/devices/{serial}/logcat", StreamLogcat(registry, origins, cfg))
	return r
}

// TestLogcatHandlerSnapshotThenLiveTail verifies that a WS client receives
// the buffered snapshot first (one line per text frame) and then live
// appended lines.
func TestLogcatHandlerSnapshotThenLiveTail(t *testing.T) {
	buf := session.NewLogcatBuffer(session.LogcatBufferOpts{Capacity: 1000})
	for i := 0; i < 50; i++ {
		buf.Append("snap-" + itoaTest(i))
	}

	sess := session.NewActiveSessionForTest("ABC123", nil)
	sess.SetLogcatBufferForTest(buf)

	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("ABC123")
	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSLogcatRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()
	defer buf.Shutdown()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/logcat"
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()

	conn, _, err := websocket.Dial(wsCtx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	readWithTimeout := func() (string, websocket.MessageType, error) {
		readCtx, c := context.WithTimeout(wsCtx, 2*time.Second)
		defer c()
		mt, data, err := conn.Read(readCtx)
		return string(data), mt, err
	}

	// Read snapshot lines.
	for i := 0; i < 50; i++ {
		line, mt, err := readWithTimeout()
		require.NoError(t, err, "snapshot line %d", i)
		assert.Equal(t, websocket.MessageText, mt)
		assert.Equal(t, "snap-"+itoaTest(i), line)
	}

	// Append more lines and assert they arrive live.
	for i := 0; i < 5; i++ {
		buf.Append("live-" + itoaTest(i))
	}
	for i := 0; i < 5; i++ {
		line, mt, err := readWithTimeout()
		require.NoError(t, err, "live line %d", i)
		assert.Equal(t, websocket.MessageText, mt)
		assert.Equal(t, "live-"+itoaTest(i), line)
	}
}

// TestLogcatHandlerActiveOrReconnecting verifies that the handler accepts
// StateActive AND StateReconnecting (Pitfall 1: don't kill logcat WS during
// recovery — buffer is alive across recovery).
func TestLogcatHandlerActiveOrReconnecting(t *testing.T) {
	buf := session.NewLogcatBuffer(session.LogcatBufferOpts{Capacity: 100})
	buf.Append("hello")

	sess := session.NewActiveSessionForTest("device-recon", nil)
	sess.SetLogcatBufferForTest(buf)
	sess.SetStateForTest(session.StateReconnecting)

	registry := session.NewRegistry()
	cfg := testConfig()
	entry := registry.GetOrCreate("device-recon")
	entry.SetSession(sess)
	entry.SetState(session.StateReconnecting)

	router := setupWSLogcatRouter(registry, cfg)
	server := httptest.NewServer(router)
	defer server.Close()
	defer buf.Shutdown()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/device-recon/logcat"
	wsCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()

	conn, _, err := websocket.Dial(wsCtx, wsURL, nil)
	require.NoError(t, err, "logcat handler must accept StateReconnecting")
	defer conn.CloseNow()

	readCtx, c2 := context.WithTimeout(wsCtx, 2*time.Second)
	defer c2()
	mt, data, err := conn.Read(readCtx)
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, mt)
	assert.Equal(t, "hello", string(data))
}

// TestLogcatHandlerOfflineDevice verifies a 404 path before WS upgrade for
// devices not in the registry.
func TestLogcatHandlerOfflineDevice(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	router := setupWSLogcatRouter(registry, cfg)

	req := httptest.NewRequest(http.MethodGet, "/devices/MISSING/logcat", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
