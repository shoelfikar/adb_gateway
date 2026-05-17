// Package api — handlers_apps.go implements D-AM-01..04, D-AM-08:
//
//	GET    /devices/{serial}/apps                  list installed packages (D-AM-01/04)
//	GET    /devices/{serial}/apps/{pkg}            rich package details via dumpsys (D-AM-02)
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
		stdoutBytes, _ := io.ReadAll(stdout)
		stderrBytes, _ := io.ReadAll(stderr)
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