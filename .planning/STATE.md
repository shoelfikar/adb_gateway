---
gsd_state_version: 1.0
milestone: v1.0
milestone_name: milestone
status: executing
last_updated: "2026-05-08T03:19:40Z"
progress:
  total_phases: 4
  completed_phases: 0
  total_plans: 14
  completed_plans: 10
---

# Project State: ADB Gateway

**Initialized:** 2026-05-06
**Mode:** YOLO, sequential, standard granularity
**Last updated:** 2026-05-08 (Phase 2 executing, Wave 1 + 02-03 complete)

## Project Reference

**Core value:** Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

**Current focus:** Phase 2 — Multi-Client + Control. 6 plans across 4 waves. Plans 02-01 and 02-02 complete, Wave 1 done.

## Current Position

| Field | Value |
|-------|-------|
| Phase | 2 — Multi-Client + Control |
| Plan | 6 plans (02-01 through 02-06) |
| Status | executing |
| Phase progress | 3/6 plans complete (02-01, 02-02, 02-03 done) |
| Overall progress | 0/4 phases complete |

```
[██████████] 100%  Phase 1 (7/7 plans complete)
[█████░░░░░] 50%  Phase 2 (3/6 plans complete)
[░░░░░░░░░░] 0%   Phase 3
[░░░░░░░░░░] 0%   Phase 4
```

## Performance Metrics

| Metric | Value |
|--------|-------|
| Phases completed | 0 |
| Plans completed | 9 (01-01..01-07, 02-01..02-03) |
| Requirements shipped | 0 / 68 |
| Validated requirements | 0 |
| Decisions logged | 8 (in PROJECT.md Key Decisions, all `— Pending`) |

## Accumulated Context

### Active Decisions Carried Into Planning

1. **Vendor + pin one scrcpy `server.jar` version**, embedded via `//go:embed`. Treat as a vendored protocol, not a library — no auto-update.
2. **In-house `reverse:forward` helper (~200-260 LOC)** against AOSP `SERVICES.TXT` — no Go ADB library implements it, and scrcpy cannot start without it.
3. **`prife/goadb` v0.4.8** as the ADB base; **`coder/websocket` v1.8.14** for WS; **`go-chi/chi/v5`** for routing; **`koanf/v2`** for config; **`log/slog`** for logging; **`go-redis/v9`** at Phase 4 only; **Prometheus** for metrics.
4. **Per-device mutex, never global**; every ADB call bounded by context with timeout; per-device `errgroup`-supervised goroutine tree.
5. **Drop-on-slow fan-out** with bounded per-client buffers (4–8 frames); cached config + keyframe for late joiners.
6. **API-key auth from day one** with constant-time compare and primary/secondary key rotation; bind to `127.0.0.1` or private interface.
7. **Apache-2.0 attribution** for embedded `server.jar`: ship `THIRD_PARTY_NOTICES`, expose via `--licenses` / endpoint, record pinned version + commit SHA in `--version`.
8. **Coordination is opt-in** — single-instance deployments compile and run without Redis; `internal/coord/` only wired in Phase 4.
9. **DeviceSession placeholder type** in `internal/session/registry.go` — real implementation with errgroup and video relay deferred to Plan 05.
10. **TransitionTo is a pure function** — caller assigns result under per-device mutex; no side effects inside the FSM.
11. **WatchDevices treats "device", "recovery", "offline" as connect states** — offline devices are still tracked so session manager can attempt connection.
12. **Used crypto/rand for SCID generation** instead of math/rand — stronger randomness for session IDs that identify reverse tunnels.
13. **Launcher treats RunShellCommand errors as non-fatal** — scrcpy server process starts in background; actual failure caught by Accept timeout.
14. **LaunchResult.CodecMeta stores raw 12 bytes** read from connection, reconstructed for logging — avoids double-read issue with io.Reader.
15. **scrcpy v3.3.4 server.jar SHA-256: 8588238c9a5a00aa542906b6ec7e6d5541d9ffb9b5d0f6e1bc0e365e2303079e** — pinned and verified.
16. **session.Launcher interface** for testability — avoids circular import (session -> api), enables mock injection. *scrcpy.Launcher satisfies the interface.
17. **IsSessionActive reads fields directly when caller holds lock** — prevents deadlock between handler (entry.Lock) and getter methods (entry.mu.Lock).
18. **WebSocket compression disabled** — raw H.264 does not compress well, adds CPU overhead per STR-01.
19. **Frame boundary preservation** — each WS message is 12-byte raw header + payload, preserving frame boundaries for browser WebCodecs decoder.
20. **Error mapping via string matching** — launch errors mapped to domain codes using strings.Contains on error message prefixes. Simple, sufficient for Phase 1.
21. **Runtime file reading for --licenses** instead of //go:embed — Go embed cannot reference files in parent directories; THIRD_PARTY_NOTICES is in project root while main.go is in cmd/gateway/.
22. **Best-effort startup reconciliation** — errors logged but gateway continues starting; partial cleanup is better than refusing to start.
23. **cenkalti/backoff/v4 for ADB reconnection** — 100ms initial, 5s max, indefinite retry (context cancel is the only exit).
24. **WatchDevices returns bool** — true signals ADB disconnect (channel close), false signals graceful shutdown (context cancel). Caller distinguishes between the two to decide whether to reconnect.
25. **MarkAllDisconnected removes idle entries** — StateIdle->StateFailed is not a valid FSM transition, so idle entries are deleted rather than transitioned. Active/starting/stopping entries transition to StateFailed and are kept for reverse forward re-issuance.
26. **ActiveSessionSpecs captures specs from existing ReverseMapping** — avoids net.Addr format inconsistencies that arise from reconstructing specs from VideoLn.Addr().
27. **Watchdog is a probe-only type** — ADBWatchdog.ProbeOnce is stateless; the caller (main.go lifecycle goroutine) manages reconnection orchestration and restarts the watchdog goroutine after reconnect.
28. **Hub unsubscribe is map-removal only** — unsubscribe does NOT close viewer.send channel; only evict() and shutdown() (both Hub-goroutine-only) close channels. This eliminates the data race between concurrent close/send that the original plan's recover-based approach would cause.
29. **Hub fan-out uses single-goroutine pattern** — the Hub goroutine owns the viewers map for writes and iterates a snapshot under RLock for sends. No per-viewer goroutines; each viewer gets a buffered chan []byte read by the WS handler goroutine.
30. **Drop counter resets on every successful send** — consecutive drops (not cumulative) trigger eviction at threshold 120. A viewer that catches up after 119 drops gets a clean slate.
31. **Touch event is 32 bytes in scrcpy v3.3.4** — verified in byte-exact unit tests; 1+1+8+4+4+2+2+2+4+4 layout, NOT 36 as older sources cite. Flag if UAT against a real device rejects the byte stream.
32. **ControlWriter.Run logs marshal errors but does NOT abort** — bad messages are dropped with a warning; only conn.Write errors terminate the writer (T-02-03-04).
33. **ControlWriter does NOT own net.Conn lifecycle** — the supervisor (plan 02-05) creates and closes the conn; ControlWriter just writes to it.

### Key Research Findings (Phase 1)

- **Critical correction:** scrcpy uses `localabstract:scrcpy_<SCID>` for device-side reverse tunnels, NOT `tcp:27183`. Semicolon separator in `reverse:forward` command.
- **Reverse-forward helper is ~200-260 LOC** (refined from ~150 estimate). Connection preservation model (keep :5037 connection open for mapping to persist).
- **Go 1.24+ required** — current system has 1.22.4, must upgrade before execution.
- **All 29 Phase 1 requirements covered** across 6 plans in 5 waves.
- **All 11 CONTEXT.md decisions honored** in plans.

### Open Questions for Execution

- **Phase 2:** "Force keyframe" strategy — accept "wait for natural keyframe" or invest in server.jar tweak
- **Phase 2:** Validate late-joiner cache against a real WebCodecs decoder
- **Phase 4:** Verify LB supports URL-path or query-param hashing; fallback to in-process WS proxy

### Todos / Followups

- (none yet — populated during plan execution)

### Blockers

- (none)

## Session Continuity

**Last action:** Plan 02-03 executed — scrcpy v3.3.4 control message marshal table (18 types, byte-exact) and single-writer ControlWriter goroutine (12 tests passing with race detector).

**Next action:** Plan 02-04 (audio reader + device message reader).

**Files of record:**

- `.planning/PROJECT.md` — vision, constraints, key decisions
- `.planning/REQUIREMENTS.md` — 68 v1 requirements with traceability table
- `.planning/ROADMAP.md` — 4 phases, success criteria, dependencies
- `.planning/phases/02-multi-client-control/02-CONTEXT.md` — Phase 2 implementation decisions
- `.planning/phases/02-multi-client-control/02-RESEARCH.md` — Phase 2 technical research (HIGH confidence)
- `.planning/phases/02-multi-client-control/02-PATTERNS.md` — Phase 2 pattern mapping
- `.planning/phases/02-multi-client-control/02-01-SUMMARY.md` — Phase 2 foundation (config, errors, metrics)
- `.planning/phases/02-multi-client-control/02-02-SUMMARY.md` — Hub fan-out with late-joiner cache
- `.planning/phases/02-multi-client-control/02-03-SUMMARY.md` — scrcpy control writer marshal table

---
*State updated: 2026-05-08 by plan 02-02 execution*