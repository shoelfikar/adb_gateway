package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// setupReservationRouter creates a test router with reservation endpoints.
func setupReservationRouter(registry *session.Registry) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/devices/{serial}/reservation", CreateReservation(registry))
	r.Patch("/devices/{serial}/reservation", ExtendReservation(registry))
	r.Delete("/devices/{serial}/reservation", ReleaseReservation(registry))
	return r
}

func TestReservationCreate(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	// Create an entry for the device
	registry.GetOrCreate("ABC123")

	// Acquire reservation with API key
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req.Header.Set("X-API-Key", "test-key-12345")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp leaseResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.LeaseID, "lease_id should be populated")
	assert.NotEmpty(t, resp.ExpiresAt, "expires_at should be populated")
}

func TestReservationCreateConflict(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	registry.GetOrCreate("ABC123")

	// First request: should succeed
	req1 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req1.Header.Set("X-API-Key", "key-1")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusCreated, w1.Code)

	// Second request: should conflict (409)
	req2 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req2.Header.Set("X-API-Key", "key-2")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestReservationExtend(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	registry.GetOrCreate("ABC123")

	// Acquire first
	req1 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req1.Header.Set("X-API-Key", "test-key")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusCreated, w1.Code)

	var createResp leaseResponse
	json.Unmarshal(w1.Body.Bytes(), &createResp)

	// Extend with matching lease ID
	body := leaseRequestBody{LeaseID: createResp.LeaseID}
	bodyBytes, _ := json.Marshal(body)
	req2 := httptest.NewRequest(http.MethodPatch, "/devices/ABC123/reservation", bytes.NewReader(bodyBytes))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusOK, w2.Code)
	var extendResp leaseResponse
	json.Unmarshal(w2.Body.Bytes(), &extendResp)
	assert.Equal(t, createResp.LeaseID, extendResp.LeaseID)
}

func TestReservationExtendMismatch(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	registry.GetOrCreate("ABC123")

	// Acquire first
	req1 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req1.Header.Set("X-API-Key", "test-key")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusCreated, w1.Code)

	// Extend with WRONG lease ID
	body := leaseRequestBody{LeaseID: "wrong-lease-id"}
	bodyBytes, _ := json.Marshal(body)
	req2 := httptest.NewRequest(http.MethodPatch, "/devices/ABC123/reservation", bytes.NewReader(bodyBytes))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusForbidden, w2.Code)
}

func TestReservationRelease(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	registry.GetOrCreate("ABC123")

	// Acquire first
	req1 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req1.Header.Set("X-API-Key", "test-key")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusCreated, w1.Code)

	var createResp leaseResponse
	json.Unmarshal(w1.Body.Bytes(), &createResp)

	// Release with matching lease ID via X-Lease-ID header
	req2 := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/reservation", nil)
	req2.Header.Set("X-Lease-ID", createResp.LeaseID)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusNoContent, w2.Code)

	// Second DELETE should return 403 (lease no longer valid)
	req3 := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/reservation", nil)
	req3.Header.Set("X-Lease-ID", createResp.LeaseID)
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)

	assert.Equal(t, http.StatusForbidden, w3.Code)
}

func TestReservationDeviceNotFound(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/devices/NONEXISTENT/reservation", nil)
		req.Header.Set("X-API-Key", "test-key")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "%s should return 404 for unknown device", method)
	}
}

func TestReservationLeaseAcquireBindsToOwnerKey(t *testing.T) {
	registry := session.NewRegistryWithOpts(session.RegistryOpts{
		LeaseTTL: 60 * time.Second,
	})
	router := setupReservationRouter(registry)

	entry := registry.GetOrCreate("ABC123")

	// Acquire with key-1
	req1 := httptest.NewRequest(http.MethodPost, "/devices/ABC123/reservation", nil)
	req1.Header.Set("X-API-Key", "key-1")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusCreated, w1.Code)

	var resp leaseResponse
	json.Unmarshal(w1.Body.Bytes(), &resp)

	// Verify the owner key was set (via LeaseManager.Snapshot)
	mgr := entry.GetLeaseManager()
	lease, ok := mgr.Snapshot()
	require.True(t, ok, "lease should exist")
	// Owner key is SHA-256 hex of "key-1"
	assert.NotEmpty(t, lease.OwnerKey, "owner key should be set")
	assert.Len(t, lease.OwnerKey, 64, "owner key should be 64-char hex (SHA-256)")
}