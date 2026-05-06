# Phase 1: Single-Device Streaming Foundation - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-06
**Phase:** 1-Single-Device Streaming Foundation
**Areas discussed:** scrcpy version pin, session startup sequence, error surface design, reconciliation strategy

---

## scrcpy Version Pin

| Option | Description | Selected |
|--------|-------------|----------|
| v3.3.4 | Latest stable, all Android 14/15/16 fixes, widest device compat. Largest protocol surface but Phase 1 only needs video codec readers. | ✓ |
| v3.1 | First stabilized v3.x. Good Android 14 fix but missing Android 16 audio fix and later bugfixes. | |
| v2.7 | End-of-line v2.x. Simpler protocol but broken on Android 14+. Only if fleet is Android 13 or below. | |

**User's choice:** v3.3.4 (Recommended)
**Notes:** Phase 1 only implements video codec readers; expanded control message surface deferred to later phases.

---

## Session Startup Sequence

| Option | Description | Selected |
|--------|-------------|----------|
| Strictly sequential | Push → tunnels → listen → app_process. Always pushes jar. Simplest, matches scrcpy's own client. | ✓ |
| Hybrid sequential | Push jar (with cache check) → tunnels → listen → app_process. Avoids re-push on repeated starts. | |
| Partial parallel | Parallel push + port allocation, then sequential tunnels+launch. Marginal gain, more error paths. | |

**User's choice:** Strictly sequential
**Notes:** Chose simplicity over marginal optimization. No jar caching in v1. Startup latency (~2-4s) acceptable for long-lived sessions.

---

## Error Surface Design

| Option | Description | Selected |
|--------|-------------|----------|
| Domain error codes | Domain-specific codes (ADB_UNAVAILABLE, DEVICE_OFFLINE, etc.) with JSON envelope. pelni_server can switch on codes for retry/user-message logic. | ✓ |
| Generic HTTP codes | Generic 500s with detailed slog internally. Simplest but pelni_server can't differentiate retryable vs fatal. | |
| gRPC-style codes | 16 canonical gRPC codes + domain detail field. Industry-standard but over-engineered for single internal consumer. | |

**User's choice:** Domain error codes (Recommended)
**Notes:** Full causal chains stay in slog. HTTP response carries only top-level domain code + human-readable message. No internal ADB error text in API responses.

---

## Reconciliation Strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Marker-based | Kill only app_process matching gateway's jar CLASSPATH, remove only reverse mappings on gateway port range. Safe for coexisting tools. | ✓ |
| Nuclear cleanup | Kill ALL app_process + remove ALL reverse/forward per device. Guaranteed clean but disrupts other tools. | |
| Reverse-only | Only remove reverse mappings, no process killing. Least disruptive but leaves orphan app_process consuming resources. | |

**User's choice:** Marker-based (Recommended)
**Notes:** Gateway-specific jar filename serves double duty: avoids stomping system scrcpy AND acts as reconciliation marker.

---

## Claude's Discretion

- Exact Go package structure under `internal/` — planner decides
- Per-device mutex implementation (sync.Mutex vs errgroup.Group) — planner decides
- Test infrastructure (fake ADB listener, fixture byte generation) — researcher/planner designs

## Deferred Ideas

None — discussion stayed within phase scope.