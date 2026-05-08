---
phase: 02-multi-client-control
plan: 04
subsystem: session-lease
tags: [lease, state-machine, ttl, grace-period, constant-time-compare, concurrency]

# Dependency graph
requires:
  - phase: 02-01
    provides: config keys (Control.LeaseTTLSeconds), domain errors, metrics collectors
provides:
  - LeaseManager type with Acquire/Extend/Release/BeginGrace/CancelGrace/ForceRelease/IsHeldBy/ReleaseChanFor/Snapshot
  - Lease snapshot type (ID, OwnerKey, ExpiresAt)
  - ReleaseReason constants (expired, admin_revoked, device_gone, client_released)
  - Sentinel errors (ErrLeaseHeldByOther, ErrLeaseNotFound, ErrLeaseMismatch)
  - ctEqual constant-time UUID compare helper
  - fingerprint owner-key logging helper
affects: [02-05, 02-06]

# Tech tracking
tech-stack:
  added: []
  patterns: [per-lease buffered release channel, time.AfterFunc for TTL/grace with stale-ID guard, constant-time UUID compare, timer.Stop in reap for Pitfall 9]

key-files:
  created:
    - internal/session/lease.go
    - internal/session/lease_test.go

key-decisions:
  - "LeaseManager lives as a field on DeviceEntry (per-device, independent mutex avoids lock-order issues with DeviceEntry.mu)"
  - "Per-lease buffered(1) release channel — closed after send, never reused across Acquire cycles"
  - "5s grace period implemented via time.AfterFunc replacing the TTL timer — same expireFromTimer callback, no timer goroutine leak (Pitfall 9)"
  - "Extend during grace re-anchors: clears graceUntil, resets TTL timer to full duration (D-10)"
  - "ctEqual uses subtle.ConstantTimeCompare on UUID strings — length leak acceptable since UUID v4 is always 36 chars"
  - "newLeaseManagerForTest unexported helper for custom grace duration in tests"

patterns-established:
  - "Lease state machine: [no lease] → Acquire → [held] → Extend/Release/BeginGrace/ForceRelease → [no lease]"
  - "reapLockedLocked pattern: stop timer, non-blocking send on buffered release channel, close channel, nil out pointer — single cleanup path for all release reasons"
  - "expireFromTimer stale-ID guard: timer callback checks lease ID before releasing, covers rapid Acquire/Release race"

requirements-completed: [CTL-01, CTL-04, CTL-05]

# Metrics
duration: 4min
completed: 2026-05-08
---

# Phase 2 Plan 04: Lease State Machine Summary

**Per-device reservation lease with TTL expiry, 5s grace on disconnect, force-release events, and constant-time UUID compare**

## Performance

- **Duration:** 4 min
- **Started:** 2026-05-08T03:25:18Z
- **Completed:** 2026-05-08T03:29:12Z
- **Tasks:** 2 (TDD: RED then GREEN)
- **Files modified:** 2 new files

## Accomplishments
- LeaseManager with 9 exported methods: Acquire, Extend, Release, ForceRelease, BeginGrace, CancelGrace, IsHeldBy, ReleaseChanFor, Snapshot
- Constant-time UUID compare via crypto/subtle.ConstantTimeCompare (CTL-01 security boundary)
- Per-lease buffered(1) release channel with 4 ReleaseReason constants for force-release event delivery
- 5s grace period on controller WS disconnect prevents "lease whiplash" (D-10)
- Timer cleanup on every release path prevents goroutine leaks (Pitfall 9)
- 12 tests passing under -race, including concurrent acquire exclusivity and 1000-cycle timer-leak check

## Task Commits

Each task was committed atomically:

1. **Task 1+2 (TDD RED): Failing tests for lease state machine** - `6bb7f1d` (test)
2. **Task 1+2 (TDD GREEN): Lease implementation** - `8364839` (feat)

## Files Created/Modified
- `internal/session/lease.go` - LeaseManager, Lease, ReleaseReason constants, sentinel errors, ctEqual, fingerprint, newLeaseManagerForTest helper
- `internal/session/lease_test.go` - 12 test functions: Exclusive, Expiry, ExtendResetsTTL, Release, Grace, PatchDuringGrace, ForceReleaseAdmin, ForceReleaseDeviceGone, ConstantTimeCompare, ExtendWrongID, ReleaseChanForeRapidReacquire, NoTimerLeak

## Decisions Made
- LeaseManager mutex is independent of DeviceEntry.mu — caller must not hold DeviceEntry.mu when calling LeaseManager methods (avoids lock-order deadlock)
- Per-lease release channel is buffered(1) and closed after send — new Acquire allocates fresh channel, stale ID lookups return nil
- Grace period reuses expireFromTimer callback — no distinction needed between TTL and grace timer since both check the current lease ID
- fingerprint() truncates OwnerKey to first 8 chars for logging — SHA-256 hex (64 chars) truncated is non-reversible per T-02-04-05

## Deviations from Plan

None - plan executed exactly as written.

## Known Stubs

None. All interfaces are fully implemented; no placeholder text or hardcoded empty values.

## Threat Flags

No new threat surface beyond what the plan's threat model documented. All mitigations are in place:
- T-02-04-01: ctEqual uses subtle.ConstantTimeCompare — verified in TestLeaseConstantTimeCompare
- T-02-04-02: BeginGrace blocks Acquire during grace — verified in TestLeaseGrace
- T-02-04-03: Per-lease releaseCh, stale ID returns nil — verified in TestLeaseReleaseChanForeRapidReacquire
- T-02-04-04: reapLockedLocked calls timer.Stop() — verified in TestLeaseNoTimerLeak (1000 cycles, bounded goroutines)
- T-02-04-05: fingerprint() truncates OwnerKey for logs — verified in code review
- T-02-04-06: ForceRelease is package-level — accept per plan; plan 02-06 adds auth middleware

## Next Phase Readiness
- LeaseManager is ready for embedding on DeviceEntry in plan 02-05
- REST handlers (POST/PATCH/DELETE /reservation) in plan 02-06 will call Acquire/Extend/Release
- Control WS handler in plan 02-06 will call IsHeldBy for lease validation and listen on ReleaseChanFor for force-release delivery

## Self-Check: PASSED

- `internal/session/lease.go` FOUND
- `internal/session/lease_test.go` FOUND
- Commit `6bb7f1d` FOUND
- Commit `8364839` FOUND
- All `TestLease*` tests PASS with `-race`

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-08*