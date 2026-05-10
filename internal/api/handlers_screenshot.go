// Package api — handlers_screenshot.go implements the /devices/{serial}/screenshot
// endpoint (OPS-06).
//
// Pipeline (D-05/D-06/D-07):
//
//	adb shell screencap -p   →   PNG bytes
//	image/png.Decode          →   image.Image
//	nativewebp.Encode         →   WebP bytes (Content-Type: image/webp)
//
// A3 RESOLVED (nativewebp v1.2.1):
//
//	github.com/HugoSmits86/nativewebp v1.2.1 exposes ONLY a lossless encode path:
//	  func Encode(w io.Writer, img image.Image, o *Options) error
//	  type Options struct { UseExtendedFormat bool }
//
//	There is no quality / lossy knob in the public surface (verified by
//	`grep -E '^func [A-Z]' writer.go` against the v1.2.1 source). The plan's
//	D-07 contract treats `?q=100` as "lossless", so we honour that as the
//	default behaviour for ALL `?q=` values: every WebP we emit is lossless.
//
//	When the client requests a lossy mode (`?q=` < 100 and not `?lossless=1`),
//	we still return a valid lossless WebP and add an informational header
//	`X-WebP-Mode: lossless-fallback` so callers can detect the resolution.
//	The DEPLOYMENT.md doc note (delivered by 03-05) records this contract.
//
// Pitfall 4 mitigation:
//
//	Per-API-key rate limit via golang.org/x/time/rate. Bucket map keyed by
//	the SHA-256 hash of the API key (handlers_reservation.go:ownerKeyFromRequest)
//	so the key never appears in memory plaintext after auth. Default
//	5 requests/sec/key (cfg.Screenshot.RatePerSecPerKey).
package api

import (
	"bytes"
	"context"
	"image/png"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// screenshotShellFn is the minimal callback the handler needs to fetch raw
// `screencap -p` PNG bytes from a device. Production wiring binds this to
// hostServices.ShellRunRaw via a closure that captures the request ctx.
type screenshotShellFn func() ([]byte, error)

// CaptureScreenshot is the production wiring for the screenshot endpoint.
// It binds hostServices.ShellRunRaw under the request ctx and delegates to
// the test-friendly CaptureScreenshotForTest.
func CaptureScreenshot(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	rl := newAPIKeyLimiter(cfg.Screenshot.RatePerSecPerKey)
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}
		runner := func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(r.Context(), screenshotTimeout)
			defer cancel()
			return hostServices.ShellRunRaw(ctx, serial, "screencap -p")
		}
		captureScreenshotImpl(w, r, registry, runner, rl, cfg)
	}
}

// CaptureScreenshotForTest builds the screenshot handler with an injectable
// shell runner. Used by tests that don't have a real *adb.HostServices.
func CaptureScreenshotForTest(registry *session.Registry, runnerFn func() ([]byte, error), cfg *config.Config) http.HandlerFunc {
	rl := newAPIKeyLimiter(cfg.Screenshot.RatePerSecPerKey)
	return func(w http.ResponseWriter, r *http.Request) {
		captureScreenshotImpl(w, r, registry, runnerFn, rl, cfg)
	}
}

const screenshotTimeout = 15 * time.Second

func captureScreenshotImpl(w http.ResponseWriter, r *http.Request, registry *session.Registry, runner screenshotShellFn, rl *apiKeyLimiter, cfg *config.Config) {
	serial := chi.URLParam(r, "serial")
	if serial == "" || !serialPattern.MatchString(serial) {
		writeError(w, ErrDeviceNotFound)
		return
	}
	if _, ok := registry.Get(serial); !ok {
		writeError(w, ErrDeviceNotFound)
		return
	}

	// Per-API-key rate limit (Pitfall 4).
	key := ownerKeyFromRequest(r)
	if key == "" {
		// Unauthenticated path (router middleware should have rejected) —
		// be defensive and bucket under a global "anon" key.
		key = "anon"
	}
	if !rl.Allow(key) {
		writeError(w, &DomainError{
			Code:       "RATE_LIMITED",
			HTTPStatus: http.StatusTooManyRequests,
			Message:    "Screenshot rate limit exceeded for this API key",
		})
		return
	}

	pngBytes, err := runner()
	if err != nil {
		slog.Warn("screenshot: shell failed", "device", serial, "error", err)
		writeError(w, ErrADBUnavailable)
		return
	}

	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		slog.Warn("screenshot: png decode failed", "device", serial, "error", err)
		writeError(w, &DomainError{
			Code:       "SCREENCAP_DECODE_FAILED",
			HTTPStatus: http.StatusInternalServerError,
			Message:    "Failed to decode screenshot PNG",
		})
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	// Document the A3 fallback to the caller (lossy not supported by
	// nativewebp v1.2.1; we always emit lossless WebP).
	w.Header().Set("X-WebP-Mode", "lossless-fallback")
	if err := nativewebp.Encode(w, img, &nativewebp.Options{}); err != nil {
		slog.Warn("screenshot: webp encode failed", "device", serial, "error", err)
		// Headers already sent — fall through; client sees a truncated body.
	}
}

// apiKeyLimiter is a per-key token-bucket map. Limiters are allocated on
// demand. Removed from memory only on process restart — bounded by the
// number of distinct API keys (in practice 1–2 from pelni_server).
type apiKeyLimiter struct {
	mu      sync.Mutex
	rate    rate.Limit
	burst   int
	buckets map[string]*rate.Limiter
}

func newAPIKeyLimiter(rps float64) *apiKeyLimiter {
	if rps <= 0 {
		rps = 5.0
	}
	return &apiKeyLimiter{
		rate:    rate.Limit(rps),
		burst:   int(rps), // burst = 1s worth
		buckets: make(map[string]*rate.Limiter),
	}
}

func (l *apiKeyLimiter) Allow(key string) bool {
	l.mu.Lock()
	lim, ok := l.buckets[key]
	if !ok {
		burst := l.burst
		if burst < 1 {
			burst = 1
		}
		lim = rate.NewLimiter(l.rate, burst)
		l.buckets[key] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}
