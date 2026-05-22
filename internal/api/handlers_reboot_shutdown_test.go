package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// setupPowerTestRouter creates a chi router with reboot/shutdown routes for testing.
// shellFn is the injectable shell command function.
func setupPowerTestRouter(registry *session.Registry, shellFn deviceShellFn) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}", func(r chi.Router) {
		r.Post("/reboot", RebootDeviceForTest(registry, shellFn))
		r.Post("/shutdown", ShutdownDeviceForTest(registry, shellFn))
	})
	return r
}

func TestRebootDeviceSuccess(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	var shellCalls atomic.Int32
	shellFn := func(ctx context.Context, cmd string) (string, error) {
		shellCalls.Add(1)
		assert.Equal(t, "reboot", cmd)
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reboot", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]string
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ABC123", resp["serial"])
	assert.Equal(t, "Device reboot initiated", resp["message"])
	assert.Equal(t, int32(1), shellCalls.Load())

	// Session should be cleared and state reset to idle
	assert.Nil(t, entry.GetSession())
	assert.Equal(t, session.StateIdle, entry.GetState())
}

func TestRebootDeviceWithActiveSession(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create an active session
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

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reboot", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	// Session should be cleared after reboot
	assert.Nil(t, entry.GetSession())
	assert.Equal(t, session.StateIdle, entry.GetState())

	client.Close()
}

func TestRebootDeviceNotFound(t *testing.T) {
	registry := session.NewRegistry()

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/NOTEXIST/reboot", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var errResp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "DEVICE_NOT_FOUND", errResp.Error.Code)
}

func TestRebootDeviceInvalidSerial(t *testing.T) {
	registry := session.NewRegistry()

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC%20DEF/reboot", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRebootDeviceShellFailed(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", context.DeadlineExceeded
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reboot", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	var errResp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "REBOOT_FAILED", errResp.Error.Code)
}

func TestShutdownDeviceSuccess(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	var shellCalls atomic.Int32
	shellFn := func(ctx context.Context, cmd string) (string, error) {
		shellCalls.Add(1)
		assert.Equal(t, "reboot -p", cmd)
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/shutdown", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]string
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ABC123", resp["serial"])
	assert.Equal(t, "Device shutdown initiated", resp["message"])
	assert.Equal(t, int32(1), shellCalls.Load())

	assert.Nil(t, entry.GetSession())
	assert.Equal(t, session.StateIdle, entry.GetState())
}

func TestShutdownDeviceWithActiveSession(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create an active session
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

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/shutdown", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	assert.Nil(t, entry.GetSession())
	assert.Equal(t, session.StateIdle, entry.GetState())

	client.Close()
}

func TestShutdownDeviceNotFound(t *testing.T) {
	registry := session.NewRegistry()

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", nil
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/NOTEXIST/shutdown", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var errResp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "DEVICE_NOT_FOUND", errResp.Error.Code)
}

func TestShutdownDeviceShellFailed(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateIdle)

	shellFn := func(ctx context.Context, cmd string) (string, error) {
		return "", context.DeadlineExceeded
	}

	router := setupPowerTestRouter(registry, shellFn)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/shutdown", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	var errResp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, "SHUTDOWN_FAILED", errResp.Error.Code)
}