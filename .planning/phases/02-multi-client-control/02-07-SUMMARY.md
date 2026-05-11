---
phase: 02-multi-client-control
plan: 07
subsystem: websocket
tags: [websocket, close-read, ping-pong, read-limit, http-timeout, coder-websocket]

# Dependency graph
requires:
  - phase: 02-multi-client-control
    provides: Hub fan-out, WS handlers, ping loop, ReadLimit config
provides:
  - ws.CloseRead in subscribeAndRelay and StreamLogcat for write-only handlers
  - Fixed HTTP server timeouts (ReadHeaderTimeout + WriteTimeout:0) for WS
  - Corrected test coverage for ping/pong, ReadLimit, and close frame processing
affects: [ws-lifecycle, ws-handlers, http-server-config]

# Tech tracking
tech-stack:
  added: []
  patterns: [CloseRead-for-write-only-WS-handlers, ReadHeaderTimeout-instead-of-ReadTimeout-for-WS]

key-files:
  created: []
  modified:
    - internal/api/ws_helpers.go
    - internal/api/handlers_logcat.go
    - cmd/gateway/main.go
    - internal/api/ws_video_test.go

key-decisions:
  - "CloseRead(ctx) placed before pingLoop in subscribeAndRelay — both run concurrently, CloseRead handles pong/close frames while pingLoop sends outbound pings"
  - "CloseRead(ctx) placed before snapshot replay in StreamLogcat — same pattern, write-only handler needs a read goroutine"
  - "ReadHeaderTimeout replaces ReadTimeout — only covers request headers, not the upgraded WS body"
  - "WriteTimeout set to 0 — prevents 65s deadline from persisting on hijacked net.Conn after WS upgrade"

patterns-established:
  - "Write-only WS handlers must call ws.CloseRead(ctx) to process control frames (ping/pong/close)"
  - "HTTP servers that upgrade to WebSocket must use ReadHeaderTimeout + WriteTimeout:0 to avoid deadline leaks on hijacked connections"

requirements-completed: [STR-07, STR-08, STR-09]

# Metrics
duration: 6min
completed: 2026-05-11
---

# Phase 02 Plan 07: WebSocket Code 1006 Fix Summary

**Fixed two compounding bugs causing WS code 1006 on /video, /audio, /logcat: missing CloseRead in write-only handlers and HTTP WriteTimeout persisting on hijacked connections**

## Performance

- **Duration:** 6 min
- **Started:** 2026-05-11T03:34:31Z
- **Completed:** 2026-05-11T03:40:38Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Added ws.CloseRead(ctx) to subscribeAndRelay and StreamLogcat — enables pong processing, ping timeouts, ReadLimit enforcement, and close frame handling
- Fixed HTTP server timeouts: ReadTimeout -> ReadHeaderTimeout, WriteTimeout -> 0 — prevents 65s write deadline from persisting on hijacked WS connections
- Replaced false-positive tests with proper coverage: TestStreamVideoPingPongCycle validates actual ping/pong processing, TestStreamVideoReadLimitApplied validates server-side ReadLimit with StatusMessageTooBig, TestStreamVideoCloseFrameProcessed validates clean close frame handling

## Task Commits

Each task was committed atomically:

1. **Task 1: Add ws.CloseRead() to subscribeAndRelay and StreamLogcat; fix HTTP server timeouts** - `a5b4fe3` (fix)
2. **Task 2: Fix false-positive WS tests, add ping/pong ReadLimit and close frame coverage** - `d1c07f4` (test)

## Files Created/Modified
- `internal/api/ws_helpers.go` - Added ws.CloseRead(ctx) call before pingLoop in subscribeAndRelay
- `internal/api/handlers_logcat.go` - Added ws.CloseRead(ctx) call before snapshot replay in StreamLogcat
- `cmd/gateway/main.go` - Changed ReadTimeout to ReadHeaderTimeout, set WriteTimeout to 0
- `internal/api/ws_video_test.go` - Replaced TestStreamVideoPingLoop with TestStreamVideoPingPongCycle, replaced TestStreamVideoReadLimitApplied with server-side enforcement test, added TestStreamVideoCloseFrameProcessed

## Decisions Made
- CloseRead(ctx) placed before pingLoop goroutine in subscribeAndRelay — they serve different purposes (CloseRead processes inbound control frames, pingLoop sends outbound pings), both must run concurrently
- CloseRead(ctx) placed before snapshot replay loop in StreamLogcat — identical pattern, the main loop writes but never reads from the WS perspective
- ReadHeaderTimeout (Go 1.8+) only sets deadline on reading request headers, not the body — safe for WS because the upgraded connection is the body, not headers
- WriteTimeout:0 removes the deadline that was persisting on hijacked net.Conn — WS idle/ping timeouts are handled by our own pingLoop, not HTTP timeouts

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- WS lifecycle bugs fixed: connections now survive beyond 65s, pings/pongs work correctly, ReadLimit is enforced, close frames are processed
- All existing tests pass with race detector
- Ready for any remaining Phase 2 gap closure or Phase 4 work

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-11*