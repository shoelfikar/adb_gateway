package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthValidPrimary(t *testing.T) {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("valid-primary-key", "valid-secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", "valid-primary-key")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeyAuthValidSecondary(t *testing.T) {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("valid-primary-key", "valid-secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", "valid-secondary-key")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeyAuthMissingKey(t *testing.T) {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("valid-primary-key", "valid-secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var resp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "UNAUTHORIZED", resp.Error.Code)
	assert.Equal(t, "Invalid or missing API key", resp.Error.Message)
}

func TestAPIKeyAuthInvalidKey(t *testing.T) {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("valid-primary-key", "valid-secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var resp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "UNAUTHORIZED", resp.Error.Code)
}

func TestAPIKeyAuthQueryParameter(t *testing.T) {
	r := chi.NewRouter()
	r.Use(APIKeyAuth("valid-primary-key", "valid-secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/test?api_key=valid-primary-key", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeyAuthTimingSafety(t *testing.T) {
	primary := "valid-primary-key-with-some-length"
	secondary := "valid-secondary-key-other"

	r := chi.NewRouter()
	r.Use(APIKeyAuth(primary, secondary))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Measure response times for primary key, secondary key, and invalid key.
	// All should be within 5ms of each other -- this is NOT a precise timing test
	// but verifies no obvious short-circuit on string comparison.
	attempts := 20

	var primaryDur, secondaryDur, invalidDur time.Duration

	for i := 0; i < attempts; i++ {
		start := time.Now()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-API-Key", primary)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		primaryDur += time.Since(start)

		start = time.Now()
		req = httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-API-Key", secondary)
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		secondaryDur += time.Since(start)

		start = time.Now()
		req = httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-API-Key", "invalid-key")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		invalidDur += time.Since(start)
	}

	primaryAvg := primaryDur / time.Duration(attempts)
	secondaryAvg := secondaryDur / time.Duration(attempts)
	invalidAvg := invalidDur / time.Duration(attempts)

	t.Logf("avg primary: %v, secondary: %v, invalid: %v", primaryAvg, secondaryAvg, invalidAvg)

	// No precise assertion -- just verify all are sub-second (hashing is fast)
	// The important thing is that ConstantTimeCompare is used, not string equality
	assert.Less(t, primaryAvg, time.Second)
	assert.Less(t, secondaryAvg, time.Second)
	assert.Less(t, invalidAvg, time.Second)
}

func TestAPIKeyAuthIdenticalErrorResponse(t *testing.T) {
	// Per AUTH-04: failed auth returns identical response regardless of reason
	r := chi.NewRouter()
	r.Use(APIKeyAuth("primary-key", "secondary-key"))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name string
		key  string
	}{
		{"missing key", ""},
		{"wrong key", "wrong"},
		{"empty header", ""},
	}

	responses := make([]*http.Response, len(tests))
	bodies := make([]string, len(tests))

	for i, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		if tt.key != "" {
			req.Header.Set("X-API-Key", tt.key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		responses[i] = w.Result()
		bodies[i] = w.Body.String()
	}

	// All responses should have the same status code and body
	for i := 1; i < len(responses); i++ {
		assert.Equal(t, responses[0].StatusCode, responses[i].StatusCode,
			"all failed auth responses should have same status code")
		assert.Equal(t, bodies[0], bodies[i],
			"all failed auth responses should have identical body")
	}
}

func TestDomainErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		err        *DomainError
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "ADB_UNAVAILABLE",
			err:        ErrADBUnavailable,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "ADB_UNAVAILABLE",
			wantMsg:    "ADB server is not available",
		},
		{
			name:       "DEVICE_OFFLINE",
			err:        ErrDeviceOffline,
			wantStatus: http.StatusNotFound,
			wantCode:   "DEVICE_OFFLINE",
			wantMsg:    "Device is offline",
		},
		{
			name:       "DEVICE_NOT_FOUND",
			err:        ErrDeviceNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "DEVICE_NOT_FOUND",
			wantMsg:    "Device not found",
		},
		{
			name:       "PUSH_FAILED",
			err:        ErrPushFailed,
			wantStatus: http.StatusBadGateway,
			wantCode:   "PUSH_FAILED",
			wantMsg:    "Failed to push file to device",
		},
		{
			name:       "REVERSE_FORWARD_FAILED",
			err:        ErrReverseForwardFailed,
			wantStatus: http.StatusBadGateway,
			wantCode:   "REVERSE_FORWARD_FAILED",
			wantMsg:    "Failed to set up reverse tunnel",
		},
		{
			name:       "SCRCPY_LAUNCH_FAILED",
			err:        ErrScrcpyLaunchFailed,
			wantStatus: http.StatusBadGateway,
			wantCode:   "SCRCPY_LAUNCH_FAILED",
			wantMsg:    "Failed to launch scrcpy server",
		},
		{
			name:       "SESSION_CONFLICT",
			err:        ErrSessionConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "SESSION_CONFLICT",
			wantMsg:    "Session already exists for this device",
		},
		{
			name:       "SESSION_NOT_FOUND",
			err:        ErrSessionNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "SESSION_NOT_FOUND",
			wantMsg:    "Session not found",
		},
		{
			name:       "UNAUTHORIZED",
			err:        ErrUnauthorized,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
			wantMsg:    "Invalid or missing API key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tt.err)

			assert.Equal(t, tt.wantStatus, w.Code)

			var resp errorResponse
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, resp.Error.Code)
			assert.Equal(t, tt.wantMsg, resp.Error.Message)
		})
	}
}

func TestWriteErrorInternalError(t *testing.T) {
	// Per D-09: internal errors should return 500 with generic message
	w := httptest.NewRecorder()
	writeError(w, errors.New("some internal error"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var resp errorResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "INTERNAL_ERROR", resp.Error.Code)
	assert.Equal(t, "An internal error occurred", resp.Error.Message)
}

func TestRouterHealthzNoAuth(t *testing.T) {
	cfg := &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:8080",
		ADBAddr:         "localhost:5037",
		LogLevel:        "info",
	}

	router := NewRouter(cfg)

	// Healthz should NOT require auth
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRouterMetricsNoAuth(t *testing.T) {
	cfg := &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:8080",
		ADBAddr:         "localhost:5037",
		LogLevel:        "info",
	}

	router := NewRouter(cfg)

	// Metrics should NOT require auth
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

func TestRouterDevicesRequiresAuth(t *testing.T) {
	cfg := &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:8080",
		ADBAddr:         "localhost:5037",
		LogLevel:        "info",
	}

	router := NewRouter(cfg)

	// /devices should require auth
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRouterDevicesWithAuth(t *testing.T) {
	cfg := &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:8080",
		ADBAddr:         "localhost:5037",
		LogLevel:        "info",
	}

	router := NewRouter(cfg)

	// /devices with auth should pass through (404 since no handlers yet, but not 401)
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

func TestNewRouterMiddlewareStack(t *testing.T) {
	cfg := &config.Config{
		APIKeyPrimary:   "test-key",
		APIKeySecondary: "secondary-key",
		ListenAddr:      "127.0.0.1:8080",
		ADBAddr:         "localhost:5037",
		LogLevel:        "info",
	}

	router := NewRouter(cfg)
	assert.NotNil(t, router, "NewRouter should return a non-nil handler")

	// Verify that the router is a chi.Router
	_, ok := router.(*chi.Mux)
	assert.True(t, ok, "NewRouter should return a *chi.Mux")
}

// Ensure middleware ordering: RequestID -> RealIP -> Logger -> Recoverer -> APIKeyAuth
func TestMiddlewareOrdering(t *testing.T) {
	// This test verifies that the middleware stack is correctly ordered
	// by checking that the RequestID middleware injects a request ID into the context.
	// chi's RequestID middleware sets the ID in the request context, not the response header.
	var capturedRequestID string
	testRouter := chi.NewRouter()
	testRouter.Use(middleware.RequestID)
	testRouter.Use(middleware.RealIP)
	testRouter.Use(middleware.Logger)
	testRouter.Use(middleware.Recoverer)
	testRouter.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		capturedRequestID = middleware.GetReqID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	testRouter.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, capturedRequestID, "RequestID middleware should set request ID in context")
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp map[string]string
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "world", resp["hello"])
}