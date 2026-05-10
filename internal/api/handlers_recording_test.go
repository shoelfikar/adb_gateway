package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// setupRecordingRouter wires the three /recordings handlers for tests.
func setupRecordingRouter(t *testing.T, registry *session.Registry, dir string) *chi.Mux {
	t.Helper()
	cfg := testConfig()
	cfg.Recording.Dir = dir
	cfg.Recording.MaxFileBytes = 0
	cfg.Recording.Container = "mkv"

	r := chi.NewRouter()
	r.Route("/devices/{serial}", func(r chi.Router) {
		r.Post("/recordings", StartRecording(registry, cfg))
		r.Get("/recordings", ListRecordings(registry))
		r.Delete("/recordings/{id}", StopRecording(registry))
	})
	return r
}

// activeSessionWithHub builds a DeviceSession with a running Hub for tests.
// Returns the cancel func; caller must defer cancel() to stop the Hub.
func activeSessionWithHub(t *testing.T, registry *session.Registry, serial string) context.CancelFunc {
	t.Helper()
	hub := session.NewHub(session.HubOpts{
		Stream:              "video",
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.Default(),
	})
	hub.SetCodecMeta([12]byte{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)

	sess := session.NewActiveSessionForTest(serial, hub)
	entry := registry.GetOrCreate(serial)
	entry.SetSession(sess)
	entry.SetState(session.StateActive)
	return cancel
}

func TestStartStopRecording(t *testing.T) {
	dir := t.TempDir()
	registry := session.NewRegistry()
	stopHub := activeSessionWithHub(t, registry, "ABC123")
	defer stopHub()

	r := setupRecordingRouter(t, registry, dir)

	// POST /recordings
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	var startResp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &startResp))
	id, _ := startResp["recording_id"].(string)
	require.NotEmpty(t, id, "recording_id must be returned")
	assert.Contains(t, startResp, "path")

	// GET /recordings should now list one entry.
	req = httptest.NewRequest(http.MethodGet, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var listResp []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.Len(t, listResp, 1, "exactly one active recording")

	// DELETE /recordings/{id}
	req = httptest.NewRequest(http.MethodDelete, "/devices/ABC123/recordings/"+id, nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var stopResp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &stopResp))
	assert.Equal(t, id, stopResp["recording_id"])
	assert.Contains(t, stopResp, "path")
	assert.Contains(t, stopResp, "bytes")
	assert.Contains(t, stopResp, "frames")
	assert.Contains(t, stopResp, "dropped")

	// GET /recordings now empty.
	req = httptest.NewRequest(http.MethodGet, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.Len(t, listResp, 0, "active recordings must be empty after stop")
}

func TestRecordingConcurrentRejected(t *testing.T) {
	dir := t.TempDir()
	registry := session.NewRegistry()
	stopHub := activeSessionWithHub(t, registry, "ABC123")
	defer stopHub()

	r := setupRecordingRouter(t, registry, dir)

	// First POST → 201.
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Second POST → 503 DEVICE_BUSY.
	req = httptest.NewRequest(http.MethodPost, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "DEVICE_BUSY")
}

func TestRecordingDeviceNotActive(t *testing.T) {
	dir := t.TempDir()
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateIdle)

	r := setupRecordingRouter(t, registry, dir)
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/recordings", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusCreated, w.Code, "recording on non-Active session must be rejected")
}

func TestRecordingNotFound(t *testing.T) {
	dir := t.TempDir()
	registry := session.NewRegistry()
	stopHub := activeSessionWithHub(t, registry, "ABC123")
	defer stopHub()

	r := setupRecordingRouter(t, registry, dir)
	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/recordings/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "RECORDING_NOT_FOUND")
}
