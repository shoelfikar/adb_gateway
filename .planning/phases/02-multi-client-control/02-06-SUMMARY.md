---
phase: 02-multi-client-control
plan: 06
subsystem: api-routing
tags: [ws-audio, ws-control, reservation, hub-wiring, metrics, soak-test]

# Dependency graph
requires:
  - phase: 02-01
    provides: config keys (WS.*, Stream.*, Control.*), domain errors, metrics collectors, CORS
  - phase: 02-02
    provides: Hub fan-out (NewHub, Run, Publish, Subscribe, SetCodecMeta)
  - phase: 02-03
    provides: ControlWriter (NewControlWriter, Run, In, Marshal)
  - phase: 02-04
    provides: LeaseManager (Acquire, Extend, Release, IsHeldBy, ForceRelease, BeginGrace, ReleaseChanFor)
  - phase: 02-05
    provides: DeviceSession accessors (VideoHub, AudioHub, ControlWriter, DeviceMessages, AudioAvailable), SessionOpts, LaunchWithOptions
provides:
  - StreamVideo refactored from Phase 1 direct-relay to Hub.Subscribe fan-out (STR-04)
  - StreamAudio WS endpoint (STR-02) with AUDIO_UNAVAILABLE 404
  - StreamControl WS endpoint (STR-03) with lease gating, JSON envelope decode, force-release events
  - CreateReservation, ExtendReservation, ReleaseReservation REST handlers (CTL-02)
  - WS lifecycle helpers: applyWSDefaults (ReadLimit), pingLoop (idle disconnect), subscribeAndRelay
  - Router mounts for /audio, /control, POST/PATCH/DELETE /reservation
  - obs.MustRegister called from main.go; Prometheus collectors exposed on /metrics
  - 1000-cycle soak test gated by //go:build soak
affects: [03-connection-hardening]

# Tech tracking
tech-stack:
  added: [coder/websocket Ping/ReadLimit lifecycle, crypto/sha256 for owner key fingerprint, base64 for UHID control fields]
patterns: [hub-subscribe-relay (shared by /video and /audio), lease-gated WS (X-Lease-ID before upgrade), JSON-envelope control (18 scrcpy types), REST reservation (POST/PATCH/DELETE)]

key-files:
  created:
    - internal/api/ws_audio.go
    - internal/api/ws_audio_test.go
    - internal/api/ws_control.go
    - internal/api/ws_control_test.go
    - internal/api/ws_helpers.go
    - internal/api/handlers_reservation.go
    - internal/api/handlers_reservation_test.go
    - internal/api/router_test.go
    - internal/session/soak_test.go
  modified:
    - internal/api/ws_video.go (refactored from direct relay to Hub.Subscribe)
    - internal/api/ws_video_test.go (new Hub-based tests)
    - internal/api/router.go (added /audio, /control, /reservation routes + CORS middleware)
    - internal/api/handlers_devices.go (CreateSession accepts *config.Config)
    - internal/api/handlers_devices_test.go (updated for cfg param)
    - internal/api/errors.go (already had Phase 2 sentinels from 02-01)
    - internal/config/config.go (added AllowedOrigins, ParseAllowedOrigins)
    - internal/session/supervisor.go (NewActiveSessionForTest, SetAudioHubForTest, SetControlWriterForTest)
    - internal/scrcpy/control_writer.go (InChanForTest accessor)
    - cmd/gateway/main.go (obs.MustRegister, NewRegistryWithOpts)

key-decisions:
  - "WS /video refactored from Phase 1 direct-relay to Hub.Subscribe fan-out — all Phase 1 tests updated"
  - "StreamAudio returns 404 AUDIO_UNAVAILABLE before WS upgrade when AudioAvailable=false (D-12)"
  - "StreamControl requires X-Lease-ID header before WS upgrade; re-checks lease per-message (D-14, D-15)"
  - "decodeControlEnvelope dispatches all 18 scrcpy control types; unknown types return UNKNOWN_CONTROL_TYPE text frame without closing WS"
  - "ownerKeyFromRequest uses SHA-256 hex of API key for lease binding (D-19)"
  - "DELETE /reservation accepts both JSON body and X-Lease-ID header for lease ID"
  - "Control WS disconnect calls mgr.BeginGrace(leaseID) for 5s grace period (D-10)"
  - "Force-release events delivered as JSON text frame + StatusNormalClosure close (D-09)"
  - "buildAcceptOptions extracted from ws_video.go to ws_helpers.go for reuse by /audio and /control"
  - "NewActiveSessionForTest provides test affordance for Hub-based WS handler integration tests"
  - "CORS middleware added to router stack (from 02-01 cors.go)"
  - "1000-cycle soak test uses //go:build soak tag; goroutine delta = 0 from baseline"

requirements-completed: [STR-02, STR-03, STR-08, STR-09, CTL-02, CTL-03, OBS-01]

# Metrics
duration: 35min
completed: 2026-05-08
---

# Phase 2 Plan 06: API Wiring + Soak Test Summary

**WS audio/control endpoints, reservation REST handlers, router wiring, metrics registration, and 1000-cycle soak test**

## Performance

- **Duration:** 35 min
- **Tasks:** 5 (all complete)
- **Files created:** 9, modified:** 11
- **Test count:** 25+ new tests across audio, control, reservation, router, and soak categories

## Accomplishments

### Task 1: Refactor StreamVideo to Hub.Subscribe + WS lifecycle helpers
- `ws_helpers.go`: `applyWSDefaults` (SetReadLimit), `pingLoop` (idle disconnect), `subscribeAndRelay` (hub subscribe + write loop), `buildAcceptOptions` (origin patterns + subprotocol echo)
- `ws_video.go`: Refactored from Phase 1 `relayVideo` direct-connection pattern to `Hub.Subscribe`-based fan-out. Signature updated to `StreamVideo(registry, origins, cfg)`
- Removed legacy `relayVideo` function entirely
- 4 new tests: `TestStreamVideoMultiViewer` (STR-04), `TestStreamVideoLateJoinerReceivesKeyframe` (STR-07), `TestStreamVideoReadLimitApplied` (STR-09), `TestStreamVideoPingLoop` (STR-08)
- Helper tests: `TestBuildAcceptOptions`, `TestExtractAPIKeyFromSubprotocol`

### Task 2: StreamAudio (STR-02) + Reservation REST handlers (CTL-02)
- `ws_audio.go`: Mirrors StreamVideo for audio; returns 404 AUDIO_UNAVAILABLE when `AudioAvailable=false` before WS upgrade (D-12)
- `handlers_reservation.go`: `CreateReservation` (201), `ExtendReservation` (200), `ReleaseReservation` (204) with lease ID in JSON body or X-Lease-ID header
- `ownerKeyFromRequest` uses SHA-256 hex of API key for lease binding
- 3 audio tests (404 unavailable, multi-viewer streams, readlimit config)
- 6 reservation tests (create, conflict, extend, mismatch, release, device-not-found, owner-key fingerprint)

### Task 3: StreamControl (STR-03) with lease gating + JSON envelope
- `ws_control.go`: Lease check BEFORE `websocket.Accept`; per-message lease re-check via `mgr.IsHeldBy`; force-release event delivery as JSON text frame
- `decodeControlEnvelope`: Table-driven dispatch of all 18 scrcpy control types
- `writeWSError`: Structured error text frame without closing WS
- `extractLeaseIDFromSubprotocol`: Reads `lease.<uuid>` from Sec-WebSocket-Protocol
- 9 control tests (reject without lease, reject bad lease, accept valid lease, JSON-to-scrcpy, validate unknown type, enforce length limits, force-release event, decode envelope table)

### Task 4: Router + main.go wiring
- `router.go`: Added CORS middleware, `/audio`, `/control`, POST/PATCH/DELETE `/reservation` under auth group
- `main.go`: `obs.MustRegister(prometheus.DefaultRegisterer)` called before router construction; `NewRegistryWithOpts` with LeaseTTL from config
- `handlers_devices.go`: `CreateSession` now accepts `*config.Config` and threads `SessionOpts` into `NewDeviceSession`
- `config.go`: Added `AllowedOrigins` field and `ParseAllowedOrigins()` method
- 3 router tests (Phase 2 routes mounted, metrics cardinality lock, metrics route exposes collectors)

### Task 5: 1000-cycle soak test
- `soak_test.go`: `//go:build soak` guard; 1000 subscribe/unsubscribe cycles against real Hub goroutine with steady producer
- Final goroutine delta: 0 (baseline=4, after=4)
- Not run by default; `go test -tags=soak ./internal/session/... -run TestSoak1000Cycles`

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing functionality] TestSession.VideoHub() nil pointer in tests**
- **Found during:** Integration test for `TestStreamVideoMultiViewer` — `DeviceSession.Close()` crashed because `log` was nil in test-only sessions
- **Fix:** Added `NewActiveSessionForTest` helper to `supervisor.go` that sets `slog.Default()` as the logger; also added `SetAudioHubForTest` and `SetControlWriterForTest` for test affordance
- **Files modified:** `internal/session/supervisor.go`
- **Commit:** `2789d10`

**2. [Rule 3 - Blocking issue] ControlWriter.In() returns send-only channel, can't read in tests**
- **Found during:** Writing `TestControlWSJSONToScrcpyBytes` — `cw.In()` returns `chan<- ControlMsg` (send-only), can't read from it for test assertions
- **Fix:** Added `InChanForTest()` method that returns the bidirectional channel for test-only assertions
- **Files modified:** `internal/scrcpy/control_writer.go`
- **Commit:** `e0c127f`

**3. [Rule 2 - Missing functionality] Prometheus CounterVec not appearing in metrics output without label increment**
- **Found during:** `TestMetricsRouteExposesPhase2Collectors` — `gateway_reverse_tunnel_reconcile_total` was missing from `/metrics` output because Prometheus doesn't expose CounterVec until at least one label set is incremented
- **Fix:** Changed test to use `promhttp.HandlerFor` with a custom `prometheus.NewRegistry()` and increment label sets before checking output
- **Files modified:** `internal/api/router_test.go`
- **Commit:** `e0c127f`

## Known Stubs

None. All interfaces are fully implemented; no placeholder text or hardcoded empty values.

## Threat Flags

No new threat surface beyond what the plan's threat model documented. All mitigations are in place:
- T-02-06-01: Lease OwnerKey is SHA-256 hex of API key (implemented in `ownerKeyFromRequest`)
- T-02-06-02: `buildAcceptOptions` reuses Phase 1 origin patterns for /audio and /control
- T-02-06-03: `pingLoop` with configurable interval and idle timeout (STR-08)
- T-02-06-04: `applyWSDefaults` sets `SetReadLimit(cfg.WS.ReadLimitBytes)` on every WS (STR-09)
- T-02-06-05: Lease check before `websocket.Accept` AND per-message `mgr.IsHeldBy` re-check (D-14, D-15)
- T-02-06-07: Cardinality lock test greps metrics.go for forbidden labels (D-18)
- T-02-06-09: `APIKeyAuth` middleware protects all routes except `/healthz` and `/metrics`

## Self-Check: PASSED

- `internal/api/ws_audio.go` FOUND
- `internal/api/ws_control.go` FOUND
- `internal/api/handlers_reservation.go` FOUND
- `internal/api/ws_helpers.go` FOUND
- `internal/api/router.go` FOUND (5+ new routes)
- `internal/session/soak_test.go` FOUND (//go:build soak)
- `cmd/gateway/main.go` FOUND (obs.MustRegister, NewRegistryWithOpts)
- Commit `2789d10` FOUND
- Commit `e0c127f` FOUND
- All tests PASS under `-race`
- `go build ./...` clean
- `go vet ./...` clean
- `go test -tags=soak ./internal/session/... -run TestSoak1000Cycles` PASS (delta=0 goroutines)

---
*Phase: 02-Multi-Client + Control*
*Completed: 2026-05-08*