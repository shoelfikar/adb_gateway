// Package api — handlers_files.go implements OPS-08:
//
//	POST   /devices/{serial}/files?path=<absolute>                stream-push (D-13/D-14)
//	GET    /devices/{serial}/files?path=<absolute>                sync pull   (D-15)
//	DELETE /devices/{serial}/files?path=<absolute>               shell rm -f (D-16)
//	DELETE /devices/{serial}/files?path=<absolute>&recursive=1    shell rm -rf (D-FB-10)
//
// Recursive delete is opt-in; default behavior is single-file rm -f (Phase 3
// D-16). Single-flight applies ONLY to the recursive branch (Pitfall 9 —
// guard scope minimized).
//
// Every handler validates the requested path BEFORE issuing any ADB call
// (D-11): the path is single-URL-decoded, path.Clean'd, then prefix-checked
// against cfg.Files.AllowedBasePaths. Failure returns 403 PATH_NOT_ALLOWED
// and the test suite asserts ZERO ADB calls were made for traversal inputs
// (the security invariant in 03-VALIDATION.md).
//
// Defence in depth: DeleteFile additionally shell-quotes the cleaned path
// before splicing into `rm -f` or `rm -rf`. shellQuote wraps in single quotes
// and escapes any embedded single quote (`' -> '\''`) — this is belt-and-braces
// because ValidateDevicePath already canonicalizes shell metachars away.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// FileShellRunner is the minimal interface the file handlers need from the
// ADB transport. *adb.HostServices satisfies it (the methods exist and
// match by signature). Tests inject a fake (handlers_files_test.go).
type FileShellRunner interface {
	SyncPushReader(ctx context.Context, serial, dest string, src io.Reader, mode os.FileMode) error
	SyncPullWriter(ctx context.Context, serial, src string, dst io.Writer) error
	ShellRunRaw(ctx context.Context, serial, cmd string) ([]byte, error)
	ShellV2Stream(ctx context.Context, serial, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)
}

// UploadFile is the production wiring for POST /files.
func UploadFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return UploadFileForTest(registry, hostServices, cfg)
}

// DownloadFile is the production wiring for GET /files.
func DownloadFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return DownloadFileForTest(registry, hostServices, cfg)
}

// DeleteFile is the production wiring for DELETE /files.
func DeleteFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return DeleteFileForTest(registry, hostServices, cfg)
}

// UploadFileForTest builds the upload handler with an injectable runner.
func UploadFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		cleaned, err := ValidateDevicePath(r.URL.Query().Get("path"), cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		// Cap the request body at MaxUploadBytes (D-14). MaxBytesReader
		// returns *http.MaxBytesError after the limit; we surface that as
		// 413 FILE_TOO_LARGE.
		body := http.MaxBytesReader(w, r.Body, cfg.Files.MaxUploadBytes)
		defer body.Close()

		if err := runner.SyncPushReader(r.Context(), serial, cleaned, body, 0644); err != nil {
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) {
				writeError(w, ErrFileTooLarge)
				return
			}
			// Inspect the error message for the wrapped MaxBytes case
			// (some readers wrap, ours uses io.Copy in adb/shell.go).
			if strings.Contains(err.Error(), "http: request body too large") {
				writeError(w, ErrFileTooLarge)
				return
			}
			slog.Warn("files: push failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrPushFailed)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "uploaded",
			"path":   cleaned,
		})
	}
}

// DownloadFileForTest builds the download handler with an injectable runner.
func DownloadFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		cleaned, err := ValidateDevicePath(r.URL.Query().Get("path"), cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		base := path.Base(cleaned)
		// Quote-escape filename for the Content-Disposition header. Cleaned
		// paths cannot contain newlines (path.Clean strips them); we still
		// escape embedded quotes defensively.
		dispName := strings.ReplaceAll(base, "\"", "\\\"")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, dispName))

		if err := runner.SyncPullWriter(r.Context(), serial, cleaned, w); err != nil {
			slog.Warn("files: pull failed", "device", serial, "path", cleaned, "error", err)
			// Headers may already be sent; we cannot rewrite a domain envelope.
			return
		}
	}
}

// DeleteFileForTest builds the delete handler with an injectable runner.
// Supports ?recursive=1 for recursive tree delete (D-FB-10), gated by
// DeviceEntry.WriteInFlight single-flight. Default (no recursive flag)
// preserves Phase 3 single-file rm -f behavior unchanged.
//
// Recursive delete of a base directory (e.g. /sdcard) is explicitly blocked
// to prevent accidental wipe of the entire user storage. The base directory
// path must be inside an allowed base, not the base itself.
func DeleteFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		cleaned, err := ValidateDevicePath(r.URL.Query().Get("path"), cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		recursive := r.URL.Query().Get("recursive") == "1"

		if recursive {
			// Block recursive delete of an allowed base directory (e.g. /sdcard).
			// This prevents accidental `rm -rf /sdcard` which would wipe all user data.
			if IsBaseDirPath(cleaned, cfg.Files.AllowedBasePaths) {
				writeError(w, ErrBaseDirDelete)
				return
			}

			// Acquire WriteInFlight single-flight gate (Pitfall 9).
			if !entry.WriteInFlight.CompareAndSwap(false, true) {
				writeError(w, ErrDeviceBusy)
				return
			}
			defer entry.WriteInFlight.Store(false)

			// Bounded ctx independent of r.Context() (Pitfall 3 — recursive
			// delete must survive client disconnect).
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cmd := "rm -rf " + shellQuote(cleaned)
			if _, err := runner.ShellRunRaw(ctx, serial, cmd); err != nil {
				slog.Warn("files: rm -rf failed", "device", serial, "path", cleaned, "error", err)
				writeError(w, ErrADBUnavailable)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    "deleted",
				"path":      cleaned,
				"recursive": true,
			})
			return
		}

		// Phase 3 single-file rm -f (D-16) — unchanged.
		cmd := "rm -f " + shellQuote(cleaned)
		if _, err := runner.ShellRunRaw(r.Context(), serial, cmd); err != nil {
			slog.Warn("files: rm failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrADBUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "deleted",
			"path":   cleaned,
		})
	}
}

// validateSerial reads chi URL param "serial" and rejects malformed values.
// Returns (serial, true) on success or writes 404 and returns (_, false).
func validateSerial(w http.ResponseWriter, r *http.Request) (string, bool) {
	serial := chi.URLParam(r, "serial")
	if serial == "" || !serialPattern.MatchString(serial) {
		writeError(w, ErrDeviceNotFound)
		return "", false
	}
	return serial, true
}

// shellQuote wraps s in single quotes for safe inclusion in a shell command.
// Embedded single quotes are escaped as `'\''` (close, escaped, reopen).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}