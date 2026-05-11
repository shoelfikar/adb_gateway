---
status: complete
phase: 02-multi-client-control
source:
  - 02-01-SUMMARY.md
  - 02-02-SUMMARY.md
  - 02-03-SUMMARY.md
  - 02-04-SUMMARY.md
  - 02-05-SUMMARY.md
  - 02-06-SUMMARY.md
started: 2026-05-11T00:00:00Z
updated: 2026-05-11T09:37:22Z
---

## Current Test
<!-- OVERWRITE each test - shows where we are -->

[verification complete — all gaps closed]

## Tests

### 1. Cold Start Smoke Test
expected: Kill any running gateway process. Start the application from scratch. The server boots without errors, /healthz returns 200, and /metrics returns Prometheus text with at least the Phase 2 collectors (gateway_reverse_tunnel_reconcile_total, gateway_lease_*, gateway_hub_*).
result: pass

### 2. CORS Middleware Applied
expected: Sending a request with an Origin header from an allowed origin to any API route returns Access-Control-Allow-Origin in the response. A disallowed origin gets no ACAO header (or is rejected per config).
result: pass

### 3. POST /devices/{id}/reservation — Create Lease (CTL-02)
expected: With a valid API key and a known device ID, POST creates a lease and returns 201 with a JSON body containing a UUID lease ID and an expiry. A second POST from a different owner returns 409 (lease held).
result: pass

### 4. PATCH /reservation — Extend Lease
expected: PATCH with the holder's lease ID extends the TTL and returns 200 with updated expiry. PATCH with a wrong lease ID returns 4xx (mismatch/not-found).
result: pass

### 5. DELETE /reservation — Release Lease
expected: DELETE with the holder's lease ID (in body or X-Lease-ID header) returns 204 and the device becomes acquirable again by another owner.
result: pass

### 6. WS /video — Multi-Viewer Fan-Out (STR-04)
expected: Two WS clients connect to /devices/{id}/video on the same device. Both receive the H.264 stream (same frames) without one starving the other.
result: pass

### 7. WS /video — Late Joiner Receives Keyframe (STR-07)
expected: Connect viewer A, wait until frames flow. Connect viewer B mid-stream. B immediately receives codec metadata + the cached keyframe before the next live frame, so its decoder can start without waiting for the next IDR.
result: pass
fix: 02-07 (ws.CloseRead + WriteTimeout:0)

### 8. WS /audio — Returns 404 When Audio Unavailable (STR-02 / D-12)
expected: For a device whose AudioAvailable=false, connecting to /devices/{id}/audio returns HTTP 404 with body/code AUDIO_UNAVAILABLE BEFORE the WS upgrade. No partial WS handshake.
result: pass

### 9. WS /audio — Multi-Viewer Stream
expected: For a device with audio enabled, two WS clients on /audio both receive the audio stream concurrently.
result: pass

### 10. WS /control — Rejects Without Lease (STR-03)
expected: Connecting to /devices/{id}/control without an X-Lease-ID header (or lease.<uuid> subprotocol) is rejected before WS upgrade with a 4xx and an error code (LEASE_REQUIRED or similar).
result: pass

### 11. WS /control — Accepts With Valid Lease, Forwards Control
expected: Holding a valid lease and supplying X-Lease-ID, the WS upgrade succeeds. Sending a JSON envelope control message (e.g., key event) results in the device receiving the corresponding scrcpy control bytes (visible effect on device screen, e.g., a key press).
result: pass

### 12. WS /control — Force-Release Event on Revoke
expected: While a control WS is open, force-releasing the lease (admin or device-gone) delivers a JSON text frame to the client describing the release and then closes the WS with StatusNormalClosure.
result: pass

### 13. WS Lifecycle — Idle Ping/ReadLimit
expected: Idle WS connections receive periodic pings (pingLoop). Sending a frame larger than configured ReadLimitBytes terminates the connection cleanly without crashing the server.
result: pass
fix: 02-07 (ws.CloseRead + corrected ping/pong + ReadLimit tests)

### 14. /metrics Exposes Phase 2 Collectors (OBS-01)
expected: After exercising endpoints (create a lease, stream video), curl /metrics shows Phase 2 collectors — e.g., gateway_lease_acquired_total, gateway_lease_released_total, gateway_ws_frames_sent_total, gateway_hub_viewers_active — with label values populated.
result: pass
fix: 02-08 (4 missing collectors defined, registered, instrumented)

## Summary

total: 14
passed: 14
issues: 0
pending: 0
skipped: 0

## Gaps

- truth: "Late joiner receives codec metadata + cached keyframe immediately; video renders without waiting for next IDR"
  status: closed
  reason: "User reported: right now, video not show when i try to stream — WebSocket error: code=1006 reason="
  severity: blocker
  test: 7
  root_cause: "Two compounding bugs: (1) subscribeAndRelay() never calls ws.CloseRead() — write-only WS handlers (/video, /audio) never read from the connection, so pongs and close frames sit unread, causing Ping() to timeout and connections to drop with code 1006 after ~75-90s. (2) http.Server WriteTimeout/ReadTimeout persist after WebSocket hijack — deadlines remain on net.Conn after Hijack(), causing writes to fail after 65s."
  artifacts:
    - path: "internal/api/ws_helpers.go"
      issue: "Missing ws.CloseRead(ctx) call in subscribeAndRelay()"
    - path: "cmd/gateway/main.go"
      issue: "WriteTimeout/ReadTimeout persist after WebSocket hijack"
    - path: "internal/api/ws_video.go"
      issue: "Uses subscribeAndRelay() — inherits missing CloseRead bug"
    - path: "internal/api/ws_audio.go"
      issue: "Uses subscribeAndRelay() — inherits missing CloseRead bug"
  missing:
    - "Add ws.CloseRead(ctx) in subscribeAndRelay() before the relay loop"
    - "Clear HTTP server deadlines after WebSocket upgrade (use ReadHeaderTimeout instead of ReadTimeout, set WriteTimeout: 0)"
  debug_session: .planning/debug/video-stream-code-1006.md

- truth: "Idle WS connections receive periodic pings; oversized frames terminate cleanly without server crash"
  status: closed
  reason: "User reported: no, websocket error"
  severity: major
  test: 13
  root_cause: "Same root cause as gap 1: subscribeAndRelay() lacks ws.CloseRead(). Without a Read() goroutine, (1) Ping() blocks forever waiting for pongs that are never consumed, causing false idle-timeout disconnections after ~75-90s, (2) SetReadLimit() is never enforced because Read() is never called, (3) client close frames are never processed. TestStreamVideoPingLoop and TestStreamVideoReadLimitApplied are false positives."
  artifacts:
    - path: "internal/api/ws_helpers.go"
      issue: "Missing ws.CloseRead(ctx) call — pingLoop blocks on pong that never arrives"
    - path: "internal/api/handlers_logcat.go"
      issue: "StreamLogcat has same pattern — ping loop with no Read path"
    - path: "internal/api/ws_video_test.go"
      issue: "TestStreamVideoPingLoop is false positive — only verifies no crash in 2s, not that pongs work"
    - path: "internal/api/ws_video_test.go"
      issue: "TestStreamVideoReadLimitApplied is false positive — passes on client-side timeout, not server enforcement"
  missing:
    - "Add ws.CloseRead(ctx) in subscribeAndRelay() and StreamLogcat()"
    - "Fix TestStreamVideoPingLoop to verify actual ping/pong cycles"
    - "Fix TestStreamVideoReadLimitApplied to verify server-side ReadLimit enforcement"
  debug_session: .planning/debug/ws-lifecycle-ping-readlimit.md

- truth: "Phase 2 metrics collectors are visible and populated in /metrics response after exercising endpoints"
  status: closed
  reason: "User reported: Phase 2 metrics collectors (gateway_lease_*, gateway_hub_*, etc.) not present in /metrics response"
  severity: major
  test: 14
  root_cause: "The four Phase 2 metric names (gateway_lease_acquired_total, gateway_lease_released_total, gateway_ws_frames_sent_total, gateway_hub_viewers_active) were never implemented. Plan 02-01 defined only six D-18 baseline collectors from Phase 1. No subsequent Phase 2 plan added lease, hub-viewer, or WS-frame metrics. LeaseManager has zero obs calls; Hub has no viewer gauge. The UAT expectation was written against the full OBS-02 requirement, but only the D-18 cardinality-locked subset was shipped."
  artifacts:
    - path: "internal/obs/metrics.go"
      issue: "Only 7 collectors defined; missing 4 Phase 2 collectors"
    - path: "internal/session/lease.go"
      issue: "Zero metric instrumentation for lease acquire/release events"
    - path: "internal/session/hub.go"
      issue: "No viewer count gauge; existing FramesEmittedTotal partially covers WS frame sending but under different name"
  missing:
    - "Define 4 missing collectors in obs/metrics.go (lease_acquired_total, lease_released_total, ws_frames_sent_total, hub_viewers_active)"
    - "Add MustRegister calls for new collectors"
    - "Instrument LeaseManager with acquire/release counters"
    - "Instrument Hub with viewer gauge and WS-frame counter"
  debug_session: .planning/debug/missing-phase2-metrics.md