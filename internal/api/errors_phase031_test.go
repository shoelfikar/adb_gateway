package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhase031ErrorSentinels verifies Phase 03.1 D-ERR-01 sentinels: code,
// status, envelope shape. Mirrors errors_phase3_test.go.
func TestPhase031ErrorSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    *DomainError
		status int
		code   string
	}{
		{"rename_cross_fs", ErrRenameCrossFS, http.StatusConflict, "RENAME_CROSS_FS"},
		{"rename_failed", ErrRenameFailed, http.StatusInternalServerError, "RENAME_FAILED"},
		{"invalid_package", ErrInvalidPackage, http.StatusBadRequest, "INVALID_PACKAGE"},
		{"package_not_found", ErrPackageNotFound, http.StatusNotFound, "PACKAGE_NOT_FOUND"},
		{"uninstall_failed", ErrUninstallFailed, http.StatusInternalServerError, "UNINSTALL_FAILED"},
		{"backup_failed", ErrBackupFailed, http.StatusInternalServerError, "BACKUP_FAILED"},
		{"list_failed", ErrListFailed, http.StatusInternalServerError, "LIST_FAILED"},
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