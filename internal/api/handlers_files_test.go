package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/session"
)

// fakeFileRunner is a test double for the FileShellRunner interface used by
// the file handlers. It records all push/pull/delete calls so the tests can
// assert path-traversal inputs never reach ADB (the security invariant).
type fakeFileRunner struct {
	mu           sync.Mutex
	pushCalls    int
	pullCalls    int
	rmCalls      int
	pushed       map[string][]byte // dest -> body
	stored       map[string][]byte // dest -> body for pull lookup
	pushErr      error
	pullErr      error
	rmErr        error
	shellV2Calls []string
}

func newFakeFileRunner() *fakeFileRunner {
	return &fakeFileRunner{
		pushed: make(map[string][]byte),
		stored: make(map[string][]byte),
	}
}

func (f *fakeFileRunner) SyncPushReader(_ context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCalls++
	if f.pushErr != nil {
		return f.pushErr
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	f.pushed[dest] = body
	f.stored[dest] = body
	return nil
}

func (f *fakeFileRunner) SyncPullWriter(_ context.Context, _, src string, dst io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullCalls++
	if f.pullErr != nil {
		return f.pullErr
	}
	body, ok := f.stored[src]
	if !ok {
		return errors.New("no such file")
	}
	_, err := dst.Write(body)
	return err
}

func (f *fakeFileRunner) ShellRunRaw(_ context.Context, _, cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(cmd, "rm -f ") {
		f.rmCalls++
		if f.rmErr != nil {
			return nil, f.rmErr
		}
	}
	return nil, nil
}

// ShellV2Stream — added 2026-05-17 to satisfy extended FileShellRunner.
// Wave 0 Phase 3 tests do not exercise this method; Wave 1 plan 01b's
// fakes will introduce richer instrumented fakes per handler test file.
func (f *fakeFileRunner) ShellV2Stream(_ context.Context, _, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error) {
	f.mu.Lock()
	f.shellV2Calls = append(f.shellV2Calls, cmd)
	f.mu.Unlock()
	ch := make(chan int, 1)
	ch <- 0
	close(ch)
	return io.NopCloser(strings.NewReader("")), io.NopCloser(strings.NewReader("")), ch, nil
}

func (f *fakeFileRunner) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushCalls + f.pullCalls + f.rmCalls
}

func setupFilesRouter(registry *session.Registry, runner FileShellRunner) *chi.Mux {
	cfg := testConfig()
	cfg.Files.AllowedBasePaths = []string{"/sdcard/", "/data/local/tmp/"}
	cfg.Files.MaxUploadBytes = 5 * 1024 * 1024 // 5 MiB cap for fast test

	r := chi.NewRouter()
	r.Route("/devices/{serial}/files", func(r chi.Router) {
		r.Post("/", UploadFileForTest(registry, runner, cfg))
		r.Get("/", DownloadFileForTest(registry, runner, cfg))
		r.Delete("/", DeleteFileForTest(registry, runner, cfg))
	})
	return r
}

func TestFilesPushPullRoundtrip(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newFakeFileRunner()
	r := setupFilesRouter(registry, runner)

	// Push 1 MiB of random bytes.
	payload := make([]byte, 1<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/files?path=/sdcard/foo.bin", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "push body=%s", w.Body.String())

	// Pull and assert byte-equality.
	req = httptest.NewRequest(http.MethodGet, "/devices/ABC123/files?path=/sdcard/foo.bin", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "foo.bin")
	assert.Equal(t, payload, w.Body.Bytes(), "round-trip body must be byte-identical")
}

func TestFilesPushOversize(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newFakeFileRunner()
	r := setupFilesRouter(registry, runner)

	// Cap is 5 MiB; send 6 MiB.
	body := make([]byte, 6*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/files?path=/sdcard/big.bin", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "FILE_TOO_LARGE")
}

func TestFilesDelete(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newFakeFileRunner()
	r := setupFilesRouter(registry, runner)

	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/files?path=/sdcard/foo.bin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, runner.rmCalls)
}

// TestFilesPathTraversal is the security invariant test: every traversal
// input must be rejected BEFORE any ADB call (zero ADB calls observed).
// Note: /sdcard/ is no longer in this list because base directories are
// now valid paths for file browsing (see TestFilesBaseDirAccess).
func TestFilesPathTraversal(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newFakeFileRunner()
	r := setupFilesRouter(registry, runner)

	bad := []string{
		"/sdcard/../etc/passwd",
		"/sdcard/%2e%2e/etc",
		"/SDCARD/foo",
		"/etc/shadow",
		"",
	}

	for _, p := range bad {
		// POST
		req := httptest.NewRequest(http.MethodPost, "/devices/ABC123/files?path="+p, bytes.NewReader([]byte("x")))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code, "POST %q should be forbidden", p)

		// GET
		req = httptest.NewRequest(http.MethodGet, "/devices/ABC123/files?path="+p, nil)
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code, "GET %q should be forbidden", p)

		// DELETE
		req = httptest.NewRequest(http.MethodDelete, "/devices/ABC123/files?path="+p, nil)
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code, "DELETE %q should be forbidden", p)
	}

	assert.Zero(t, runner.totalCalls(), "no ADB calls must be made for traversal inputs")
}

// TestFilesBaseDirAccess verifies that base directories are now accessible
// for file operations (required for file browsing to work).
func TestFilesBaseDirAccess(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)
	runner := newFakeFileRunner()
	r := setupFilesRouter(registry, runner)

	// DELETE /sdcard/ (non-recursive) should pass path validation and
	// attempt rm -f on the directory (which fails on device, but the
	// handler should at least accept the path).
	req := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/files?path=/sdcard/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Path validation should pass; the rm -f will succeed in the fake
	assert.Equal(t, http.StatusOK, w.Code, "base directory /sdcard/ should pass path validation for delete")

	// DELETE /sdcard/?recursive=1 should be BLOCKED (base dir recursive delete).
	req2 := httptest.NewRequest(http.MethodDelete, "/devices/ABC123/files?path=/sdcard/&recursive=1", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusForbidden, w2.Code, "recursive delete of base dir /sdcard/ should be forbidden")
	assert.Contains(t, w2.Body.String(), "BASE_DIR_DELETE_NOT_ALLOWED")
}