//go:build phase031_wave1

// W2 NDJSON Completeness Contract:
//
// Every tar entry in upload-folder produces exactly one NDJSON progress line.
// The final line is always a "summary" line with ok/err/total_bytes keys.
// Error lines from early-abort COUNT as per-entry lines:
//   len(lines) == entries_attempted + 1
// where entries_attempted = number of distinct tar headers the loop saw
// before exit, and the +1 is the summary line.
//
// This contract is tested by TestUploadFolder_NDJSONCompleteness and
// TestUploadFolder_NDJSONCompleteness_EarlyAbort below.
package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// recordingFolderRunner is an instrumented fake FileShellRunner for folder
// upload/download handler tests.
type recordingFolderRunner struct {
	mu    sync.Mutex
	calls atomic.Int32
	cmds  map[string][]string // method -> captured cmd strings

	// pushFn lets a test customise SyncPushReader behaviour.
	pushFn func(ctx context.Context, dest string, src io.Reader) error

	// pullFn lets a test customise SyncPullWriter behaviour.
	pullFn func(ctx context.Context, src string, dst io.Writer) error

	// shellFn lets a test customise ShellRunRaw behaviour.
	shellFn func(ctx context.Context, cmd string) ([]byte, error)

	// shellV2Fn lets a test customise ShellV2Stream behaviour.
	shellV2Fn func(ctx context.Context, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)

	shellOutput []byte
}

func newRecordingFolderRunner() *recordingFolderRunner {
	return &recordingFolderRunner{
		cmds: make(map[string][]string),
	}
}

func (r *recordingFolderRunner) record(method, cmd string) {
	r.mu.Lock()
	r.cmds[method] = append(r.cmds[method], cmd)
	r.mu.Unlock()
	r.calls.Add(1)
}

func (r *recordingFolderRunner) Calls() int { return int(r.calls.Load()) }

func (r *recordingFolderRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	r.record("syncPush", dest)
	if r.pushFn != nil {
		return r.pushFn(ctx, dest, src)
	}
	io.Copy(io.Discard, src)
	return nil
}

func (r *recordingFolderRunner) SyncPullWriter(ctx context.Context, _, src string, dst io.Writer) error {
	r.record("syncPull", src)
	if r.pullFn != nil {
		return r.pullFn(ctx, src, dst)
	}
	return nil
}

func (r *recordingFolderRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	r.record("shell", cmd)
	if r.shellFn != nil {
		return r.shellFn(ctx, cmd)
	}
	return r.shellOutput, nil
}

func (r *recordingFolderRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
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

// setupFolderRouter wires the folder upload/download handlers for testing.
func setupFolderRouter(registry *session.Registry, runner FileShellRunner, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}/files", func(r chi.Router) {
		r.Post("/upload-folder", UploadFolderForTest(registry, runner, cfg))
		r.Get("/download-folder", DownloadFolderForTest(registry, runner, cfg))
	})
	return r
}

// buildTarHelper creates a tar archive in memory with the given entries.
func buildTarHelper(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: e.typeflag,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if len(e.body) > 0 {
			_, err := tw.Write(e.body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	mode     int64
	body     []byte
	typeflag byte
}

// TestUploadFolder_TarTraversalEntries validates the Tar entry path invariant
// (VALIDATION.md property 4): path.Clean(entry.Name) cannot escape upload
// root. Absolute paths and ../ entries produce UNSUPPORTED_ENTRY/PATH_NOT_ALLOWED
// NDJSON lines. SyncPushReader is called only for legitimate entries.
func TestUploadFolder_TarTraversalEntries(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	var pushCount atomic.Int32
	runner := newRecordingFolderRunner()
	runner.pushFn = func(ctx context.Context, dest string, src io.Reader) error {
		io.Copy(io.Discard, src)
		pushCount.Add(1)
		return nil
	}

	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	// Build tar with traversal entries + one legit entry.
	tarBody := buildTarHelper(t, []tarEntry{
		{name: "../../../etc/passwd", mode: 0644, body: []byte("pwned"), typeflag: tar.TypeReg},
		{name: "/absolute/etc/passwd", mode: 0644, body: []byte("pwned2"), typeflag: tar.TypeReg},
		{name: "subdir/legit.txt", mode: 0644, body: []byte("hello"), typeflag: tar.TypeReg},
	})

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/upload-folder?path=/sdcard/dest", bytes.NewReader(tarBody))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Parse NDJSON response lines.
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 3, "should have at least 3 lines (2 errors + 1 ok + summary)")

	// SyncPushReader called exactly once (only for the legit entry).
	assert.Equal(t, int32(1), pushCount.Load(),
		"SyncPushReader must be called only for legitimate entries")
}

// TestUploadFolder_NDJSONCompleteness validates the NDJSON completeness
// invariant (VALIDATION.md property 6): every tar entry produces exactly one
// progress line, and the summary line is always emitted.
//
// W2 contract: error lines from early-abort COUNT as per-entry lines.
// len(lines) == entries_attempted + 1, where +1 is the summary line.
func TestUploadFolder_NDJSONCompleteness(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	pushCount := int32(0)
	runner := newRecordingFolderRunner()
	runner.pushFn = func(ctx context.Context, dest string, src io.Reader) error {
		n := atomic.AddInt32(&pushCount, 1)
		io.Copy(io.Discard, src)
		if n == 2 {
			// Second entry fails.
			return fmt.Errorf("sync push failed: connection reset")
		}
		return nil
	}

	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	// Build tar with 3 entries where the second will fail.
	tarBody := buildTarHelper(t, []tarEntry{
		{name: "a.txt", mode: 0644, body: []byte("aaa"), typeflag: tar.TypeReg},
		{name: "b.txt", mode: 0644, body: []byte("bbb"), typeflag: tar.TypeReg},
		{name: "c.txt", mode: 0644, body: []byte("ccc"), typeflag: tar.TypeReg},
	})

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/upload-folder?path=/sdcard/dest", bytes.NewReader(tarBody))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// 3 entries attempted + 1 summary = 4 lines (W2 contract).
	assert.Equal(t, 4, len(lines),
		"W2: len(lines) == entries_attempted + 1 (3 entries + 1 summary)")

	// Last line must be the summary.
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &summary))
	assert.Contains(t, summary, "summary",
		"last line must contain 'summary' key")

	summaryObj, ok := summary["summary"].(map[string]any)
	require.True(t, ok, "summary must be an object")
	assert.Equal(t, float64(2), summaryObj["ok"], "2 entries should succeed")
	assert.Equal(t, float64(1), summaryObj["err"], "1 entry should fail")
}

// TestUploadFolder_NDJSONCompleteness_EarlyAbort tests the W2 contract variant
// where the tar body is truncated mid-entry. The early-abort error line
// COUNTS as a per-entry line.
func TestUploadFolder_NDJSONCompleteness_EarlyAbort(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingFolderRunner()
	runner.pushFn = func(ctx context.Context, dest string, src io.Reader) error {
		io.Copy(io.Discard, src)
		return nil
	}

	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	// Build a valid tar, then truncate it mid-entry-3.
	fullTar := buildTarHelper(t, []tarEntry{
		{name: "a.txt", mode: 0644, body: []byte("aaa"), typeflag: tar.TypeReg},
		{name: "b.txt", mode: 0644, body: []byte("bbb"), typeflag: tar.TypeReg},
		{name: "c.txt", mode: 0644, body: []byte("ccc"), typeflag: tar.TypeReg},
	})
	// Truncate to ~2/3 of the full tar to cut mid-entry-3.
	truncated := fullTar[:len(fullTar)*2/3]

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/upload-folder?path=/sdcard/dest", bytes.NewReader(truncated))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// Early-abort variant: 2 ok entries + 1 TAR_CORRUPT err + 1 summary = 4 lines.
	assert.Equal(t, 4, len(lines),
		"W2 early-abort: len(lines) == 3 (2 ok + 1 TAR_CORRUPT) + 1 summary")

	// Last line is summary.
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &summary))
	assert.Contains(t, summary, "summary")
}

// TestUploadFolder_MaxBytesReader validates the Stream bounds invariant
// (VALIDATION.md property 5): http.MaxBytesReader wraps every upload;
// overflow surfaces as FILE_TOO_LARGE.
func TestUploadFolder_MaxBytesReader(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingFolderRunner()
	cfg := browseTestConfig()
	cfg.Files.MaxUploadBytes = 100 // Very small cap for testing.
	r := setupFolderRouter(registry, runner, cfg)

	// Build tar larger than the cap.
	tarBody := buildTarHelper(t, []tarEntry{
		{name: "big.txt", mode: 0644, body: make([]byte, 200), typeflag: tar.TypeReg},
	})

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/upload-folder?path=/sdcard/dest", bytes.NewReader(tarBody))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Expect either HTTP 413 or an NDJSON line with FILE_TOO_LARGE.
	if w.Code == http.StatusRequestEntityTooLarge {
		assert.Contains(t, w.Body.String(), "FILE_TOO_LARGE")
	} else {
		// Some implementations surface as NDJSON error line within 200.
		assert.Contains(t, w.Body.String(), "FILE_TOO_LARGE")
	}
}

// TestUploadFolder_SkipNonRegular validates that symlinks, block devices, and
// sockets in tar are skipped with UNSUPPORTED_ENTRY NDJSON line (D-FB-09).
func TestUploadFolder_SkipNonRegular(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingFolderRunner()
	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	tarBody := buildTarHelper(t, []tarEntry{
		{name: "link", mode: 0777, typeflag: tar.TypeSymlink},
		{name: "block", mode: 0600, typeflag: tar.TypeBlock},
	})

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/files/upload-folder?path=/sdcard/dest", bytes.NewReader(tarBody))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "UNSUPPORTED_ENTRY")
}

// TestDownloadFolder_ContentTypeTar verifies download-folder response has
// Content-Type == "application/x-tar" per D-FB-06.
func TestDownloadFolder_ContentTypeTar(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	lsOutput := "-rw-rw---- 1 u0_a123 sdcard_rw 100 2026-05-17 10:23:45.000000000 +0000 file.txt\n"
	runner := newRecordingFolderRunner()
	runner.shellOutput = []byte(lsOutput)
	runner.pullFn = func(ctx context.Context, src string, dst io.Writer) error {
		_, _ = dst.Write([]byte("content"))
		return nil
	}

	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/files/download-folder?path=/sdcard/mydir", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/x-tar", w.Header().Get("Content-Type"))
}

// TestDownloadFolder_SkippedEntriesHeader verifies the X-Skipped-Entries
// header is present when non-regular entries are skipped during download
// (W3 contract from plan 03 task 2).
func TestDownloadFolder_SkippedEntriesHeader(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	// ls output includes a symlink entry.
	lsOutput := "lrwxrwxrwx 1 root root 11 2026-05-10 12:00:00.000000000 +0000 link -> /sdcard/foo\n" +
		"-rw-rw---- 1 u0_a123 sdcard_rw 100 2026-05-17 10:23:45.000000000 +0000 file.txt\n"
	runner := newRecordingFolderRunner()
	runner.shellOutput = []byte(lsOutput)
	runner.pullFn = func(ctx context.Context, src string, dst io.Writer) error {
		_, _ = dst.Write([]byte("content"))
		return nil
	}

	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/files/download-folder?path=/sdcard/mydir", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	skipped := w.Header().Get("X-Skipped-Entries")
	assert.NotEmpty(t, skipped,
		"X-Skipped-Entries header must be present when non-regular entries are skipped (W3)")
	assert.Contains(t, skipped, "link",
		"X-Skipped-Entries must contain the symlink's relative path")
}

// TestDownloadFolder_PathValidation validates the Path Validation invariant
// for download-folder: bad path produces 403 with zero ls/sync-pull calls.
func TestDownloadFolder_PathValidation(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingFolderRunner()
	cfg := browseTestConfig()
	r := setupFolderRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/files/download-folder?path=/etc/shadow", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 0, runner.Calls(),
		"ZERO ADB calls for traversal path in download-folder")
}