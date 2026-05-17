package api

import "net/http"

// CORS returns a middleware that sets permissive CORS headers.
// For Phase 1 this is a dev/test convenience — pelni_server will proxy
// in production. Allowed origins are configurable; default allows all.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	// Empty list = allow all origins (dev mode).
	allowAll := len(allowedOrigins) == 0

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = "*"
			}

			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				for _, ao := range allowedOrigins {
					if ao == origin {
						w.Header().Set("Access-Control-Allow-Origin", origin)
						break
					}
				}
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-Lease-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")

			// Preflight.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}