---
status: complete
phase: 01-single-device-streaming-foundation
source: 01-01-SUMMARY.md, 01-02-SUMMARY.md, 01-03-SUMMARY.md, 01-04-SUMMARY.md, 01-05-SUMMARY.md, 01-06-SUMMARY.md
started: 2026-05-07T12:30:00Z
updated: 2026-05-07T13:45:00Z
---

## Current Test

[testing complete]

## Tests

### 1. Cold Start Smoke Test
expected: Kill any running gateway. Set ADB_GW_API_KEY_PRIMARY=testkey and run `go run ./cmd/gateway/`. Server boots without errors. Startup log records version, scrcpy_version, build_sha, listen_addr, adb_addr, log_level. API keys do not appear in any log line.
result: pass
note: build_sha shows "unknown" when running via `go run` — expected, ldflags set it at build time

### 2. Healthz Endpoint
expected: While gateway is running, `curl http://localhost:8080/healthz` returns HTTP 200 with JSON body containing `status`, `version`, `build_sha`, and `scrcpy_version` fields. No API key required.
result: pass
note: build_sha returns "unknown" in dev mode, works as designed

### 3. Auth Blocks Unauthenticated Requests
expected: `curl http://localhost:8080/devices` (no API key header) returns HTTP 401 with JSON body `{"error":{"code":"UNAUTHORIZED","message":"Invalid or missing API key"}}`.
result: pass

### 4. Auth Allows Authenticated Requests
expected: `curl -H 'X-API-Key: testkey' http://localhost:8080/devices` returns HTTP 200 with a JSON array (empty if no devices connected, or list of devices if ADB is running with devices attached).
result: pass

### 5. Auth Query Parameter Fallback
expected: `curl 'http://localhost:8080/devices?api_key=testkey'` returns HTTP 200 same as header-based auth.
result: pass

### 6. Device List with Connected Device
expected: With a USB-attached Android device (USB debugging enabled), `GET /devices` with auth returns a JSON array containing the device serial and state (e.g. `[{"serial":"ABCD1234","state":"device"}]`).
result: pass

### 7. Create Session
expected: `curl -X POST -H 'X-API-Key: testkey' http://localhost:8080/devices/{serial}/sessions` returns HTTP 201 with JSON body containing `session_id`. Gateway logs show the full launch sequence (push jar, reverse forward, launch app_process, accept, codec meta). Session state becomes "active".
result: pass

### 8. Idempotent Session Creation
expected: Calling `POST /devices/{serial}/sessions` again while session is active returns HTTP 200 (not 201) with the same session ID. No duplicate session is created.
result: pass

### 9. WebSocket Video Stream
expected: Connect a WebSocket client to `ws://localhost:8080/devices/{serial}/video` with `X-API-Key: testkey` header. First message is a 12-byte binary codec metadata packet. Subsequent messages are H.264 video frames (12-byte header + payload). Stream continues until client disconnects.
result: pass

### 10. WebSocket Auth Required
expected: Connecting to `ws://localhost:8080/devices/{serial}/video` without an API key returns HTTP 401 before WebSocket upgrade completes.
result: pass

### 11. Delete Session
expected: `curl -X DELETE -H 'X-API-Key: testkey' http://localhost:8080/devices/{serial}/sessions/{session_id}` returns HTTP 204. Device session closes, resources are cleaned up, and the device returns to "idle" state.
result: pass

### 12. ADB Reconnection
expected: While a session is active, kill the ADB server (`adb kill-server` or kill the adb process). Gateway detects disconnection, reconnects with exponential backoff, re-issues reverse forwards, and resumes streaming within 10 seconds — without restarting the gateway process.
result: pass
note: Gap closure fix verified via human UAT — gateway reconnects, re-issues reverse forwards, resumes session

### 13. Graceful Shutdown
expected: Send SIGTERM to the running gateway process. Gateway drains active sessions within 30 seconds, removes reverse:forward mappings, and exits cleanly. No stale reverse mappings or orphan processes remain on the device.
result: pass

### 14. Startup Reconciliation
expected: After a `kill -9` of the gateway (dirty shutdown), restart the gateway. It detects and kills orphan `app_process` instances matching `scrcpy-server-gateway.jar` on devices, removes stale `localabstract:scrcpy_*` reverse forwards, and starts cleanly.
result: pass

### 15. Version and Licenses Flags
expected: Run `./gateway --version` and see version + scrcpy version output. Run `./gateway --licenses` and see the THIRD_PARTY_NOTICES content (Apache-2.0 for scrcpy v3.3.4 plus Go dependency attributions).
result: pass
note: build_sha shows "unknown" in dev builds — expected, set via ldflags at release build time

### 16. Systemd Unit Deployment
expected: Install `deploy/adb-gateway.service` on a Linux host with systemd, configure environment, run `systemctl start adb-gateway`. Service starts, healthz returns 200. `systemctl stop adb-gateway` drains within 30 seconds.
result: pass

### 17. Secondary API Key Auth
expected: Configure `ADB_GW_API_KEY_SECONDARY=secondarykey`. Requests with `X-API-Key: secondarykey` return 200 just like the primary key. Enables key rotation without downtime.
result: pass

## Summary

total: 17
passed: 17
issues: 0
pending: 0
skipped: 0
blocked: 0

## Gaps

- truth: "After adb kill-server, gateway reconnects to ADB, re-issues reverse forwards, and resumes the active session within 10 seconds without restarting"
  status: resolved
  reason: "User reported: device list still shows device after adb kill-server (stale data), and creating a new session fails with error. Gateway does not recover cleanly."
  resolution: "Gap closure fix (01-07, 01-08): WatchDevices restart, MarkAllDisconnected, ADBWatchdog, ReissueReverseForwards wired in main.go lifecycle loop. Verified via human UAT on 2026-05-07."
  severity: major
  test: 12
  root_cause: |
    Three interrelated bugs in the ADB reconnection path:

    1. **WatchDevices goroutine dies silently when ADB connection drops.** `hostServices.NewDeviceWatcher()` creates a goadb DeviceWatcher at startup. When the ADB server is killed, goadb's watcher channel closes. The WatchDevices goroutine (registry.go:130-162) exits on `!ok` (line 136-138), but nothing restarts it. The registry retains stale device entries.

    2. **No stale device cleanup on ADB disconnect.** When WatchDevices stops, the registry still holds entries for devices that are no longer reachable. `GET /devices` returns stale data. There is no mechanism to mark devices as disconnected when ADB dies.

    3. **Reconnector exists but is never called after initial setup.** `reconnect.go` implements `AwaitADBReady` and `ReissueReverseForwards`, but `main.go` only calls `AwaitADBReady` once at startup. There is no runtime loop that detects ADB connection loss, reconnects, restarts the device watcher, or re-issues reverse forwards for active sessions.

    The root cause is architectural: the ADB connection lifecycle is treated as static (connect once at startup) rather than dynamic (ADB server can restart at any time).
  artifacts:
    - path: "internal/session/registry.go"
      issue: "WatchDevices exits when goadb watcher channel closes (line 136-138), no restart mechanism"
    - path: "internal/adb/host_services.go"
      issue: "NewDeviceWatcher creates a goadb DeviceWatcher that dies when ADB connection drops, no way to restart it"
    - path: "cmd/gateway/main.go"
      issue: "No ADB reconnect loop after initial startup; Reconnector.AwaitADBReady called once and never again"
  missing:
    - "Add ADB disconnect detection — watch for the DeviceWatcher channel closing or ADB call failures"
    - "On disconnect: mark all tracked devices as disconnected/unknown, close active sessions, clear stale registry entries"
    - "On reconnect: restart device watcher, re-populate registry from fresh track-devices, re-issue reverse forwards for any sessions that were active"
    - "Wire the reconnection loop into main.go as a persistent goroutine rather than a one-shot startup call"