// Package api — handlers_files_folder.go implements D-FB-06/07/08/09:
//
//   POST /devices/{serial}/files?path=<root>&op=upload-folder
//   GET  /devices/{serial}/files?path=<root>&op=download-folder
//
// No on-device tar dependency. Gateway iterates request-body tar entries
// (upload) or walks the directory via ls -lA -R (download). NDJSON progress
// is the upload response body; tar is the download response body — never
// mixed (RESEARCH.md Pattern 3/4 trade-off).
//
// Upload contract (NDJSON completeness):
//
//	Every tar entry produces exactly one NDJSON progress line.
//	The final line is always a "summary" line with ok/err/total_bytes keys.
//	Error lines from early-abort COUNT as per-entry lines:
//	  len(lines) == entries_attempted + 1
//	where entries_attempted = number of distinct tar headers the loop saw
//	before exit, and the +1 is the summary line.
//
// Known limitation (download, Pitfall 1): if a file is truncated on-device
// between the ls walk and the sync-pull, the tar stream becomes malformed at
// that entry boundary — client sees a short read. Re-stat per-file would add
// 1 ADB roundtrip per file and is deferred for v1.
package api

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// UploadFolder is the production wiring for POST /files?op=upload-folder.
func UploadFolder(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return UploadFolderForTest(registry, hostServices, cfg)
}

// DownloadFolder is the production wiring for GET /files?op=download-folder.
func DownloadFolder(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return DownloadFolderForTest(registry, hostServices, cfg)
}

// UploadFolderForTest builds the upload-folder handler with an injectable runner.
// Accepts an application/x-tar request body, iterates entries, issues one
// SyncPushReader per regular file, and emits NDJSON progress as the response body.
//
// Security invariants:
//   - Root path validated via ValidateDevicePath BEFORE any I/O.
//   - Per-entry path cleaned + re-validated after path.Join (T-03.1-03-01).
//   - MaxBytesReader caps the tar stream (T-03.1-03-02).
//   - WriteInFlight single-flight gate (T-03.1-03-03).
//   - Non-regular tar entries (symlink/block/char/fifo) skipped with UNSUPPORTED_ENTRY (T-03.1-03-05).
func UploadFolderForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		// Validate root path BEFORE any I/O (T-03.1-03-01).
		root, err := ValidateDevicePath(r.URL.Query().Get("path"), cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		// Acquire WriteInFlight single-flight (this IS a write op per Pitfall 9).
		if !entry.WriteInFlight.CompareAndSwap(false, true) {
			writeError(w, ErrDeviceBusy)
			return
		}
		defer entry.WriteInFlight.Store(false)

		// Set NDJSON response headers BEFORE first body write (Pitfall 10).
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		emit := func(v any) {
			_ = enc.Encode(v)
			if flusher != nil {
				flusher.Flush()
			}
		}

		// Bound the upload ctx via context.Background() so client disconnect
		// doesn't abort mid-push (mirror handlers_apk.go D-08 pattern).
		timeout := 10 * time.Minute
		uploadCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Wrap body with MaxBytesReader (T-03.1-03-02).
		body := http.MaxBytesReader(w, r.Body, cfg.Files.MaxUploadBytes)
		defer body.Close()
		tr := tar.NewReader(body)

		var okCount, errCount int
		var totalBytes int64

		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				// MaxBytesReader overflow surfaces here mid-stream.
				var mbErr *http.MaxBytesError
				if errors.As(err, &mbErr) || strings.Contains(err.Error(), "http: request body too large") {
					emit(map[string]any{"path": "", "status": "err", "error": "FILE_TOO_LARGE"})
					errCount++
					break
				}
				emit(map[string]any{"path": "", "status": "err", "error": "TAR_CORRUPT: " + err.Error()})
				errCount++
				break
			}

			// Defeat tar-bomb path-escape (D-FB-09 belt-and-braces, T-03.1-03-01).
			// Check original name for ".." — even though path.Clean resolves them,
			// any tar entry containing ".." is suspicious and rejected.
			if strings.Contains(hdr.Name, "..") {
				emit(map[string]any{"path": hdr.Name, "status": "err", "error": "UNSUPPORTED_ENTRY"})
				errCount++
				continue
			}
			// Reject absolute paths in tar entries — these are suspicious (T-03.1-03-01).
			// path.Clean anchors at "/" which would lose the absolute prefix info.
			if strings.HasPrefix(hdr.Name, "/") {
				emit(map[string]any{"path": hdr.Name, "status": "err", "error": "UNSUPPORTED_ENTRY"})
				errCount++
				continue
			}
			cleanRel := path.Clean("/" + hdr.Name)
			// Belt-and-braces: reject if Clean somehow preserved "..".
			if strings.Contains(cleanRel, "..") {
				emit(map[string]any{"path": hdr.Name, "status": "err", "error": "UNSUPPORTED_ENTRY"})
				errCount++
				continue
			}
			dest := path.Join(root, strings.TrimPrefix(cleanRel, "/"))

			// Per-entry allowlist re-check — root was validated, but path.Join
			// could land outside (T-03.1-03-01 extra defence).
			if _, verr := ValidateDevicePath(dest, cfg.Files.AllowedBasePaths); verr != nil {
				emit(map[string]any{"path": hdr.Name, "status": "err", "error": "PATH_NOT_ALLOWED"})
				errCount++
				continue
			}

			switch hdr.Typeflag {
			case tar.TypeDir:
				if _, err := runner.ShellRunRaw(uploadCtx, serial, "mkdir -p "+shellQuote(dest)); err != nil {
					emit(map[string]any{"path": hdr.Name, "status": "err", "error": "MKDIR_FAILED"})
					errCount++
					continue
				}
				emit(map[string]any{"path": hdr.Name, "status": "ok", "bytes": int64(0)})
				okCount++
			case tar.TypeReg:
				// Ensure parent dir exists — tar entry order is not guaranteed
				// parents-first.
				_, _ = runner.ShellRunRaw(uploadCtx, serial, "mkdir -p "+shellQuote(path.Dir(dest)))
				if err := runner.SyncPushReader(uploadCtx, serial, dest, tr, 0644); err != nil {
					emit(map[string]any{"path": hdr.Name, "status": "err", "error": "PUSH_FAILED: " + err.Error()})
					errCount++
					continue
				}
				emit(map[string]any{"path": hdr.Name, "status": "ok", "bytes": hdr.Size})
				okCount++
				totalBytes += hdr.Size
			default:
				// tar.TypeSymlink / TypeLink / TypeBlock / TypeChar / TypeFifo —
				// SKIP per D-FB-09 (T-03.1-03-05).
				emit(map[string]any{"path": hdr.Name, "status": "err", "error": "UNSUPPORTED_ENTRY"})
				errCount++
			}
		}

		// Summary line is ALWAYS emitted, even on early abort (NDJSON completeness).
		emit(map[string]any{"summary": map[string]any{"ok": okCount, "err": errCount, "total_bytes": totalBytes}})
	}
}

// DownloadFolderForTest builds the download-folder handler with an injectable
// runner. Walks the directory via ls -lA -R, parses into a flat entry list,
// then streams a tar response with one SyncPullWriter per regular file.
//
// Does NOT acquire WriteInFlight — this is a READ op (Pitfall 9; minimize
// guard scope).
//
// Security: root path validated via ValidateDevicePath BEFORE any ADB call.
// Non-regular entries (symlinks, devices, sockets) are silently skipped in
// the tar stream (D-FB-09, T-03.1-03-04) but reported in X-Skipped-Entries
// response header (W3).
func DownloadFolderForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate root path BEFORE any ADB call.
		root, err := ValidateDevicePath(r.URL.Query().Get("path"), cfg.Files.AllowedBasePaths)
		if err != nil {
			writeError(w, ErrPathNotAllowed)
			return
		}

		// Bounded ctx independent of r.Context() (D-08 pattern).
		// Recursive ls + pull operations may be long-running; client
		// disconnect should not abort them mid-transfer.
		dlCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		// Walk the directory recursively.
		walkCmd := "ls -lA -R --time-style=full-iso " + shellQuote(root)
		out, err := runner.ShellRunRaw(dlCtx, serial, walkCmd)
		if err != nil {
			slog.Warn("files: ls -R failed", "device", serial, "path", root, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		entries := parseLSRecursive(out, root)

		// Pass 1: scan for non-regular entries to populate X-Skipped-Entries (W3).
		var skipped []string
		for _, e := range entries {
			rel := strings.TrimPrefix(e.Path, root)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				continue
			}
			if e.Type != "file" && e.Type != "dir" {
				skipped = append(skipped, rel)
			}
		}
		if len(skipped) > 0 {
			hdr := strings.Join(skipped, ",")
			// Truncate at 4 KiB with "...+N more" suffix to bound header size.
			const maxHeader = 4096
			if len(hdr) > maxHeader {
				trunc := hdr[:maxHeader]
				if idx := strings.LastIndex(trunc, ","); idx > 0 {
					trunc = trunc[:idx]
				}
				remaining := len(skipped) - strings.Count(trunc, ",") - 1
				hdr = trunc + fmt.Sprintf(",...+%d more", remaining)
			}
			w.Header().Set("X-Skipped-Entries", hdr)
		}

		// Set tar response headers (must come AFTER X-Skipped-Entries so that
		// header lands before tar.NewWriter commits headers via first write).
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, path.Base(root)))
		w.Header().Set("X-Accel-Buffering", "no")

		tw := tar.NewWriter(w)
		defer tw.Close()

		// Pass 2: emit the tar.
		for _, e := range entries {
			rel := strings.TrimPrefix(e.Path, root)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				continue // skip the root itself
			}
			switch e.Type {
			case "dir":
				_ = tw.WriteHeader(&tar.Header{
					Name:     rel + "/",
					Mode:     0755,
					Typeflag: tar.TypeDir,
					ModTime:  e.MTime,
				})
			case "file":
				if err := tw.WriteHeader(&tar.Header{
					Name:     rel,
					Mode:     0644,
					Size:     e.Size,
					Typeflag: tar.TypeReg,
					ModTime:  e.MTime,
				}); err != nil {
					return // tar stream potentially malformed; client sees short read
				}
				if err := runner.SyncPullWriter(dlCtx, serial, e.Path, tw); err != nil {
					slog.Warn("download-folder: pull failed", "device", serial, "path", e.Path, "error", err)
					return // partial transfer surfaces as short read at entry boundary
				}
			default:
				// symlink / device / socket — SKIP per D-FB-09.
				// Already accounted for in X-Skipped-Entries above.
				continue
			}
		}
	}
}

// parseLSRecursive parses the output of `ls -lA -R --time-style=full-iso`
// into a flat slice of Entry with absolute Path fields. Splits on blank lines
// and "<dir>:" section headers per toybox -R output format:
//
//	<dir>:\n<entries>\n\n<subdir>:\n...
func parseLSRecursive(out []byte, root string) []Entry {
	var entries []Entry
	s := bufio.NewScanner(strings.NewReader(string(out)))
	var sectionDir string

	for s.Scan() {
		line := s.Text()

		// Section header: "/sdcard/subdir:" or "." etc.
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "-") &&
			!strings.HasPrefix(line, "d") && !strings.HasPrefix(line, "l") &&
			!strings.HasPrefix(line, "total ") {
			// Strip the trailing colon to get the directory path.
			sectionDir = strings.TrimSuffix(line, ":")
			continue
		}

		// Blank line — just skip.
		if strings.TrimSpace(line) == "" {
			continue
		}

		dir := sectionDir
		if dir == "" {
			dir = root
		}

		entry, ok := ParseLSLine(line, dir)
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries
}