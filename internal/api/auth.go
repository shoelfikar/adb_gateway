package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
)

// APIKeyAuth returns a chi-compatible middleware that validates API keys using
// SHA-256 hashing and constant-time comparison. Keys are checked from the
// X-API-Key header first, falling back to the api_key query parameter.
//
// Per AUTH-04: failed auth returns identical 401 response regardless of reason
// (missing key, wrong key, wrong length). SHA-256 hashing before ConstantTimeCompare
// prevents the length-dependent early return inherent in ConstantTimeCompare.
func APIKeyAuth(primary, secondary string) func(http.Handler) http.Handler {
	// Pre-compute SHA-256 hashes at middleware creation time (not per-request).
	primaryHash := sha256.Sum256([]byte(primary))
	secondaryHash := sha256.Sum256([]byte(secondary))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = r.URL.Query().Get("api_key")
				if key != "" {
					slog.Warn("api key passed via query parameter; prefer X-API-Key header for security")
				}
			}
			// Browser WebSocket clients cannot set custom headers during upgrade.
			// Fall back to Sec-WebSocket-Protocol subprotocol ("api.<key>" format).
			if key == "" {
				key = extractAPIKeyFromSubprotocol(r)
			}

			if key == "" {
				writeError(w, ErrUnauthorized)
				return
			}

			keyHash := sha256.Sum256([]byte(key))
			matchPrimary := subtle.ConstantTimeCompare(keyHash[:], primaryHash[:]) == 1
			matchSecondary := subtle.ConstantTimeCompare(keyHash[:], secondaryHash[:]) == 1

			if !matchPrimary && !matchSecondary {
				writeError(w, ErrUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}