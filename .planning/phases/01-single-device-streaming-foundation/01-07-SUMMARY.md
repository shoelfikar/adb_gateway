---
phase: 01-single-device-streaming-foundation
plan: 07
subsystem: adb-lifecycle
tags: [adb, reconnect, watchdog, registry-cleanup, reverse-forward-reissuance, device-watcher-restart]

requires:
  - phase: 01
    provides: ADB client, host services, reverse forward, session registry, session supervisor, config, observability, API router, healthz, reconnect with backoff, reconciliation, graceful shutdown

provides:
  - ADB disconnect detection via watchdog probes (ADBWatchdog.ProbeOnce)
  - Registry cleanup on disconnect (MarkAllDisconnected removes idle, transitions active to failed)
  - ADB reconnection loop in main.go that survives adbd restarts
  - Reverse forward re-issuance for active sessions after reconnect
  - Device watcher restart after reconnect with StateFailed->StateIdle recovery
  - HostServices.ReinitializeGoadb for fresh goadb connection post-reconnect
  - DeviceSession accessors (ReverseMap, SetReverseMap, VideoLn) for re-issuance

affects: [02-multi-client-broadcast, 03-multi-device-scaling, 04-production-hardening]

tech-stack:
  added: []
  patterns: [watchdog-probe, lifecycle-goroutine, state-recovery-on-reconnect]

key-files:
  created: []
  modified:
    - internal/adb/reconnect.go
    - internal/adb/reconnect_test.go
    - internal/adb/host_services.go
    - internal/session/registry.go
    - internal/session/registry_test.go
    - internal/session/supervisor.go
    - cmd/gateway/main.go

decisions:
  - ActiveSessionSpecs captures reverse mapping specs from the existing ReverseMapping struct rather than reconstructing from VideoLn.Addr(), avoiding net.Addr format inconsistencies
  - WatchDevices returns bool (true=ADB disconnect, false=graceful shutdown) to let the caller distinguish between the two exit causes
  - MarkAllDisconnected removes idle entries rather than transitioning them to StateFailed (StateIdle->StateFailed is not a valid FSM transition)
  - Watchdog probing uses a dedicated goroutine with a ticker, not a background goroutine in the ADBWatchdog type itself; main.go orchestrates restart

metrics:
  duration: 21m
  completed: 2026-05-07
---

# Phase 01 Plan 07: ADB Reconnection Gap Closure Summary

Gateway detects ADB disconnects via watchdog probes, reconnects with exponential backoff, clears stale registry entries, re-issues reverse forwards for active sessions, and restarts the device watcher -- all without restarting the gateway process.

## Changes Made

### Task 1: ADB disconnect detection and registry cleanup methods

**internal/adb/reconnect.go** -- Added `ADBWatchdog` type with `ProbeOnce` method for single liveness probes and `Interval` accessor. The watchdog does not manage reconnection; it only probes. The caller (main.go) decides what to do on disconnect.

**internal/adb/host_services.go** -- Added `ReinitializeGoadb()` method that creates a fresh `goadb.Adb` instance with the same server config. Called after ADB reconnects so that `NewDeviceWatcher`, `ListDevices`, etc. use a fresh goadb connection.

**internal/session/registry.go** -- Three additions:
1. `MarkAllDisconnected()` -- Iterates all entries; removes idle entries (StateIdle with no session) from the registry, transitions active/starting/stopping entries to StateFailed. StateFailed entries are kept so the reconnect loop can identify which sessions need reverse forwards re-issued.
2. `WatchDevices` now returns `bool` -- `true` when the event channel closes (ADB disconnect), `false` when context is cancelled (graceful shutdown). On device connect events, entries in StateFailed are transitioned back to StateIdle (post-reconnect state recovery).
3. `ActiveSessionSpecs()` -- Returns a map of device serial to reverse mapping specs for all entries with StateActive sessions. Must be called before MarkAllDisconnected since it queries StateActive entries.

**internal/session/supervisor.go** -- Added three thread-safe accessors:
- `ReverseMap() *adb.ReverseMapping` -- Returns the current reverse mapping.
- `SetReverseMap(rm *adb.ReverseMapping)` -- Replaces the reverse mapping (for re-issuance).
- `VideoLn() net.Listener` -- Returns the video listener.

### Task 2: Wire ADB reconnection loop in main.go

**cmd/gateway/main.go** -- Refactored startup into an ADB lifecycle loop:
1. Initial startup unchanged: AwaitADBReady, Reconcile, create device watcher, start WatchDevices goroutine.
2. Added `adbDisconnected` channel (buffered 1) for disconnect signaling.
3. Watchdog goroutine probes ADB every 2 seconds; on failure, signals `adbDisconnected` and stops.
4. WatchDevices goroutine signals `adbDisconnected` when event channel closes (returns true).
5. Main loop: on `adbDisconnected`, captures `ActiveSessionSpecs`, calls `MarkAllDisconnected`, reconnects with `AwaitADBReady`, reinitializes goadb, reconciles, re-issues reverse forwards for each active session, creates new device watcher, restarts WatchDevices goroutine and watchdog.
6. Graceful shutdown on context cancellation (SIGTERM/SIGINT) unchanged.

## Deviations from Plan

None -- plan executed exactly as written.

## Known Stubs

No stubs. All methods have real implementations.

## Threat Flags

No new threat surface beyond what was in the plan's threat model.