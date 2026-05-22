// Package api — handlers_apps.go implements D-AM-01..04, D-AM-08, D-AM-09:
//
//	GET    /devices/{serial}/apps                  list installed packages (D-AM-01/04)
//	GET    /devices/{serial}/apps/{pkg}            rich package details via dumpsys (D-AM-02)
//	POST   /devices/{serial}/apps/{pkg}/launch     launch app on device (D-AM-09)
//	DELETE /devices/{serial}/apps/{pkg}            uninstall package (D-AM-08)
//
// List uses a single `pm list packages` call with flags selected by the
// ?include= query parameter (D-AM-03). Cheap list — no per-entry dumpsys
// (D-AM-04). Details uses `dumpsys package <pkg>`. Uninstall mirrors the
// APK install error contract (Pitfall 4 — check both exit and stdout).
//
// Every /apps/{pkg} route validates the package name with pkgPattern BEFORE
// any shell call (REQ-AM-PKG-VALIDATE invariant).
package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ---------------------------------------------------------------------------
// ListApps — GET /devices/{serial}/apps
// ---------------------------------------------------------------------------

// ListApps is the production wiring for GET /apps.
func ListApps(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return ListAppsForTest(registry, hostServices, cfg)
}

// ListAppsForTest builds the list-apps handler with an injectable runner.
//
// Query parameters:
//
//	?include=         — "" (default: user-only), "system", "disabled", "all" (D-AM-03)
//	?name=<substr>    — case-insensitive substring filter on package name (D-AM-03)
//
// The exact pm command flags per D-AM-04:
//
//	include=""        -> pm list packages -3 -U -i --show-versioncode
//	include="system"  -> pm list packages -U -i --show-versioncode        (no -3)
//	include="disabled"-> pm list packages -3 -d -U -i --show-versioncode
//	include="all"     -> pm list packages -d -U -i --show-versioncode    (no -3)
//
// W4 — System/Enabled derivation lives in ParsePMList (the parser), not in
// post-hoc handler loops. Handler computes includeSystem/includeDisabled from
// the query param and hands them to the parser.
func ListAppsForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Parse query parameters (D-AM-03).
		include := r.URL.Query().Get("include") // "", "system", "disabled", "all"
		nameFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("name")))

		// Build cmd flags per D-AM-03 + D-AM-04 EXACTLY.
		// Base: -U (uid), -i (installer), --show-versioncode (Android 8+,
		// parser leniently handles missing).
		flags := []string{"pm", "list", "packages"}
		switch include {
		case "system":
			// Omit -3; do not add -d. Includes system + user, enabled only.
		case "disabled":
			// -3 -d: user-installed only, disabled packages (-d returns
			// disabled only when combined with -3).
			flags = append(flags, "-3", "-d")
		case "all":
			// -d: includes both enabled + disabled, system + user.
			// (Android returns disabled-only when -d is set without other
			// filters; treat as disabled-scope.)
			flags = append(flags, "-d")
		default:
			// Default: user-installed only (enabled).
			flags = append(flags, "-3")
		}
		flags = append(flags, "-U", "-i", "--show-versioncode")
		cmd := strings.Join(flags, " ")

		out, err := runner.ShellRunRaw(r.Context(), serial, cmd)
		if err != nil {
			slog.Warn("apps: pm list failed", "device", serial, "cmd", cmd, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		// W4 — derive include flags ONCE and pass to parser; do NOT mutate
		// entries post-hoc.
		//
		//	include=""           -> -3        : user-only, enabled
		//	include="system"     -> (no -3)   : system+user, enabled
		//	include="disabled"   -> -3 -d     : user-only, disabled
		//	include="all"        -> -d        : system+user, disabled-scope
		includeSystem := include == "system" || include == "all"
		includeDisabled := include == "disabled" || include == "all"
		entries := ParsePMList(out, includeSystem, includeDisabled)

		// Apply name filter (case-insensitive substring match on Package field).
		if nameFilter != "" {
			filtered := entries[:0]
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Package), nameFilter) {
					filtered = append(filtered, e)
				}
			}
			entries = filtered
		}

		writeJSON(w, http.StatusOK, entries)
	}
}

// ---------------------------------------------------------------------------
// GetAppDetails — GET /devices/{serial}/apps/{pkg}
// ---------------------------------------------------------------------------

// GetAppDetails is the production wiring for GET /apps/{pkg}.
func GetAppDetails(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return GetAppDetailsForTest(registry, hostServices, cfg)
}

// GetAppDetailsForTest builds the app-details handler with an injectable runner.
//
// Validates pkg with validatePackage BEFORE any shell call (REQ-AM-PKG-VALIDATE).
// Runs `dumpsys package 'pkg'` and parses via ParseDumpsysPackage.
// Detects "Unable to find package" in dumpsys output for 404 PACKAGE_NOT_FOUND.
//
// Optional ?include_size=1 adds disk usage via `du -sk /data/data/<pkg>`.
// This adds ~500ms-2s per request on apps with large data dirs (du is slow);
// opt-in only per CONTEXT.md Claude's Discretion.
func GetAppDetailsForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate package name BEFORE any shell call (REQ-AM-PKG-VALIDATE).
		pkg, ok := validatePackage(w, r)
		if !ok {
			return
		}

		cmd := "dumpsys package " + shellQuote(pkg)
		out, err := runner.ShellRunRaw(r.Context(), serial, cmd)
		if err != nil {
			slog.Warn("apps: dumpsys failed", "device", serial, "pkg", pkg, "error", err)
			writeError(w, ErrListFailed)
			return
		}

		// Some Android versions return success with body "Unable to find package: <pkg>"
		if bytes.Contains(out, []byte("Unable to find package")) {
			writeError(w, ErrPackageNotFound)
			return
		}

		detail, _ := ParseDumpsysPackage(out, pkg)

		// Optional size: ?include_size=1 adds du -sk /data/data/<pkg>
		if r.URL.Query().Get("include_size") == "1" {
			sizeCmd := "du -sk " + shellQuote("/data/data/"+pkg) + " 2>/dev/null | awk '{print $1}'"
			sizeOut, sizeErr := runner.ShellRunRaw(r.Context(), serial, sizeCmd)
			if sizeErr == nil {
				if kib, perr := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64); perr == nil {
					detail.TotalSizeBytes = kib * 1024
				}
			}
		}

		writeJSON(w, http.StatusOK, detail)
	}
}

// AppDetailsForTest is an alias for GetAppDetailsForTest used by the
// phase031_wave1 test router (setupAppsRouter wires it as AppDetailsForTest).
func AppDetailsForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return GetAppDetailsForTest(registry, runner, cfg)
}

// ---------------------------------------------------------------------------
// UninstallApp — DELETE /devices/{serial}/apps/{pkg}
// ---------------------------------------------------------------------------

// UninstallApp is the production wiring for DELETE /apps/{pkg}.
func UninstallApp(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return UninstallAppForTest(registry, hostServices, cfg)
}

// UninstallAppForTest builds the uninstall handler with an injectable runner.
//
// Validates pkg with validatePackage BEFORE any shell call.
// Acquires DeviceEntry.WriteInFlight single-flight gate (Pitfall 9).
// Uses bounded context.Background() ctx (mirror APK install D-08) so
// uninstall survives client disconnect.
// Uses ShellV2Stream for Pitfall 4 — pm uninstall may exit 0 with
// "Failure" in stdout.
//
// Error mapping per D-AM-08:
//   - "not installed for" substring -> 404 PACKAGE_NOT_FOUND
//   - other failures -> 500 UNINSTALL_FAILED with truncated stderr
func UninstallAppForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		// Validate package name BEFORE any shell call (REQ-AM-PKG-VALIDATE).
		pkg, ok := validatePackage(w, r)
		if !ok {
			return
		}

		// WriteInFlight single-flight gate (Pitfall 9).
		if !entry.WriteInFlight.CompareAndSwap(false, true) {
			writeError(w, ErrDeviceBusy)
			return
		}
		defer entry.WriteInFlight.Store(false)

		// Bounded ctx independent of r.Context() (mirror APK install D-08).
		// Uninstall must survive client disconnect.
		timeout := time.Duration(cfg.APK.InstallTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		uninstallCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		cmd := "pm uninstall " + shellQuote(pkg)
		stdout, stderr, exitCh, err := runner.ShellV2Stream(uninstallCtx, serial, cmd)
		if err != nil {
			slog.Warn("apps: pm uninstall shell-v2 failed", "device", serial, "pkg", pkg, "error", err)
			writeError(w, ErrUninstallFailed)
			return
		}
		stdoutBytes, stdoutErr := io.ReadAll(stdout)
		if stdoutErr != nil {
			slog.Warn("apps: stdout read incomplete", "device", serial, "error", stdoutErr)
		}
		stderrBytes, stderrErr := io.ReadAll(stderr)
		if stderrErr != nil {
			slog.Warn("apps: stderr read incomplete", "device", serial, "error", stderrErr)
		}
		stdout.Close()
		stderr.Close()
		var exit int
		select {
		case exit = <-exitCh:
		case <-time.After(5 * time.Second):
			exit = -1
		}

		// Map error per D-AM-08 + Pitfall 4.
		stdoutStr := string(stdoutBytes)
		stderrStr := string(stderrBytes)
		combined := stdoutStr + "\n" + stderrStr
		lowCombined := strings.ToLower(combined)

		if exit != 0 || strings.Contains(stdoutStr, "Failure") {
			// 404 PACKAGE_NOT_FOUND if "not installed for" appears (D-AM-08 + Pitfall 4)
			if strings.Contains(lowCombined, "not installed for") {
				writeError(w, ErrPackageNotFound)
				return
			}
			// Generic uninstall failure with truncated stderr/stdout in message
			msg := strings.TrimSpace(stderrStr)
			if msg == "" {
				msg = strings.TrimSpace(stdoutStr)
			}
			if len(msg) > 256 {
				msg = msg[:256]
			}
			writeError(w, &DomainError{
				Code:       ErrUninstallFailed.Code,
				HTTPStatus: ErrUninstallFailed.HTTPStatus,
				Message:    ErrUninstallFailed.Message + ": " + msg,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"status": "uninstalled", "package": pkg})
	}
}

// ---------------------------------------------------------------------------
// LaunchApp — POST /devices/{serial}/apps/{pkg}/launch
// ---------------------------------------------------------------------------

// LaunchApp is the production wiring for POST /apps/{pkg}/launch.
func LaunchApp(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return LaunchAppForTest(registry, hostServices, cfg)
}

// LaunchAppForTest builds the launch-app handler with an injectable runner.
//
// Launches the app using a two-step resolve-then-launch approach:
//  1. Resolve the launcher activity via `cmd package resolve-activity --brief`
//     (Android 7.0+). If a component is found, use `am start -n <component>`
//     for reliable activity launch with proper error reporting.
//  2. If resolve fails (older device, command unavailable, or no component
//     found), fall back to `monkey -p <pkg> -c ... 1`.
//
// `am start` is preferred because it directly starts the activity via intent
// rather than injecting a synthetic UI event. Monkey can report success
// (exit 0, "Events injected: 1") without actually starting the app on screen.
//
// Error mapping:
//   - package not found on device -> 404 PACKAGE_NOT_FOUND
//   - launch reports error in output -> 500 LAUNCH_FAILED
func LaunchAppForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		if _, ok := registry.Get(serial); !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate package name BEFORE any shell call (REQ-AM-PKG-VALIDATE).
		pkg, ok := validatePackage(w, r)
		if !ok {
			return
		}

		// Step 1: Try to resolve the launcher activity (Android 7.0+).
		// am start provides more reliable launching and better error reporting
		// than monkey, which can report success without actually starting the app.
		component := resolveLauncherActivity(r.Context(), runner, serial, pkg)

		var out []byte
		var err error
		var usedAmStart bool

		if component != "" {
			// Launch using am start with the resolved component.
			launchCmd := "am start -n " + shellQuote(component)
			out, err = runner.ShellRunRaw(r.Context(), serial, launchCmd)
			usedAmStart = true
		}

		// Fallback: Use monkey for older devices or if resolve-activity failed.
		if !usedAmStart {
			cmd := "monkey -p " + shellQuote(pkg) + " -c android.intent.category.LAUNCHER 1"
			out, err = runner.ShellRunRaw(r.Context(), serial, cmd)
		}

		if err != nil {
			method := "monkey"
			if usedAmStart {
				method = "am-start"
			}
			slog.Warn("apps: launch failed", "device", serial, "pkg", pkg, "method", method, "error", err)
			writeError(w, ErrLaunchFailed)
			return
		}

		outStr := strings.TrimSpace(string(out))
		if usedAmStart {
			// Parse am start output for errors.
			// Success: "Starting: Intent { cmp=... }"
			// Failure: "Error: Activity does not exist", "SecurityException", etc.
			lower := strings.ToLower(outStr)
			if strings.Contains(lower, "does not exist") || strings.Contains(lower, "not found") {
				writeError(w, ErrPackageNotFound)
				return
			}
			if strings.Contains(lower, "error") || strings.Contains(lower, "exception") {
				writeError(w, &DomainError{
					Code:       ErrLaunchFailed.Code,
					HTTPStatus: ErrLaunchFailed.HTTPStatus,
					Message:    ErrLaunchFailed.Message + ": " + truncateOutput(outStr, 256),
				})
				return
			}
		} else {
			// Parse monkey output — monkey reports errors in output even with exit 0.
			// Common patterns:
			//   "Error: Unknown package: <pkg>" — package not installed
			//   "// Error: No activities found" — no launcher activity
			//   "** No activities found to run, monkey aborted." — Android TV apps
			lower := strings.ToLower(outStr)
			if strings.Contains(lower, "error: unknown package") || strings.Contains(lower, "not installed") {
				writeError(w, ErrPackageNotFound)
				return
			}
			if strings.Contains(lower, "error") || strings.Contains(lower, "// error") ||
				strings.Contains(lower, "no activities found") || strings.Contains(lower, "monkey aborted") {
				writeError(w, &DomainError{
					Code:       ErrLaunchFailed.Code,
					HTTPStatus: ErrLaunchFailed.HTTPStatus,
					Message:    ErrLaunchFailed.Message + ": " + truncateOutput(outStr, 256),
				})
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "launched",
			"package": pkg,
		})
	}
}

// parseResolveActivity extracts the component name from cmd package
// resolve-activity --brief output. Returns empty string if no valid component found.
//
// Expected output format (Android 7.0+):
//
//	android.intent.action.MAIN
//	com.example/com.example.MainActivity
//
// The component line may also use the short form:
//
//	com.example/.MainActivity
func parseResolveActivity(output, pkg string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "android.") {
			continue
		}
		if strings.HasPrefix(line, pkg+"/") {
			return line
		}
	}
	return ""
}

// resolveLauncherActivity finds the main launcher activity for pkg.
// Tries android.intent.category.LAUNCHER first, then falls back to
// android.intent.category.LEANBACK_LAUNCHER for Android TV apps.
// Returns the component name (e.g. "com.foo/.MainActivity") or empty string.
func resolveLauncherActivity(ctx context.Context, runner FileShellRunner, serial, pkg string) string {
	categories := []string{
		"android.intent.category.LAUNCHER",
		"android.intent.category.LEANBACK_LAUNCHER",
	}
	for _, cat := range categories {
		resolveCmd := "cmd package resolve-activity --brief -c " + cat + " -a android.intent.action.MAIN " + shellQuote(pkg)
		resolveOut, resolveErr := runner.ShellRunRaw(ctx, serial, resolveCmd)
		if resolveErr != nil {
			continue
		}
		if component := parseResolveActivity(string(resolveOut), pkg); component != "" {
			return component
		}
	}
	return ""
}

// truncateOutput truncates s to maxLen bytes for inclusion in error messages.
func truncateOutput(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}