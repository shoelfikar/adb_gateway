---
gsd_state_version: 1.0
milestone: v1.0
milestone_name: milestone
status: executing
last_updated: "2026-05-07T04:34:11Z"
progress:
  total_phases: 4
  completed_phases: 0
  total_plans: 6
  completed_plans: 0
---

# Project State: ADB Gateway

**Initialized:** 2026-05-06
**Mode:** YOLO, sequential, standard granularity
**Last updated:** 2026-05-07 (Phase 1 executing, Plans 01-04 complete)

## Project Reference

**Core value:** Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

**Current focus:** Phase 1 — Single-Device Streaming Foundation. 6 plans across 5 waves. Research complete, plans verified, ready to execute.

## Current Position

| Field | Value |
|-------|-------|
| Phase | 1 — Single-Device Streaming Foundation |
| Plan | 6 plans (01-01 through 01-06) |
| Status | executing |
| Phase progress | 4/6 plans complete |
| Overall progress | 0/4 phases complete |

```
[████░░░░░░] 67%   Phase 1 (executing)
[░░░░░░░░░░] 0%   Phase 2
[░░░░░░░░░░] 0%   Phase 3
[░░░░░░░░░░] 0%   Phase 4
```

## Performance Metrics

| Metric | Value |
|--------|-------|
| Phases completed | 0 |
| Plans completed | 4 (01-01, 01-02, 01-03, 01-04) |
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

### Key Research Findings (Phase 1)

- **Critical correction:** scrcpy uses `localabstract:scrcpy_<SCID>` for device-side reverse tunnels, NOT `tcp:27183`. Semicolon separator in `reverse:forward` command.
- **Reverse-forward helper is ~200-260 LOC** (refined from ~150 estimate). Connection preservation model (keep :5037 connection open for mapping to persist).
- **Go 1.24+ required** — current system has 1.22.4, must upgrade before execution.
- **All 29 Phase 1 requirements covered** across 6 plans in 5 waves.
- **All 11 CONTEXT.md decisions honored** in plans.

### Open Questions for Execution

- **Phase 1:** Test prife/goadb shell:v2 against real Android 14+ device during execution
- **Phase 1:** Download scrcpy v3.3.4 server.jar and verify SHA-256 (first Plan 04 task)
- **Phase 2:** "Force keyframe" strategy — accept "wait for natural keyframe" or invest in server.jar tweak
- **Phase 2:** Validate late-joiner cache against a real WebCodecs decoder
- **Phase 4:** Verify LB supports URL-path or query-param hashing; fallback to in-process WS proxy

### Todos / Followups

- (none yet — populated during plan execution)

### Blockers

- (none)

## Session Continuity

**Last action:** Plan 01-04 executed — scrcpy v3.3.4 server.jar embedded, 8-step launcher with cleanup-on-failure, video frame reader with io.ReadFull discipline. 4/6 plans complete.

**Next action:** Continue executing Phase 1 — Plans 01-05 and 01-06 remaining.

**Files of record:**

- `.planning/PROJECT.md` — vision, constraints, key decisions
- `.planning/REQUIREMENTS.md` — 68 v1 requirements with traceability table
- `.planning/ROADMAP.md` — 4 phases, success criteria, dependencies
- `.planning/phases/01-single-device-streaming-foundation/01-CONTEXT.md` — Phase 1 implementation decisions
- `.planning/phases/01-single-device-streaming-foundation/01-RESEARCH.md` — Phase 1 technical research (HIGH confidence)
- `.planning/phases/01-single-device-streaming-foundation/01-PATTERNS.md` — Phase 1 pattern mapping
- `.planning/phases/01-single-device-streaming-foundation/01-VALIDATION.md` — Phase 1 validation strategy
- `.planning/phases/01-single-device-streaming-foundation/01-01-PLAN.md` through `01-06-PLAN.md` — 6 execution plans
- `.planning/research/SUMMARY.md` — research synthesis (HIGH confidence)
- `.planning/research/{STACK,FEATURES,ARCHITECTURE,PITFALLS}.md` — domain research artifacts
- `android-monitoring-architecture.md` — original architecture sketch (still consistent with researched plan)

---
*State updated: 2026-05-07 by plan 01-03 execution*