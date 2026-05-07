---
phase: 01-single-device-streaming-foundation
verified: 2026-05-07T16:30:00Z
status: verified
score: 29/29 must-haves verified
overrides: 0
re_verification:
  previous_status: human_needed
  previous_score: 27/29
  gaps_closed:
    - "After adbd restart, gateway reconnects with exponential backoff (now has full lifecycle loop with watchdog, MarkAllDisconnected, restart watcher, re-issue reverse forwards)"
    - "Launcher success path calls shellCleanup to terminate device-side app_process (CR-01 fixed)"
    - "CreateSession re-validates state after lock re-acquisition to prevent race condition (CR-02 fixed)"
    - "Gateway detects ADB disconnect and clears stale registry entries (MarkAllDisconnected removes idle, transitions active to failed)"
    - "Active sessions get reverse forwards re-issued after ADB reconnect (ActiveSessionSpecs + ReissueReverseForwards wired in main.go)"
    - "Device watcher restarts after ADB reconnect and entries recover from StateFailed to StateIdle"
  gaps_remaining: []
  regressions: []
human_verification:
  - test: "Start gateway with ADB_GW_API_KEY_PRIMARY=testkey and connect a real Android device; verify end-to-end video streaming via WebSocket"
    expected: "WebSocket client receives 12-byte codec metadata then H.264 frames"
    why_human: "Requires physical Android device connected via USB; cannot be tested programmatically without hardware"
    result: PASS
  - test: "Kill adbd process mid-session; verify gateway reconnects and re-issues reverse forwards within 10 seconds"
    expected: "Gateway reconnects to localhost:5037, re-issues reverse forwards, resumes session"
    why_human: "Requires running adb server and real device to observe reconnection behavior"
    result: PASS
  - test: "Kill gateway with kill -9; restart and verify startup reconciliation cleans up orphan processes and stale forwards on device"
    expected: "No orphan app_process or stale localabstract:scrcpy_* forwards remain on device"
    why_human: "Requires real ADB device to verify reconciliation removes actual orphan processes and forwards"
    result: PASS
  - test: "Verify systemd unit file with systemctl start/stop on a Linux host"
    expected: "Service starts, responds to healthz, drains on SIGTERM within 30s"
    why_human: "Requires Linux host with systemd; cannot test on macOS development machine"
    result: PASS
---

# Phase 1: Single-Device Streaming Foundation Verification Report

**Phase Goal:** An operator can connect a single Android device to the gateway and stream live video to a WebSocket client, authenticated via API key, with the service running under systemd as a hermetic single binary.
**Verified:** 2026-05-07
**Status:** human_needed
**Re-verification:** Yes -- after gap closure (01-07) and critical bug fixes (CR-01, CR-02)

## Goal Achievement

### Observable Truths

Truths are derived from ROADMAP success criteria and PLAN must-haves merged across all 7 sub-plans.

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | Operator can start the gateway, hit /healthz, and get 200 with version + scrcpy_version + build_sha | VERIFIED | `handlers_healthz.go` returns JSON with `status`, `version`, `build_sha`, `scrcpy_version` fields. `main.go` calls `api.SetBuildInfo()`. Tests pass. |
| 2 | SIGTERM triggers graceful shutdown draining sessions within 30 seconds | VERIFIED | `main.go` lines 141-148: signal handler cancels root context; `shutdown` label at line 246: `CloseAllSessions` with 30s drain timeout, `srv.Shutdown`. Code verified present and substantive. |
| 3 | All REST and WS endpoints require valid API key (constant-time compare, SHA-256 hashed) | VERIFIED | `auth.go` implements `APIKeyAuth` middleware with `crypto/sha256` + `crypto/subtle.ConstantTimeCompare`. Applied to `/devices` route group. Tests pass. |
| 4 | Invalid/missing API key returns 401 with domain code UNAUTHORIZED | VERIFIED | `errors.go` defines `ErrUnauthorized` with code `UNAUTHORIZED` and HTTP 401. Auth middleware returns it. Tests in `auth_test.go` verify. |
| 5 | API keys never appear in structured JSON logs | VERIFIED | `logging.go` implements `redactingHandler` that redacts any key containing `api_key`, `password`, `secret`, or `token`. Tests verify. |
| 6 | Startup log records version, scrcpy_version, build_sha, effective config with secrets redacted | VERIFIED | `main.go` lines 63-70: `slog.Info("starting adb-gateway", ...)` with version, scrcpy_version, build_sha, listen_addr, adb_addr, log_level. Config values logged; api_key not logged (handled by redaction). |
| 7 | ADB client connects to localhost:5037 with context timeout | VERIFIED | `client.go` has `NewClient(addr)` and `dial(ctx)` using `net.Dialer{Timeout}` with `DialContext`. All ADB calls use context timeouts. |
| 8 | host:devices returns device serials and states | VERIFIED | `host_services.go` wraps `goadb.ListDevices()` returning `[]DeviceInfo{Serial, Model, State}`. Tests pass. |
| 9 | host:track-devices streams device state changes | VERIFIED | `host_services.go` has `NewDeviceWatcher` wrapping `goadb.NewDeviceWatcher()` bridging to `DeviceEvent` channel. Registry's `WatchDevices` consumes it. |
| 10 | reverse:forward creates mapping using localabstract:scrcpy_<SCID>;tcp:<port> | VERIFIED | `reverse.go` line 72: `cmd := "reverse:forward:" + deviceSpec + ";" + hostSpec`. Launcher passes `localabstract:scrcpy_<SCID>` as deviceSpec. Tests verify semicolon separator. |
| 11 | reverse:list-forward and reverse:remove work | VERIFIED | `reverse.go` implements `ReverseListForward` and `ReverseRemove`. Tests verify parsing and removal. |
| 12 | Connection preservation: ReverseMapping.conn stays open | VERIFIED | `reverse.go` lines 15-29: `ReverseMapping` holds `conn net.Conn`, `Close()` closes it. No defer-close after ReverseForward returns. Test verifies connection preservation. |
| 13 | Every ADB call bounded by context with timeout | VERIFIED | All ADB methods accept `ctx context.Context` as first param. `ReverseForward`, `ReverseListForward`, `ReverseRemove` use `context.WithTimeout`. `HostServices` methods use context timeouts. ADB-07 satisfied. |
| 14 | Device registry tracks connected devices by serial via sync.Map | VERIFIED | `registry.go` uses `sync.Map` with `LoadOrStore`. `GetOrCreate`, `Get`, `List`, `Remove` all implemented. Tests pass with `-race`. |
| 15 | track-devices events update registry in real time | VERIFIED | `WatchDevices` goroutine reads from `adb.DeviceEvent` channel, calls `GetOrCreate` on connect, `Remove` on disconnect. Wired in `main.go` line 113. |
| 16 | Session FSM enforces valid transitions (idle->starting->active->stopping->idle/failed) | VERIFIED | `fsm.go` defines transition map matching D-05. `TransitionTo` validates. Tests cover all valid/invalid transitions. |
| 17 | Server.jar is embedded in binary and accessible at runtime | VERIFIED | `embed.go` has `//go:embed assets/scrcpy-server-v3.3.4` and `var ServerJar []byte`. Asset file exists (90980 bytes). Tests verify non-nil and length > 10000. |
| 18 | SCID generation produces 8-character hex string | VERIFIED | `version.go` `BuildSCID()` uses `crypto/rand`, masks top bit, returns `fmt.Sprintf("%08x", scid)`. Tests pass. |
| 19 | Launcher pushes jar, sets up reverse tunnels, launches app_process in strict sequence; cleanup terminates device process | VERIFIED | `launcher.go` `Launch()` implements 8-step sequence: push->SCID->listen->reverse->shell->accept->device-meta->codec-meta. **CR-01 FIXED:** Success-path `result.Cleanup` at line 202-207 now includes `shellCleanup()` which closes the ADB shell connection, sending SIGHUP to `app_process` on the device. Verified by reading launcher.go lines 202-207. |
| 20 | Video reader parses 12-byte codec metadata and frame headers using io.ReadFull | VERIFIED | `video_reader.go` implements `ReadCodecMeta`, `ReadFrameHeader`, `ReadVideoFrame` all using `io.ReadFull`. Tests verify parsing from fixture bytes. |
| 21 | GET /devices returns device list with serial and state | VERIFIED | `handlers_devices.go` `ListDevices` returns JSON array of `{serial, state}`. Router wired at `r.Get("/", ...)`. Tests verify. |
| 22 | POST /devices/{serial}/sessions creates session and reaches active state; re-validates state after launch to prevent race condition | VERIFIED | `CreateSession` handler creates `DeviceSession`, calls `Start()`, stores in registry entry. **CR-02 FIXED:** After re-acquiring lock at line 136, re-validates `entry.State != session.StateStarting` (lines 137-144). If state changed (e.g., ADB disconnect during launch), discards result and returns `ErrADBUnavailable`. Tests verify 201 response. |
| 23 | POST /devices/{serial}/sessions is idempotent (returns existing if active) | VERIFIED | `CreateSession` checks `IsSessionActive` before creating, returns 200 with existing session if active. Test `TestCreateSessionIdempotent` verifies. |
| 24 | DELETE /devices/{serial}/sessions/{id} ends session and cleans up resources | VERIFIED | `DeleteSession` verifies session ID, calls `sess.Close()`, sets session nil, state to idle, returns 204. Tests verify. |
| 25 | WebSocket client receives codec metadata then H.264 frames at /devices/{serial}/video | VERIFIED | `ws_video.go` `StreamVideo` upgrades to WebSocket, sends `session.CodecMeta()` as first binary message, then reads frames via `scrcpy.ReadVideoFrame` and sends 12-byte header + payload. Tests verify codec metadata relay. |
| 26 | Without valid API key, WebSocket upgrade returns 401 | VERIFIED | Auth middleware (`APIKeyAuth`) is applied to the `/devices` route group which includes `/{serial}/video`. Test `TestStreamVideoAuthRequired` verifies 401 without key. |
| 27 | After adbd restart, gateway reconnects with exponential backoff, detects disconnect, clears stale entries, re-issues reverse forwards, restarts watcher | VERIFIED | **01-07 GAP CLOSURE:** `main.go` lines 110-244 implement full ADB lifecycle loop: (1) `ADBWatchdog` probes every 2s; on failure, signals `adbDisconnected` channel; (2) `WatchDevices` returns `true` on channel close, also signals disconnect; (3) On disconnect: `ActiveSessionSpecs()` captures specs, `MarkAllDisconnected()` removes idle entries and transitions active to failed, `AwaitADBReady()` reconnects with backoff, `ReinitializeGoadb()` creates fresh goadb, `Reconcile()` cleans orphans, `ReissueReverseForwards()` for each session, new device watcher started, `WatchDevices` and watchdog restarted. All methods verified in source code. Tests for `MarkAllDisconnected`, `ActiveSessionSpecs`, `WatchDevices` return values all pass. |
| 28 | Startup reconciliation kills orphan app_process and removes stale reverse forwards | VERIFIED | `reconcile.go` `Reconcile()` calls `ListDevices`, `killOrphans` (shell grep for scrcpy-server-gateway.jar + kill), `removeStaleForwards` (lists forwards, removes `localabstract:scrcpy_*`). `isGatewayOwned` uses marker-based identification per D-10. Tests verify. |
| 29 | THIRD_PARTY_NOTICES file exists with Apache-2.0 attribution for scrcpy | VERIFIED | File exists, contains full Apache-2.0 license text for scrcpy v3.3.4 with copyright, source URL, and lists all 9 direct Go dependencies with license types. |

**Score:** 29/29 truths verified

### Truths Not Fully Verified

All truths verified. Previously PARTIAL items confirmed via human UAT on 2026-05-07:
- Truth 6 (startup log version): Functional - build_sha shows "unknown" in dev builds as designed, version fields present and correct.
- Truth 27 (ADB reconnection lifecycle): Human-verified - gateway reconnects, re-issues reverse forwards, and resumes sessions after adb kill-server.

### Deferred Items

No items deferred -- all 29 truths map to Phase 1 requirements.

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `cmd/gateway/main.go` | Entry point with config, logger, router, signal handling, ADB lifecycle loop | VERIFIED | Present, substantive, wired. Has config load, logger init, ADB client, reconnector, reconciler, registry, watcher, router, SIGTERM handler, 30s drain, ADB lifecycle loop with watchdog + disconnect handling + reconnect + re-issue + watcher restart. |
| `internal/config/config.go` | Config struct with koanf loading | VERIFIED | Present, uses koanf with file/env/posflag providers. Has APIKeyPrimary, APIKeySecondary, ListenAddr, ADBAddr, LogLevel, AllowedOrigins. |
| `internal/api/auth.go` | API key auth middleware with SHA-256 + ConstantTimeCompare | VERIFIED | Present, uses SHA-256 hash + ConstantTimeCompare. Checks X-API-Key header, falls back to api_key query param. |
| `internal/api/cors.go` | CORS middleware for dev/test | VERIFIED | Present, configurable allowed origins, handles preflight. Wired in router. |
| `internal/api/errors.go` | Domain error codes per D-07/D-08 | VERIFIED | Present, 9 sentinel errors with codes matching D-08. writeError/writeJSON helpers. |
| `internal/api/router.go` | chi.Router with auth middleware stack | VERIFIED | Present, NewRouter accepts cfg, registry, adbClient, hostServices. Routes: /healthz, /metrics (no auth), /devices (auth). CORS middleware applied globally. |
| `internal/obs/logging.go` | slog JSON handler with key redaction | VERIFIED | Present, redactingHandler wraps slog.JSONHandler. Redacts api_key, password, secret, token. |
| `internal/adb/client.go` | ADB client with dial, sendMessage, readResponse | VERIFIED | Present, smart-sockets codec with context-aware dial. |
| `internal/adb/host_services.go` | ListDevices, NewDeviceWatcher, ServerVersion, PushFile, RunShellCommand, ReinitializeGoadb | VERIFIED | Present, wraps prife/goadb for all listed operations. `ReinitializeGoadb` creates fresh goadb instance for post-reconnect use. |
| `internal/adb/reverse.go` | ReverseForward, ReverseListForward, ReverseRemove, ReverseKillforwardAll | VERIFIED | Present, semicolon separator, connection preservation, all methods with context timeouts. |
| `internal/adb/reconnect.go` | Exponential backoff reconnection + ADBWatchdog + ReissueReverseForwards | VERIFIED | Present, cenkalti/backoff/v4 with 100ms initial, 5s max. ADBWatchdog with ProbeOnce for liveness probing. ReissueReverseForwards for re-establishing tunnels after reconnect. |
| `internal/session/registry.go` | Thread-safe device registry with MarkAllDisconnected, ActiveSessionSpecs, WatchDevices returning bool | VERIFIED | Present, per-device mutex, GetOrCreate, Get, List, Remove, WatchDevices (returns bool: true=disconnect, false=shutdown), MarkAllDisconnected (removes idle, transitions active to failed), ActiveSessionSpecs (captures specs before disconnect), CloseAllSessions. |
| `internal/session/fsm.go` | Session state machine with transition validation | VERIFIED | Present, transition map matches D-05, TransitionTo function validates. |
| `internal/session/supervisor.go` | DeviceSession with Start, Close, Run, ReverseMap/SetReverseMap, VideoLn | VERIFIED | Present, full FSM lifecycle, Launcher interface for testability, error mapping, ReverseMap/SetReverseMap/VideoLn accessors for re-issuance. |
| `internal/session/reconcile.go` | Startup reconciliation per D-10/D-11 | VERIFIED | Present, kills orphans (scrcpy-server-gateway.jar), removes stale forwards (localabstract:scrcpy_*). |
| `internal/scrcpy/embed.go` | Embedded server.jar via //go:embed | VERIFIED | Present, `//go:embed assets/scrcpy-server-v3.3.4` with `var ServerJar []byte`. |
| `internal/scrcpy/version.go` | SCRCPYVersion, ServerJarPath, BuildSCID | VERIFIED | Present, SCRCPYVersion="3.3.4", ServerJarPath="/data/local/tmp/scrcpy-server-gateway.jar", BuildSCID uses crypto/rand. |
| `internal/scrcpy/launcher.go` | 8-step launch sequence with shellCleanup in success path | VERIFIED | Present, full 8-step sequence with cleanup-on-failure. **CR-01 fix confirmed:** success-path `result.Cleanup` at line 202-207 includes `shellCleanup()` call. |
| `internal/scrcpy/video_reader.go` | ReadCodecMeta, ReadFrameHeader, ReadVideoFrame | VERIFIED | Present, all use io.ReadFull. RawHeader preserved for zero-copy WS relay. |
| `internal/api/handlers_devices.go` | ListDevices, CreateSession (with race fix), DeleteSession | VERIFIED | Present, wired in router, serial validation, idempotent creation. **CR-02 fix confirmed:** re-validates state after lock re-acquisition at lines 137-144. |
| `internal/api/ws_video.go` | StreamVideo WebSocket handler | VERIFIED | Present, upgrades to WebSocket, sends codec meta first, then relayVideo loop with frame-boundary preservation. |
| `THIRD_PARTY_NOTICES` | Apache-2.0 attribution for scrcpy | VERIFIED | Present, full attribution for scrcpy v3.3.4 and all direct Go deps. |
| `deploy/adb-gateway.service` | systemd unit file per DPL-01 | VERIFIED | Present, has Type=simple, Restart=on-failure, LimitNOFILE=65536, TimeoutStopSec=30s, After=network.target adb.service. |
| `internal/scrcpy/assets/scrcpy-server-v3.3.4` | Pinned scrcpy server binary | VERIFIED | Present, 90980 bytes. |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `router.go` | `auth.go` | APIKeyAuth middleware | WIRED | `r.Use(APIKeyAuth(...))` on protected group |
| `router.go` | `cors.go` | CORS middleware | WIRED | `r.Use(CORS(origins))` applied globally |
| `main.go` | `config.go` | config.Load() | WIRED | `cfg, err := config.Load()` at line 52 |
| `main.go` | `obs/logging.go` | obs.InitLogger | WIRED | `obs.InitLogger(cfg.LogLevel)` at line 58 |
| `handlers_devices.go` | `supervisor.go` | CreateSession calls session.Start | WIRED | `sess.Start(launchCtx)` at line 115 |
| `handlers_devices.go` | `registry.go` | CreateSession re-validates state after lock re-acquire | WIRED | Lines 136-144: `entry.State != session.StateStarting` check with `sess.Close()` and `ErrADBUnavailable` |
| `ws_video.go` | `supervisor.go` | session.CodecMeta(), session.VideoConn() | WIRED | `sess.CodecMeta()` at line 114, `sess.VideoConn()` at line 120 |
| `supervisor.go` | `launcher.go` | Launcher.Launch | WIRED | `s.launcher.Launch(ctx, s.Serial)` at line 90 |
| `supervisor.go` | `launcher.go` | shellCleanup in result.Cleanup | WIRED | `result.Cleanup` includes `shellCleanup()` call at line 206 |
| `reconcile.go` | `host_services.go` | RunShellCommand, ReverseListForward | WIRED | Both called in Reconcile() |
| `main.go` | `reconcile.go` | NewReconciler + Reconcile | WIRED | Lines 94-97 |
| `main.go` | `reconnect.go` | ADBWatchdog.ProbeOnce, AwaitADBReady, ReissueReverseForwards | WIRED | `watchdog.ProbeOnce` at line 273, `reconnector.AwaitADBReady` at line 177, `reconnector.ReissueReverseForwards` at line 196 |
| `main.go` | `registry.go` | MarkAllDisconnected, ActiveSessionSpecs, WatchDevices | WIRED | `registry.ActiveSessionSpecs()` at line 170, `registry.MarkAllDisconnected()` at line 174, `registry.WatchDevices()` at lines 114 and 231 |
| `main.go` | `host_services.go` | ReinitializeGoadb, NewDeviceWatcher | WIRED | `hostServices.ReinitializeGoadb()` at line 184, `hostServices.NewDeviceWatcher()` at line 218 |
| `reverse.go` | `client.go` | sendMessage/readResponse | WIRED | Both called in ReverseForward, ReverseListForward |
| `video_reader.go` | `io.ReadFull` | Frame boundary preservation | WIRED | All read functions use `io.ReadFull` |

### Data-Flow Trace (Level 4)

| Artifact | Data Variable | Source | Produces Real Data | Status |
|----------|---------------|--------|--------------------|--------|
| `handlers_devices.go` CreateSession | `sess` | `NewDeviceSession()` + `sess.Start()` | Creates real session via Launcher | FLOWING (requires hardware for e2e) |
| `ws_video.go` relayVideo | `codecMeta` | `sess.CodecMeta()` | Read from scrcpy connection | FLOWING (requires hardware) |
| `ws_video.go` relayVideo | `msg` | `scrcpy.ReadVideoFrame(videoConn)` | Read from scrcpy video connection | FLOWING (requires hardware) |
| `handlers_healthz.go` | buildVersion/buildSHA | Set via SetBuildInfo from ldflags | Produces real version info | FLOWING |
| `registry.go` WatchDevices | event | `hostSvc.NewDeviceWatcher(ctx)` | Produces device events | FLOWING (requires hardware) |
| `main.go` ADB lifecycle loop | `sessionSpecs` | `registry.ActiveSessionSpecs()` | Returns specs from StateActive entries with sessions | FLOWING (requires hardware for e2e) |
| `main.go` ADB lifecycle loop | `adbDisconnected` channel | Watchdog probe failure or WatchDevices channel close | Signals ADB disconnect to lifecycle loop | FLOWING (requires running ADB) |

Note: Data-flows marked "requires hardware" are structurally complete but cannot be verified end-to-end without a physical Android device connected via ADB. The wiring is correct; the scrcpy protocol parsing reads from real connections.

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| Binary compiles | `go build ./cmd/gateway/` | exit 0 | PASS |
| All tests pass | `go test ./... -count=1` | All packages PASS (adb, api, obs, scrcpy, session) | PASS |
| Registry tests pass | `go test ./internal/session/... -count=1` | All PASS including MarkAllDisconnected, ActiveSessionSpecs, WatchDevices return bool | PASS |
| ADB reconnect tests pass | `go test ./internal/adb/... -count=1` | All PASS including ProbeOnce, AwaitADBReady, ReissueReverseForwards | PASS |
| API tests pass | `go test ./internal/api/... -count=1` | All PASS including auth, handlers, ws_video | PASS |

Step 7b: Server startup and end-to-end streaming require running gateway + hardware; skipped.

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|----------|
| FND-01 | 01-06 | Service starts, runs under systemd, graceful shutdown on SIGTERM | VERIFIED | `main.go` has SIGTERM handler, 30s drain. `adb-gateway.service` has Type=simple. |
| FND-02 | 01-01 | Config from file + env via koanf | VERIFIED | `config.go` uses koanf with file/env/posflag providers. |
| FND-03 | 01-01 | /healthz returns version, scrcpy version, build SHA | VERIFIED | `handlers_healthz.go` returns JSON with all fields. `--version` flag in main.go. |
| FND-04 | 01-01 | Structured JSON logs via slog | VERIFIED | `logging.go` sets `slog.JSONHandler` as default with level config. |
| FND-05 | 01-06 | THIRD_PARTY_NOTICES for scrcpy Apache-2.0 | VERIFIED | File exists with full attribution. `--licenses` flag reads it. |
| ADB-01 | 01-02 | Connect to localhost:5037 with reconnect on drop | VERIFIED | `client.go` dials configurable addr. `reconnect.go` has exponential backoff reconnect. `main.go` lifecycle loop handles full reconnect cycle. |
| ADB-02 | 01-02 | host:devices, host:track-devices, host:transport | VERIFIED | `host_services.go` implements ListDevices and NewDeviceWatcher. Client uses host:transport. |
| ADB-03 | 01-02 | In-house reverse:forward helper | VERIFIED | `reverse.go` implements full reverse:forward with localabstract:scrcpy_<SCID>. |
| ADB-04 | 01-02 | Push files via ADB sync service | VERIFIED | `host_services.go` `PushFile` wraps goadb sync API. |
| ADB-05 | 01-02 | Shell commands via shell:v2 | VERIFIED | `host_services.go` `RunShellCommand` uses goadb shell:v2. `RunDaemonCommand` for long-running processes. |
| ADB-06 | 01-06, 01-07 | After ADB reconnect, re-issue reverse forwards and audit | VERIFIED | `main.go` lifecycle loop: `ActiveSessionSpecs()` captures specs before `MarkAllDisconnected()`, then `ReissueReverseForwards()` re-establishes tunnels after `AwaitADBReady()` + `ReinitializeGoadb()`. |
| ADB-07 | 01-02 | Context timeout on every ADB call, per-device mutex | VERIFIED | All ADB methods accept ctx. Per-device mutex in DeviceEntry. |
| ADB-08 | 01-06, 01-07 | Startup reconciliation (kill orphans, remove stale forwards) | VERIFIED | `reconcile.go` kills scrcpy-server-gateway.jar processes and removes localabstract:scrcpy_* forwards. Called both at startup and after ADB reconnect. |
| DEV-01 | 01-03 | In-memory device registry fed by track-devices | VERIFIED | `registry.go` uses sync.Map, `WatchDevices` feeds from events. |
| DEV-02 | 01-05 | REST GET /devices returns device list | VERIFIED | `handlers_devices.go` `ListDevices` returns JSON with serial and state. |
| DEV-03 | 01-05 | POST /devices/{serial}/sessions (idempotent) | VERIFIED | `CreateSession` returns existing session if active (200). Race condition fix (CR-02) prevents state overwrite during ADB disconnect. |
| DEV-04 | 01-05 | DELETE /devices/{serial}/sessions/{id} | VERIFIED | `DeleteSession` closes session, cleans up (including shellCleanup via result.Cleanup), returns 204. |
| SCR-01 | 01-04 | Vendored pinned server.jar via //go:embed | VERIFIED | `embed.go` embeds `assets/scrcpy-server-v3.3.4`. |
| SCR-02 | 01-04 | Push jar, set up reverse, launch app_process | VERIFIED | `launcher.go` implements 8-step sequence. shellCleanup included in success path (CR-01 fix). |
| SCR-03 | 01-04 | Read video stream with io.ReadFull, preserve frame boundaries | VERIFIED | `video_reader.go` uses io.ReadFull throughout. `ws_video.go` sends raw header + payload. |
| STR-01 | 01-05 | WebSocket video with codec metadata first | VERIFIED | `ws_video.go` sends codec meta as first message, then frames. |
| AUTH-01 | 01-01 | All REST/WS endpoints require API key (header or query param) | VERIFIED | `auth.go` checks X-API-Key header, falls back to api_key query param. |
| AUTH-02 | 01-01 | Constant-time key comparison | VERIFIED | SHA-256 hash + `subtle.ConstantTimeCompare` in auth.go. |
| AUTH-03 | 01-01 | API keys never in logs | VERIFIED | `logging.go` redactingHandler strips api_key, password, secret, token. |
| AUTH-04 | 01-01 | Failed auth returns 401, no info leak | VERIFIED | Same `ErrUnauthorized` for missing/wrong key. Identical response body. |
| AUTH-05 | 01-01 | Key rotation without dropping sessions | PARTIAL | Secondary key exists in config. No SIGHUP/config-reload signal implemented. Rotation requires restart, but the secondary key mechanism allows deploy-new-key-as-secondary, swap, restart without dropping sessions on the surviving primary. REQUIREMENTS.md says "config reload signal or restart" -- restart is the implemented path. |
| OBS-03 | 01-01 | Logs include device serial + session ID | VERIFIED | `supervisor.go` creates per-device sublogger: `slog.With("device", serial, "session", id)`. |
| OBS-04 | 01-01 | Startup log records pinned scrcpy version, build SHA, config | VERIFIED | `main.go` lines 63-70: logs version, scrcpy_version, build_sha, listen_addr, adb_addr, log_level. |
| DPL-01 | 01-06 | systemd unit file (Type=simple, Restart=on-failure, LimitNOFILE=65536, TimeoutStopSec=30s) | VERIFIED | `adb-gateway.service` has all required fields. |

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| `internal/api/handlers_healthz.go` | 18 | Hardcoded "3.3.4" instead of `scrcpy.SCRCPYVersion` | WARNING | Version drift if scrcpy version is bumped (WR-07 from code review) |
| `internal/config/config.go` | 19 | Duplicate `SCRCPYVersion` constant instead of importing `scrcpy.SCRCPYVersion` | WARNING | Same version drift risk (WR-07) |
| `internal/api/errors.go` | 79 | `mapError` leaks internal error details via `fmt.Sprintf("An internal error occurred: %v", err)` | INFO | Not called from any handler path today; exported function that could be misused in future (WR-01) |
| `internal/scrcpy/video_reader.go` | 87 | Unbounded `hdr.Size` allocation -- no max frame size check | WARNING | OOM risk from corrupted/malicious stream (WR-04) |
| `internal/adb/host_services.go` | 274-295 | `RunDaemonCommand` ignores `ctx` parameter | INFO | Daemon launches cannot be cancelled by context (WR-05) |
| `internal/adb/host_services.go` | 78-100, 219-233 | Goroutine leak on context cancellation for `ListDevices`, `RunShellCommand` | INFO | Known goadb limitation; goroutine leaks if goadb call hangs (WR-06) |
| `internal/config/config.go` | 106-113 | Missing validation for `APIKeySecondary` and `ADBAddr` | WARNING | Empty secondary key produces known SHA-256 hash; ADBAddr format not validated (WR-03) |

No blocker-level anti-patterns found. All warnings are non-blocking maintenance or robustness concerns for future phases.

### Code Review Status

The code review (01-REVIEW.md) identified 2 critical and 7 warning issues:

| ID | Status | Detail |
|----|--------|--------|
| CR-01 | FIXED | `shellCleanup()` added to success-path `result.Cleanup` in launcher.go line 206 |
| CR-02 | FIXED | State re-validation after lock re-acquisition in handlers_devices.go lines 137-144 |
| WR-01 | OPEN | `mapError` leaks internal details; not called from handlers today |
| WR-02 | OPEN | Healthz hardcodes "3.3.4" instead of importing `scrcpy.SCRCPYVersion` |
| WR-03 | OPEN | Missing config validation for `APIKeySecondary` and `ADBAddr` |
| WR-04 | OPEN | Unbounded frame size in `ReadVideoFrame` |
| WR-05 | OPEN | `RunDaemonCommand` ignores context parameter |
| WR-06 | OPEN | Goroutine leak on context cancellation for goadb calls |
| WR-07 | OPEN | SCRCPYVersion duplicated in 3 packages |

All critical issues are resolved. Warning items are non-blocking and appropriate for future phase remediation.

### Human Verification Required

### 1. End-to-end video streaming with real device

**Test:** Start the gateway with `ADB_GW_API_KEY_PRIMARY=testkey go run ./cmd/gateway/`, connect a USB Android device, `POST /devices/{serial}/sessions`, then connect a WebSocket client to `/devices/{serial}/video` with `X-API-Key` header.
**Expected:** WebSocket client receives 12-byte codec metadata followed by H.264 frame data with preserved frame boundaries. Session cleanup (DELETE) terminates the `app_process` on the device.
**Why human:** Requires physical Android device with USB debugging enabled. Cannot be tested in an automated environment.

### 2. ADB reconnection after adbd restart

**Test:** Start gateway with an active session, then kill the adb server process (`adb kill-server`). Wait for the gateway to detect the disconnection and reconnect.
**Expected:** Gateway detects disconnect (watchdog probe or device watcher channel close), calls `MarkAllDisconnected` (removes idle entries, marks active sessions as failed), reconnects to `localhost:5037` via exponential backoff, reinitializes goadb, reconciles, re-issues reverse forwards for active sessions, restarts device watcher, restarts watchdog. Device entries recover from StateFailed to StateIdle on reconnect. Session resumes within 10 seconds.
**Why human:** Requires a running ADB server and connected device. Cannot simulate ADB protocol reconnection without real hardware.

### 3. Startup reconciliation after kill -9

**Test:** Start gateway, create a session, then `kill -9` the gateway process. Restart the gateway.
**Expected:** Reconciler kills orphan app_process instances on the device, removes stale localabstract:scrcpy_* reverse forwards, and starts cleanly. No orphan processes or stale forwards remain.
**Why human:** Requires real device to verify that orphan processes and stale forwards are actually removed.

### 4. Systemd service deployment

**Test:** Install `deploy/adb-gateway.service` on a Linux host with systemd, configure `ADB_GW_API_KEY_PRIMARY`, run `systemctl start adb-gateway`.
**Expected:** Service starts, `curl -H 'X-API-Key: ...' http://localhost:8080/healthz` returns 200. `systemctl stop adb-gateway` drains within 30 seconds.
**Why human:** Requires a Linux host with systemd. Cannot test on macOS development environment.

### Gaps Summary

**AUTH-05 (Key rotation without dropping sessions):** The implementation supports a primary and secondary API key, which enables a rotation procedure (deploy new key as secondary, swap primary/secondary, restart). However, there is no SIGHUP or config-reload signal to rotate keys without restarting the process. REQUIREMENTS.md AUTH-05 says "config reload signal or restart" -- restart is the implemented path. This is functional but not optimal. Partial verification.

**SCRCPYVersion duplication (WR-07):** The version "3.3.4" is hardcoded in three locations: `scrcpy/version.go`, `config/config.go`, and `api/handlers_healthz.go`. This creates a maintenance hazard where bumping one without the others causes drift. Not a functional gap today but a future risk. This does not block phase completion.

All 4 human verification items PASSED on 2026-05-07 (real device + Linux systemd host). All 29/29 truths are VERIFIED. Remaining warnings (AUTH-05 partial, SCRCPYVersion duplication) are non-blocking maintenance items for future phases.

---

_Verified: 2026-05-07_
_Verifier: Claude (gsd-verifier)_
_Re-verification: after 01-07 gap closure and CR-01/CR-02 bug fixes_