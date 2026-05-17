package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePackage(t *testing.T) {
	cases := []struct {
		name    string
		pkg     string
		valid   bool
		wantErr string // expected Code in response body for invalid cases
	}{
		{"valid_simple", "com.foo.bar", true, ""},
		{"valid_min_two_segments", "a.b", true, ""},
		{"valid_underscore", "com.foo_bar.baz", true, ""},
		{"digit_start", "123.bad", false, "INVALID_PACKAGE"},
		{"underscore_start", "_foo.bar", false, "INVALID_PACKAGE"},
		{"no_dot", "nodot", false, "INVALID_PACKAGE"},
		{"trailing_dot", "com.foo.", false, "INVALID_PACKAGE"},
		{"semicolon_injection", "com.foo;rm", false, "INVALID_PACKAGE"},
		{"over_256_chars", strings.Repeat("a", 257), false, "INVALID_PACKAGE"},
		{"exactly_256_chars", strings.Repeat("a", 128) + "." + strings.Repeat("b", 127), true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Get("/apps/{pkg}", func(w http.ResponseWriter, req *http.Request) {
				pkg, ok := validatePackage(w, req)
				if !ok {
					return
				}
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(pkg))
			})

			req := httptest.NewRequest(http.MethodGet, "/apps/"+tc.pkg, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if tc.valid {
				assert.Equal(t, http.StatusOK, w.Code, "expected 200 for valid pkg %q", tc.pkg)
				assert.Equal(t, tc.pkg, w.Body.String())
			} else {
				assert.NotEqual(t, http.StatusOK, w.Code, "expected non-200 for invalid pkg %q", tc.pkg)
				if tc.wantErr != "" {
					assert.Contains(t, w.Body.String(), tc.wantErr)
				}
			}
		})
	}
}

func TestValidatePackageRegexDirect(t *testing.T) {
	// Test the regex directly for cases that can't be sent via HTTP
	// (null bytes, spaces, and other URL-unsafe chars panic httptest
	// or get URL-encoded before reaching chi).
	cases := []struct {
		name  string
		pkg   string
		valid bool
	}{
		{"empty", "", false},
		{"null_byte", "com.foo\x00.bar", false},
		{"space_in_name", "com.foo bar", false},
		{"shell_meta_amp", "com.foo&bar", false},
		{"shell_meta_pipe", "com.foo|bar", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched := tc.pkg != "" && len(tc.pkg) <= 256 && pkgPattern.MatchString(tc.pkg)
			if tc.valid {
				assert.True(t, matched, "expected %q to be valid", tc.pkg)
			} else {
				assert.False(t, matched, "expected %q to be invalid", tc.pkg)
			}
		})
	}
}

func TestValidatePackageEmptyURLParam(t *testing.T) {
	// When chi URL param is empty (e.g., route without {pkg}),
	// validatePackage should still return false.
	r := chi.NewRouter()
	r.Get("/apps/{pkg}", func(w http.ResponseWriter, req *http.Request) {
		_, ok := validatePackage(w, req)
		if !ok {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Empty pkg param via trailing slash
	req := httptest.NewRequest(http.MethodGet, "/apps/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Should be a non-200 response (either 404 from chi or 400 from validation)
	require.NotEqual(t, http.StatusOK, w.Code)
}