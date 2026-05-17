package api

import (
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
)

// pkgPattern is the strict Android package-name regex per D-AM-02.
// Anchored: must start with a letter, segments separated by dots, each
// segment starts with a letter. This rejects shell metacharacters, digits
// at the start, and single-segment names.
var pkgPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z][a-zA-Z0-9_]*)+$`)

// validatePackage reads chi URL param "pkg" and rejects malformed values
// BEFORE any shell call is made (REQ-AM-PKG-VALIDATE invariant). The
// 256-byte cap is belt-and-braces against pathological regex inputs even
// though pkgPattern is anchored and linear.
func validatePackage(w http.ResponseWriter, r *http.Request) (string, bool) {
	pkg := chi.URLParam(r, "pkg")
	if pkg == "" || len(pkg) > 256 || !pkgPattern.MatchString(pkg) {
		writeError(w, ErrInvalidPackage)
		return "", false
	}
	return pkg, true
}