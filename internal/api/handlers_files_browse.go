// Package api — handlers_files_browse.go implements Phase 03.1 file browser
// non-streaming operations:
//
//	GET    /files?op=list             list directory entries (D-FB-01/D-FB-03)
//	GET    /files?op=stat             stat single entry (D-FB-01/D-FB-04)
//	POST   /files?op=mkdir           create directory (D-FB-01/D-FB-05)
//	PATCH  /files?op=rename&to=<dst>  rename entry (D-FB-01/D-FB-02/D-FB-11)
//
// Every handler validates the requested path BEFORE issuing any ADB call
// (D-11): ValidateDevicePath gates on cfg.Files.AllowedBasePaths. Failure
// returns 403 PATH_NOT_ALLOWED and the test suite asserts ZERO ADB calls
// for traversal inputs (VALIDATION.md property 1).
//
// Defence in depth: shell commands use shellQuote on cleaned paths. ValidateDevicePath
// already canonicalizes away shell metachars; the quoting is belt-and-braces.
//
// These handlers are read-mostly (list/stat/mkdir) and MUST NOT use
// DeviceEntry.WriteInFlight (Pitfall 9 — guard is for write ops only).
// RenameFile is a write op but does NOT need WriteInFlight because mv is
// atomic on POSIX and does not risk the destructive scope of rm -rf or bu backup.
package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ListFiles is the production wiring for GET /files?op=list.
func ListFiles(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return ListFilesForTest(registry, hostServices, cfg)
}

// StatFile is the production wiring for GET /files?op=stat.
func StatFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return StatFileForTest(registry, hostServices, cfg)
}

// MkdirFile is the production wiring for POST /files?op=mkdir.
func MkdirFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return MkdirFileForTest(registry, hostServices, cfg)
}

// MkdirForTest is an alias for MkdirFileForTest (the test scaffold in plan 01b
// used the shorter name). Both names refer to the same handler.
func MkdirForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return MkdirFileForTest(registry, runner, cfg)
}

// RenameFile is the production wiring for PATCH /files?op=rename.
func RenameFile(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return RenameFileForTest(registry, hostServices, cfg)
}

// ListFilesForTest builds the list handler with an injectable runner.
// Returns a JSON array of Entry objects matching D-FB-03 schema.
func ListFilesForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		cmd := "ls -lA --time-style=full-iso " + shellQuote(cleaned)
		out, err := runner.ShellRunRaw(r.Context(), serial, cmd)
		if err != nil {
			slog.Warn("files: ls failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		entries := ParseLSOutput(out, cleaned)
		writeJSON(w, http.StatusOK, entries)
	}
}

// StatFileForTest builds the stat handler with an injectable runner.
// Returns a single Entry object (D-FB-04). Uses ls -lAd to stat the entry
// itself (not its contents if it is a directory).
func StatFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		cmd := "ls -lAd --time-style=full-iso " + shellQuote(cleaned)
		out, err := runner.ShellRunRaw(r.Context(), serial, cmd)
		if err != nil {
			slog.Warn("files: stat ls failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		entries := ParseLSOutput(out, cleaned)
		if len(entries) != 1 {
			slog.Warn("files: stat returned unexpected entry count",
				"device", serial, "path", cleaned, "entries", len(entries))
			writeError(w, ErrListFailed)
			return
		}

		writeJSON(w, http.StatusOK, entries[0])
	}
}

// MkdirFileForTest builds the mkdir handler with an injectable runner.
// Idempotent per D-FB-05: returns {"path": ..., "existed": true|false}.
// No error if the directory already exists (mkdir -p semantics).
// Uses ErrListFailed for mkdir operational failure in v1 (grouped with
// ls/pm operational failures; document this in the handler header comment).
func MkdirFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		// Check existence first: test -d exits 0 if dir exists.
		testCmd := "test -d " + shellQuote(cleaned) + " && echo exists || true"
		out, err := runner.ShellRunRaw(r.Context(), serial, testCmd)
		if err != nil {
			slog.Warn("files: test -d failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrListFailed)
			return
		}
		existed := strings.Contains(string(out), "exists")

		// Create the directory (mkdir -p is idempotent).
		mkdirCmd := "mkdir -p " + shellQuote(cleaned)
		if _, err := runner.ShellRunRaw(r.Context(), serial, mkdirCmd); err != nil {
			slog.Warn("files: mkdir failed", "device", serial, "path", cleaned, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"path":    cleaned,
			"existed": existed,
		})
	}
}

// RenameFileForTest builds the rename handler with an injectable runner.
// Validates BOTH src and dst paths independently before any mv shell call
// (VALIDATION.md property 2 — dual-path invariant).
// Cross-filesystem renames (EXDEV) surface as 409 RENAME_CROSS_FS without
// fallback copy (D-FB-11). Other failures surface as 500 RENAME_FAILED with
// truncated stderr (D-ERR-01 addendum, T-03.1-02-04).
// Uses ShellV2Stream to capture stderr separately for EXDEV detection.
func RenameFileForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		srcRaw := r.URL.Query().Get("path")
		dstRaw := r.URL.Query().Get("to")

		// Dual-path validation (VALIDATION.md property 2):
		// Both src and dst must pass independently BEFORE any mv call.
		srcCleaned, err := ValidateDevicePath(srcRaw, cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}
		dstCleaned, err := ValidateDevicePath(dstRaw, cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		cmd := "mv " + shellQuote(srcCleaned) + " " + shellQuote(dstCleaned)

		// Use bounded context independent of r.Context() (D-08 pattern).
		renameCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Use ShellV2Stream to capture stderr separately for EXDEV detection.
		stdout, stderr, exitCh, err := runner.ShellV2Stream(renameCtx, serial, cmd)
		if err != nil {
			slog.Warn("files: mv shell-v2 stream open failed", "device", serial, "error", err)
			writeError(w, ErrADBUnavailable)
			return
		}
		_, _ = io.ReadAll(stdout) // mv has no stdout; drain anyway
		stderrBytes, _ := io.ReadAll(stderr)
		stdout.Close()
		stderr.Close()

		var exit int
		select {
		case exit = <-exitCh:
		case <-time.After(5 * time.Second):
			exit = -1
		}

		// EXDEV detection — case-insensitive substring match on stderr.
		low := strings.ToLower(string(stderrBytes))
		if exit != 0 && (strings.Contains(low, "cross-device") || strings.Contains(low, "exdev")) {
			writeError(w, ErrRenameCrossFS)
			return
		}

		// Other failures → ErrRenameFailed sentinel with truncated stderr (B3 fix,
		// D-ERR-01 addendum, T-03.1-02-04).
		if exit != 0 {
			msg := strings.TrimSpace(string(stderrBytes))
			if len(msg) > 256 {
				msg = msg[:256]
			}
			slog.Warn("files: mv failed",
				"device", serial, "src", srcCleaned, "dst", dstCleaned,
				"exit", exit, "stderr_excerpt", truncate(msg, 256))
			writeError(w, &DomainError{
				Code:       ErrRenameFailed.Code,
				HTTPStatus: ErrRenameFailed.HTTPStatus,
				Message:    ErrRenameFailed.Message + ": " + msg,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"path": dstCleaned,
			"from": srcCleaned,
		})
	}
}

// FilesDispatcherForTest builds a DELETE handler that dispatches between
// single-file (Phase 3) and recursive (Phase 03.1 D-FB-10) delete based on
// the ?recursive=1 query param. This is the handler the browse test router
// wires; the production wiring is plan 07's responsibility.
func FilesDispatcherForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return DeleteFileForTest(registry, runner, cfg)
}