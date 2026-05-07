package api

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// setupWSVideoRouter creates a test router with the video WebSocket endpoint.
func setupWSVideoRouter(registry *session.Registry) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/devices/{serial}/video", StreamVideo(registry))
	return r
}

// setupAuthWSVideoRouter creates a test router with auth middleware and video WebSocket.
func setupAuthWSVideoRouter(registry *session.Registry) *chi.Mux {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("test-key", "secondary-key"))
	r.Get("/devices/{serial}/video", StreamVideo(registry))
	return r
}

func TestStreamVideoNoSession(t *testing.T) {
	registry := session.NewRegistry()
	router := setupWSVideoRouter(registry)

	// Device exists but no session
	registry.GetOrCreate("ABC123")

	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoNoDevice(t *testing.T) {
	registry := session.NewRegistry()
	router := setupWSVideoRouter(registry)

	req := httptest.NewRequest(http.MethodGet, "/devices/NONEXISTENT/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoAuthRequired(t *testing.T) {
	registry := session.NewRegistry()
	router := setupAuthWSVideoRouter(registry)

	// WebSocket upgrade without X-API-Key should return 401
	req := httptest.NewRequest(http.MethodGet, "/devices/ABC123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestStreamVideoAuthWithKey(t *testing.T) {
	// Create a session with a mock launcher that provides a video connection
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create a mock launch result with a TCP connection pair
	result := createTestLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher)
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	// Set up router with auth middleware
	r := chi.NewRouter()
	r.Use(APIKeyAuth("test-key", "secondary-key"))
	r.Get("/devices/{serial}/video", StreamVideo(registry))

	server := httptest.NewServer(r)
	defer server.Close()

	// Connect WebSocket client with API key in header
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-API-Key": []string{"test-key"}},
	})
	require.NoError(t, err, "WebSocket upgrade should succeed with valid API key")
	defer conn.CloseNow()

	// Read codec metadata (first 12 bytes)
	msgType, data, err := conn.Read(ctx)
	require.NoError(t, err, "Should receive codec metadata")
	assert.Equal(t, websocket.MessageBinary, msgType)
	assert.Len(t, data, 12, "Codec metadata should be 12 bytes")

	// Verify codec metadata contains expected values
	codecID := string(data[:4])
	assert.Equal(t, "h264", codecID, "Codec ID should be h264")

	// Close the connection to clean up
	sess.Close(context.Background())
	client.Close()
}

func TestStreamVideoInvalidSerial(t *testing.T) {
	registry := session.NewRegistry()
	router := setupWSVideoRouter(registry)

	req := httptest.NewRequest(http.MethodGet, "/devices/ABC@123/video", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should get 404 because invalid serial is rejected
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamVideoWithSession(t *testing.T) {
	// This test creates a session with a real TCP connection and verifies
	// that the WebSocket relay sends codec metadata and then frame data.
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create a mock launch result with a video connection we control
	result := createTestLaunchResult()

	// Use a TCP listener for the video stream
	// We write data to one end and read from the other via WebSocket
	videoServer, videoClient := net.Pipe()
	defer videoServer.Close()
	result.VideoConn = videoClient

	launcher := &mockLauncherForAPI{result: result}
	sess := session.NewDeviceSession("ABC123", nil, launcher)
	err := sess.Start(context.Background())
	require.NoError(t, err)

	entry.SetSession(sess)
	entry.SetState(session.StateActive)

	router := setupWSVideoRouter(registry)
	server := httptest.NewServer(router)
	defer server.Close()

	// Connect WebSocket client
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devices/ABC123/video"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err, "WebSocket upgrade should succeed")
	defer conn.CloseNow()

	// Read codec metadata (first 12 bytes)
	msgType, data, err := conn.Read(ctx)
	require.NoError(t, err, "Should receive codec metadata")
	assert.Equal(t, websocket.MessageBinary, msgType)
	assert.Len(t, data, 12, "Codec metadata should be 12 bytes")

	// Verify codec metadata contains expected values
	codecID := string(data[:4])
	assert.Equal(t, "h264", codecID, "Codec ID should be h264")

	// Write a video frame to the video connection
	// Frame header: 12 bytes (PTS with config+keyframe bits + size)
	framePayload := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05} // 6 bytes of payload
	var frameHeader [12]byte
	// Config packet flag (bit 63) + keyframe flag (bit 62) + PTS
	binary.BigEndian.PutUint64(frameHeader[:8], uint64(1<<63|1<<62|1234)) // config + keyframe + PTS=1234
	binary.BigEndian.PutUint32(frameHeader[8:12], uint32(len(framePayload)))

	// Write the frame to the video server side
	videoServer.Write(frameHeader[:])
	videoServer.Write(framePayload)

	// Read the frame from the WebSocket
	msgType, data, err = conn.Read(ctx)
	require.NoError(t, err, "Should receive video frame")
	assert.Equal(t, websocket.MessageBinary, msgType)
	assert.Equal(t, 12+len(framePayload), len(data), "Frame message should be 12-byte header + payload")

	// Verify the header bytes match
	assert.Equal(t, frameHeader[:], data[:12], "Frame header should be preserved")

	// Verify the payload bytes match
	assert.Equal(t, framePayload, data[12:], "Frame payload should be preserved")

	// Close session and cleanup
	sess.Close(context.Background())
	videoClient.Close()
}