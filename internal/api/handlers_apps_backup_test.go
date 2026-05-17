//go:build phase031_wave1

// W5 STRICT Peek Contract:
//
// Plan 05 task 1 STRICT peek: the handler peeks exactly 4 bytes before
// committing HTTP headers. n < 4 -> BACKUP_FAILED envelope (JSON, 500).
// n == 4 -> commit headers (octet-stream, Content-Disposition) + stream
// peek + remainder.
//
// This contract is tested by:
//   - TestBackup_EmptyStreamBeforeHeaders (n == 0)
//   - TestBackup_StrictPeek4bytes (n == 3, the STRICT boundary)
//   - TestBackup_NonEmptyStream (n >= 4, the happy path)
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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// recordingBackupRunner is an instrumented fake FileShellRunner for backup
// handler tests.
type recordingBackupRunner struct {
	mu    sync.Mutex
	calls atomic.Int32
	cmds  map[string][]string // method -> captured cmd strings

	// shellV2Fn lets a test customise ShellV2Stream behaviour.
	shellV2Fn func(ctx context.Context, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)

	// shellFn lets a test customise ShellRunRaw behaviour.
	shellFn func(ctx context.Context, cmd string) ([]byte, error)

	shellOutput []byte
}

func newRecordingBackupRunner() *recordingBackupRunner {
	return &recordingBackupRunner{
		cmds: make(map[string][]string),
	}
}

func (r *recordingBackupRunner) record(method, cmd string) {
	r.mu.Lock()
	r.cmds[method] = append(r.cmds[method], cmd)
	r.mu.Unlock()
	r.calls.Add(1)
}

func (r *recordingBackupRunner) Calls() int { return int(r.calls.Load()) }

func (r *recordingBackupRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	r.record("syncPush", dest)
	io.Copy(io.Discard, src)
	return nil
}

func (r *recordingBackupRunner) SyncPullWriter(ctx context.Context, _, src string, dst io.Writer) error {
	r.record("syncPull", src)
	return nil
}

func (r *recordingBackupRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	r.record("shell", cmd)
	if r.shellFn != nil {
		return r.shellFn(ctx, cmd)
	}
	return r.shellOutput, nil
}

func (r *recordingBackupRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
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

// setupBackupRouter wires the backup handler for testing.
func setupBackupRouter(registry *session.Registry, runner FileShellRunner, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}/apps", func(r chi.Router) {
		r.Post("/{pkg}/backup", BackupAppForTest(registry, runner, cfg))
	})
	return r
}

// TestBackup_EmptyStreamBeforeHeaders validates the Backup peek-before-headers
// invariant (VALIDATION.md property 7): bu backup empty stream -> BACKUP_FAILED
// envelope returned (HTTP headers NOT yet committed). Response must be JSON
// with BACKUP_FAILED code, NOT octet-stream.
func TestBackup_EmptyStreamBeforeHeaders(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingBackupRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		// Empty stdout — user cancelled on-device.
		return io.NopCloser(strings.NewReader("")),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBackupRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/backup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"),
		"empty stream must return JSON, NOT octet-stream")
	assert.Contains(t, w.Body.String(), "BACKUP_FAILED")
}

// TestBackup_StrictPeek4bytes validates W5 STRICT peek: any n < 4 on the
// first ReadFull must result in BACKUP_FAILED envelope. The handler MUST
// require n == 4 to commit headers. A partial peek (n == 3 in this case)
// aborts cleanly and returns JSON error, never octet-stream.
//
// W5: Plan 05 task 1 STRICT peek — n<4 -> BACKUP_FAILED envelope;
// n==4 -> commit headers + stream peek + remainder.
func TestBackup_StrictPeek4bytes(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingBackupRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		// Only 3 bytes — less than the required 4-byte peek.
		return io.NopCloser(strings.NewReader("AND")),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBackupRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/backup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"),
		"W5: partial peek (n<4) must return JSON, NOT octet-stream")
	assert.Contains(t, w.Body.String(), "BACKUP_FAILED",
		"W5: partial peek must produce BACKUP_FAILED envelope")
}

// TestBackup_NonEmptyStream verifies that when ShellV2Stream stdout produces
// >= 4 bytes on the first ReadFull (i.e. the AB magic "ANDR"), the handler
// commits octet-stream headers and streams the full payload (D-AM-05).
func TestBackup_NonEmptyStream(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	backupData := "ANDROID BACKUP\n" + strings.Repeat("x", 1024)
	runner := newRecordingBackupRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		return io.NopCloser(strings.NewReader(backupData)),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBackupRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/backup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "com.foo.bar.ab")
	// Body must include the full payload (peek + remainder).
	assert.Contains(t, w.Body.String(), "ANDROID BACKUP")
}

// TestBackup_SingleFlight validates the Concurrency single-flight invariant
// (VALIDATION.md property 8): two concurrent backup requests on the same
// device -> second returns 503 DEVICE_BUSY.
func TestBackup_SingleFlight(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.Active)

	started := make(chan struct{})
	release := make(chan struct{})
	runner := newRecordingBackupRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		close(started)
		<-release
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		backupData := "ANDROID BACKUP\n" + strings.Repeat("x", 100)
		return io.NopCloser(strings.NewReader(backupData)),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupBackupRouter(registry, runner, cfg)

	// First request blocks inside backup.
	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/devices/ABC123/apps/com.foo.bar/backup", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		done1 <- w
	}()

	// Wait for first to start.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first backup never started")
	}

	// Second concurrent request must get DEVICE_BUSY.
	req2 := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/backup", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "DEVICE_BUSY")

	close(release)
	w1 := <-done1
	assert.Equal(t, http.StatusOK, w1.Code, "first backup should succeed")
}

// TestBackup_InvalidPkg verifies that an invalid package name returns 400
// INVALID_PACKAGE with zero ShellV2Stream calls (pkg regex invariant).
func TestBackup_InvalidPkg(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.Active)

	runner := newRecordingBackupRunner()
	cfg := browseTestConfig()
	r := setupBackupRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/123bad/backup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_PACKAGE")
	assert.Equal(t, 0, runner.Calls(),
		"ZERO ShellV2Stream calls for invalid package name")
}