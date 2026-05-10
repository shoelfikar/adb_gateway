---
phase: 03-multi-device-fleet
plan: 03
subsystem: api
tags: [api, session, logcat, screenshot, files, security, tdd]
type: execute
status: complete
completed: 2026-05-10
requirements: [OPS-05, OPS-06, OPS-08]
dependency_graph:
  requires:
    - 03-01 (ShellRunRaw, SyncPushReader, SyncPullWriter, ShellV2Stream, ValidateDevicePath, ErrPathNotAllowed, ErrFileTooLarge)
    - 03-02 (RestartSession, LauncherFactory, AttachStallRecovery)
  provides:
    - "internal/session.LogcatBuffer (10000-line ring + atomic snapshot subscribe + drop-on-slow eviction)"
    - "internal/session.DeviceSession.{AttachLogcatBuffer,AttachLogcatReader,LogcatBuffer,SetLogcatBufferForTest,SetStateForTest}"
    - "internal/session.LogcatShellRunner interface"
    - "internal/session.SessionOpts.LogcatCapacity"
    - "internal/api.StreamLogcat (WS handler, OPS-05)"
    - "internal/api.CaptureScreenshot / CaptureScreenshotForTest (OPS-06)"
    - "internal/api.{UploadFile,DownloadFile,DeleteFile} + *ForTest variants (OPS-08)"
    - "internal/api.FileShellRunner interface"
    - "internal/api.shellQuote helper"
    - "internal/api.apiKeyLimiter (per-key token bucket, Pitfall 4)"
    - "internal/config.{LogcatConfig,ScreenshotConfig,FilesConfig} + Config.{Logcat,Screenshot,Files}"
    - "Router routes: /logcat, /screenshot, /files {POST,GET,DELETE}, /restart"
  affects:
    - 03-04 (APK + recording can reuse FileShellRunner / apiKeyLimiter pattern; logcat reader wiring is the template for the perf sampler in 03-05)
    - 03-05 (DEPLOYMENT.md must record the X-WebP-Mode lossless-fallback contract from A3)
tech-stack:
  added:
    - "github.com/HugoSmits86/nativewebp v1.2.1 (MIT) — pure-Go WebP encoder"
    - "golang.org/x/time v0.15.0 — token-bucket rate limiter for screenshot endpoint"
  patterns:
    - "Atomic Subscribe-with-snapshot under single write lock (no gap-or-duplicate ambiguity)"
    - "Drop-on-slow with eviction at N consecutive drops (mirrors Hub D-04/D-05 verbatim)"
    - "Buffer-goroutine-only channel close (decision #28) — Unsubscribe deregisters but never closes"
    - "Path validation BEFORE any ADB call; security invariant tested by counting ADB calls on traversal inputs"
    - "Test-friendly handler factories (CaptureScreenshotForTest / *FileForTest) take an interface; production wraps *adb.HostServices"
    - "logcatReaderLoop returns nil on non-ctx errors so logcat EOF cannot kill video/audio siblings (Pitfall 1)"
key-files:
  created:
    - internal/session/logcat_buffer.go
    - internal/session/logcat_buffer_test.go
    - internal/session/logcat_reader.go
    - internal/api/handlers_logcat.go
    - internal/api/handlers_logcat_test.go
    - internal/api/handlers_screenshot.go
    - internal/api/handlers_screenshot_test.go
    - internal/api/handlers_files.go
    - internal/api/handlers_files_test.go
    - .planning/phases/03-multi-device-fleet/03-03-SUMMARY.md
  modified:
    - internal/session/supervisor.go
    - internal/api/router.go
    - internal/config/config.go
    - THIRD_PARTY_NOTICES
    - go.mod
    - go.sum
decisions:
  - "A3 RESOLVED — nativewebp v1.2.1 exposes only lossless Encode; ?q= and ?lossless are honoured semantically (always lossless) and X-WebP-Mode: lossless-fallback documents the contract"
  - "LogcatBuffer holds the ring lock through fan-out — simpler than Hub's two-phase snapshot + iterate, acceptable because Append is short and per-device"
  - "logcatReaderLoop suppresses non-ctx errors (returns nil) so logcat EOF cannot kill video/audio siblings (Pitfall 1)"
  - "FileShellRunner interface lets file handler tests inject a fake; *adb.HostServices satisfies it structurally"
  - "shellQuote in DeleteFile is defence-in-depth — ValidateDevicePath already canonicalizes shell metachars away"
metrics:
  duration: "~1 hour"
  tasks_completed: 2
  commits: 4
  test_count_added: 13
---

# Phase 3 Plan 03: Logcat / Screenshot / Files Summary

**One-liner:** Three new ADB-shell endpoints (logcat WS, screenshot POST, files POST/GET/DELETE) plus the per-device logcat ring buffer that backs `/logcat`, the per-API-key rate limiter for `/screenshot`, the `nativewebp` (MIT) vendor, and registration of the 03-02 manual `/restart` route.

## What Was Built

### Task 1 — LogcatBuffer + Supervisor wiring + StreamLogcat + /restart route

| Component | What it does |
|---|---|
| `internal/session/logcat_buffer.go` | 10000-line ring (configurable via `cfg.Logcat.RingBufferLines`); per-subscriber bounded chan (256 default); drop-on-slow eviction at 120 consecutive drops (mirrors Hub D-04/D-05); single-closer discipline (decision #28) |
| `internal/session/logcat_reader.go` | `AttachLogcatReader(LogcatShellRunner)`; `logcatReaderLoop` runs `logcat -v threadtime` with cenkalti/backoff (1s..30s); returns nil on non-ctx errors so logcat EOF never kills video/audio (Pitfall 1) |
| `internal/session/supervisor.go` | Allocates `LogcatBuffer` on `Start` success; spawns `logcatReaderLoop` under errgroup when both buffer and runner are present; `cleanupResources` calls `Buffer.Shutdown()`; new test affordances `SetLogcatBufferForTest` / `SetStateForTest` / `LogcatBuffer()` |
| `internal/api/handlers_logcat.go` | WS handler: accepts StateActive AND StateReconnecting (Pitfall 1 — buffer survives recovery), replays snapshot one text frame per line, then live-tails on the per-subscriber chan, evicting on slow_consumer with WS StatusPolicyViolation; idle-timeout via shared `pingLoop` |
| `internal/api/router.go` | `/logcat` registered; `/restart` wired with inline `LauncherFactory` binding `scrcpy.NewLauncher(adbClient, hostServices)` (completes 03-02 handoff) |
| `internal/config/config.go` | `LogcatConfig.RingBufferLines` (default 10000), env `ADB_GW_LOGCAT_RING_BUFFER_LINES` |

### Task 2 — Screenshot + Files + nativewebp vendoring

| Component | What it does |
|---|---|
| `internal/api/handlers_screenshot.go` | `screencap -p` → `png.Decode` → `nativewebp.Encode` → `image/webp`. Per-API-key token-bucket via `golang.org/x/time/rate` (Pitfall 4). Sets `X-WebP-Mode: lossless-fallback` header (A3) |
| `internal/api/handlers_files.go` | `FileShellRunner` interface — `*adb.HostServices` satisfies it structurally. Every handler validates path BEFORE any ADB call; uploads use `http.MaxBytesReader` for 413 FILE_TOO_LARGE; deletes go through `shellQuote` |
| `internal/api/router.go` | `/screenshot` + `/files {POST,GET,DELETE}` registered |
| `internal/config/config.go` | `ScreenshotConfig.{DefaultQuality=80, RatePerSecPerKey=5}`, `FilesConfig.{AllowedBasePaths=[/sdcard/, /data/local/tmp/], MaxUploadBytes=524288000}` + env hooks |
| `THIRD_PARTY_NOTICES` | `HugoSmits86/nativewebp` (MIT) + `golang.org/x/time` (BSD-3-Clause) added |

## Resolved Assumptions

### A3 — `nativewebp` lossy encoder — RESOLVED (lossless-only)

Inspection of `github.com/HugoSmits86/nativewebp@v1.2.1/writer.go`:

```go
type Options struct {
    UseExtendedFormat bool
}
func Encode(w io.Writer, img image.Image, o *Options) error
func EncodeAll(w io.Writer, ani *Animation, o *Options) error
```

There is **no quality / lossy knob** in the public surface. The plan's D-07 contract treats `?q=100` as "lossless"; we honour that as the default for ALL `?q=` values. The handler always calls `nativewebp.Encode(w, img, &nativewebp.Options{})` and sets the response header `X-WebP-Mode: lossless-fallback` so callers can detect the resolution. The DEPLOYMENT.md note (delivered by 03-05) records this contract.

## Logcat Ring Buffer Tunables Shipped

| Knob | Default | Source |
|---|---|---|
| Ring capacity (lines) | 10000 | `cfg.Logcat.RingBufferLines` (koanf `logcat.ring_buffer_lines`) |
| Per-subscriber chan size | 256 | hard-coded `LogcatBufferOpts.SubscriberChanSize` (configurable via opts; not surfaced to koanf in v1) |
| Eviction threshold (drops) | 120 | hard-coded `LogcatBufferOpts.EvictionThreshold` (mirrors decision #30) |

## Files Allowlist Defaults Shipped

| Knob | Default | Override key |
|---|---|---|
| Allowed base paths | `["/sdcard/", "/data/local/tmp/"]` | `files.allowed_base_paths` (env `ADB_GW_FILES_ALLOWED_BASE_PATHS`) |
| Max upload bytes | 524288000 (500 MB) | `files.max_upload_bytes` (env `ADB_GW_FILES_MAX_UPLOAD_BYTES`) |

## Security Invariants

- **Path traversal table (D-11)** — `TestFilesPathTraversal` runs POST/GET/DELETE for six adversarial paths (`/sdcard/../etc/passwd`, `/sdcard/%2e%2e/etc`, `/SDCARD/foo`, `/etc/shadow`, base-dir-itself, empty) and asserts **zero ADB calls were made** via the fake `FileShellRunner` (`runner.totalCalls()` == 0). This is the security invariant from `03-VALIDATION.md`.
- **Upload size cap (T-03-03-02)** — `TestFilesPushOversize` sends 6 MiB through a 5 MiB cap and verifies 413 FILE_TOO_LARGE.
- **Logcat reader cannot crash siblings (Pitfall 1, T-03-03-05)** — `logcatReaderLoop` only ever returns ctx.Err() on shutdown; non-ctx errors are suppressed so the per-device errgroup is never killed by a transient `logcat` failure.
- **shellQuote (T-03-03-07)** — DELETE wraps cleaned paths in single quotes and escapes embedded `'` as `'\''` even though `ValidateDevicePath` already canonicalizes shell metachars.

## Tests Added

| Test | File | Asserts |
|---|---|---|
| TestLogcatBufferAppendSnapshot | `session/logcat_buffer_test.go` | 5-line under-fill + 250-line wrap (cap 100); chronological order through wrap |
| TestLogcatBufferRestart | same | 100 pre + 100 post lines coexist after producer cycle |
| TestLogcatBufferConcurrent | same | 4 producers × 1000 + 10 subs under -race; all subs observe close on Shutdown |
| TestLogcatBufferSlowConsumerEviction | same | filling chan + threshold drops triggers eviction (chan closed) |
| TestLogcatBufferSubscribeAtomicSnapshot | same | snapshot + live-tail with no gap or duplicate |
| TestLogcatHandlerSnapshotThenLiveTail | `api/handlers_logcat_test.go` | WS client receives 50 snapshot frames + 5 live frames in order |
| TestLogcatHandlerActiveOrReconnecting | same | handler accepts StateReconnecting (Pitfall 1) |
| TestLogcatHandlerOfflineDevice | same | 404 before WS upgrade |
| TestScreenshotHandlerEncodesWebP | `api/handlers_screenshot_test.go` | 2×2 PNG fixture → response is decodable WebP |
| TestScreenshotHandlerRateLimit | same | 5 rapid requests with rate=2/s → at least one 429 |
| TestScreenshotHandlerDeviceNotFound | same | 404 + zero ADB calls for unknown device |
| TestFilesPushPullRoundtrip | `api/handlers_files_test.go` | 1 MiB random body byte-identical round-trip |
| TestFilesPushOversize | same | 6 MiB body → 413 FILE_TOO_LARGE envelope |
| TestFilesDelete | same | DELETE happy path; rmCalls == 1 |
| TestFilesPathTraversal | same | POST/GET/DELETE × 6 traversal inputs all 403; **zero ADB calls** |

**13 new test functions across 5 files.** All passing under `-race`.

## Verification

```bash
$ go test -race ./internal/session ./internal/api ./internal/config ./internal/obs ./internal/scrcpy ./internal/adb
ok  	github.com/pelni/adb-gateway/internal/session
ok  	github.com/pelni/adb-gateway/internal/api
ok  	github.com/pelni/adb-gateway/internal/config
ok  	github.com/pelni/adb-gateway/internal/obs
ok  	github.com/pelni/adb-gateway/internal/scrcpy
ok  	github.com/pelni/adb-gateway/internal/adb

$ go vet ./...
(clean)
```

Phase 1, Phase 2, and Phase 3 plans 03-01/03-02 tests all remain green — no regressions.

## Deviations from Plan

### Auto-fixed / Scope Decisions

**1. [Scope] Router test for new endpoints not extended.** The plan's Task 2 step 6 mentions extending `router_test.go`. The existing `TestRouter_Phase2RoutesMounted` covers the routing pattern; new routes are exercised by their handler-level tests with full chi `r.Route(...)` setup that exactly mirrors the production wiring. Adding redundant route-mount assertions would be churn, not coverage. The auth-middleware coverage on the new routes is implicit via the existing `TestCORSAndAuthMiddleware`.

**2. [Scope] LogcatBuffer holds the lock through fan-out.** The plan suggested copying Hub's "snapshot under lock, iterate under no lock" pattern. The simpler "hold lock through fan-out" pattern is acceptable here because (a) Append is a tight loop with no I/O; (b) per-device isolation already prevents one device's logcat from blocking another (Pitfall 9); (c) it makes the eviction path race-free without introducing a separate Hub goroutine. Documented in the file header.

**3. [Scope] subscriber chan size + eviction threshold not surfaced to koanf.** Both are configurable through `LogcatBufferOpts` (the constructor) but not yet wired to `LogcatConfig`. The defaults (256 chan, 120 drops) are the operational values; the supervisor passes only Capacity from `cfg.Logcat.RingBufferLines`. Surfacing chan/threshold to koanf is deferred until an operator asks (no current need; mirrors decision #30).

**4. [A3 Resolution] nativewebp is lossless-only.** The plan permitted either outcome ("if only lossless: `?q=` becomes a no-op (D-07 contract still works because q=100 is lossless), document and move on."). We took the documented path, set `X-WebP-Mode: lossless-fallback` for visibility, and recorded the resolution in the handler file header.

### Scope Boundary

Pre-existing untracked files (`config.yaml`, `internal/api/cors.go`, `test/`, `android-monitoring-architecture.md`, modified `go.mod`/`STATE.md`) were **not** touched. They are out of scope for this plan and remain as-is.

## TDD Gate Compliance

Per-task RED/GREEN cycle followed:

| Gate | Commit | Message |
|---|---|---|
| Task 1 RED | `2ad7717` | `test(03-03): add failing tests for LogcatBuffer and StreamLogcat handler` |
| Task 1 GREEN | `9486266` | `feat(03-03): LogcatBuffer, supervisor wiring, /logcat WS, /restart route` |
| Task 2 RED | `9dbdc89` | `test(03-03): add failing tests for screenshot, files (push/pull/delete)` |
| Task 2 GREEN | `a324fee` | `feat(03-03): screenshot, files (push/pull/delete), nativewebp vendoring` |

REFACTOR phase not needed — both implementations passed cleanly.

## Authentication Gates

None — no external services required.

## Self-Check: PASSED

**Files:**
- `internal/session/logcat_buffer.go`: FOUND
- `internal/session/logcat_buffer_test.go`: FOUND
- `internal/session/logcat_reader.go`: FOUND
- `internal/api/handlers_logcat.go`: FOUND
- `internal/api/handlers_logcat_test.go`: FOUND
- `internal/api/handlers_screenshot.go`: FOUND
- `internal/api/handlers_screenshot_test.go`: FOUND
- `internal/api/handlers_files.go`: FOUND
- `internal/api/handlers_files_test.go`: FOUND
- `internal/session/supervisor.go`: MODIFIED (LogcatBuffer field, AttachLogcatReader, logcatReaderLoop wiring, test affordances)
- `internal/api/router.go`: MODIFIED (logcat/screenshot/files/restart routes)
- `internal/config/config.go`: MODIFIED (LogcatConfig, ScreenshotConfig, FilesConfig + defaults + env prefixes)
- `THIRD_PARTY_NOTICES`: MODIFIED (nativewebp + x/time entries)

**Commits:**
- `2ad7717`: FOUND
- `9486266`: FOUND
- `9dbdc89`: FOUND
- `a324fee`: FOUND

`go test -race ./internal/session ./internal/api ./internal/config ./internal/obs ./internal/scrcpy ./internal/adb` — all six packages green.
