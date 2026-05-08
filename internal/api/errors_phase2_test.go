package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase2ErrorSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    *DomainError
		status int
		code   string
	}{
		{"lease_required", ErrLeaseRequired, http.StatusForbidden, "LEASE_REQUIRED"},
		{"lease_invalid", ErrLeaseInvalid, http.StatusForbidden, "LEASE_INVALID"},
		{"lease_held_by_other", ErrLeaseHeldByOther, http.StatusConflict, "LEASE_HELD_BY_OTHER"},
		{"not_controller", ErrNotController, http.StatusForbidden, "NOT_CONTROLLER"},
		{"audio_unavailable", ErrAudioUnavailable, http.StatusNotFound, "AUDIO_UNAVAILABLE"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tc.err)

			assert.Equal(t, tc.status, w.Code)

			var body errorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, tc.code, body.Error.Code)

			ct := w.Header().Get("Content-Type")
			assert.Equal(t, "application/json", ct)
		})
	}
}