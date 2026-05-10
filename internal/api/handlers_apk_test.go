package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// fakeAPKRunner is a test double for APKShellRunner. It records every
// SyncPushReader and ShellRunRaw call so tests can assert cleanup
// invocation, stdout/stderr capture, and context provenance.
type fakeAPKRunner struct {
	mu sync.Mutex

	// pushFn lets a test simulate push behaviour (e.g. error or block).
	// Default: drain src and succeed.
	pushFn func(ctx context.Context, dest string, src io.Reader) error

	// shellFn lets a test simulate `pm install` output. Default: stdout
	// "Success\n", exit 0, no stderr.
	shellFn func(ctx context.Context, cmd string) (stdout, stderr []byte, exit int, err error)

	pushes    []apkPushCall
	shells    []apkShellCall
	rmCalls   atomic.Int32
	rmCtxBg   atomic.Bool // set true if cleanup ran with a non-cancelled ctx
}

type apkPushCall struct {
	Dest string
	Body []byte
}

type apkShellCall struct {
	Cmd        string
	CtxErr     error
	CtxBgClean bool
}

func (f *fakeAPKRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	if f.pushFn != nil {
		return f.pushFn(ctx, dest, src)
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.pushes = append(f.pushes, apkPushCall{Dest: dest, Body: body})
	f.mu.Unlock()
	return nil
}

// ShellV2Stream returns synthetic stdout/stderr/exit. We expose it as the
// signature handlers_apk.go expects from APKShellRunner.
func (f *fakeAPKRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
	var out, errb []byte
	exit := 0
	var err error
	if f.shellFn != nil {
		out, errb, exit, err = f.shellFn(ctx, cmd)
	} else {
		out = []byte("Success\n")
	}
	if err != nil {
		return nil, nil, nil, err
	}
	f.mu.Lock()
	f.shells = append(f.shells, apkShellCall{Cmd: cmd, CtxErr: ctx.Err(), CtxBgClean: ctx.Err() == nil})
	f.mu.Unlock()

	if strings.HasPrefix(cmd, "rm -f ") {
		f.rmCalls.Add(1)
		if ctx.Err() == nil {
			f.rmCtxBg.Store(true)
		}
	}

	exitCh := make(chan int, 1)
	exitCh <- exit
	close(exitCh)
	return io.NopCloser(bytes.NewReader(out)), io.NopCloser(bytes.NewReader(errb)), exitCh, nil
}

// ShellRunRaw — used for the cleanup `rm -f`.
func (f *fakeAPKRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	if strings.HasPrefix(cmd, "rm -f ") {
		f.rmCalls.Add(1)
		if ctx.Err() == nil {
			f.rmCtxBg.Store(true)
		}
		f.mu.Lock()
		f.shells = append(f.shells, apkShellCall{Cmd: cmd, CtxErr: ctx.Err(), CtxBgClean: ctx.Err() == nil})
		f.mu.Unlock()
	}
	return nil, nil
}

func setupAPKRouter(t *testing.T, registry *session.Registry, runner APKShellRunner, cfg *config.Config) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/devices/{serial}", func(r chi.Router) {
		r.Post("/apks", InstallAPKForTest(registry, runner, cfg))
	})
	return r
}

func apkTestConfig() *config.Config {
	cfg := testConfig()
	cfg.APK = config.APKConfig{
		MaxBytes:                 50 * 1024 * 1024, // 50 MiB
		InstallTimeoutSeconds:    300,
		InstallsPerMinutePerKey:  5.0,
	}
	return cfg
}

func TestAPKInstallSuccess(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := &fakeAPKRunner{}
	cfg := apkTestConfig()
	r := setupAPKRouter(t, registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK_BYTES")))
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "installed")

	// Cleanup must have been invoked.
	assert.Equal(t, int32(1), runner.rmCalls.Load(), "cleanup rm -f must be invoked")
	assert.True(t, runner.rmCtxBg.Load(), "cleanup must run with a non-cancelled ctx (context.Background)")
}

func TestAPKInstallFailure(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := &fakeAPKRunner{
		shellFn: func(ctx context.Context, cmd string) ([]byte, []byte, int, error) {
			if strings.HasPrefix(cmd, "rm -f ") {
				return nil, nil, 0, nil
			}
			return []byte("Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]\n"),
				[]byte("INSTALL_FAILED_INSUFFICIENT_STORAGE\n"), 1, nil
		},
	}
	cfg := apkTestConfig()
	r := setupAPKRouter(t, registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK")))
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "INSTALL_FAILED")
	assert.Contains(t, w.Body.String(), "INSUFFICIENT_STORAGE")

	// Cleanup STILL invoked.
	assert.Equal(t, int32(1), runner.rmCalls.Load())
}

func TestAPKInstallCleanupOnPushError(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := &fakeAPKRunner{
		pushFn: func(ctx context.Context, dest string, src io.Reader) error {
			io.Copy(io.Discard, src)
			return errors.New("sync push failed: connection reset")
		},
	}
	cfg := apkTestConfig()
	r := setupAPKRouter(t, registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK")))
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Either PUSH_FAILED or INSTALL_FAILED, but cleanup MUST run.
	assert.GreaterOrEqual(t, w.Code, 500)
	assert.Equal(t, int32(1), runner.rmCalls.Load(), "cleanup must run even when push fails")
}

func TestAPKInstallCleanupOnClientDisconnect(t *testing.T) {
	// D-08 contract: the install runs under context.Background()-derived
	// timeout ctx, NOT the request ctx. Therefore client disconnect does
	// NOT abort the install — we only verify cleanup uses a non-cancelled
	// ctx by cancelling the client and forcing the push to fail. Cleanup
	// MUST still run with a non-cancelled ctx (Pitfall 5).
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := &fakeAPKRunner{
		pushFn: func(ctx context.Context, dest string, src io.Reader) error {
			io.Copy(io.Discard, src)
			// Simulate a transport-level error mid-push (independent of
			// the client ctx — the install ctx is still alive).
			return errors.New("sync push failed: midstream io error")
		},
	}
	cfg := apkTestConfig()
	r := setupAPKRouter(t, registry, runner, cfg)

	clientCtx, cancelClient := context.WithCancel(context.Background())
	cancelClient() // Pre-cancel the client ctx — simulates "client gave up".
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK")))
	req = req.WithContext(clientCtx)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}

	assert.Equal(t, int32(1), runner.rmCalls.Load(), "cleanup must run even when client ctx is cancelled")
	assert.True(t, runner.rmCtxBg.Load(), "cleanup must use context.Background — not the cancelled client ctx (Pitfall 5)")
}

func TestAPKRateLimit(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := &fakeAPKRunner{}
	cfg := apkTestConfig()
	cfg.APK.InstallsPerMinutePerKey = 5.0 // 5/min, burst=5
	r := setupAPKRouter(t, registry, runner, cfg)

	rejected := 0
	for i := 0; i < 7; i++ {
		req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK")))
		req.Header.Set("X-API-Key", "ratelimit-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			rejected++
		}
	}
	assert.GreaterOrEqual(t, rejected, 1, "at least one of 7 rapid requests must be rate-limited (burst=5)")
}

func TestAPKConcurrencyBusy(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	pushStart := make(chan struct{}, 2)
	pushRelease := make(chan struct{})
	runner := &fakeAPKRunner{
		pushFn: func(ctx context.Context, dest string, src io.Reader) error {
			io.Copy(io.Discard, src)
			pushStart <- struct{}{}
			<-pushRelease
			return nil
		},
	}
	cfg := apkTestConfig()
	r := setupAPKRouter(t, registry, runner, cfg)

	doRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader([]byte("APK")))
		req.Header.Set("X-API-Key", "test-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	r1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r1Done <- doRequest()
	}()

	// Wait for first to start pushing.
	select {
	case <-pushStart:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never started pushing")
	}

	w2 := doRequest()
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "DEVICE_BUSY")

	close(pushRelease)
	w1 := <-r1Done
	assert.Equal(t, http.StatusOK, w1.Code, "first request should still succeed")
}

func TestAPKOversize(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := &fakeAPKRunner{}
	cfg := apkTestConfig()
	cfg.APK.MaxBytes = 1024 // 1 KiB cap
	r := setupAPKRouter(t, registry, runner, cfg)

	body := make([]byte, 2048) // 2 KiB > cap
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/apks", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Either FILE_TOO_LARGE (413) or INSTALL_FAILED — but never silent success.
	assert.NotEqual(t, http.StatusOK, w.Code, "oversize must not report installed")
}
