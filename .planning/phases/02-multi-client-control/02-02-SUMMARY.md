---
phase: 02-multi-client-control
plan: 02
subsystem: session
tags: [fan-out, hub, atomic-pointer, slow-consumer-eviction, late-joiner-cache, prometheus, websocket-relay]
requires:
  - phase: 02-01
    provides: obs.FramesEmittedTotal, obs.FramesDroppedTotal (CounterVec with stream label), StreamConfig.ViewerBufferFrames, StreamConfig.MaxConsecutiveDrops
provides:
  - Hub type with NewHub, Run, Publish, Subscribe, SetCodecMeta
  - Frame type with wireBytes method
  - Per-viewer bounded buffer with slow-consumer eviction (STR-04/05/06)
  - Late-joiner cache: codec metadata + atomic keyframe cache (STR-07)
  - Drop counter resets on success (Pitfall 2)
  - Race-free unsubscribe (map removal only; channel closure by Hub goroutine)
affects: [02-05, 02-06, 03-streaming-hardening]
tech-stack:
  added: []
  patterns: [atomic.Pointer for lock-free cache reads, Hub goroutine single-owner channel closure, non-blocking fan-out with select/default]
key-files:
  created:
    - internal/session/hub.go
    - internal/session/hub_test.go
  modified: []
decisions:
  - "Unsubscribe removes from map but does NOT close viewer.send channel — only evict() and shutdown() close channels (eliminates data race between goroutines)"
  - "ViewerCountForTest test affordance added for non-racy eviction assertions"
  - "Publish is non-blocking on h.in (capacity 16); producer-side drops counted under same stream label as viewer-side drops"
  - "Drop counter resets on every successful send (consecutive, not cumulative — Pitfall 2)"
requirements-completed: [STR-04, STR-05, STR-06, STR-07]
duration: 15min
completed: 2026-05-08
---

# Phase 2 Plan 02: Hub Fan-Out Summary

**Per-device Hub with 1:N fan-out, bounded per-viewer buffers, slow-consumer eviction at 120 consecutive drops, and atomic late-joiner cache (metadata + keyframe)**

## Performance

- **Duration:** 15 min
- **Started:** 2026-05-08T02:39:10Z
- **Completed:** 2026-05-08T02:54:11Z
- **Tasks:** 2 (TDD: RED then GREEN)
- **Files modified:** 2

## Accomplishments
- Hub type implementing 1:N fan-out with non-blocking sends and drop accounting
- Late-joiner cache using `atomic.Pointer[Frame]` for keyframes and `atomic.Pointer[[12]byte]` for codec metadata
- Slow-consumer eviction: 120 consecutive drops triggers channel close with "slow_consumer" reason
- Drop counter resets on every successful send (Pitfall 2 explicitly tested)
- Race-free design: only the Hub goroutine closes viewer channels (via evict/shutdown), unsubscribe only removes from map
- 8 test functions all passing under `-race` detector

## Task Commits

Each task was committed atomically (TDD cycle):

1. **Task 1 (RED): Failing tests for Hub fan-out** - `17dbd12` (test)
2. **Task 2 (GREEN): Implement Hub type** - `9daabd2` (feat)

## Files Created/Modified
- `internal/session/hub.go` - Hub type with NewHub, Run, Publish, Subscribe, SetCodecMeta, evict, shutdown; Frame type with wireBytes; viewer type; ErrHubClosed sentinel
- `internal/session/hub_test.go` - 8 test functions: TestHubMultiViewer, TestHubBackpressure, TestHubSlowDisconnect, TestHubLateJoiner, TestHubDropCounterResets, TestHubKeyframeReplacedAtomically, TestHubRunCancel, TestHubPublishWhenInFull

## Decisions Made
- Unsubscribe removes viewer from map but does NOT close the send channel — eliminates data race between WS handler goroutine and Hub goroutine. Channel closure is exclusively done by the Hub goroutine (via `evict()` for slow consumers and `shutdown()` for context cancellation). WS handlers detect exit via context cancellation, not channel close.
- `ViewerCountForTest()` added as a test-only method on Hub for non-racy assertions about eviction state (same package access, no `_internal` needed).
- `Publish()` uses non-blocking send on `h.in` (capacity 16). Producer-side drops are counted under the same `obs.FramesDroppedTotal{stream}` label as viewer-side drops, giving a single source of truth for total frame drops.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Data race between unsubscribe close() and Hub goroutine send()**
- **Found during:** Task 2 - race detector flagged concurrent close/send on viewer.send channel
- **Issue:** Plan's original `Subscribe` unsubscribe function called `close(v.send)` with a `recover()` guard, but this races with the Hub goroutine's `v.send <- msg` non-blocking send. The race detector correctly identifies this as a data race even with the recover guard.
- **Fix:** Removed `close(v.send)` from unsubscribe. Unsubscribe now only removes the viewer from the map under write lock. Channel closure is exclusively done by the Hub goroutine (via `evict()` for slow consumers and `shutdown()` for context cancellation). WS handlers exit via context cancellation, not channel close.
- **Files modified:** `internal/session/hub.go` (Subscribe method), `internal/session/hub_test.go` (removed defer v1unsub patterns that relied on channel close for test cleanup)
- **Verification:** All 8 tests pass with `-race` flag
- **Committed in:** 9daabd2

**2. [Rule 1 - Bug] TestHubRunCancel failed because channel had buffered data**
- **Found during:** Task 2 - test expected immediate closed-channel read after Run exit, but channel had preloaded metadata
- **Issue:** After context cancellation, `shutdown()` closes all viewer channels. But channels had 1 buffered message (metadata) from Subscribe preloading. Reading once from the channel returned the metadata with `ok=true`, not the closed signal.
- **Fix:** Added `drainAndVerifyClosed` helper that drains all messages from a channel before asserting it's closed. Updated `TestHubRunCancel` to use this helper.
- **Files modified:** `internal/session/hub_test.go`
- **Verification:** TestHubRunCancel passes
- **Committed in:** 9daabd2

**3. [Rule 1 - Bug] TestHubSlowDisconnect failed because not enough frames reached the Hub goroutine**
- **Found during:** Task 2 - ViewerCountForTest returned 1 (viewer not evicted) after publishing 200 frames
- **Issue:** The Hub's input channel `h.in` has capacity 16. When 200 frames are published in a tight loop, many are dropped at the Publish level (h.in full) before reaching the Hub goroutine for fan-out. The Hub goroutine couldn't process enough frames to accumulate 120 consecutive viewer-side drops.
- **Fix:** Changed the test to publish 250 frames with periodic yields (`time.Sleep(20ms)` every 50 frames) to ensure the Hub goroutine has time to process frames between batches. Used `assert.Eventually` for eviction assertion instead of fixed sleep.
- **Files modified:** `internal/session/hub_test.go`
- **Verification:** TestHubSlowDisconnect passes with race detector
- **Committed in:** 9daabd2

---

**Total deviations:** 3 auto-fixed (3 bugs)
**Impact on plan:** All auto-fixes were correctness fixes for race conditions and test timing. No scope creep. The core design (no channel close in unsubscribe) is a more robust pattern than the plan's original recover-based approach.

## Issues Encountered
- None beyond the auto-fixes documented above.

## User Setup Required
None - no external service configuration required.

## How to Consume from Downstream Plans

```go
// Plan 02-05 (supervisor wiring) will integrate Hub like this:
videoHub := session.NewHub(session.HubOpts{
    Stream:              "video",
    BufFrames:           cfg.Stream.ViewerBufferFrames,
    MaxConsecutiveDrops: cfg.Stream.MaxConsecutiveDrops,
    Log:                 slog.With("device", serial, "stream", "video"),
})
videoHub.SetCodecMeta(result.CodecMeta) // 12-byte codec metadata from scrcpy launch

// Supervisor wires: videoReader -> videoHub.Publish(frame)
// WS handler subscribes: ch, unsub := videoHub.Subscribe(viewerID)

// Start fan-out goroutine:
g.Go(func() error {
    return videoHub.Run(ctx)
})
```

## Next Phase Readiness
- Hub type is complete and ready for wiring in Plan 02-05 (supervisor integration)
- WS handlers in Plan 02-06 will use `Hub.Subscribe()` for video/audio stream relay
- Slow-consumer eviction will trigger WS close code 1008 in Plan 02-06
- Late-joiner cache (metadata + keyframe) will be consumed by WS handlers on initial connection

## Self-Check: PASSED

- `internal/session/hub.go` FOUND
- `internal/session/hub_test.go` FOUND
- `.planning/phases/02-multi-client-control/02-02-SUMMARY.md` FOUND
- Commit `17dbd12` FOUND
- Commit `9daabd2` FOUND
- All 8 TestHub* tests PASS with `-race`

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-08*