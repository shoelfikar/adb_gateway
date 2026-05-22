//go:build phase031_wave1

package api

import (
	"context"
	"fmt"
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

// recordingAppsRunner is an instrumented fake FileShellRunner for app manager
// handler tests (list/details/uninstall). It records every call by method
// and cmd string so tests can assert zero shell calls for invalid package names.
type recordingAppsRunner struct {
	mu    sync.Mutex
	calls atomic.Int32
	cmds  map[string][]string // method -> captured cmd strings

	// shellFn lets a test customise ShellRunRaw behaviour.
	shellFn func(ctx context.Context, cmd string) ([]byte, error)

	// shellV2Fn lets a test customise ShellV2Stream behaviour.
	shellV2Fn func(ctx context.Context, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)

	shellOutput []byte
}

func newRecordingAppsRunner() *recordingAppsRunner {
	return &recordingAppsRunner{
		cmds: make(map[string][]string),
	}
}

func (r *recordingAppsRunner) record(method, cmd string) {
	r.mu.Lock()
	r.cmds[method] = append(r.cmds[method], cmd)
	r.mu.Unlock()
	r.calls.Add(1)
}

func (r *recordingAppsRunner) Calls() int { return int(r.calls.Load()) }

func (r *recordingAppsRunner) SyncPushReader(ctx context.Context, _, dest string, src io.Reader, _ os.FileMode) error {
	r.record("syncPush", dest)
	io.Copy(io.Discard, src)
	return nil
}

func (r *recordingAppsRunner) SyncPullWriter(ctx context.Context, _, src string, dst io.Writer) error {
	r.record("syncPull", src)
	return nil
}

func (r *recordingAppsRunner) ShellRunRaw(ctx context.Context, _, cmd string) ([]byte, error) {
	r.record("shell", cmd)
	if r.shellFn != nil {
		return r.shellFn(ctx, cmd)
	}
	return r.shellOutput, nil
}

func (r *recordingAppsRunner) ShellV2Stream(ctx context.Context, _, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
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

// setupAppsRouter wires the app manager handlers for testing.
func setupAppsRouter(registry *session.Registry, runner FileShellRunner, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/devices/{serial}/apps", func(r chi.Router) {
		r.Get("/", ListAppsForTest(registry, runner, cfg))
		r.Get("/{pkg}", AppDetailsForTest(registry, runner, cfg))
		r.Post("/{pkg}/launch", LaunchAppForTest(registry, runner, cfg))
		r.Delete("/{pkg}", UninstallAppForTest(registry, runner, cfg))
	})
	return r
}

// TestApps_InvalidPkg_ZeroShellCalls validates the Package-name regex invariant
// (VALIDATION.md property 3): every /apps/{pkg} route rejects pkg failing
// the strict regex BEFORE any shell call. Zero runner calls observed.
func TestApps_InvalidPkg_ZeroShellCalls(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	badPkgs := []struct {
		name string
		pkg  string
	}{
		{"numeric-start", "123.bad"},
		{"no-dot", "nodot"},
		{"shell-inject", ";rm"},
		// "empty" pkg case removed: chi routes /apps/ to the list handler (GET /),
		// never reaching /{pkg}. validatePackage handles empty correctly when
		// called, but httptest.NewRequest cannot exercise the route. The empty
		// case is covered by pkg_validate_test.go direct regex testing.
		{"too-long", strings.Repeat("a", 257)},
	}

	for _, tc := range badPkgs {
		t.Run(tc.name, func(t *testing.T) {
			runner := newRecordingAppsRunner()
			cfg := browseTestConfig()
			r := setupAppsRouter(registry, runner, cfg)

			// GET /apps/{pkg}
			req := httptest.NewRequest(http.MethodGet,
				"/devices/ABC123/apps/"+tc.pkg, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "INVALID_PACKAGE")

			// DELETE /apps/{pkg}
			req2 := httptest.NewRequest(http.MethodDelete,
				"/devices/ABC123/apps/"+tc.pkg, nil)
			w2 := httptest.NewRecorder()
			r.ServeHTTP(w2, req2)
			assert.Equal(t, http.StatusBadRequest, w2.Code)
			assert.Contains(t, w2.Body.String(), "INVALID_PACKAGE")

			assert.Equal(t, 0, runner.Calls(),
				"ZERO shell calls for invalid package name (pkg regex invariant)")
		})
	}
}

// TestListApps_DefaultUserOnly_ExactCommand verifies the exact shell command
// for listing apps (D-AM-03/D-AM-04).
func TestListApps_DefaultUserOnly_ExactCommand(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	var capturedCmd string
	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		capturedCmd = cmd
		return []byte("package:com.foo.bar versionCode:42 installer=com.android.vending uid:10123\n"), nil
	}

	cfg := browseTestConfig()

	tests := []struct {
		name           string
		query          string
		expectedCmd    string
	}{
		{
			"default-user-only",
			"",
			"pm list packages -3 -U -i --show-versioncode",
		},
		{
			"include-system",
			"?include=system",
			"pm list packages -U -i --show-versioncode",
		},
		{
			"include-disabled",
			"?include=disabled",
			"pm list packages -3 -d -U -i --show-versioncode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capturedCmd = ""
			runner2 := newRecordingAppsRunner()
			runner2.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
				capturedCmd = cmd
				return []byte("package:com.foo.bar versionCode:42 installer=com.android.vending uid:10123\n"), nil
			}
			r2 := setupAppsRouter(registry, runner2, cfg)

			req := httptest.NewRequest(http.MethodGet,
				"/devices/ABC123/apps"+tc.query, nil)
			w := httptest.NewRecorder()
			r2.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tc.expectedCmd, capturedCmd)
		})
	}
}

// TestListApps_NameFilter verifies case-insensitive substring filtering on
// package name via ?name= query parameter (D-AM-03).
func TestListApps_NameFilter(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	pmOutput := "package:com.foo.bar versionCode:42 installer=com.android.vending uid:10123\n" +
		"package:com.baz.qux versionCode:7 installer=null uid:10456\n" +
		"package:com.foo.baz versionCode:3 installer=null uid:10789\n"
	runner := newRecordingAppsRunner()
	runner.shellOutput = []byte(pmOutput)

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps?name=FOO", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// Should filter to the 2 packages containing "foo" (case-insensitive).
	body := w.Body.String()
	assert.Contains(t, body, "com.foo.bar")
	assert.Contains(t, body, "com.foo.baz")
	assert.NotContains(t, body, "com.baz.qux")
}

// TestListApps_FailureMapping verifies ShellRunRaw error maps to 500
// LIST_FAILED (D-ERR-01).
func TestListApps_FailureMapping(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "LIST_FAILED")
}

// TestDetails_DumpsysCmd verifies the handler issues `dumpsys package 'pkg'`
// (shellQuote single-quotes) and returns parsed details (D-AM-04).
func TestDetails_DumpsysCmd(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	var capturedCmd string
	dumpsysOutput := `Packages:
  Package [com.foo.bar] (12abc34):
    versionCode=42 minSdk=24 targetSdk=34
    versionName=1.2.3
    firstInstallTime=2025-12-01 10:23:45
    lastUpdateTime=2026-04-15 18:00:01
    Signing cert SHA-256: AA:BB:CC:DD:EE:FF
    apk signing version: 3
    requested permissions:
      android.permission.INTERNET
      android.permission.CAMERA
    runtime permissions:
      android.permission.CAMERA: granted=true`

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		capturedCmd = cmd
		return []byte(dumpsysOutput), nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/devices/ABC123/apps/com.foo.bar", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// Verify shell-quoting: must be single-quoted.
	assert.Equal(t, "dumpsys package 'com.foo.bar'", capturedCmd)

	body := w.Body.String()
	assert.Contains(t, body, "version_name")
	assert.Contains(t, body, "requested_permissions")
}

// TestUninstall_PackageNotFound verifies that pm uninstall failure with
// "not installed" semantics maps to 404 PACKAGE_NOT_FOUND (D-AM-08).
func TestUninstall_PackageNotFound(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		return io.NopCloser(strings.NewReader("Failure [DELETE_FAILED_INTERNAL_ERROR]\n")),
			io.NopCloser(strings.NewReader("not installed for 0")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/apps/com.foo.bar", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "PACKAGE_NOT_FOUND")
}

// TestUninstall_GenericFailure verifies that pm uninstall failure without
// "not installed" semantics maps to 500 UNINSTALL_FAILED (D-AM-08).
func TestUninstall_GenericFailure(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		ch := make(chan int, 1)
		ch <- 1
		close(ch)
		return io.NopCloser(strings.NewReader("Failure [DELETE_FAILED_SOMETHING_ELSE]\n")),
			io.NopCloser(strings.NewReader("some other error")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/apps/com.foo.bar", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "UNINSTALL_FAILED")
}

// TestUninstall_Success_SingleFlight validates the Concurrency single-flight
// invariant (VALIDATION.md property 8): two concurrent uninstall requests on
// the same device -> second returns 503 DEVICE_BUSY.
func TestUninstall_Success_SingleFlight(t *testing.T) {
	registry := session.NewRegistry()
	entry := registry.GetOrCreate("ABC123")
	entry.SetState(session.StateActive)

	started := make(chan struct{})
	release := make(chan struct{})
	runner := newRecordingAppsRunner()
	runner.shellV2Fn = func(ctx context.Context, cmd string) (io.ReadCloser, io.ReadCloser, <-chan int, error) {
		if strings.Contains(cmd, "pm uninstall") {
			close(started)
			<-release
		}
		ch := make(chan int, 1)
		ch <- 0
		close(ch)
		return io.NopCloser(strings.NewReader("Success\n")),
			io.NopCloser(strings.NewReader("")),
			ch, nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	// First request blocks inside pm uninstall.
	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete,
			"/devices/ABC123/apps/com.foo.bar", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		done1 <- w
	}()

	// Wait for first to start.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first uninstall never started")
	}

	// Second concurrent request must get DEVICE_BUSY.
	req2 := httptest.NewRequest(http.MethodDelete,
		"/devices/ABC123/apps/com.foo.bar", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "DEVICE_BUSY")

	close(release)
	w1 := <-done1
	assert.Equal(t, http.StatusOK, w1.Code, "first uninstall should succeed")
}

// ---------------------------------------------------------------------------
// LaunchApp tests
// ---------------------------------------------------------------------------

// TestLaunchApp_AmStartSuccess verifies the primary launch path:
// resolve-activity returns a component, am start succeeds.
func TestLaunchApp_AmStartSuccess(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			return []byte("android.intent.action.MAIN\ncom.foo.bar/com.foo.bar.MainActivity\n"), nil
		}
		if strings.Contains(cmd, "am start") {
			return []byte("Starting: Intent { cmp=com.foo.bar/com.foo.bar.MainActivity }\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"launched"`)
	assert.Contains(t, w.Body.String(), "com.foo.bar")
}

// TestLaunchApp_AmStartShortComponent verifies resolve-activity returning
// the short component form (pkg/.Activity) works with am start.
func TestLaunchApp_AmStartShortComponent(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			return []byte("android.intent.action.MAIN\ncom.foo.bar/.MainActivity\n"), nil
		}
		if strings.Contains(cmd, "am start") {
			return []byte("Starting: Intent { cmp=com.foo.bar/.MainActivity }\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"launched"`)
}

// TestLaunchApp_MonkeyFallback verifies that when resolve-activity fails
// (e.g. older device), the handler falls back to monkey and succeeds.
func TestLaunchApp_MonkeyFallback(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			// Older device: cmd package resolve-activity not available.
			return nil, fmt.Errorf("command not found")
		}
		if strings.Contains(cmd, "monkey") {
			return []byte("Events injected: 1\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"launched"`)
}

// TestLaunchApp_MonkeyFallbackEmptyResolve verifies fallback to monkey when
// resolve-activity succeeds but returns no matching component.
func TestLaunchApp_MonkeyFallbackEmptyResolve(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			// No launcher activity found (empty/bogus output).
			return []byte("No activity found\n"), nil
		}
		if strings.Contains(cmd, "monkey") {
			return []byte("Events injected: 1\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"launched"`)
}

// TestLaunchApp_AmStartPackageNotFound verifies that am start "does not exist"
// maps to 404 PACKAGE_NOT_FOUND.
func TestLaunchApp_AmStartPackageNotFound(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			return []byte("android.intent.action.MAIN\ncom.nonexistent.app/com.nonexistent.app.MainActivity\n"), nil
		}
		if strings.Contains(cmd, "am start") {
			return []byte("Error: Activity does not exist\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.nonexistent.app/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "PACKAGE_NOT_FOUND")
}

// TestLaunchApp_AmStartSecurityException verifies that am start
// SecurityException output maps to 500 LAUNCH_FAILED.
func TestLaunchApp_AmStartSecurityException(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			return []byte("android.intent.action.MAIN\ncom.foo.bar/com.foo.bar.MainActivity\n"), nil
		}
		if strings.Contains(cmd, "am start") {
			return []byte("java.lang.SecurityException: Permission Denial: starting Intent\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "LAUNCH_FAILED")
}

// TestLaunchApp_MonkeyPackageNotFound verifies that monkey "Error: Unknown package"
// output maps to 404 PACKAGE_NOT_FOUND (fallback path).
func TestLaunchApp_MonkeyPackageNotFound(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			return nil, fmt.Errorf("command not found")
		}
		return []byte("// Error: Unknown package: com.nonexistent.app\n"), nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.nonexistent.app/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "PACKAGE_NOT_FOUND")
}

// TestLaunchApp_MonkeyNoActivity verifies that monkey "// Error: No activities found"
// maps to 500 LAUNCH_FAILED (fallback path when both resolves return empty).
func TestLaunchApp_MonkeyNoActivity(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			// Both LAUNCHER and LEANBACK_LAUNCHER return empty.
			return []byte(""), nil
		}
		return []byte("// Error: No activities found\n"), nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "LAUNCH_FAILED")
}

// TestLaunchApp_LeanbackLauncher verifies that Android TV apps using
// LEANBACK_LAUNCHER are found when LAUNCHER returns no activity.
// This is the exact scenario for id.iptv.pelni.
func TestLaunchApp_LeanbackLauncher(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") && strings.Contains(cmd, "LEANBACK_LAUNCHER") {
			return []byte("priority=0 preferredOrder=0 match=0x108000\nid.iptv.pelni/.SplashScreenActivity\n"), nil
		}
		if strings.Contains(cmd, "resolve-activity") {
			// Standard LAUNCHER returns nothing.
			return []byte("No activity found\n"), nil
		}
		if strings.Contains(cmd, "am start") {
			return []byte("Starting: Intent { cmp=id.iptv.pelni/.SplashScreenActivity }\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/id.iptv.pelni/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"launched"`)
	assert.Contains(t, w.Body.String(), "id.iptv.pelni")
}

// TestLaunchApp_MonkeyAborted verifies that monkey "No activities found to run,
// monkey aborted" output (Android TV apps on monkey fallback) maps to 500
// LAUNCH_FAILED. This is the exact output seen for id.iptv.pelni when
// both resolve-activity categories and monkey all fail.
func TestLaunchApp_MonkeyAborted(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		if strings.Contains(cmd, "resolve-activity") {
			// Both LAUNCHER and LEANBACK_LAUNCHER resolve return empty.
			return []byte(""), nil
		}
		return []byte("** No activities found to run, monkey aborted.\n"), nil
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/id.iptv.pelni/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "LAUNCH_FAILED")
}

// TestLaunchApp_InvalidPkg verifies that an invalid package name returns 400
// INVALID_PACKAGE with ZERO shell calls (REQ-AM-PKG-VALIDATE invariant).
func TestLaunchApp_InvalidPkg(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/;rm/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_PACKAGE")
	assert.Equal(t, 0, runner.Calls(), "ZERO shell calls for invalid package name")
}

// TestLaunchApp_ShellError verifies that ShellRunRaw errors map to 500 LAUNCH_FAILED.
func TestLaunchApp_ShellError(t *testing.T) {
	registry := session.NewRegistry()
	registry.GetOrCreate("ABC123").SetState(session.StateActive)

	runner := newRecordingAppsRunner()
	runner.shellFn = func(ctx context.Context, cmd string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}

	cfg := browseTestConfig()
	r := setupAppsRouter(registry, runner, cfg)

	req := httptest.NewRequest(http.MethodPost,
		"/devices/ABC123/apps/com.foo.bar/launch", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "LAUNCH_FAILED")
}

// ---------------------------------------------------------------------------
// parseResolveActivity tests
// ---------------------------------------------------------------------------

func TestParseResolveActivity_StandardFormat(t *testing.T) {
	got := parseResolveActivity("android.intent.action.MAIN\ncom.foo.bar/com.foo.bar.MainActivity\n", "com.foo.bar")
	assert.Equal(t, "com.foo.bar/com.foo.bar.MainActivity", got)
}

func TestParseResolveActivity_ShortForm(t *testing.T) {
	got := parseResolveActivity("android.intent.action.MAIN\ncom.foo.bar/.MainActivity\n", "com.foo.bar")
	assert.Equal(t, "com.foo.bar/.MainActivity", got)
}

func TestParseResolveActivity_LeanbackFormat(t *testing.T) {
	output := "priority=0 preferredOrder=0 match=0x108000 specificIndex=-1 isDefault=false\nid.iptv.pelni/.SplashScreenActivity\n"
	got := parseResolveActivity(output, "id.iptv.pelni")
	assert.Equal(t, "id.iptv.pelni/.SplashScreenActivity", got)
}

func TestParseResolveActivity_NoActivityFound(t *testing.T) {
	got := parseResolveActivity("No activity found\n", "com.foo.bar")
	assert.Equal(t, "", got)
}

func TestParseResolveActivity_EmptyOutput(t *testing.T) {
	got := parseResolveActivity("", "com.foo.bar")
	assert.Equal(t, "", got)
}