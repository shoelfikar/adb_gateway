---
phase: 01-single-device-streaming-foundation
plan: 08
gap_closure: true
requirements: [ADB-08]
status: complete
started: 2026-05-07
completed: 2026-05-07
---

# Plan 01-08: Fix ADB Reconnection Gap

## Objective
Fix ADB reconnection gap: stale device data shown after disconnect, new sessions fail after reconnect, and session resources leak.

## What Changed

### Task 1: Add ReleaseResources method and fix MarkAllDisconnected
- **Added `DeviceSession.ReleaseResources()`** in `supervisor.go`: idempotent method that closes videoLn, videoConn, reverseMap, and calls cleanup — all without FSM transitions. Called by MarkAllDisconnected during ADB disconnect cleanup.
- **Rewrote `MarkAllDisconnected()`** in `registry.go`: now releases session resources for ALL entries (not just idle ones) and removes ALL entries from the registry. Previous implementation kept StateFailed entries causing stale device data in GET /devices. WatchDevices re-populates the registry on reconnect.
- **Removed `ActiveSessionSpecs()`** from `registry.go`: dead code after reconnect loop simplification (no longer re-issues reverse forwards for dead sessions).
- **Updated tests**: TestMarkAllDisconnected now verifies ALL entries are removed (not just idle ones). Added TestMarkAllDisconnected_ReleasesSessionResources and TestReleaseResources_Idempotent.

### Task 2: Simplify reconnect loop and fix ListDevices
- **Simplified ADB reconnect loop** in `cmd/gateway/main.go`: removed ActiveSessionSpecs capture and the entire reverse forward re-issuance block. MarkAllDisconnected now handles all cleanup. Reconciliation still kills orphan processes and removes stale forwards on the device. WatchDevices re-populates the registry when devices reconnect.
- **Added StateFailed filter to `ListDevices`** in `handlers_devices.go`: defense-in-depth so GET /devices never shows stale/unavailable devices, even if entries somehow remain in StateFailed.
- **Added TestListDevicesExcludesFailed**: verifies that StateFailed entries are filtered from GET /devices responses.

## Key Design Decisions
1. **ReleaseResources is separate from Close**: Close transitions FSM states (Active->Stopping->Idle), while ReleaseResources just releases file descriptors without state transitions. ADB disconnect means the scrcpy server on the device is dead — there's nothing to cleanly transition, so we just free resources.
2. **Remove ALL entries, not transition to StateFailed**: The previous approach of transitioning active entries to StateFailed and keeping them was flawed. After ADB disconnect, those entries reference dead sessions and stale reverse forwards. Removing them completely and letting WatchDevices re-populate is simpler and correct.
3. **No reverse forward re-issuance**: The scrcpy server on the device is dead after ADB disconnect. Re-issuing reverse forwards for a dead server is futile. The reconciliation step on reconnect cleans up stale forwards on the device, and new sessions create fresh reverse forwards.

## Verification
- `go test ./internal/session/... -v -count=1` — all tests pass
- `go test ./internal/api/... -v -run "TestListDevices" -count=1` — all tests pass
- `go build ./cmd/gateway/` — compiles without errors
- `go vet ./...` — no issues

## Deviations
None — plan executed as specified.

## Key Files Created/Modified
- `internal/session/supervisor.go` — added ReleaseResources() method
- `internal/session/registry.go` — rewrote MarkAllDisconnected(), removed ActiveSessionSpecs()
- `internal/session/registry_test.go` — updated tests for new behavior
- `cmd/gateway/main.go` — simplified reconnect loop
- `internal/api/handlers_devices.go` — added StateFailed filter to ListDevices
- `internal/api/handlers_devices_test.go` — added TestListDevicesExcludesFailed