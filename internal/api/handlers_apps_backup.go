// Package api — handlers_apps_backup.go implements D-AM-05:
//
//   POST /devices/{serial}/apps/{pkg}/backup
//
// Streams `bu backup -f - -noshared -apk <pkg>` stdout straight to the
// response body. The on-device confirmation tap is the user's problem
// (D-AM-05) — if the user cancels (or Android 14+ silently produces an
// empty stream — Pitfall 3), we surface BACKUP_FAILED as a proper JSON
// envelope by peeking 4 bytes BEFORE committing response headers.
//
// Single-flight via DeviceEntry.WriteInFlight (Pitfall 9 — write op).
// Bounded ctx via context.Background() so client disconnect does not
// abort an in-flight backup (D-08 pattern from handlers_apk.go).
package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// BackupApp is the production wiring for POST /apps/{pkg}/backup.
func BackupApp(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return BackupAppForTest(registry, hostServices, cfg)
}

// BackupAppForTest builds the backup handler with an injectable runner.
//
// Peek-before-headers contract (W5 STRICT):
//
//	The handler reads exactly 4 bytes from the bu backup stdout BEFORE
//	committing any HTTP response headers. If n < 4 (empty or partial stream),
//	it returns a 500 BACKUP_FAILED JSON envelope — headers NOT yet committed,
//	so the error envelope works correctly. If n == 4, it commits
//	Content-Type: application/octet-stream and streams the peek + remainder.
//
// This prevents the "200 OK with 0 bytes" misrepresentation that would occur
// if headers were set before peeking (Pitfall 3 / T-03.1-05-04).
func BackupAppForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		// Validate package name BEFORE any shell call (REQ-AM-PKG-VALIDATE).
		pkg, ok := validatePackage(w, r)
		if !ok {
			return
		}

		// WriteInFlight single-flight gate (Pitfall 9).
		if !entry.WriteInFlight.CompareAndSwap(false, true) {
			writeError(w, ErrDeviceBusy)
			return
		}
		defer entry.WriteInFlight.Store(false)

		// Bounded ctx independent of r.Context() (mirror APK install D-08).
		// Backup must survive client disconnect — bu continues until the
		// on-device user taps confirm/cancel or the timeout fires.
		timeout := time.Duration(cfg.APK.InstallTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		backupCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Start the backup stream (D-AM-05).
		cmd := "bu backup -f - -noshared -apk " + shellQuote(pkg)
		stdout, stderr, exitCh, err := runner.ShellV2Stream(backupCtx, serial, cmd)
		if err != nil {
			slog.Warn("backup: shell-v2 stream open failed", "device", serial, "pkg", pkg, "error", err)
			writeError(w, ErrBackupFailed)
			return
		}
		defer stdout.Close()
		defer stderr.Close()

		// W5 STRICT: Peek exactly 4 bytes BEFORE committing response headers.
		// Android Backup format magic is "ANDROID BACKUP\n" (15 bytes); a
		// legitimate backup always produces >=4 bytes on the first read.
		// Any partial read (n<4) means the stream produced fewer bytes than
		// expected — treat identically to an empty stream.
		peek := make([]byte, 4)
		n, _ := io.ReadFull(stdout, peek)
		if n != 4 {
			stderrBytes, _ := io.ReadAll(stderr)
			slog.Info("backup: short stream", "device", serial, "pkg", pkg, "peek_bytes", n, "stderr", truncate(string(stderrBytes), 256))
			writeError(w, ErrBackupFailed) // headers NOT yet committed -> JSON envelope works
			return
		}

		// n == 4 — commit headers now and stream peek + rest.
		filename := fmt.Sprintf("%s.ab", pkg)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(peek[:n]); err != nil {
			slog.Info("backup: client write failed (peek)", "device", serial, "error", err)
			return
		}
		if _, err := io.Copy(w, stdout); err != nil {
			slog.Info("backup: client write failed (body)", "device", serial, "error", err)
			// headers already committed — cannot surface error envelope;
			// client sees short stream
		}

		// Drain exit / stderr (best-effort).
		select {
		case <-exitCh:
		case <-backupCtx.Done():
		}
	}
}