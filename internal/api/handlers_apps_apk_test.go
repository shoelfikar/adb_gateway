//go:build phase031_wave1

package api

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// recordingApkRunner is an instrumented fake FileShellRunner for APK export
// handler tests.
type recordingApkRunner struct {
	mu    sync.Mutex
	calls atomic.Int32
	cmds  map[string][]string // method -> captured cmd strings

	// shellFn lets a test customise ShellRunRaw behaviour.
	shellFn func(ctx context.Context, cmd string) ([]byte, error)

	// shellV2Fn lets a test customise ShellV2Stream behaviour.
	shellV2Fn func(ctx context.Context, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)

	// pullFn lets a test customise SyncPullWriter behaviour.
	pullFn func(ctx context.Context, src string, dst io.Writer) error

	shellOutput []byte
}

func newRecordingApkRunner() *recordingApkRunner {
	return &recordingApkRunner{
		cmds: make(map[string][]string),
	}
}

func (r *recordingApkRunner) record(method, cmd string) {
	r.mu.Lock()
	r.cmds[method] = append(r.cmds[method], cmd)
	r.mu.Unlock()
	r.calls.Add(1)
}

func (r *recordingApkRunner) Calls() int { return int(r.calls.Load()) }

func (r *recordingApkRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	r.record("syncPush", dest)
	io.Copy(io.Discard, src)
	return nil
}

func (r *recordingApkRunner) SyncPullWriter(ctx context.Context, _, src string, dst io.Writer) error {
	r.record("syncPull", src)
	if r.pullFn != nil {
		return r.pullFn(ctx, src, dst)
	}
	// Default: write a small APK-like payload.
	_, _ = dst.Write([]byte("APK_CONTENT"))
	return nil
}

func (r *recordingApkRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	r.record("shell", cmd)
	if r.shellFn != nil {
		return r.shellFn(ctx, cmd)
	}
	return r.shellOutput, nil
}

func (r *recordingApkRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
	r.record("shellV2", cmd)
	if r.shellV2Fn != nil {
		return r.shellV2Fn(ctx, cmd)
	}
	ch := make(chan int, 1)
	ch <- 0
	close(ch)
	return io.NopCloser(strings.NewReader("")),
		io.NopCloser(strings.NewReader("")),
		ch, nil
}

// setupApkExportRouter wires the APK export handler for testing.
func setupApkExportRouter(registry *session.Registry, runner FileShellRunner, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}/apps", func(r chi.Router) {
		r.Get("/{pkg}/apk", ExportAPKForTest(registry, runner, cfg))
	})
	return r
}

// TestAPKExport_SinglePath verifies that when pm path returns a single APK
// path, the response is application/vnd.android.package-archive with the
// correct filename ending in .apk (D-AM-06).
func TestAPKExport_SinglePath(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingApkRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "pm path") {
			return []byte("package:/data/app/com.foo.bar/base.apk\n"), nil
		}
		return nil, nil
	}

	cfg := browseTestConfig()
	r := setupApkExportRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps/com.foo.bar/apk", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/vnd.android.package-archive",
		w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), ".apk")
}

// TestAPKExport_SplitAPK_Tar verifies that when pm path returns multiple APK
// paths (split APKs), the response is application/x-tar with tar body
// containing entries for each APK (D-AM-06).
func TestAPKExport_SplitAPK_Tar(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingApkRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "pm path") {
			return []byte("package:/data/app/com.foo.bar/base.apk\n" +
				"package:/data/app/com.foo.bar/split_config.arm64_v8a.apk\n" +
				"package:/data/app/com.foo.bar/split_config.en.apk\n"), nil
		}
		return nil, nil
	}

	cfg := browseTestConfig()
	r := setupApkExportRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps/com.foo.bar/apk", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/x-tar", w.Header().Get("Content-Type"))

	// Parse the tar body and count entries.
	tr := tar.NewReader(w.Body)
	var entryNames []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		entryNames = append(entryNames, path.Base(hdr.Name))
	}
	assert.Equal(t, 3, len(entryNames),
		"split APK tar must contain 3 entries")
	assert.Contains(t, entryNames, "base.apk")
	assert.Contains(t, entryNames, "split_config.arm64_v8a.apk")
	assert.Contains(t, entryNames, "split_config.en.apk")
}

// TestAPKExport_PackageNotFound verifies that empty pm path output maps to
// 404 PACKAGE_NOT_FOUND (D-AM-08).
func TestAPKExport_PackageNotFound(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingApkRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "pm path") {
			return []byte(""), nil // empty output
		}
		return nil, nil
	}

	cfg := browseTestConfig()
	r := setupApkExportRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps/com.nonexistent/apk", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "PACKAGE_NOT_FOUND")
}

// TestAPKExport_NoBaseOnlyFallback validates D-AM-07: when multiple APK paths
// are present (split APKs), the response must be a tar — never a single APK
// with only the base. No base-only fallback.
func TestAPKExport_NoBaseOnlyFallback(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingApkRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "pm path") {
			return []byte("package:/data/app/com.foo.bar/base.apk\n" +
				"package:/data/app/com.foo.bar/split_config.arm64_v8a.apk\n"), nil
		}
		return nil, nil
	}

	cfg := browseTestConfig()
	r := setupApkExportRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps/com.foo.bar/apk", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// MUST be tar (not single APK) — D-AM-07 No base-only fallback.
	assert.Equal(t, "application/x-tar", w.Header().Get("Content-Type"),
		"multiple paths must produce tar, never single APK (D-AM-07)")

	// Verify tar contains both base and split.
	tr := tar.NewReader(w.Body)
	var entryNames []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		entryNames = append(entryNames, path.Base(hdr.Name))
	}
	assert.GreaterOrEqual(t, len(entryNames), 2,
		"tar must contain at least base + one split")
}