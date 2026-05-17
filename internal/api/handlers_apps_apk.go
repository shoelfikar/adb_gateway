// Package api -- handlers_apps_apk.go implements D-AM-06/07:
//
//	GET /devices/{serial}/apps/{pkg}/apk   export APK (single or split)
//
// D-AM-06: Content-Type signals whether the response is a single APK
// (application/vnd.android.package-archive) or a tar of split APK parts
// (application/x-tar). Explicit ergonomics trade-off accepted in CONTEXT.md.
//
// D-AM-07: No base-only fallback. When pm path returns multiple paths (split
// APKs), the response is a tar containing ALL parts. Pulling only base.apk
// for a split package would silently produce an uninstallable artifact.
//
// A6 [ASSUMED -- non-rooted devices expose split APKs via pm path; verify Wave 1]:
// /data/app/.../*.apk files are world-readable on non-rooted devices;
// SyncPullWriter works against them.
//
// Read-op: does NOT acquire WriteInFlight (Pitfall 9; CONTEXT.md Claude's
// Discretion confirms apk-export is read-only).
//
// Split-APK export buffers each part in memory before writing the tar header
// (need known Size up front). APK parts are typically <100 MiB; total memory
// bounded by largest single part. For very large bundles, consider per-file
// `ls -l` to learn size before pull -- deferred to v2.
package api

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ExportAPK is the production wiring for GET /apps/{pkg}/apk.
func ExportAPK(registry *session.Registry, hostServices *adb.HostServices, cfg *config.Config) http.HandlerFunc {
	return ExportAPKForTest(registry, hostServices, cfg)
}

// ExportAPKForTest builds the APK export handler with an injectable runner.
//
// Validates pkg with validatePackage BEFORE any shell call (REQ-AM-PKG-VALIDATE).
// Runs `pm path <pkg>` to discover APK paths. Branches on path count:
//
//	0 paths  -> 404 PACKAGE_NOT_FOUND
//	1 path   -> Content-Type application/vnd.android.package-archive, single APK stream
//	2+ paths -> Content-Type application/x-tar, tar of all parts (D-AM-07: no base-only fallback)
//
// Version code for filename is determined via ParseDumpsysPackage (I1 — reuse
// the same parser plan 04 uses for /apps/{pkg}); best-effort — failure leaves
// filename with version 0.
func ExportAPKForTest(registry *session.Registry, runner FileShellRunner, cfg *config.Config) http.HandlerFunc {
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

		// Run pm path to discover APK paths on device.
		cmd := "pm path " + shellQuote(pkg)
		out, err := runner.ShellRunRaw(r.Context(), serial, cmd)
		if err != nil {
			slog.Warn("apk-export: pm path failed", "device", serial, "pkg", pkg, "error", err)
			writeError(w, ErrListFailed)
			return
		}
		paths := ParsePMPath(out)
		if len(paths) == 0 {
			writeError(w, ErrPackageNotFound)
			return
		}

		// I1 — reuse ParseDumpsysPackage to determine version code for the
		// filename. Same parser that plan 04 uses for /apps/{pkg}. Best-effort:
		// failure leaves version code as "0" — never panics.
		versionCode := "0"
		if dumpOut, err := runner.ShellRunRaw(r.Context(), serial, "dumpsys package "+shellQuote(pkg)); err == nil {
			if detail, perr := ParseDumpsysPackage(dumpOut, pkg); perr == nil && detail.VersionCode > 0 {
				versionCode = strconv.FormatInt(detail.VersionCode, 10)
			}
		}

		switch {
		case len(paths) == 1:
			// Single APK — stream directly.
			p := paths[0]
			filename := fmt.Sprintf("%s-%s.apk", pkg, versionCode)
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
			if err := runner.SyncPullWriter(r.Context(), serial, p, w); err != nil {
				slog.Warn("apk-export: pull failed", "device", serial, "path", p, "error", err)
				// Headers already sent; client sees short read.
				return
			}
			return

		default:
			// Multiple paths — split APK (D-AM-07: NO base-only fallback).
			filename := fmt.Sprintf("%s-%s.tar", pkg, versionCode)
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
			tw := tar.NewWriter(w)
			defer tw.Close()
			for _, p := range paths {
				base := path.Base(p)
				// Buffer each part to learn size before writing tar header.
				// APK parts are typically <100 MiB; acceptable memory cost.
				var buf bytes.Buffer
				if err := runner.SyncPullWriter(r.Context(), serial, p, &buf); err != nil {
					slog.Warn("apk-export: split pull failed", "device", serial, "path", p, "error", err)
					// Headers sent; one entry skipped; tar will be missing this file.
					continue
				}
				if err := tw.WriteHeader(&tar.Header{
					Name:     base,
					Mode:     0644,
					Size:     int64(buf.Len()),
					Typeflag: tar.TypeReg,
				}); err != nil {
					slog.Warn("apk-export: tar header failed", "device", serial, "path", p, "error", err)
					return
				}
				if _, err := io.Copy(tw, &buf); err != nil {
					slog.Warn("apk-export: tar body failed", "device", serial, "path", p, "error", err)
					return
				}
			}
			return
		}
	}
}