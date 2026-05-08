package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/obs"
	"github.com/pelni/adb-gateway/internal/session"
)

func TestRouter_Phase2RoutesMounted(t *testing.T) {
	cfg := testConfig()
	registry := session.NewRegistry()
	h := NewRouter(cfg, registry, nil, nil)

	for _, route := range []string{"/devices/abc/audio", "/devices/abc/control"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("X-API-Key", "test-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// Assert NOT 404 MethodNotAllowed (which chi returns for unmounted routes)
		require.NotEqual(t, http.StatusMethodNotAllowed, w.Code, "route %s not mounted", route)
	}

	for _, m := range []string{"POST", "PATCH", "DELETE"} {
		var body strings.Reader
		if m != "POST" {
			body = *strings.NewReader(`{"lease_id":"x"}`)
		}
		req := httptest.NewRequest(m, "/devices/abc/reservation", &body)
		req.Header.Set("X-API-Key", "test-key")
		if m != "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusMethodNotAllowed, w.Code, "%s /reservation not mounted", m)
	}
}

func TestMetricsCardinalityLock(t *testing.T) {
	body, err := os.ReadFile("../obs/metrics.go")
	require.NoError(t, err)
	// Check that label declarations don't include forbidden high-cardinality labels.
	// We check for label declarations in the var block, not in comments.
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip comment lines
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		for _, forbidden := range []string{"device_serial", "viewer_id", "session_id"} {
			assert.NotContains(t, trimmed, `"`+forbidden+`"`, "metrics.go must not declare %q label (D-18)", forbidden)
		}
	}
}

func TestMetricsRouteExposesPhase2Collectors(t *testing.T) {
	// Use a fresh prometheus registry to avoid conflicts with global state.
	// Create our own collectors and register them on a custom registry,
	// then serve metrics from that registry.
	testReg := prometheus.NewRegistry()
	obs.MustRegister(testReg)

	// CounterVec metrics need at least one label set to appear in exposition output.
	obs.DevicesTotal.WithLabelValues("idle").Add(0)
	obs.SessionsTotal.WithLabelValues("active").Add(0)
	obs.FramesEmittedTotal.WithLabelValues("video").Add(0)
	obs.FramesDroppedTotal.WithLabelValues("video").Add(0)
	obs.ReverseTunnelReconcileTotal.WithLabelValues("success").Add(0)

	handler := promhttp.HandlerFor(testReg, promhttp.HandlerOpts{})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "/metrics should return 200")
	body := w.Body.String()
	for _, name := range []string{
		"gateway_devices_total",
		"gateway_sessions_total",
		"gateway_frames_emitted_total",
		"gateway_frames_dropped_total",
		"gateway_adb_call_seconds",
		"gateway_reverse_tunnel_reconcile_total",
	} {
		assert.Contains(t, body, name, "metric %s missing from /metrics", name)
	}
}

func TestCORSAndAuthMiddleware(t *testing.T) {
	cfg := testConfig()
	registry := session.NewRegistry()
	h := NewRouter(cfg, registry, nil, nil)

	// Public endpoints should work without auth
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// /metrics should work without auth
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Protected endpoints should require auth
	req = httptest.NewRequest(http.MethodGet, "/devices/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// With auth, should work
	req = httptest.NewRequest(http.MethodGet, "/devices/", nil)
	req.Header.Set("X-API-Key", "test-key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}