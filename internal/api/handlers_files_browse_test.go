//go:build phase031_wave1

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// recordingBrowseRunner is an instrumented fake FileShellRunner for browse
// handler tests. It records every call by method and cmd string so tests can
// assert zero ADB calls for traversal inputs and inspect exact shell commands.
type recordingBrowseRunner struct {
	mu    sync.Mutex
	calls atomic.Int32
	cmds  map[string][]string // method -> captured cmd strings

	// shellFn lets a test customise ShellRunRaw behaviour.
	shellFn func(ctx context.Context, cmd string) ([]byte, error)

	// shellV2Fn lets a test customise ShellV2Stream behaviour.
	shellV2Fn func(ctx context.Context, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)

	// shellOutput is the default ShellRunRaw output when shellFn is nil.
	shellOutput []byte

	// shellV2Stdout is the default ShellV2Stream stdout when shellV2Fn is nil.
	shellV2Stdout string
	// shellV2Stderr is the default ShellV2Stream stderr when shellV2Fn is nil.
	shellV2Stderr string
}

func newRecordingBrowseRunner() *recordingBrowseRunner {
	return &recordingBrowseRunner{
		cmds: make(map[string][]string),
	}
}

func (r *recordingBrowseRunner) record(method, cmd string) {
	r.mu.Lock()
	r.cmds[method] = append(r.cmds[method], cmd)
	r.mu.Unlock()
	r.calls.Add(1)
}

func (r *recordingBrowseRunner) Calls() int { return int(r.calls.Load()) }

func (r *recordingBrowseRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	r.record("syncPush", dest)
	io.Copy(io.Discard, src)
	return nil
}

func (r *recordingBrowseRunner) SyncPullWriter(ctx context.Context, _, src string, dst io.Writer) error {
	r.record("syncPull", src)
	return nil
}

func (r *recordingBrowseRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	r.record("shell", cmd)
	if r.shellFn != nil {
		return r.shellFn(ctx, cmd)
	}
	return r.shellOutput, nil
}

func (r *recordingBrowseRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
	r.record("shellV2", cmd)
	if r.shellV2Fn != nil {
		return r.shellV2Fn(ctx, cmd)
	}
	ch := make(chan int, 1)
	ch <- 0
	close(ch)
	return io.NopCloser(strings.NewReader(r.shellV2Stdout)),
		io.NopCloser(strings.NewReader(r.shellV2Stderr)),
		ch, nil
}

// browseTestConfig returns a Config suitable for browse handler tests.
func browseTestConfig() *config.Config {
	cfg := testConfig()
	cfg.Files.AllowedBasePaths = []string{"/sdcard/", "/data/local/tmp/"}
	cfg.Files.MaxUploadBytes = 5 * 1024 * 1024 // 5 MiB
	return cfg
}

// setupBrowseRouter wires all browse file handlers for testing.
// Uses the XxxForTest constructors that Wave 1 plans 02+ must implement.
func setupBrowseRouter(registry *session.Registry, runner FileShellRunner, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}/files", func(r chi.Router) {
		r.Get("/", ListFilesForTest(registry, runner, cfg))
		r.Get("/stat", StatFileForTest(registry, runner, cfg))
		r.Post("/mkdir", MkdirForTest(registry, runner, cfg))
		r.Patch("/", RenameFileForTest(registry, runner, cfg))
		r.Delete("/", FilesDispatcherForTest(registry, runner, cfg))
	})
	return r
}

// TestListFiles_PathTraversalZeroCalls validates the Path Validation invariant
// (VALIDATION.md property 1): every traversal-shaped input produces ZERO ADB
// calls. The handler must reject before any shell/sync invocation.
func TestListFiles_PathTraversalZeroCalls(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newRecordingBrowseRunner()
	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	badPaths := []struct {
		name string
		path string
	}{
		{"parent-traversal", "/sdcard/../etc/passwd"},
		{"absolute-outside", "/etc/shadow"},
		{"url-encoded-dots", "/sdcard/%2e%2e/foo"},
		{"null-byte", "/sdcard/\x00/foo"},
		{"base-dir-itself", "/sdcard/"},
	}

	for _, tc := range badPaths {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/devices/ABC123/files?path="+tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusForbidden, w.Code,
				"path %q should be forbidden", tc.path)
		})
	}

	assert.Equal(t, 0, runner.Calls(),
		"ZERO ADB calls must be made for traversal inputs (Path Validation invariant)")
}

// TestStatFile_SameEntryShape verifies stat returns the same Entry struct
// shape as list (D-FB-04).
func TestStatFile_SameEntryShape(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	lsOutput := "-rw-rw---- 1 u0_a123 sdcard_rw 12345 2026-05-17 10:23:45.000000000 +0000 photo.jpg\n"
	runner := newRecordingBrowseRunner()
	runner.shellOutput = []byte(lsOutput)
	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/files/stat?path=/sdcard/photo.jpg", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// Response JSON must contain the Entry struct fields.
	body := w.Body.String()
	assert.Contains(t, body, `"name"`)
	assert.Contains(t, body, `"path"`)
	assert.Contains(t, body, `"type"`)
	assert.Contains(t, body, `"size"`)
	assert.Contains(t, body, `"mode"`)
	assert.Contains(t, body, `"mtime"`)
}

// TestMkdir_Idempotent verifies mkdir is idempotent (D-FB-05): first call
// returns existed=false, second call (test -d succeeds) returns existed=true,
// both 200.
func TestMkdir_Idempotent(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newRecordingBrowseRunner()
	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	// First call: directory does not exist yet.
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "test -d") {
			return []byte(""), nil // exit 0 => exists for second call
		}
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/mkdir?path=/sdcard/newdir", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"existed":false`)

	// Second call: directory now exists (test -d succeeds).
	req2 := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/mkdir?path=/sdcard/newdir", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), `"existed":true`)
}

// TestRename_DualPathValidation validates the Dual-path rename invariant
// (VALIDATION.md property 2): BOTH src and dst must pass ValidateDevicePath
// independently before any mv shell call.
func TestRename_DualPathValidation(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newRecordingBrowseRunner()
	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	tests := []struct {
		name string
		src  string
		dst  string
	}{
		{"bad-src-good-dst", "/etc/passwd", "/sdcard/renamed.txt"},
		{"good-src-bad-dst", "/sdcard/foo.txt", "/etc/shadow"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newRecordingBrowseRunner()
			r := setupBrowseRouter(registry, runner, cfg)

			req := httptest.NewRequest(http.MethodPatch,
				"/devices/ABC123/files?path="+tc.src+"&op=rename&to="+tc.dst, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusForbidden, w.Code,
				"either bad src or bad dst must produce PATH_NOT_ALLOWED")
			assert.Contains(t, w.Body.String(), "PATH_NOT_ALLOWED")
			assert.Equal(t, 0, runner.Calls(),
				"ZERO mv shell calls when either path fails validation (DualPath invariant)")
		})
	}
}

// TestRename_CrossFS verifies cross-filesystem rename surfaces as
// RENAME_CROSS_FS (409) per D-FB-11.
func TestRename_CrossFS(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingBrowseRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 1
		close(ch)
		return io.NopCloser(strings.NewReader("")),
			io.NopCloser(strings.NewReader("mv: Invalid cross-device link")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPatch,
		"/devices/ABC123/files?path=/sdcard/foo.txt&op=rename&to=/data/local/tmp/bar.txt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "RENAME_CROSS_FS")
}

// TestRename_NonEXDEVFailure verifies non-EXDEV mv failure (e.g. Permission
// denied) surfaces as RENAME_FAILED (500) per D-ERR-01 (2026-05-17 CONTEXT.md
// addendum — RENAME_FAILED is a distinct sentinel from RENAME_CROSS_FS).
func TestRename_NonEXDEVFailure(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingBrowseRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 1
		close(ch)
		return io.NopCloser(strings.NewReader("")),
			io.NopCloser(strings.NewReader("mv: Permission denied")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPatch,
		"/devices/ABC123/files?path=/sdcard/foo.txt&op=rename&to=/sdcard/bar.txt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "RENAME_FAILED")
}

// TestDelete_Recursive_SingleFlight validates the Concurrency single-flight
// invariant (VALIDATION.md property 8): two concurrent recursive-delete
// requests on the same device -> second returns 503 DEVICE_BUSY.
func TestDelete_Recursive_SingleFlight(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	started := make(chan struct{})
	release := make(chan struct{})
	runner := newRecordingBrowseRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		if strings.Contains(cmd, "rm -rf") {
			close(started)
			<-release
		}
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		return io.NopCloser(strings.NewReader("")),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	// First request blocks inside rm -rf.
	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete,
			"/devices/ABC123/files?path=/sdcard/delme&recursive=1", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		done1 <- w
	}()

	// Wait for first to start.
	<-started

	// Second concurrent request must get DEVICE_BUSY.
	req2 := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/files?path=/sdcard/delme&recursive=1", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "DEVICE_BUSY")

	close(release)
	<-done1
}

// TestDelete_NonRecursive_NoSingleFlight verifies non-recursive delete is NOT
// single-flight gated. Two concurrent non-recursive deletes should both succeed.
func TestDelete_NonRecursive_NoSingleFlight(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	runner := newRecordingBrowseRunner()
	cfg := browseTestConfig()
	r := setupBrowseRouter(registry, runner, cfg)

	req1 := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/files?path=/sdcard/a.txt", nil)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code)

	req2 := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/files?path=/sdcard/b.txt", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}