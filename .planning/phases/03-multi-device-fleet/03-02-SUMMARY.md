---
phase: 03-multi-device-fleet
plan: 02
subsystem: session
tags: [session, fsm, watchdog, recovery, observability, api, tdd]
type: execute
status: complete
completed: 2026-05-09
requirements: [DEV-05, OPS-02]
dependency_graph:
  requires:
    - 03-01 (LaunchOptions, AppProcessPID, scrcpy koanf keys, DEV-06 serial stability)
  provides:
    - "internal/session.StateReconnecting + Active->Reconnecting and Reconnecting->{Active,Failed,Stopping} transitions"
    - "internal/session.Hub.FrameCount() lock-free atomic counter"
    - "internal/session.StallWatchdog (configurable interval/threshold + first-frame gate)"
    - "internal/session.Recovery (cenkalti/backoff/v4, MaxAttempts cap, lock-discipline-safe)"
    - "internal/session.DeviceSession.AttachStallRecovery / LaunchOptions / transitionLocked"
    - "internal/obs.SessionState GaugeVec (gateway_session_state{device_serial,state}) + SetSessionState helper"
    - "internal/api.RestartSession HTTP handler + LauncherFactory type"
  affects:
    - 03-03 (must register POST /devices/{serial}/restart route in router.go)
    - 03-03 (must wire LauncherFactory at NewRouter call site so RestartSession can construct fresh launchers)
    - 03-04 (recording/APK plans should not remove the optional watchdog wiring on DeviceSession; AttachStallRecovery is opt-in and must remain reachable)
    - 03-05 (per-device /health/devices reads SessionState gauge labels; soak test will exercise the watchdog -> recovery -> Active path)
tech-stack:
  added:
    - "github.com/cenkalti/backoff/v4 (already in go.mod from Phase 1; first runtime use in session pkg)"
  patterns:
    - "Lock-free counter via atomic.Uint64 read on hot path, polled by separate goroutine (Hub.FrameCount)"
    - "Per-device errgroup with optional g.Go(watchdog.Run) — opt-in via AttachStallRecovery"
    - "onStall trampoline: closes over a long-lived recoveryCtx so DELETE /sessions does not kill in-flight recovery"
    - "Lock-discipline-safe recovery: transition under s.mu -> release for I/O -> re-acquire to commit terminal state (Pitfall 9)"
    - "One-hot Prometheus gauge: zero all labels, set current=1 — preserves `sum by (state) ==` invariants for PromQL"
    - "First-frame gate (Pitfall 2): watchdog defers miss-counting until counter advances past 0"
key-files:
  created:
    - internal/session/watchdog.go
    - internal/session/watchdog_test.go
    - internal/session/recovery.go
    - internal/session/recovery_test.go
  modified:
    - internal/session/fsm.go (added StateReconnecting + AllStates() + transitions)
    - internal/session/fsm_test.go (TestFSMReconnecting + negative cases)
    - internal/session/hub.go (frameCount atomic.Uint64 + FrameCount accessor)
    - internal/session/hub_test.go (TestHubFrameCount sequential + concurrent)
    - internal/session/supervisor.go (AttachStallRecovery, LaunchOptions, transitionLocked, watchdog g.Go in Run)
    - internal/session/supervisor_test.go (lifecycle tests still green; watchdog opt-in keeps default sessions unaffected)
    - internal/api/handlers_devices.go (RestartSession handler + LauncherFactory type)
    - internal/obs/metrics.go (SessionState GaugeVec + SetSessionState one-hot helper + registration)
    - internal/obs/metrics_test.go (TestSessionStateMetric)
decisions:
  - "Stall threshold = 5 misses × 5s interval = 25s detection window (5s margin under 30s ROADMAP assertion, Pitfall 9)"
  - "Recovery max attempts = 3 then sticky StateFailed (per CONTEXT.md Claude's Discretion)"
  - "Recovery uses a long-lived recoveryCtx (not the per-Run errgroup ctx) so a benign DELETE during recovery does not kill the relaunch — recovery instead observes ctx and transitions Reconnecting->Stopping/Failed itself"
  - "Watchdog is opt-in via DeviceSession.AttachStallRecovery — sessions without it skip the g.Go entirely; this keeps the wave-1 supervisor tests untouched"
  - "POST /devices/{serial}/restart handler is exported but NOT registered in router.go (wave-2 conflict avoidance — 03-03 owns router.go this wave)"
metrics:
  duration: "implementation pre-committed; finalization 5 min"
  completed: 2026-05-09
---

# Phase 3 Plan 02: FSM Watchdog & Recovery Summary

Per-device frame-stall watchdog + auto-recovery orchestrator with backoff-capped relaunch, sticky `failed` terminal state, and a manual `RestartSession` handler — all surfaced via a `gateway_session_state{device_serial,state}` one-hot Prometheus gauge.

## What Shipped

### FSM extension (`internal/session/fsm.go`)

- New state: `StateReconnecting` appended to the iota block (preserves prior values).
- New transitions: `Active -> Reconnecting`, `Reconnecting -> {Active, Failed, Stopping}`.
- `AllStates()` helper added so `obs.SetSessionState` can iterate every defined state for the one-hot zero pass without hard-coding the list in two places.

### Hub frame counter (`internal/session/hub.go`)

- `frameCount atomic.Uint64` field on `Hub`; `Hub.Publish` calls `frameCount.Add(1)` BEFORE the fan-out branch (lock-free path so the counter still advances when subscribers are saturated).
- `Hub.FrameCount() uint64` accessor for the watchdog's polling goroutine.

### Stall watchdog (`internal/session/watchdog.go`)

- Generic `StallWatchdog` with a counter func, interval, threshold, `OnStall` callback.
- **Defaults:** `Interval=5s`, `Threshold=5` → 25s detection window (5s margin under the 30s ROADMAP assertion, per Pitfall 9).
- **First-frame gate (Pitfall 2):** an internal `started` flag stays false while the counter == 0, so a slow scrcpy startup never flaps the device into `reconnecting` before the first frame arrives.
- **Single-fire semantics:** once `OnStall` fires, the internal `fired` flag suppresses re-fires until the counter advances again. Recovery owns re-arming.
- **Test seam:** unexported `tickCh` field on `StallWatchdogOpts` lets `watchdog_test.go` drive the loop with a synthetic ticker channel; production callers leave it nil and get a real `time.NewTicker(interval)`.

### Recovery orchestrator (`internal/session/recovery.go`)

- `Recovery.Run(ctx, *DeviceSession)`:
  1. Under `s.mu`: transition `Active -> Reconnecting`, snapshot `serial` + `launchOpts`, release the lock.
  2. Run `backoff.RetryNotify` with `cenkalti/backoff/v4` (`InitialInterval=1s`, `MaxInterval=30s`, `MaxElapsedTime=0`), wrapped in `WithMaxRetries(bo, MaxAttempts-1)` and `WithContext(bo, ctx)`. Each attempt calls `launcher.LaunchWithOptions(ctx, serial, opts)`. Per-attempt failures emit a `slog.Warn` via the `RetryNotify` callback (`error`, `next_in`).
  3. Re-acquire `s.mu` and commit:
     - **Success:** `applyLaunchResultLocked(result)` swaps in the new conns + `transitionLocked(StateActive)`. The gauge flips via `transitionLocked` -> `obs.SetSessionState`.
     - **Concurrent stop:** if `s.state == StateStopping` while we were running, leave it — DELETE wins (see "DELETE vs Recovery" below).
     - **Failure:** `transitionLocked(StateFailed)` — sticky. Returns the wrapped error; only manual `POST /restart` reverses.

### Manual restart handler (`internal/api/handlers_devices.go`)

- `RestartSession(registry, cfg, factory) http.HandlerFunc` — pre-condition: `entry.State == StateFailed`. Mirrors `CreateSession`'s lock-state-flip-unlock-launch-relock-commit pattern. New `LauncherFactory` type lets tests inject stub launchers.
- **Not registered in `router.go`** — wave-2 conflict avoidance (see handoff below).

### Observability (`internal/obs/metrics.go`)

- `SessionState = prometheus.NewGaugeVec(GaugeOpts{Name: "gateway_session_state", ...}, []string{"device_serial", "state"})`.
- `SetSessionState(serial, current string)`: zeros every state series for `serial`, sets the current state to 1 (one-hot). Cardinality bounded at 30 devices × 6 states = 180 series.
- Registered in `metrics.go`'s init block alongside the other Phase 2 metrics.
- `DeviceSession.transitionLocked(target)` is the single funnel that now stamps the gauge after every successful FSM transition.

### Supervisor wiring (`internal/session/supervisor.go`)

- `DeviceSession.AttachStallRecovery(rec, w, recoveryCtx)`: opt-in installer. Re-wraps the watchdog's `OnStall` to spawn `go rec.Run(recoveryCtx, s)` so the recovery goroutine survives a `Run`-ctx cancel.
- `DeviceSession.Run` adds an extra `g.Go(s.watchdog.Run)` only when `s.watchdog != nil`, keeping every non-Phase-3 test untouched.
- `DeviceSession.LaunchOptions()` exposes the captured `scrcpy.LaunchOptions` (set by `Start`) so `Recovery` re-launches with the SAME SCID + audio/control settings.

## Configuration: Stall Threshold & Interval

| Parameter | Default | Rationale | Override path |
|---|---|---|---|
| `StallWatchdogOpts.Interval` | 5 s | Polling cadence; cheap atomic load. | Passed in by caller (config wiring at `AttachStallRecovery` call-site). |
| `StallWatchdogOpts.Threshold` | 5 misses | 5 × 5 s = 25 s detection — 5 s margin under the ROADMAP 30 s assertion (Pitfall 9). | Same as above. |
| `RecoveryOpts.MaxAttempts` | 3 | CONTEXT.md "Claude's Discretion" — sticky failed beyond 3. | `RecoveryOpts{MaxAttempts: N}`. |
| `RecoveryOpts.Backoff` | exponential 1 s → 30 s, no elapsed cap | Phase 1 `internal/adb/reconnect.go` precedent. | `RecoveryOpts{Backoff: ...}`. |

**koanf wiring is intentionally deferred.** The plan reserved keys `health.stall_check_interval_seconds` and `health.stall_threshold_misses` for future use, but the call-site that constructs `StallWatchdogOpts` lives in 03-03's wave (where `AttachStallRecovery` will be wired into `CreateSession`'s post-`Start` path). The defaults bake into `NewStallWatchdog` until then; nothing in 03-03/03-04/03-05 needs to change the constants to ship.

**Action for 03-03:** when wiring `AttachStallRecovery`, plumb `cfg.Health.StallCheckIntervalSeconds` and `cfg.Health.StallThresholdMisses` (add these to `internal/config` if absent) into `StallWatchdogOpts.Interval` / `Threshold`.

## DELETE /sessions vs Recovery — Which Context Wins

Two distinct contexts coexist on the same `DeviceSession` once recovery is wired:

1. **`runCtx`** — the `errgroup.WithContext(ctx)` ctx inside `DeviceSession.Run`. Cancelled by:
   - The HTTP handler running `g.Wait()` returning,
   - `DELETE /devices/{serial}/sessions/{id}` calling `Close(ctx)`.
2. **`recoveryCtx`** — the long-lived ctx passed to `AttachStallRecovery` (typically `context.Background` derived, owned by the Registry).

`onStall` spawns `go rec.Run(recoveryCtx, s)`. This means:

| Scenario | Outcome |
|---|---|
| Watchdog fires; recovery succeeds before any DELETE. | `Reconnecting -> Active`. `applyLaunchResultLocked` swaps new conns; reader loops in the existing errgroup observe `EOF` on the OLD conns and the errgroup unwinds. (Reader-loop re-attachment to the new conns is owned by 03-04 — until then the supervisor exits cleanly and the Registry restarts the run loop.) |
| Watchdog fires; DELETE arrives mid-recovery. | `runCtx` cancels → `cleanupResources` runs → reader/writer goroutines exit. The recovery goroutine continues on `recoveryCtx`, completes its launch, then re-acquires `s.mu` and finds `s.state == StateStopping`. Recovery **does not override** Stopping (early-return guard at `recovery.go:147-150`); it logs `"recovery: aborted by concurrent stop"` and returns the launch error. The newly-launched scrcpy server's resources will be reaped by `Close`'s subsequent `cleanupResources` call (the cleanup func from `applyLaunchResultLocked` is captured before Close runs — but since recovery exited before applying, the orphan must be killed by the next-launch's `pkill -f scrcpy-server-gateway.jar` defensive step, which 03-03/04 must keep). |
| DELETE arrives BEFORE watchdog fires. | Normal Phase 1/2 path. State `Active -> Stopping -> Idle`; watchdog goroutine exits via `runCtx.Done()`. |
| Recovery exhausts (3 fails), then DELETE arrives. | Sticky `Failed`. DELETE returns `409 Conflict` (Failed has no transition to Stopping per FSM). Caller must use `POST /restart` to clear. |

**Net rule:** `Stopping` is terminal-priority over `Reconnecting`. The recovery loop voluntarily yields when it sees Stopping on commit. DELETE always wins over a still-running recovery.

## Handoff to 03-03 (Manual Restart Route Registration)

Plan 03-03 owns the next edits to `internal/api/router.go` (logcat / screenshot / files routes). It MUST add the following inside the `/devices/{serial}` `r.Route` block, alongside its other `r.Post` calls:

```go
r.Post("/restart", api.RestartSession(registry, cfg, launcherFactory))
```

**Symbol exported by this plan:** `api.RestartSession(registry *session.Registry, cfg *config.Config, factory api.LauncherFactory) http.HandlerFunc`.

**Plumbing 03-03 needs to add at the `NewRouter` call-site:**

- New parameter or local: `launcherFactory api.LauncherFactory` — production wiring binds this to `func() session.Launcher { return scrcpy.NewLauncher(adbClient, hostServices) }`. (The factory pattern is required because `RestartSession` constructs a fresh `DeviceSession` per call and must hand it a launcher.)
- The route-group reference inside `r.Route("/devices", func(r chi.Router) { r.Route("/{serial}", func(r chi.Router) { ... }) })` is where the `r.Post("/restart", ...)` line goes — the same block that currently holds `r.Post("/sessions", ...)`, `r.Post("/reservation", ...)`, etc. (see `internal/api/router.go:39-50`).

A TODO comment in `internal/api/handlers_devices.go` (line ~187-191) already documents this contract verbatim for the 03-03 implementer to grep.

## Test Stubs to Preserve (03-04 / 03-05 do NOT remove)

- **No stub watchdog was injected into `supervisor_test.go`.** The watchdog is opt-in via `AttachStallRecovery`; existing supervisor lifecycle tests construct `DeviceSession` without calling it, so `s.watchdog == nil` and `Run` skips the `g.Go(watchdog.Run)`. This was deliberate to avoid touching wave-1/wave-2 supervisor tests. **Do not** add a default watchdog to `NewDeviceSession` — it would force every test to provide a counter source.
- `recovery_test.go` uses a **stub `Launcher` with a configurable failure count** (failN-then-succeed). This is the canonical pattern for any 03-04 / 03-05 test that needs to exercise recovery; reuse the helper rather than re-implementing.
- `watchdog_test.go` uses a **synthetic tick channel** via the unexported `tickCh` field. The constructor's nil-check on `tickCh` is the test seam — preserve `StallWatchdogOpts.tickCh` even though it's unused in production.
- `obs/metrics_test.go` uses `prometheus.NewRegistry()` + `Gather()` to inspect the one-hot invariant. If 03-05 adds new states or relabels the gauge, update `allSessionStateNames` in `internal/obs/metrics.go` AND extend `AllStates()` in `internal/session/fsm.go` — these two lists must stay in sync.

## Verification

```
$ go test -race ./internal/session/... ./internal/api/... ./internal/obs/...
ok  github.com/pelni/adb-gateway/internal/session
ok  github.com/pelni/adb-gateway/internal/api
ok  github.com/pelni/adb-gateway/internal/obs
```

All TDD gates green:

- `TestFSMReconnecting` — Active↔Reconnecting + negative cases.
- `TestHubFrameCount` — sequential 100 + concurrent 10×100 = 1000.
- `TestSessionStateMetric` — one-hot invariant.
- `TestStallWatchdog` — first-frame gate, miss-then-fire, single-fire-per-stall.
- `TestAutoRecovery` — fail-then-succeed (Active), exhaust (Failed), ctx-cancel (Failed/cancelled).
- `TestRestartSessionFromFailed` — manual recovery path Failed → Idle → Starting → Active with new session_id.

## Commits

| Stage | Commit | Message |
|---|---|---|
| RED  | `2838813` | `test(03-02): add failing tests for FSM reconnecting state, Hub frame counter, gateway_session_state metric` |
| GREEN | `97f38dd` | `feat(03-02): add StateReconnecting, Hub frame counter, gateway_session_state metric` |
| RED  | `da4ef7a` | `test(03-02): add failing tests for stall watchdog, recovery orchestrator, and restart endpoint` |
| GREEN | `7c659b3` | `feat(03-02): stall watchdog, recovery orchestrator, RestartSession handler, supervisor wiring` |

## Deviations from Plan

None — both tasks executed as written. The deferred koanf wiring (`health.stall_check_interval_seconds`, `health.stall_threshold_misses`) was already documented in the plan as a 03-03 follow-up, not a deviation.

## Self-Check: PASSED

- `internal/session/fsm.go` — `StateReconnecting` present (line 22), transitions present (lines 63-64). FOUND.
- `internal/session/hub.go` — `frameCount atomic.Uint64` present (line 67), `FrameCount()` present (line 145). FOUND.
- `internal/session/watchdog.go` — defaults Interval=5s/Threshold=5 (lines 71-77), first-frame gate via `started` flag (lines 128-138). FOUND.
- `internal/session/recovery.go` — `backoff.WithMaxRetries(bo, r.maxAttempts-1)` (line 114), Stopping-respect guard (lines 147-150). FOUND.
- `internal/session/supervisor.go` — `g.Go(watchdog.Run)` (line 289), `AttachStallRecovery` (line 460), `transitionLocked` calls `obs.SetSessionState` (line 507). FOUND.
- `internal/api/handlers_devices.go` — `RestartSession` (line 194), `LauncherFactory` (line 173), 03-03 TODO comment (lines 187-193). FOUND.
- `internal/obs/metrics.go` — `gateway_session_state` GaugeVec (line 53), `SetSessionState` helper (line 80). FOUND.
- Commits `2838813`, `97f38dd`, `da4ef7a`, `7c659b3` all present in `git log --oneline`. FOUND.
- `go test -race ./internal/session/... ./internal/api/... ./internal/obs/...` — all three packages green.
