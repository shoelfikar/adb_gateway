// Package api — handlers_apps_stubs_wave1.go provides stub implementations
// for Wave 1 app-manager handler symbols that plans 04/05/06 will implement.
// These stubs exist solely so the phase031_wave1 test suite compiles; they
// return 501 Not Implemented. Remove this file once all Wave 1 plans land.
package api

import (
	"net/http"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ListAppsForTest is a stub for plan 04. Returns 501.
func ListAppsForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "ListApps: plan 04"})
	}
}

// AppDetailsForTest is a stub for plan 04. Returns 501.
func AppDetailsForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "AppDetails: plan 04"})
	}
}

// UninstallAppForTest is a stub for plan 04. Returns 501.
func UninstallAppForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "UninstallApp: plan 04"})
	}
}

// BackupAppForTest is a stub for plan 05. Returns 501.
func BackupAppForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "BackupApp: plan 05"})
	}
}

// ExportAPKForTest is a stub for plan 06. Returns 501.
func ExportAPKForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "ExportAPK: plan 06"})
	}
}

// UploadFolderForTest is a stub for plan 03. Returns 501.
func UploadFolderForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "UploadFolder: plan 03"})
	}
}

// DownloadFolderForTest is a stub for plan 03. Returns 501.
func DownloadFolderForTest(_ *session.Registry, _ FileShellRunner, _ *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &DomainError{Code: "NOT_IMPLEMENTED", HTTPStatus: http.StatusNotImplemented, Message: "DownloadFolder: plan 03"})
	}
}