package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhase3ErrorSentinels verifies Phase 3 D-19 sentinels: code, status,
// envelope shape. Mirrors errors_phase2_test.go.
func TestPhase3ErrorSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    *DomainError
		status int
		code   string
	}{
		{"path_not_allowed", ErrPathNotAllowed, http.StatusForbidden, "PATH_NOT_ALLOWED"},
		{"file_too_large", ErrFileTooLarge, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE"},
		{"install_failed", ErrInstallFailed, http.StatusInternalServerError, "INSTALL_FAILED"},
		{"device_busy", ErrDeviceBusy, http.StatusServiceUnavailable, "DEVICE_BUSY"},
		{"recording_failed", ErrRecordingFailed, http.StatusInternalServerError, "RECORDING_FAILED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tc.err)

			assert.Equal(t, tc.status, w.Code)

			var body errorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, tc.code, body.Error.Code)
			assert.NotEmpty(t, body.Error.Message)

			ct := w.Header().Get("Content-Type")
			assert.Equal(t, "application/json", ct)
		})
	}
}
