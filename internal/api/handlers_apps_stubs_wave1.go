// Package api — handlers_apps_stubs_wave1.go provides stub implementations
// for Wave 1 app-manager handler symbols that plans 05/06 will implement.
// These stubs exist solely so the phase031_wave1 test suite compiles; they
// return 501 Not Implemented. Remove this file once all Wave 1 plans land.
package api

import (
	"net/http"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ExportAPKForTest is a stub for plan 06. Returns 501.
func ExportAPKForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "ExportAPK: plan 06"})
	}
}

