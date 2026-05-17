// Package api — handlers_apk.go implements OPS-07:
//
//	POST /devices/{serial}/apks   sync push to /data/local/tmp/<uuid>.apk +
//	                              `pm install -r` + cleanup
//
// Discipline (Pitfall 5 — APK temp file leak):
//
//	The cleanup defer is registered IMMEDIATELY after generating tmpPath,
//	BEFORE calling SyncPushReader. It uses context.Background() so a client
//	disconnect mid-push still cleans up — the request ctx is gone but the
//	file lingers if we use the request ctx for cleanup. T-03-04-01.
//
// Concurrency / rate (T-03-04-02 / T-03-04-03):
//
//	- Per-device admission via DeviceEntry.InstallInFlight atomic.Bool CAS.
//	  Second concurrent install gets 503 DEVICE_BUSY. Independent of
//	  DeviceEntry.mu (Pitfall 9).
//	- Per-key rate limit via golang.org/x/time/rate (5/min/key default).
//	  Key is the SHA-256 hash of the API key (handlers_reservation.go).
//
// Bounded ctx (CONTEXT.md D-08 / D-09):
//
//	The install runs under context.WithTimeout(context.Background(), ...) —
//	NOT r.Context() — so the install survives client disconnect mid-operation.
//	Default timeout 300s (cfg.APK.InstallTimeoutSeconds). The handler returns
//	when the install completes or times out.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// APKShellRunner is the minimal interface the APK handler needs. The
// production *adb.HostServices satisfies it structurally (the methods exist
// with matching signatures). Tests inject fakeAPKRunner.
type APKShellRunner interface {
	SyncPushReader(ctx context.Context, serial, dest string, src io.Reader, mode os.FileMode) error
	ShellV2Stream(ctx context.Context, serial, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)
	ShellRunRaw(ctx context.Context, serial, cmd string) ([]byte, error)
}

// InstallAPK is the production wiring for POST /apks.
func InstallAPK(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return InstallAPKForTest(registry, hostServices, cfg)
}

// InstallAPKForTest builds the APK install handler with an injectable runner.
func InstallAPKForTest(registry *session.Registry, runner APKShellRunner, cfg *config.Config) http.HandlerFunc {
	rl := newKeyLimiter(cfg.APK.InstallsPerMinutePerKey)
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

		// Per-key rate limit (Pitfall 4 + T-03-04-03).
		key := ownerKeyFromRequest(r)
		if key == "" {
			key = "anon"
		}
		if !rl.Allow(key) {
			writeError(w, &DomainError{
				Code:       "RATE_LIMITED",
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "APK install rate limit exceeded for this API key",
			})
			return
		}

		// Per-device admission (T-03-04-02). CAS is lock-free and
		// independent of entry.mu (Pitfall 9).
		if !entry.InstallInFlight.CompareAndSwap(false, true) {
			writeError(w, ErrDeviceBusy)
			return
		}
		defer entry.InstallInFlight.Store(false)

		// Bounded install ctx — NOT request ctx (D-08). Install survives
		// client disconnect mid-operation up to InstallTimeoutSeconds.
		timeout := time.Duration(cfg.APK.InstallTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		installCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Generate tmpPath and register cleanup IMMEDIATELY (Pitfall 5).
		// Cleanup uses context.Background() so it runs even if installCtx
		// has been cancelled by a timeout or panic.
		tmpPath := fmt.Sprintf("/data/local/tmp/adbgw-%s.apk", uuid.New().String())
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			if _, err := runner.ShellRunRaw(cleanupCtx, serial, "rm -f "+shellQuote(tmpPath)); err != nil {
				slog.Warn("apk: cleanup rm failed", "device", serial, "tmp", tmpPath, "error", err)
			}
		}()

		// Cap the request body. http.MaxBytesReader returns *MaxBytesError
		// after the limit; we pass that through as 413 FILE_TOO_LARGE.
		maxBytes := cfg.APK.MaxBytes
		if maxBytes <= 0 {
			maxBytes = 524288000 // 500 MiB default
		}
		body := http.MaxBytesReader(w, r.Body, maxBytes)
		defer body.Close()

		// Push the APK. We pass installCtx (NOT r.Context) because the
		// push must survive client disconnect — D-08.
		if err := runner.SyncPushReader(installCtx, serial, tmpPath, body, 0644); err != nil {
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) || strings.Contains(err.Error(), "http: request body too large") {
				writeError(w, ErrFileTooLarge)
				return
			}
			// Client cancellation surfaces as ctx error; report as 499-style.
			if errors.Is(err, context.Canceled) {
				slog.Info("apk: push cancelled", "device", serial, "error", err)
				writeError(w, &DomainError{
					Code:       "PUSH_CANCELLED",
					HTTPStatus: 499,
					Message:    "APK upload cancelled",
				})
				return
			}
			slog.Warn("apk: push failed", "device", serial, "tmp", tmpPath, "error", err)
			writeError(w, ErrPushFailed)
			return
		}

		// Run pm install. Capture stdout / stderr / exit.
		cmd := "pm install -r " + shellQuote(tmpPath)
		stdout, stderr, exitCh, err := runner.ShellV2Stream(installCtx, serial, cmd)
		if err != nil {
			slog.Warn("apk: shell-v2 stream open failed", "device", serial, "error", err)
			writeError(w, ErrInstallFailed)
			return
		}
		stdoutBytes, _ := io.ReadAll(stdout)
		stderrBytes, _ := io.ReadAll(stderr)
		stdout.Close()
		stderr.Close()
		var exit int
		select {
		case exit = <-exitCh:
		case <-time.After(5 * time.Second):
			exit = -1
		}

		// pm install reports failure as either exit != 0 OR stdout
		// containing "Failure". Some Android versions ALWAYS exit 0 and
		// rely on stdout, so we check both.
		stdoutStr := string(stdoutBytes)
		stderrStr := string(stderrBytes)
		if exit != 0 || strings.Contains(stdoutStr, "Failure") {
			msg := strings.TrimSpace(stderrStr)
			if msg == "" {
				msg = strings.TrimSpace(stdoutStr)
			}
			if len(msg) > 256 {
				msg = msg[:256]
			}
			slog.Info("apk: install failed",
				"device", serial,
				"exit", exit,
				"owner_key_hash", key,
				"stderr_excerpt", truncate(stderrStr, 256),
			)
			writeError(w, &DomainError{
				Code:       ErrInstallFailed.Code,
				HTTPStatus: ErrInstallFailed.HTTPStatus,
				Message:    fmt.Sprintf("%s: %s", ErrInstallFailed.Message, msg),
			})
			return
		}

		slog.Info("apk: install ok",
			"device", serial,
			"owner_key_hash", key,
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "installed",
		})
	}
}

// truncate returns at most n bytes of s.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// keyLimiter is a per-key token bucket sized for "N ops per minute".
// Bucket burst = N (so a fresh key gets N before the per-minute drip kicks in,
// matching CONTEXT.md "X/min/key" semantics with full burst).
type keyLimiter struct {
	mu      sync.Mutex
	rate    rate.Limit
	burst   int
	buckets map[string]*rate.Limiter
}

func newKeyLimiter(perMin float64) *keyLimiter {
	if perMin <= 0 {
		perMin = 5.0
	}
	// "5/min" = rate.Every(time.Minute / 5) = 12s between tokens.
	limit := rate.Every(time.Minute / time.Duration(perMin))
	burst := int(perMin)
	if burst < 1 {
		burst = 1
	}
	return &keyLimiter{
		rate:    limit,
		burst:   burst,
		buckets: make(map[string]*rate.Limiter),
	}
}

func (l *keyLimiter) Allow(key string) bool {
	l.mu.Lock()
	lim, ok := l.buckets[key]
	if !ok {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.buckets[key] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

// requireWriteRateLimit returns a middleware that token-bucket-limits per
// owner-key. Caller constructs one keyLimiter per logical bucket (e.g.
// "fileapp_write") and shares it across the relevant handlers.
func requireWriteRateLimit(limiter *keyLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := ownerKeyFromRequest(r)
			if key == "" {
				key = "anon"
			}
			if !limiter.Allow(key) {
				writeError(w, &DomainError{
					Code:       "RATE_LIMITED",
					HTTPStatus: http.StatusTooManyRequests,
					Message:    "Write-op rate limit exceeded for this API key",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
