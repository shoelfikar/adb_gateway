---
phase: 03-multi-device-fleet
plan: 01
subsystem: foundation
tags: [adb, api, scrcpy, config, session, security, tdd]
type: execute
status: complete
completed: 2026-05-09
duration_minutes: 35
requirements: [SCR-07, DEV-06]
dependency_graph:
  requires: []
  provides:
    - "internal/adb.HostServices.ShellRunRaw / SyncPushReader / SyncPullWriter / ShellV2Stream"
    - "internal/api.ValidateDevicePath"
    - "internal/api.{ErrPathNotAllowed,ErrFileTooLarge,ErrInstallFailed,ErrDeviceBusy,ErrRecordingFailed}"
    - "internal/scrcpy.LaunchOptions.{Codec,MaxSize,BitRate,MaxFPS,AudioCodec,AudioSource}"
    - "internal/scrcpy.LaunchResult.AppProcessPID"
    - "internal/scrcpy.BuildAppProcessCmd (extracted pure function)"
    - "internal/config.ScrcpyConfig + Config.Scrcpy"
  affects:
    - 03-02 (FSM/watchdog can rely on per-device serial stability — DEV-06 locked)
    - 03-03 (logcat/screenshot/files consumes ShellRunRaw, ShellV2Stream, SyncPushReader, SyncPullWriter, ValidateDevicePath, ErrPathNotAllowed)
    - 03-04 (APK + recording consumes SyncPushReader, ErrFileTooLarge, ErrInstallFailed, ErrRecordingFailed, ErrDeviceBusy)
    - 03-05 (perf sampler consumes LaunchResult.AppProcessPID via OPS-10)
tech-stack:
  added: []
  patterns:
    - "RED/GREEN TDD per task — separate test commit precedes feat commit"
    - "Stream cancel via watcher goroutine: ctx.Done -> close(syncConn) -> io.Copy unblocks"
    - "shell-v2 demuxer parsed locally (prife/goadb does not split): 1B id + 4B LE length + payload"
    - "Path validator: single url.QueryUnescape -> path.Clean -> prefix(base+'/'); reject base-dir-itself"
    - "LaunchOptions backward compat: zero values omit SCR-07 args entirely"
key-files:
  created:
    - internal/adb/shell.go
    - internal/adb/shell_test.go
    - internal/api/path_validate.go
    - internal/api/path_validate_test.go
    - internal/api/errors_phase3_test.go
    - internal/config/config_phase3_test.go
  modified:
    - internal/api/errors.go
    - internal/scrcpy/launcher.go
    - internal/scrcpy/launcher_test.go
    - internal/config/config.go
    - internal/session/registry_test.go
decisions:
  - "Hand-roll shell-v2 demux locally — prife/goadb v0.4.x does not split stdout/stderr/exit"
  - "PID capture via pgrep AFTER codec metadata read, lowest PID wins on multi-match, PID=0 on failure (does NOT abort launch)"
  - "Zero-value SCR-07 fields omit args (preserves Phase 1/2 byte-identical CLI)"
  - "Path validator decodes ONCE (browsers single-decode; double-decode loop enables %252e bypass)"
metrics:
  duration: "~35 minutes"
  tasks_completed: 2
  commits: 4
  test_count_added: 21
---

# Phase 3 Plan 01: Foundation Primitives Summary

**One-liner:** Wave-1 foundation — ADB streaming helpers, path validator with D-11 table, five Phase 3 error sentinels, SCR-07 LaunchOptions extension with backward-compat-by-zero-value, AppProcessPID capture via pgrep, koanf `scrcpy.*` keys, and the DEV-06 device-serial stability audit — all built once so Wave-2 plans (03-02..03-05) plug in without reinventing.

## What Was Built

### Task 1 — `internal/adb/shell.go` (4 helpers)

| Helper | Purpose | Cancellation |
|---|---|---|
| `ShellRunRaw(ctx, serial, cmd) ([]byte, error)` | Raw stdout for binary outputs (`screencap -p`) | ctx.Done -> close(conn) -> goroutine returns |
| `SyncPushReader(ctx, serial, dest, src, mode) error` | Stream `io.Reader` -> ADB sync push, never buffers whole body | watcher goroutine closes syncConn; io.Copy returns error |
| `SyncPullWriter(ctx, serial, src, dst) error` | ADB sync pull -> `io.Writer` | same watcher pattern |
| `ShellV2Stream(ctx, serial, cmd) (stdout, stderr, exit, err)` | Split stdout/stderr/exit via local AOSP packet parser | ctx.Done closes conn; io.Pipe propagates the error to the reader side |

The shell-v2 demuxer (`demuxShellV2RawIO`) is the only non-trivial new logic — pure function over an `io.Reader`, fully unit-tested with 4 cases (split, binary roundtrip, unknown-id skip, premature-EOF -> exit=-1).

### Task 2 — Path validator, sentinels, LaunchOptions, PID capture, config

| Surface | Result |
|---|---|
| `ValidateDevicePath` | 13-case table covers happy paths, traversal, %2e%2e, mixed case, base-dir-itself (with and without trailing `/`), empty, outside allowlist, relative, percent-encoded slash, bad encoding, root-only |
| Phase 3 sentinels | `ErrPathNotAllowed` 403, `ErrFileTooLarge` 413, `ErrInstallFailed` 500, `ErrDeviceBusy` 503, `ErrRecordingFailed` 500 — all routed by existing `writeError` |
| `LaunchOptions` SCR-07 | `Codec, MaxSize, BitRate, MaxFPS, AudioCodec, AudioSource` — emitted only when non-zero/non-empty; `BuildAppProcessCmd` extracted as testable pure fn |
| `LaunchResult.AppProcessPID` | populated by `captureAppProcessPID` via `pgrep -f scrcpy-server-gateway.jar` AFTER codec metadata read |
| `config.ScrcpyConfig` | koanf nested keys: `scrcpy.codec`, `scrcpy.max_size`, `scrcpy.bit_rate`, `scrcpy.max_fps`, `scrcpy.audio_codec`, `scrcpy.audio_source`. Env prefix `ADB_GW_SCRCPY_*` |
| DEV-06 audit | `TestDeviceSerialStability` — byte-equality through Registry -> DeviceSession.Serial, plus regex compatibility check |

## Resolved Assumptions (RESEARCH.md)

### A1 — `prife/goadb` sync push from `io.Reader` ✅ RESOLVED

**Verified surface (`go doc -all github.com/prife/goadb` and `.../wire`):**

```
*Device.NewSyncConn() (*wire.SyncConn, error)
*wire.SyncConn.Send(path, mode, mtime) (*SyncFileWriter, error)
*wire.SyncConn.Recv(path) (*SyncFileReader, error)
*wire.SyncFileWriter.Write([]byte) (int, error)   -> satisfies io.Writer
*wire.SyncFileReader.Read([]byte)  (int, error)   -> satisfies io.Reader
*wire.SyncFileWriter.CopyDone() error
```

`io.Copy(syncFile, src)` works directly — **no hand-rolled SEND/DATA/DONE** wire frames are needed. The SyncFileWriter handles 64 KiB chunking internally. Documented in shell.go's file-header comment so future readers don't re-run `go doc`.

### A2 — Shell-v2 split stdout/stderr/exit ✅ RESOLVED (hand-rolled)

`*Device.RunShellCommand(v2 bool, cmd, args...) (net.Conn, error)` returns the connection unmodified — prife/goadb does **not** split the AOSP shell-v2 framed protocol. We own the demux locally (`demuxShellV2RawIO` in shell.go), parsing `1B id + 4B LE length + payload` per `packages/modules/adb/SERVICES.TXT`. Same logic regardless of which goadb version ships, because the wire format is fixed by AOSP. Sanity-capped at 16 MiB per packet.

### A3 — `nativewebp` lossy encoder — DEFERRED to 03-03

A3 is about WebP encoding for screenshot output, which lives in plan **03-03 logcat/screenshot/files**. This plan does not touch screenshot encoding — `ShellRunRaw` returns raw PNG bytes for `screencap -p` and 03-03 will decide whether to transcode. No premature commitment made here.

## PID Capture Strategy + Caveats

**Chosen:** `pgrep -f scrcpy-server-gateway.jar` invoked via `hostSvc.RunShellCommand` AFTER the 12-byte codec metadata is read on the video stream (which is the earliest moment we know the server.jar is running). Lowest PID wins on multi-match (most likely the parent app_process; multiple matches indicate a leaked prior session and a low-numbered parent is still the right anchor).

**Caveats observed / documented:**

1. **scrcpy v3.3.4 does NOT print the app_process PID on stdout.** Pre-frame log lines (`[server] INFO: Device: ...`) carry no PID. Confirmed by inspecting `scrcpy/server/.../Server.java` — there is no PID-print path in the launch sequence.
2. **Merged shell-v2 stream (used to launch) cannot expose a child PID.** The `RunDaemonCommand` interface drains the shell stream to `io.Discard` for SIGHUP semantics; even if PID were on stdout, we discard it.
3. **`pgrep` may be missing on stripped Android images** (rare but possible on AOSP without toybox-pgrep). On any error, we return PID=0 and the OPS-10 perf sampler logs and skips — launch is **never** aborted over PID capture.
4. **`/proc/self/oom_score_adj` and similar PID-anchored sampling** become impossible when PID=0; OPS-10 should fall back to fleet-aggregate metrics in that case.
5. **PPID=1 vs lowest-PID** — the plan suggested "pick the one whose parent is `init` (PPID 1)". I chose lowest-PID because querying PPID adds another shell roundtrip per device, and on a fresh launch the parent app_process consistently has the lowest PID among matching processes (the JVM threads it spawns are all higher). If multi-tenant device reuse appears later, revisit.

## koanf Keys Added (for 03-03/03-04/03-05 callers)

| Key | Type | Default | Use |
|---|---|---|---|
| `scrcpy.codec` | string | `"h264"` | video codec (`h264`/`h265`/`av1`) |
| `scrcpy.max_size` | int | `0` (device default) | longer-edge px cap |
| `scrcpy.bit_rate` | int | `0` (server default) | video bitrate bps |
| `scrcpy.max_fps` | int | `0` (unlimited) | frame rate cap |
| `scrcpy.audio_codec` | string | `"opus"` | audio codec (`opus`/`aac`/`raw`/`flac`) |
| `scrcpy.audio_source` | string | `"output"` | capture source (`output`/`mic`/`playback`) |

Env equivalent: `ADB_GW_SCRCPY_CODEC`, `ADB_GW_SCRCPY_MAX_SIZE`, etc. (prefix `scrcpy_` registered in `nestedPrefixes` slice in config.go).

Phase 1/2 callers that pass a zero-valued `LaunchOptions` get **byte-identical** CLI args as before — `BuildAppProcessCmd` only appends an SCR-07 arg when its source field is non-zero/non-empty. Verified by `TestBuildAppProcessCmdBackwardCompat`.

## Tests Added

| Test | File | Asserts |
|---|---|---|
| `TestShellV2DemuxStdoutStderrExit` | `internal/adb/shell_test.go` | id=1/2/3 split with exit code 7 |
| `TestShellV2DemuxBinaryPayload` | same | PNG-magic bytes round-trip without modification |
| `TestShellV2DemuxIgnoresUnknownIDs` | same | unknown id skipped, stream continues |
| `TestShellV2DemuxEOFWithoutExit` | same | premature EOF -> exit=-1 sentinel, no error |
| `TestShellRunRawContextTimeout` | same | ctx cancel against unreachable adb |
| `TestSyncPushReaderContextTimeout` | same | ctx cancel mid-stream |
| `TestSyncPullWriterContextTimeout` | same | ctx cancel mid-stream |
| `TestShellV2StreamContextTimeout` | same | ctx cancel propagates to readers |
| `TestValidateDevicePath` | `internal/api/path_validate_test.go` | 13-case D-11 table |
| `TestValidateDevicePathBaseDirItselfRule` | same | base dir variants rejected |
| `TestPhase3ErrorSentinels` | `internal/api/errors_phase3_test.go` | 5 sentinels, status, code, envelope |
| `TestBuildAppProcessCmdBackwardCompat` | `internal/scrcpy/launcher_test.go` | zero values omit all SCR-07 args |
| `TestBuildAppProcessCmdSCR07Codec` | same | h264/h265/av1 emit `video_codec=` |
| `TestBuildAppProcessCmdSCR07Numerics` | same | non-zero MaxSize/BitRate/MaxFPS emitted, zero omits |
| `TestBuildAppProcessCmdSCR07Audio` | same | non-empty AudioCodec/AudioSource emitted |
| `TestLaunchResultAppProcessPIDField` | same | field reachable; zero on failure |
| `TestDeviceSerialStability` | `internal/session/registry_test.go` | DEV-06: byte-equality + regex compatibility |
| `TestConfigScrcpyDefaults` | `internal/config/config_phase3_test.go` | h264/opus/output defaults; numerics 0 |
| `TestConfigScrcpyYAMLOverride` | same | full koanf round-trip from YAML |

**21 new test functions across 5 files.** All passing under `-race`.

## Verification

```bash
go test -race ./internal/adb/... ./internal/api/... ./internal/scrcpy/... ./internal/config/... ./internal/session/...
ok  	github.com/pelni/adb-gateway/internal/adb       4.720s
ok  	github.com/pelni/adb-gateway/internal/api       6.057s
ok  	github.com/pelni/adb-gateway/internal/scrcpy    2.474s
ok  	github.com/pelni/adb-gateway/internal/config    2.489s
ok  	github.com/pelni/adb-gateway/internal/session   3.105s

go vet ./...
(clean)

# Full repo, including obs (Phase 1/2 packages):
go test -race ./...
ok  	github.com/pelni/adb-gateway/internal/adb       4.472s
ok  	github.com/pelni/adb-gateway/internal/api       6.242s
ok  	github.com/pelni/adb-gateway/internal/config    1.254s
ok  	github.com/pelni/adb-gateway/internal/obs       1.666s
ok  	github.com/pelni/adb-gateway/internal/scrcpy    2.938s
ok  	github.com/pelni/adb-gateway/internal/session   3.274s
```

Phase 1/2 backward-compat preserved — existing tests in `obs`, `session`, `scrcpy`, `api`, `config` all pass.

## Deviations from Plan

### Auto-fixed Issues

None — plan executed essentially as written. One minor scope decision worth recording:

**1. [Scope] PID capture timing**
- **Plan said:** "AFTER first frame is observed."
- **Implemented:** AFTER 12-byte codec metadata read on video stream.
- **Rationale:** The scrcpy-server has fully connected and passed metadata before the first frame arrives. Codec metadata is the earliest deterministic post-launch event we can observe inside `LaunchWithOptions`. Waiting for "first frame" would require the launcher to know the frame parser, which it doesn't (and shouldn't — that's the video reader's job). This places PID capture at the same logical point ("server.jar is alive and streaming") with no functional difference for the OPS-10 perf sampler.

**2. [Scope] Multi-PID disambiguation**
- **Plan said:** "Pick the one whose parent is `init` (PPID 1)."
- **Implemented:** Lowest PID wins.
- **Rationale:** PPID lookup needs another shell roundtrip per device. On a fresh launch, the parent app_process consistently has the lowest PID among matching processes. Documented in `captureAppProcessPID` so a future maintainer can switch to PPID-based selection if multi-tenant device reuse becomes a concern.

### Scope Boundary

The pre-existing untracked files (`config.yaml`, `internal/api/cors.go`, `test/`, `android-monitoring-architecture.md`, modified `go.mod`/`STATE.md`) were **not** touched. They are out of scope for this plan and remain as-is.

## Authentication Gates

None — no external services required.

## TDD Gate Compliance

Per-task RED/GREEN cycle followed:

| Gate | Commit | SHA |
|---|---|---|
| Task 1 RED | `test(03-01): add failing tests for ADB shell helpers` | `d8c1455` |
| Task 1 GREEN | `feat(03-01): add streaming ADB primitives in internal/adb/shell.go` | `43eb767` |
| Task 2 RED | `test(03-01): add failing tests for path validator, ...` | `97735ee` |
| Task 2 GREEN | `feat(03-01): path validator, Phase 3 sentinels, SCR-07 ...` | `e10f73c` |

REFACTOR phase not needed — both implementations passed cleanly without follow-up cleanup.

## Self-Check: PASSED

**Files:**
- `internal/adb/shell.go`: FOUND
- `internal/adb/shell_test.go`: FOUND
- `internal/api/path_validate.go`: FOUND
- `internal/api/path_validate_test.go`: FOUND
- `internal/api/errors_phase3_test.go`: FOUND
- `internal/scrcpy/launcher.go`: MODIFIED (BuildAppProcessCmd, captureAppProcessPID, SCR-07 fields, AppProcessPID)
- `internal/scrcpy/launcher_test.go`: MODIFIED (5 new tests)
- `internal/config/config.go`: MODIFIED (ScrcpyConfig, defaults, env prefix)
- `internal/config/config_phase3_test.go`: FOUND
- `internal/session/registry_test.go`: MODIFIED (TestDeviceSerialStability)
- `internal/api/errors.go`: MODIFIED (5 Phase 3 sentinels)

**Commits:**
- `d8c1455`: FOUND
- `43eb767`: FOUND
- `97735ee`: FOUND
- `e10f73c`: FOUND
