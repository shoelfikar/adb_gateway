package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HugoSmits86/nativewebp"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// fakeShellRaw is a stub that returns canned bytes for ShellRunRaw and is
// used as the screenshot-side transport during tests.
type fakeShellRaw struct {
	bytes []byte
	err   error
	calls int
}

func (f *fakeShellRaw) ShellRunRawForTest() ([]byte, error) {
	f.calls++
	return f.bytes, f.err
}

// makePNG returns a 2x2 RGBA PNG byte stream for screenshot fixture.
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(1, 0, color.RGBA{0, 255, 0, 255})
	img.Set(0, 1, color.RGBA{0, 0, 255, 255})
	img.Set(1, 1, color.RGBA{255, 255, 0, 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestScreenshotHandlerEncodesWebP(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	cfg.Screenshot.DefaultQuality = 80
	cfg.Screenshot.RatePerSecPerKey = 5
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	pngBytes := makePNG(t)
	fakeRunner := &fakeShellRaw{bytes: pngBytes}

	r := chi.NewRouter()
	r.Post("/devices/{serial}/screenshot", CaptureScreenshotForTest(registry, fakeRunner.ShellRunRawForTest, cfg))

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/screenshot", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "image/webp", w.Header().Get("Content-Type"))
	assert.NotZero(t, fakeRunner.calls, "ADB runner must have been called")

	// Verify the body is a valid WebP by decoding it through nativewebp.
	_, err := nativewebp.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err, "response body must be a valid WebP")
}

func TestScreenshotHandlerRateLimit(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	cfg.Screenshot.DefaultQuality = 80
	cfg.Screenshot.RatePerSecPerKey = 2 // small bucket for fast test
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	pngBytes := makePNG(t)
	runner := &fakeShellRaw{bytes: pngBytes}

	r := chi.NewRouter()
	r.Post("/devices/{serial}/screenshot", CaptureScreenshotForTest(registry, runner.ShellRunRawForTest, cfg))

	// First 2 from same key should pass; 3rd-5th should hit 429 (token bucket).
	rejected := 0
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/screenshot", nil)
		req.Header.Set("X-API-Key", "rl-test-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			rejected++
		}
	}
	assert.Greater(t, rejected, 0, "rate limit must reject at least one of 5 rapid requests with rate=2/s")
}

func TestScreenshotHandlerDeviceNotFound(t *testing.T) {
	registry := session.NewRegistry()
	cfg := testConfig()
	runner := &fakeShellRaw{}

	r := chi.NewRouter()
	r.Post("/devices/{serial}/screenshot", CaptureScreenshotForTest(registry, runner.ShellRunRawForTest, cfg))

	req := httptest.NewRequest(http.MethodPost, "/devices/UNKNOWN/screenshot", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Zero(t, runner.calls, "ADB must not be called for unknown device")
}
