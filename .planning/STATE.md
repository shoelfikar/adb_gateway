---
gsd_state_version: 1.0
milestone: v1.0
milestone_name: milestone
status: context_gathered
last_updated: "2026-05-06T11:00:00.000Z"
progress:
  total_phases: 4
  completed_phases: 0
  total_plans: 0
  completed_plans: 0
---

# Project State: ADB Gateway

**Initialized:** 2026-05-06
**Mode:** YOLO, sequential, standard granularity
**Last updated:** 2026-05-06 (roadmap created)

## Project Reference

**Core value:** Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

**Current focus:** Phase 1 — Single-Device Streaming Foundation. Establish the ADB foundation (`internal/adb/` + in-house `reverse:forward` helper), pin scrcpy `server.jar`, and prove end-to-end video relay for one device, one viewer.

## Current Position

| Field | Value |
|-------|-------|
| Phase | 1 — Single-Device Streaming Foundation |
| Plan | (none — phase not yet planned) |
| Status | not_started |
| Phase progress | 0/0 plans complete |
| Overall progress | 0/4 phases complete |

```
[░░░░░░░░░░] 0%   Phase 1
[░░░░░░░░░░] 0%   Phase 2
[░░░░░░░░░░] 0%   Phase 3
[░░░░░░░░░░] 0%   Phase 4
```

## Performance Metrics

| Metric | Value |
|--------|-------|
| Phases completed | 0 |
| Plans completed | 0 |
| Requirements shipped | 0 / 68 |
| Validated requirements | 0 |
| Decisions logged | 8 (in PROJECT.md Key Decisions, all `— Pending`) |

## Accumulated Context

### Active Decisions Carried Into Planning

1. **Vendor + pin one scrcpy `server.jar` version**, embedded via `//go:embed`. Treat as a vendored protocol, not a library — no auto-update.
2. **In-house `reverse:forward` helper (~150 LOC)** against AOSP `SERVICES.TXT` — no Go ADB library implements it, and scrcpy cannot start without it.
3. **`prife/goadb` v0.4.4** as the ADB base; **`coder/websocket` v1.8.14** for WS; **`go-chi/chi/v5`** for routing; **`koanf/v2`** for config; **`log/slog`** for logging; **`go-redis/v9`** at Phase 4 only; **Prometheus** for metrics.
4. **Per-device mutex, never global**; every ADB call bounded by context with timeout; per-device `errgroup`-supervised goroutine tree.
5. **Drop-on-slow fan-out** with bounded per-client buffers (4–8 frames); cached config + keyframe for late joiners.
6. **API-key auth from day one** with constant-time compare and primary/secondary key rotation; bind to `127.0.0.1` or private interface.
7. **Apache-2.0 attribution** for embedded `server.jar`: ship `THIRD_PARTY_NOTICES`, expose via `--licenses` / endpoint, record pinned version + commit SHA in `--version`.
8. **Coordination is opt-in** — single-instance deployments compile and run without Redis; `internal/coord/` only wired in Phase 4.

### Open Questions for Plan-Phase

- **Phase 1:** Exact LOC for `reverse:forward` helper (refine via spike). Validate `prife/goadb` shell-v2 against real Android 14/15. Confirm pinned scrcpy `server.jar` version (most likely v3.x latest stable at planning time) and capture fixture bytes for codec-reader unit tests.
- **Phase 2:** "Force keyframe" strategy — accept "wait for natural keyframe" or invest in server.jar tweak. Validate late-joiner cache against a real WebCodecs decoder. Confirm proxy stack `pelni_server` will use (NGINX/HAProxy/Cloudflare).
- **Phase 4:** Verify LB supports URL-path or query-param hashing (HAProxy `balance hdr`/`url_param`, NGINX `hash`); fallback plan is in-process WS proxy.

### Todos / Followups

- (none yet — populated during plan execution)

### Blockers

- (none)

## Session Continuity

**Last action:** Phase 1 context gathered (4 decisions: scrcpy v3.3.4, strict sequential startup, domain error codes, marker-based reconciliation).

**Next action:** `/gsd-plan-phase 1` — orchestrator should trigger `/gsd-research-phase` first per Phase 1's `Research flag: yes` (spike `reverse:forward` helper, validate `prife/goadb` shell-v2 on real device, confirm v3.3.4 fixture bytes for codec readers).

**Files of record:**

- `.planning/PROJECT.md` — vision, constraints, key decisions
- `.planning/REQUIREMENTS.md` — 68 v1 requirements with traceability table
- `.planning/ROADMAP.md` — 4 phases, success criteria, dependencies
- `.planning/phases/01-single-device-streaming-foundation/01-CONTEXT.md` — Phase 1 implementation decisions
- `.planning/research/SUMMARY.md` — research synthesis (HIGH confidence)
- `.planning/research/{STACK,FEATURES,ARCHITECTURE,PITFALLS}.md` — domain research artifacts
- `android-monitoring-architecture.md` — original architecture sketch (still consistent with researched plan)

---
*State initialized: 2026-05-06 by `/gsd-roadmap`*
