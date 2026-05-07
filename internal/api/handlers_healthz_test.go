package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthzReturnsOK(t *testing.T) {
	handler := Healthz("1.0.0", "abc123def")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp healthResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err, "response should be valid JSON")

	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, "1.0.0", resp.Version)
	assert.Equal(t, "abc123def", resp.BuildSHA)
	assert.Equal(t, "3.3.4", resp.SCRCPYVersion)
}

func TestHealthzReturnsCorrectContentType(t *testing.T) {
	handler := Healthz("dev", "unknown")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

func TestHealthzDefaultValues(t *testing.T) {
	handler := Healthz("dev", "unknown")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "ok", resp["status"])
	assert.Equal(t, "dev", resp["version"])
	assert.Equal(t, "unknown", resp["build_sha"])
	assert.Equal(t, "3.3.4", resp["scrcpy_version"])
}

func TestHealthzContainsAllRequiredKeys(t *testing.T) {
	handler := Healthz("test-version", "test-sha")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	_, hasStatus := resp["status"]
	_, hasVersion := resp["version"]
	_, hasBuildSHA := resp["build_sha"]
	_, hasSCRCPYVersion := resp["scrcpy_version"]

	assert.True(t, hasStatus, "response should contain 'status' key")
	assert.True(t, hasVersion, "response should contain 'version' key")
	assert.True(t, hasBuildSHA, "response should contain 'build_sha' key")
	assert.True(t, hasSCRCPYVersion, "response should contain 'scrcpy_version' key")
}