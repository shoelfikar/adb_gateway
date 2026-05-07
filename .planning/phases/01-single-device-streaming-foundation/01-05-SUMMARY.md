---
phase: 01-single-device-streaming-foundation
plan: 05
subsystem: session, api
tags: [websocket, chi, session, fsm, supervisor, rest, h264, scrcpy]

# Dependency graph
requires:
  - phase: 01
    provides: [config, obs, auth middleware, errors, healthz, router, adb client, host services, reverse forward, registry, fsm, scrcpy launcher, video reader, embed]
provides:
  - DeviceSession with Start/Close/Run/CodecMeta/VideoConn methods
  - Session supervisor with FSM transitions (idle->starting->active->stopping->idle)
  - REST endpoints: GET /devices, POST /devices/{serial}/sessions, DELETE /devices/{serial}/sessions/{id}
  - WebSocket endpoint: GET /devices/{serial}/video (single-viewer relay)
  - Launch error mapping to domain error codes (D-08)
  - Idempotent session creation (DEV-03)
affects: [01-single-device-streaming-foundation, 02-multi-client-viewing]

# Tech tracking
tech-stack:
  added: [github.com/coder/websocket@v1.8.14, github.com/google/uuid@v1.6.0, golang.org/x/sync@v0.20.0]
  patterns: [session-supervisor-with-fsm, idempotent-handler-with-per-device-mutex, websocket-relay-with-codec-metadata, launcher-interface-for-testability, per-device-sublogger-obs-03]

key-files:
  created:
    - internal/session/supervisor.go
    - internal/session/supervisor_test.go
    - internal/api/handlers_devices.go
    - internal/api/handlers_devices_test.go
    - internal/api/ws_video.go
    - internal/api/ws_video_test.go
  modified:
    - internal/session/registry.go
    - internal/api/router.go
    - internal/api/auth_test.go
    - cmd/gateway/main.go
    - go.mod
    - go.sum

key-decisions:
  - "Launcher defined as interface in session package for testability (avoids circular import with api package)"
  - "IsSessionActive reads DeviceEntry fields directly when caller holds lock (avoids deadlock)"
  - "Handler accesses DeviceEntry fields directly under lock rather than through getter methods"
  - "Error mapping via strings.Contains on launcher error messages (simple, sufficient for Phase 1)"
  - "WebSocket compression disabled (raw H.264 doesn't compress well, adds CPU overhead)"
  - "Frame boundaries preserved: 12-byte raw header + payload as single binary WS message"

patterns-established:
  - "Session supervisor pattern: DeviceSession orchestrates scrcpy lifecycle with FSM transitions"
  - "Launcher interface pattern: session.Launcher interface enables mock injection for tests"
  - "Idempotent handler pattern: CreateSession returns 200 with existing session if StateActive"
  - "Serial validation pattern: alphanumeric + dash + colon only per T-05-02"
  - "WS video relay pattern: codec metadata first, then 12-byte header + payload frames"
  - "Error mapping pattern: launcher errors mapped to domain codes via string matching"

requirements-completed: [DEV-02, DEV-03, DEV-04, STR-01]

# Metrics
duration: 53min
completed: 2026-05-07
---

# Phase 1 Plan 05: Session Supervisor & Video Relay Summary

**Session supervisor with FSM lifecycle, idempotent REST endpoints, and single-viewer WebSocket video relay with codec metadata + frame-boundary-preserved H.264 streaming**

## Performance

- **Duration:** 53 min
- **Started:** 2026-05-07T04:42:33Z
- **Completed:** 2026-05-07T05:35:33Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments

- DeviceSession orchestrates full scrcpy lifecycle: idle -> starting -> active -> stopping -> idle
- REST endpoints for device listing, session creation (idempotent), and session deletion
- WebSocket video relay streams H.264 frames with 12-byte codec metadata as first message
- Auth middleware blocks unauthenticated WebSocket upgrades (returns 401)
- Launch error mapping translates scrcpy failures to domain error codes (D-08)
- Per-device mutex prevents hung device from blocking others (ADB-07)
- Structured logging with device serial + session ID (OBS-03)

## Task Commits

Each task was committed atomically:

1. **Task 1: Session supervisor + device/session REST endpoints** - `12b9126` (feat)
2. **Task 2: Video WebSocket relay + router wiring** - `4bd6913` (feat)

## Files Created/Modified

- `internal/session/supervisor.go` - DeviceSession with Start, Close, Run, CodecMeta, VideoConn; Launcher interface; error mapping; FSM transitions
- `internal/session/supervisor_test.go` - Tests for NewDeviceSession, Start/Close lifecycle, error categories, idempotent creation, concurrent access
- `internal/api/handlers_devices.go` - ListDevices, CreateSession (idempotent per DEV-03), DeleteSession handlers with serial validation
- `internal/api/handlers_devices_test.go` - Tests for device listing, invalid serial, idempotent creation, session deletion, auth requirements
- `internal/api/ws_video.go` - StreamVideo handler and relayVideo function for WebSocket video relay
- `internal/api/ws_video_test.go` - Tests for no session, auth required, auth with key, full frame relay with codec metadata
- `internal/api/router.go` - Updated NewRouter to accept registry, adbClient, hostServices; added device/session/video routes
- `internal/api/auth_test.go` - Updated all NewRouter calls to pass new parameters
- `internal/session/registry.go` - Removed DeviceSession placeholder; added exported Lock/Unlock/GetState/SetState/GetSession/SetSession methods
- `cmd/gateway/main.go` - Updated to initialize ADB client, host services, registry; wire graceful shutdown
- `go.mod` / `go.sum` - Added coder/websocket, google/uuid, x/sync dependencies

## Decisions Made

- **Launcher as interface:** Defined `session.Launcher` interface instead of using `*scrcpy.Launcher` directly. This avoids circular imports (session -> api) and enables mock injection in tests. The concrete `*scrcpy.Launcher` satisfies the interface.
- **IsSessionActive without lock:** Changed `IsSessionActive` to read DeviceEntry fields directly when the caller already holds the per-device lock, preventing deadlock between handler and getter methods.
- **Handler field access under lock:** Handler accesses DeviceEntry.State and DeviceEntry.Session directly when holding the lock, rather than through getter methods that would also acquire the lock.
- **Error mapping via string matching:** Launch errors are mapped to domain codes using `strings.Contains` on the error message. Simple and sufficient for Phase 1; the launcher returns well-defined error prefixes.
- **WebSocket compression disabled:** Raw H.264 does not compress well and adds CPU overhead per STR-01 and RESEARCH.md Pattern 4.
- **Frame boundary preservation:** Each WS message is 12-byte raw header + payload concatenated, preserving frame boundaries for the browser's WebCodecs decoder.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Router signature change required test updates**
- **Found during:** Task 1 (session supervisor + REST endpoints)
- **Issue:** NewRouter signature changed from `(cfg)` to `(cfg, registry, adbClient, hostServices)`, breaking existing auth tests
- **Fix:** Updated all `NewRouter` calls in auth_test.go to pass registry, nil adbClient, nil hostServices
- **Files modified:** internal/api/auth_test.go
- **Verification:** All auth tests pass
- **Committed in:** 12b9126 (Task 1 commit)

**2. [Rule 3 - Blocking] DeviceEntry.mu unexported, inaccessible from api package**
- **Found during:** Task 1 (handlers_devices.go implementation)
- **Issue:** DeviceEntry.mu field is unexported but handler needed to lock per-device mutex
- **Fix:** Added exported Lock/Unlock/GetState/SetState/GetSession/SetSession methods to DeviceEntry
- **Files modified:** internal/session/registry.go, internal/api/handlers_devices.go
- **Verification:** All tests pass
- **Committed in:** 12b9126 (Task 1 commit)

**3. [Rule 3 - Blocking] DeviceSession placeholder removed from registry.go**
- **Found during:** Task 1 (supervisor.go implementation)
- **Issue:** Placeholder DeviceSession type and Close method in registry.go needed to be replaced with real implementation
- **Fix:** Removed placeholder from registry.go, defined full DeviceSession in supervisor.go
- **Files modified:** internal/session/registry.go, internal/session/supervisor.go
- **Verification:** All tests pass
- **Committed in:** 12b9126 (Task 1 commit)

**4. [Rule 3 - Blocking] DeleteSession error wrapping broke DomainError detection**
- **Found during:** Task 1 (handler test debugging)
- **Issue:** `fmt.Errorf("session ID mismatch: %w", ErrSessionNotFound)` wrapped the DomainError, making it not detectable by `writeError`
- **Fix:** Changed to return `ErrSessionNotFound` directly instead of wrapping
- **Files modified:** internal/api/handlers_devices.go
- **Verification:** TestDeleteSessionIDMismatch passes with 404 status
- **Committed in:** 12b9126 (Task 1 commit)

**5. [Rule 3 - Blocking] go.mod needed coder/websocket and google/uuid**
- **Found during:** Task 2 (WS video relay)
- **Issue:** Missing go.mod dependencies for coder/websocket, google/uuid, x/sync
- **Fix:** Added dependencies via `go get`
- **Files modified:** go.mod, go.sum
- **Verification:** `go build ./...` succeeds
- **Committed in:** 4bd6913 (Task 2 commit)

---

**Total deviations:** 5 auto-fixed (3 blocking type, 2 blocking type)
**Impact on plan:** All deviations necessary for build and test correctness. No scope creep.

## Issues Encountered

- Test deadlock in `TestCreateSessionIdempotent` due to IsSessionActive acquiring the mutex while handler already held it. Fixed by making IsSessionActive read fields directly without lock when caller holds it.
- Test panic in `TestRouterDevicesWithAuth` due to nil registry passed to NewRouter. Fixed by passing session.NewRegistry() to all NewRouter calls.

## Known Stubs

- DeviceSession.Run() is implemented but not called in the current codebase. The video relay reads directly from session.VideoConn() in the WS handler. Run() will be used in Phase 2 for multi-client fan-out.
- The handler creates a new scrcpy.Launcher per CreateSession request. This is fine for Phase 1 (single device, single viewer) but may need pooling or reuse in Phase 2.

## Threat Flags

| Flag | File | Description |
|------|------|-------------|
| threat_flag: tampering | internal/api/handlers_devices.go | Serial validation per T-05-02 (alphanumeric + dash + colon only) |
| threat_flag: spoofing | internal/api/ws_video.go | Auth middleware validates API key before WS upgrade per T-05-01 |
| threat_flag: tampering | internal/api/router.go | All device/video routes behind APIKeyAuth middleware |

## Next Phase Readiness

- Session lifecycle (idle->starting->active->stopping->idle) is fully operational
- REST CRUD for devices and sessions works with idempotent creation
- WebSocket video relay sends codec metadata then H.264 frames with preserved boundaries
- Auth middleware blocks unauthenticated WebSocket upgrades (returns 401 before upgrade)
- Ready for Phase 2: multi-client fan-out (Hub pattern), frame dropping on slow clients, audio relay

## Self-Check: PASSED

- All created files verified: supervisor.go, supervisor_test.go, handlers_devices.go, handlers_devices_test.go, ws_video.go, ws_video_test.go, router.go
- All commits verified: 12b9126, 4bd6913
- `go test ./internal/session/... ./internal/api/... -count=1` passes
- `go vet ./...` passes

---
*Phase: 01-single-device-streaming-foundation*
*Completed: 2026-05-07*