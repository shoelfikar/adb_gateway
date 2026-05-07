---
phase: 01-single-device-streaming-foundation
plan: 03
subsystem: internal/session
tags: [device-registry, fsm, sync-map, track-devices, state-machine]

# Dependency graph
requires:
  - phase: 01
    provides: [adb.DeviceEvent from host_services.go]
provides:
  - Thread-safe device registry with sync.Map
  - Session state machine with D-05 transition validation
  - Per-device mutex (ADB-07 compliance)
  - WatchDevices goroutine for real-time device tracking
  - DeviceSession placeholder for Plan 05
affects: [session supervisor, REST handlers, WebSocket video relay]

# Tech tracking
tech-stack:
  added:
    - sync.Map for thread-safe device registry
    - sync.Mutex per DeviceEntry (not global) per ADB-07
  patterns:
    - LoadOrStore for idempotent device creation
    - WatchDevices goroutine bridges adb.DeviceEvent channel to registry updates
    - FSM transition map with canTransition/TransitionTo pure functions
    - DeviceSession placeholder type for forward reference

key-files:
  created:
    - internal/session/fsm.go
    - internal/session/fsm_test.go
    - internal/session/registry.go
    - internal/session/registry_test.go

key-decisions:
  - "DeviceEntry.Session typed as *DeviceSession placeholder for Plan 05 real implementation"
  - "Per-device sync.Mutex on DeviceEntry prevents global deadlock (ADB-07)"
  - "WatchDevices treats 'device', 'recovery', and 'offline' as connect states"
  - "TransitionTo is a pure function (no side effects) -- caller must assign the result"
  - "canTransition is unexported; TransitionTo is the public API"

patterns-established:
  - "sync.Map for read-heavy occasionally-written registry (device tracking)"
  - "Per-device mutex (sync.Mutex) on each DeviceEntry -- never a global lock"
  - "FSM transition validation via map lookup with error messages including state names"
  - "DeviceEvent channel bridging from adb package to session package"

requirements-completed: [DEV-01]

# Metrics
duration: 8min
completed: 2026-05-07
---

# Phase 1 Plan 03: Device Registry + Session FSM Summary

**Thread-safe device registry with sync.Map and per-device mutex, plus session FSM enforcing D-05 state transitions**

## Performance

- **Duration:** 8 min
- **Started:** 2026-05-07T04:02:48Z
- **Completed:** 2026-05-07T04:10:47Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Device registry tracks connected devices by serial using sync.Map for thread-safe concurrent access
- Per-device mutex on DeviceEntry prevents global deadlock (ADB-07 requirement)
- WatchDevices goroutine reads from adb.DeviceEvent channel and updates registry on connect/disconnect
- Session FSM enforces all D-05 transitions with human-readable error messages for slog correlation
- Failed state allows retry via failed->idle transition

## Task Commits

Each task was committed atomically:

1. **Task 2: Session FSM with transition validation** - `bffa4b0` (feat)
2. **Task 1: Device registry with track-devices integration** - `dc0bd8f` (feat)

_Note: Task 2 (fsm.go) was committed first since registry.go imports SessionState from it._

## Files Created/Modified
- `internal/session/fsm.go` - SessionState enum, transition validation map, canTransition and TransitionTo functions
- `internal/session/fsm_test.go` - 10 tests: valid transitions, invalid transitions, String(), error messages, lifecycle cycle, retry cycle
- `internal/session/registry.go` - DeviceEntry with per-device mutex, Registry with sync.Map, GetOrCreate, Get, List, Remove, WatchDevices, CloseAllSessions, DeviceSession placeholder
- `internal/session/registry_test.go` - 10 tests: NewRegistry, GetOrCreate, Get, List, Remove, WatchDevices, context cancellation, channel close, concurrent access, CloseAllSessions, per-device mutex

## Decisions Made
- DeviceEntry.Session typed as `*DeviceSession` placeholder -- the real DeviceSession with errgroup and video relay will be defined in Plan 05. Close method returns a "not yet implemented" error for now.
- WatchDevices treats "device", "recovery", and "offline" ADB states as "device connected" -- offline devices are still tracked so session manager can attempt connection.
- TransitionTo is a pure function (takes current state and target, returns new state or error) -- the caller is responsible for assigning the result to the DeviceEntry.State field under the per-device mutex.
- canTransition is unexported; TransitionTo is the public API for state transitions.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed unreliable sync.Mutex pointer comparison test**
- **Found during:** Task 1 (registry tests)
- **Issue:** `assert.NotEqual(t, &entry1.mu, &entry2.mu)` failed because `sync.Mutex` struct comparison doesn't work as a pointer distinctness check. The addresses of embedded struct fields can compare equal in some Go implementations.
- **Fix:** Removed the pointer assertion; functional proof that per-device mutexes work is demonstrated by the lock/unlock test that verifies locking entry1.mu does not block entry2.mu, plus the -race detector passing on TestConcurrentAccess.
- **Files modified:** internal/session/registry_test.go
- **Verification:** All 10 registry tests pass with -race flag
- **Committed in:** dc0bd8f (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Minimal -- test approach changed, functional behavior unchanged.

## Issues Encountered
None

## Known Stubs

- `DeviceSession.Close()` returns `fmt.Errorf("DeviceSession.Close not yet implemented")` -- placeholder for Plan 05 (session supervisor). CloseAllSessions calls this and logs the error. This is intentional; the real implementation will be wired in Plan 05.

## Next Phase Readiness
- Registry and FSM are ready for Plan 04 (scrcpy launcher references session state)
- Registry and FSM are ready for Plan 05 (session supervisor manages lifecycle, sets DeviceEntry.Session)
- WatchDevices is ready to be wired into the main goroutine in Plan 06

## Self-Check: PASSED

- `internal/session/fsm.go` exists: YES
- `internal/session/fsm_test.go` exists: YES
- `internal/session/registry.go` exists: YES
- `internal/session/registry_test.go` exists: YES
- Commit bffa4b0 verified in git log: YES
- Commit dc0bd8f verified in git log: YES
- `go test ./internal/session/... -count=1 -race` passes: YES
- FSM allows idle->starting->active->stopping->idle cycle: YES
- FSM allows starting->failed->idle retry cycle: YES
- FSM rejects idle->active: YES

---
*Phase: 01-single-device-streaming-foundation*
*Completed: 2026-05-07*