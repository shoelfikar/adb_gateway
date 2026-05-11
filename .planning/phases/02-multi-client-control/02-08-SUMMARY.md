---
phase: 02-multi-client-control
plan: 08
subsystem: observability
tags: [prometheus, metrics, lease, hub, websocket]

# Dependency graph
requires:
  - phase: 02-multi-client-control
    provides: Hub and LeaseManager infrastructure from plans 02-04 and 02-05
provides:
  - Four Phase 2 Prometheus collectors (LeaseAcquiredTotal, LeaseReleasedTotal, WSFramesSentTotal, HubViewersActive)
  - Instrumentation of LeaseManager lifecycle events
  - Instrumentation of Hub viewer count and frame send events
affects: [phase-03-multi-device-fleet, phase-04-horizontal-scaling]

# Tech tracking
tech-stack:
  added: []
  patterns: [delta-based-prometheus-assertions-in-tests, package-level-counter-accumulation-guard]

key-files:
  created: []
  modified:
    - internal/obs/metrics.go
    - internal/obs/metrics_test.go
    - internal/session/lease.go
    - internal/session/hub.go

key-decisions:
  - "Used delta-based assertions (testutil.ToFloat64 before/after) in TestLeaseMetrics and TestHubViewersActiveGauge to avoid cross-test pollution from package-level Prometheus counters"
  - "Used unique stream labels (video_test_hub_gauge, audio_test_hub_gauge) in TestHubViewersActiveGauge to isolate from other tests"

patterns-established:
  - "Delta-based counter assertions: read testutil.ToFloat64 before and after operations, assert on delta"
  - "Gauge tests use unique label values to avoid cross-test interference from package-level vars"

requirements-completed: [OBS-01, OBS-02]

# Metrics
duration: 18min
completed: 2026-05-11
---

# Phase 2 Plan 08: Missing Phase 2 Prometheus Collectors Summary

**Four Phase 2 Prometheus collectors for lease lifecycle, hub viewers, and WS frame sends, with D-18 cardinality compliance**

## Performance

- **Duration:** 18 min
- **Started:** 2026-05-11T03:34:02Z
- **Completed:** 2026-05-11T03:52:16Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Defined and registered LeaseAcquiredTotal (counter), LeaseReleasedTotal (counter with reason label), WSFramesSentTotal (counter with stream label), and HubViewersActive (gauge with stream label)
- Instrumented LeaseManager.Acquire with LeaseAcquiredTotal.Inc() and reapLockedLocked with LeaseReleasedTotal.WithLabelValues(string(reason)).Inc()
- Instrumented Hub.Subscribe with HubViewersActive.Inc(), unsubscribe closure with .Dec(), evict with .Dec(), and fan-out loop with WSFramesSentTotal.Inc()
- TestPhase2MetricNames now discovers all 11 metric families (7 existing + 4 new)
- Added TestLeaseMetrics and TestHubViewersActiveGauge with delta-based assertions to handle package-level counter accumulation

## Task Commits

Each task was committed atomically:

1. **Task 1: Define and register four Phase 2 metrics collectors** - `0faf30f` (test)
2. **Task 2: Instrument LeaseManager and Hub with new metrics** - `f88d8b4` (feat)

_Note: Task 1 combined TDD RED and GREEN commits since the collector definitions and their test references are tightly coupled (tests reference the package-level vars)._

## Files Created/Modified
- `internal/obs/metrics.go` - Added 4 new collector vars (LeaseAcquiredTotal, LeaseReleasedTotal, WSFramesSentTotal, HubViewersActive) and registered them in MustRegister
- `internal/obs/metrics_test.go` - Added TestLeaseMetrics, TestHubViewersActiveGauge; updated TestPhase2MetricNames for 11 families; updated TestPhase2NoDeviceSerialLabel to exercise new collectors
- `internal/session/lease.go` - Added obs import; instrumented Acquire (LeaseAcquiredTotal.Inc) and reapLockedLocked (LeaseReleasedTotal.WithLabelValues)
- `internal/session/hub.go` - Instrumented Subscribe (HubViewersActive.Inc), unsubscribe closure (HubViewersActive.Dec), evict (HubViewersActive.Dec), and fan-out (WSFramesSentTotal.Inc)

## Decisions Made
- Used delta-based assertions (testutil.ToFloat64 before/after) instead of absolute values to avoid cross-test pollution from package-level Prometheus counters that accumulate across test runs
- Used unique stream labels in TestHubViewersActiveGauge (video_test_hub_gauge, audio_test_hub_gauge) to isolate gauge values from other tests that use "video" and "audio" labels

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] TestPhase2MetricNames missing gateway_session_state**
- **Found during:** Task 1 (Define and register four Phase 2 metrics collectors)
- **Issue:** gateway_session_state was not appearing in Gather results because it was never initialized with label values in the test
- **Fix:** Added `SessionState.WithLabelValues("test-device", "idle").Set(0)` to ensure the family appears in Gather output
- **Files modified:** internal/obs/metrics_test.go
- **Verification:** TestPhase2MetricNames now discovers all 11 families including gateway_session_state
- **Committed in:** 0faf30f (Task 1 commit)

**2. [Rule 1 - Bug] TestLeaseMetrics and TestHubViewersActiveGauge failed with absolute assertions**
- **Found during:** Task 1 (running tests after initial test creation)
- **Issue:** Package-level Prometheus counters/gauges accumulate across tests; absolute value assertions (e.g., "should be 2") failed because prior tests already incremented the same metrics
- **Fix:** Switched to delta-based assertions using testutil.ToFloat64() for before/after comparison; used unique stream labels in gauge test to avoid interference
- **Files modified:** internal/obs/metrics_test.go
- **Verification:** All obs and session tests pass with -race flag
- **Committed in:** 0faf30f (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (1 blocking, 1 bug)
**Impact on plan:** Both auto-fixes were test quality issues. No scope creep; all behavioral requirements met as specified.

## Issues Encountered
None beyond the deviations documented above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All 4 Phase 2 metrics collectors defined, registered, and instrumented
- /metrics endpoint will expose gateway_lease_acquired_total, gateway_lease_released_total, gateway_ws_frames_sent_total, gateway_hub_viewers_active
- OBS-01 and OBS-02 requirements are now satisfied
- Phase 2 multi-client-control plan set is complete

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-11*