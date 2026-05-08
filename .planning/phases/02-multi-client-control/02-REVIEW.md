---
phase: 02-multi-client-control
reviewed: 2026-05-08T12:00:00Z
depth: standard
files_reviewed: 37
files_reviewed_list:
  - internal/scrcpy/audio_reader.go
  - internal/scrcpy/audio_reader_test.go
  - internal/scrcpy/device_message.go
  - internal/scrcpy/device_message_test.go
  - internal/scrcpy/launcher.go
  - internal/scrcpy/launcher_test.go
  - internal/scrcpy/control_writer.go
  - internal/scrcpy/control_writer_test.go
  - internal/session/registry.go
  - internal/session/supervisor.go
  - internal/session/supervisor_test.go
  - internal/session/hub.go
  - internal/session/hub_test.go
  - internal/session/lease.go
  - internal/session/lease_test.go
  - internal/session/soak_test.go
  - internal/api/ws_audio.go
  - internal/api/ws_audio_test.go
  - internal/api/ws_control.go
  - internal/api/ws_control_test.go
  - internal/api/ws_helpers.go
  - internal/api/ws_video.go
  - internal/api/ws_video_test.go
  - internal/api/handlers_reservation.go
  - internal/api/handlers_reservation_test.go
  - internal/api/router.go
  - internal/api/router_test.go
  - internal/api/errors.go
  - internal/api/errors_phase2_test.go
  - internal/api/cors.go
  - internal/api/handlers_devices.go
  - internal/api/handlers_devices_test.go
  - internal/config/config.go
  - internal/config/config_phase2_test.go
  - internal/obs/metrics.go
  - internal/obs/metrics_test.go
  - cmd/gateway/main.go
findings:
  critical: 2
  warning: 8
  info: 5
  total: 15
status: issues_found
---

# Phase 2: Code Review Report

**Reviewed:** 2026-05-08T12:00:00Z
**Depth:** standard
**Files Reviewed:** 37
**Status:** issues_found

## Summary

Reviewed all 37 source files from Phase 2 (Multi-Client + Control). The codebase is generally well-structured with thoughtful concurrency patterns (Hub single-writer discipline, errgroup lifecycle, constant-time lease comparison). However, I found 2 critical issues and 8 warnings across concurrency correctness, resource leakage, and security areas. The most significant findings are: (1) a data race between Hub.Subscribe and Hub.Run where late-joiner preloading can write to a viewer's send channel before the viewer is visible to the fan-out loop, risking duplicate delivery; (2) a goroutine leak in acceptWithTimeout where the Accept goroutine is never cancelled if the context times out first.

## Critical Issues

### CR-01: Data race between Hub.Subscribe preloading and Hub.Run fan-out

**File:** `internal/session/hub.go:139-175`
**Issue:** `Subscribe` preloads metadata and keyframe into the viewer's send channel *before* adding the viewer to `h.viewers` under the write lock. Meanwhile, `Hub.Run` reads `h.viewers` under a read lock and fans out frames. Consider this sequence:

1. Subscribe creates viewer channel (capacity N=60)
2. Subscribe writes metadata (12 bytes) into the channel
3. Subscribe writes keyframe bytes into the channel
4. A concurrent Publish+Run cycle picks up a frame from `h.in`
5. Run snapshots `h.viewers` -- the new viewer is NOT yet in the map
6. Run fan-out writes to all existing viewers
7. Subscribe adds the new viewer to `h.viewers`
8. Next Run iteration fan-out writes to the new viewer

The preload is designed so late joiners get metadata+keyframe before live frames. However, there is a subtle race: between steps 5-6 and 7, the Hub goroutine could process a keyframe and update `h.keyframeCache` (via `Store`). If the Subscribe goroutine loads the keyframe in step 3 *before* Run processes it, the viewer gets the correct preloaded keyframe. But if Run processes the keyframe between step 2 and step 3, the viewer gets the old keyframe from the atomic load in step 3 (since `keyframeCache.Store` hasn't happened yet from Subscribe's perspective), and then Run also fans out the new keyframe to this viewer once it's in the map at step 7. This results in the viewer receiving *two* keyframes in sequence (the stale one from preload + the new one from fan-out), which contradicts the STR-07 ordering guarantee (metadata, last keyframe, live tail).

The root cause is that `keyframeCache` and `metaCache` are read in `Subscribe` without the write lock that `Run` uses to protect the viewers map, creating a TOCTOU window.

**Fix:** Hold the write lock during the entire Subscribe operation (preload + map insert), and have `Run` also acquire the write lock when updating `keyframeCache`. Alternatively, use an in-band subscription message: instead of preloading in Subscribe, send the metadata and keyframe as special messages through `h.in` so they are processed in the same single goroutine that runs fan-out, eliminating the race entirely.

```go
// Option A: Hold write lock during entire Subscribe (simpler but blocks Run)
func (h *Hub) Subscribe(viewerID string) (<-chan []byte, func(), error) {
    v := &viewer{
        id:   viewerID,
        send: make(chan []byte, h.bufFrames),
    }
    h.mu.Lock()
    // Preload under the same lock that Run uses for fan-out
    if meta := h.metaCache.Load(); meta != nil {
        metaCopy := make([]byte, 12)
        copy(metaCopy, meta[:])
        v.send <- metaCopy
    }
    if kf := h.keyframeCache.Load(); kf != nil {
        v.send <- kf.wireBytes()
    }
    h.viewers[viewerID] = v
    h.mu.Unlock()
    // ...
}
```

### CR-02: Goroutine leak in acceptWithTimeout when context cancels before Accept returns

**File:** `internal/scrcpy/launcher.go:279-302`
**Issue:** The `acceptWithTimeout` function spawns a goroutine to call `ln.Accept()` and waits on a select between the accept result channel and context cancellation. If the context cancels (times out), the function returns an error, but the goroutine calling `ln.Accept()` is left running forever. `net.Listener.Accept()` blocks until either a connection arrives or the listener is closed. Since the caller (`LaunchWithOptions`) does not close the listener on the timeout path (cleanupOnFailure closes it, but that happens later after the error is returned), the goroutine is orphaned and will leak until the listener is eventually closed by the cleanup function, which may not happen if the caller doesn't reach cleanup.

In practice, `LaunchWithOptions` does call `cleanupOnFailure()` on error, which closes the listener. But there is a window: the `acceptCh` goroutine calls `ln.Accept()` and blocks. After `acceptWithTimeout` returns, `cleanupOnFailure` closes the listener, which should unblock `Accept()` and cause the goroutine to exit. However, the goroutine writes to `acceptCh` (an unbuffered channel) -- since no one reads from `acceptCh` after the timeout, this goroutine will also block on `acceptCh <- conn` even after Accept returns. If a connection arrives just before cleanup closes the listener, the goroutine blocks forever on the unbuffered channel write.

**Fix:** Use a buffered channel of size 1 for `acceptCh` so the goroutine can complete its write even after the select has chosen the context cancellation path. This ensures the goroutine exits cleanly when the listener is closed.

```go
func acceptWithTimeout(ctx context.Context, ln net.Listener, name string) (net.Conn, error) {
    acceptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    acceptCh := make(chan net.Conn, 1)  // buffered to prevent goroutine leak
    acceptErr := make(chan error, 1)
    go func() {
        conn, err := ln.Accept()
        if err != nil {
            acceptErr <- err
            return
        }
        acceptCh <- conn
    }()
    // ...
}
```

## Warnings

### WR-01: Hub.Subscribe viewer ID collision silently overwrites previous viewer

**File:** `internal/session/hub.go:157-159`
**Issue:** If `Subscribe` is called twice with the same `viewerID`, the second call overwrites the first viewer in the map. The first viewer's channel is never closed, and any goroutine reading from it will never receive an eviction signal. This can happen if a WebSocket reconnects rapidly and the old unsubscribe hasn't completed yet.

**Fix:** Detect duplicate IDs and either return an error or generate a unique suffix:
```go
h.mu.Lock()
if _, exists := h.viewers[viewerID]; exists {
    h.mu.Unlock()
    return nil, nil, fmt.Errorf("hub: viewer %s already subscribed", viewerID)
}
h.viewers[viewerID] = v
h.mu.Unlock()
```

### WR-02: Hub.evict modifies viewers map while Run holds a snapshot, allowing re-registration race

**File:** `internal/session/hub.go:227-243`
**Issue:** `evict` acquires the write lock and deletes the viewer from `h.viewers`, then closes the channel. If a new `Subscribe` call with the same viewer ID arrives between `evict` deleting from the map and the `Run` loop finishing its iteration over the snapshot, the new viewer could be added and then immediately receive a frame from the current fan-out iteration (which still has the old viewer pointer in its snapshot). This is benign since the snapshot has the old (evicted) pointer and the `v.evicted` check will skip it. However, the real issue is that `evict` closes `v.send` while `Run`'s fan-out loop still has a reference to `v` in its snapshot. The `evicted` flag prevents a send to a closed channel, so this is correctly guarded. No data race, but the pattern is fragile.

**Fix:** No immediate fix needed, but document that `v.evicted` is the critical guard that prevents sends to closed channels, and that it must only be set by the Hub goroutine (which it is, since evict is only called from Run's goroutine).

### WR-03: DeviceSession.cleanupResources called twice on shutdown -- double-close risk

**File:** `internal/session/supervisor.go:212-224`
**Issue:** `cleanupResources` is called from `Run`'s closer goroutine (when context is cancelled) and also from `Close` (which transitions to StateStopping and then calls `cleanupResources`). The closer goroutine in `Run` (line 241-245) calls `s.cleanupResources()` on context cancellation. Meanwhile, `Close` (line 166-185) also calls `s.cleanupResources()`. If both paths are triggered, `cleanupResources` runs twice. Inside `cleanupResources`, it checks `if s.cleanup != nil` and then sets fields to nil under the mutex. The nil check prevents double-close of the cleanup function. However, the closer goroutine also closes connections that are captured as local variables (`audioConn`, `controlConn`). The `s.videoConn.Close()` etc. calls inside `cleanupResources` are protected by the nil check, but `net.Conn.Close()` is documented to be idempotent for `net.Pipe` connections but NOT guaranteed for all `net.Conn` implementations. Double-closing a TCP connection is generally safe but could log warnings.

**Fix:** Use `sync.Once` for cleanup to guarantee it runs exactly once:
```go
type DeviceSession struct {
    // ...
    cleanupOnce sync.Once
}

func (s *DeviceSession) cleanupResources() {
    s.cleanupOnce.Do(func() {
        if s.cleanup != nil {
            s.cleanup()
            s.mu.Lock()
            s.cleanup = nil
            // ... nil out fields
            s.mu.Unlock()
        }
    })
}
```

### WR-04: LeaseManager.Extend does not re-validate that the lease is not expired before resetting

**File:** `internal/session/lease.go:143-165`
**Issue:** `Extend` checks that `leaseID` matches the current lease, but does not verify that the lease has not already expired. If the TTL timer has fired but the `expireFromTimer` callback hasn't acquired the mutex yet, `Extend` can successfully reset the timer on what is effectively an already-expired lease. The timer callback will then find the ID doesn't match (because Extend created a new timer with the same ID string) and be a no-op, leaving the lease alive past its intended expiry. This is a TOCTOU race between the timer goroutine and Extend.

**Fix:** Add an expiry check in `Extend` to verify the lease is still valid:
```go
func (m *LeaseManager) Extend(leaseID string) (Lease, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.cur == nil {
        return Lease{}, ErrLeaseNotFound
    }
    if !ctEqual(m.cur.snapshot.ID, leaseID) {
        return Lease{}, ErrLeaseMismatch
    }
    // Reject if the lease has already expired (timer hasn't fired yet)
    now := time.Now()
    if !m.cur.graceUntil.IsZero() && now.After(m.cur.graceUntil) {
        return Lease{}, ErrLeaseNotFound // lease effectively expired
    }
    if m.cur.graceUntil.IsZero() && now.After(m.cur.snapshot.ExpiresAt) {
        return Lease{}, ErrLeaseNotFound // lease expired
    }
    // ... proceed with extend
}
```

### WR-05: CORS middleware reflects any Origin that matches the allowlist, enabling credential theft in misconfigured deployments

**File:** `internal/api/cors.go:22-26`
**Issue:** The CORS middleware does `Access-Control-Allow-Origin: <origin>` (echoing back the specific origin) without also setting `Access-Control-Allow-Credentials: true`. If the deployment later adds credential support (cookies, auth headers), the browser will NOT send credentials because `Allow-Credentials` is not set. However, the current reflection pattern is correct for non-credentialed requests. The real concern is that `Access-Control-Allow-Headers` does not include `X-Lease-ID`, which means browser clients making control WebSocket upgrades via fetch/XHR preflight with `X-Lease-ID` will have their preflight rejected. This is currently handled by the `lease.<id>` subprotocol path for browser clients, so it may be intentional. But `Access-Control-Allow-Methods` only lists `GET, POST, DELETE, OPTIONS` -- missing `PATCH`, which is needed for the `ExtendReservation` endpoint.

**Fix:** Add `PATCH` to the allowed methods in the CORS middleware:
```go
w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-Lease-ID")
```

### WR-06: ControlWriter.Run does not drain the in-channel on context cancellation -- dropped messages

**File:** `internal/scrcpy/control_writer.go:411-441`
**Issue:** When the context is cancelled, `Run` returns `ctx.Err()` immediately. Any messages remaining in the `in` channel are silently dropped. For a control connection, this means pending key events or touch events in the buffer are lost. This is acceptable for most input events (stale clicks), but the `CloseNow` on the WebSocket side in `StreamControl` happens after `Run` exits, so there's no graceful drain.

This is a design decision rather than a bug, but it should be documented. If graceful shutdown of the control channel is ever needed (e.g., sending a "release all keys" message), the channel should be drained before exit.

**Fix:** Document the behavior or add a drain loop:
```go
// Optional: drain remaining messages on cancellation
case <-ctx.Done():
    // Drain remaining messages before exit
    for {
        select {
        case msg, ok := <-cw.in:
            if !ok {
                return ctx.Err()
            }
            wire, err := Marshal(msg)
            if err != nil {
                continue
            }
            cw.conn.Write(wire) // best-effort
        default:
            return ctx.Err()
        }
    }
```

### WR-07: StreamControl force-release goroutine can write to closed WebSocket

**File:** `internal/api/ws_control.go:96-115`
**Issue:** The `forceReleaseDone` goroutine calls `ws.Write` and `ws.Close` from a separate goroutine. While `coder/websocket` documents that a single goroutine should perform writes, the `ws.Write` in the force-release goroutine (line 110) and the `ws.Read` in the main loop (line 122) could both be active simultaneously. The `coder/websocket` library does support concurrent reads and writes from different goroutines (one reader, one writer), but having *two* goroutines that can write (the force-release goroutine and the ping error path from `subscribeAndRelay`) creates a race.

In `StreamControl`, the main loop does `ws.Read`, and the force-release goroutine does `ws.Write`. The ping loop goroutine (started at line 119) also calls `ws.Ping`, which is a write. So there are potentially two concurrent writers: the force-release goroutine doing `ws.Write` + `ws.Close`, and the ping goroutine doing `ws.Ping`. After the `ws.Close` call, the ping goroutine's next `ws.Ping` will fail, but there is a window where both write concurrently.

**Fix:** Serialize all WebSocket writes through a single goroutine or use a mutex around ws.Write/ws.Close/ws.Ping:
```go
// Use a mutex to protect all ws write operations
var wsMu sync.Mutex
// Wrap ws.Write, ws.Close, and ws.Ping with wsMu.Lock/Unlock
```

### WR-08: DeviceSession.Run captures conn pointers before starting goroutines, but closer goroutine also uses cleanupResources which nils them

**File:** `internal/session/supervisor.go:230-282`
**Issue:** `Run` captures `audioConn` and `controlConn` into local variables before starting goroutines (lines 237-238), which is correct for avoiding data races with `cleanupResources`. However, `s.videoConn` is accessed via `s.VideoConn()` accessor method in `videoReaderLoop` (line 287), which acquires the mutex. This means `videoReaderLoop` could read a nil `videoConn` if the closer goroutine calls `cleanupResources` and sets `s.videoConn = nil` before `videoReaderLoop` starts reading. The `s.VideoConn()` method acquires the lock and returns the field value, but between the return and the actual Read call, the closer goroutine could close the connection. This is a classic TOCTOU issue.

The `audioReaderLoop` and `deviceMessageReaderLoop` methods correctly receive the conn as a parameter, avoiding this issue.

**Fix:** Capture `videoConn` in a local variable inside `Run` and pass it as a parameter to `videoReaderLoop`, consistent with how audioConn and controlConn are handled:
```go
videoConn := s.videoConn
g.Go(func() error { return s.videoReaderLoop(ctx, videoConn) })

func (s *DeviceSession) videoReaderLoop(ctx context.Context, conn net.Conn) error {
    // use conn parameter instead of s.VideoConn()
}
```

## Info

### IN-01: Hub.Publish drops frames silently when the input channel is full

**File:** `internal/session/hub.go:121-129`
**Issue:** When `Publish` returns `false` (input channel full), the caller (`videoReaderLoop`, `audioReaderLoop`) does not log or handle the drop. The frame is silently discarded. This is by design (drop-on-slow), but there is no metric or log to correlate producer-side drops with consumer-side drops. The `FramesDroppedTotal` metric is incremented in `Publish`, which is good, but the callers don't check the return value.

**Fix:** Consider logging at debug level when `Publish` returns false, or document that the return value is intentionally ignored because producer-side drops are already accounted for by the metric.

### IN-02: config.yaml and test/ directory are tracked but not reviewed

**Issue:** The git status shows `config.yaml` and `test/` as untracked files. `config.yaml` could contain secrets (API keys) and should be in `.gitignore`. The `test/` directory was not in the review scope.

**Fix:** Add `config.yaml` to `.gitignore` to prevent accidental secret leakage.

### IN-03: Soak test uses `//go:build soak` tag, making it invisible to normal test runs

**File:** `internal/session/soak_test.go:1`
**Issue:** The soak test is correctly gated behind a build tag, which means `go test ./...` will not run it by default. This is appropriate for a long-running test but worth noting for CI configuration.

**Fix:** Document how to run soak tests in the project's CI or contribution guide.

### IN-04: DeviceMessage UHID size limits use uint16 max (65535) but clipboard uses 262144 (256KB)

**File:** `internal/scrcpy/device_message.go:58,88`
**Issue:** The clipboard payload limit is 262144 bytes (256KB), which seems reasonable. The UHID data limit is uint16 max (65535 bytes), which matches the wire format. However, there is no size limit on the UHID name field at the parsing level (only at the wire level it's 1 byte = max 255). These are protocol-mandated limits, so this is informational only.

**Fix:** No fix needed; these limits match the scrcpy protocol specification.

### IN-05: fingerprint function truncates API key for logging but could be more conservative

**File:** `internal/session/lease.go:316-323`
**Issue:** The `fingerprint` function returns the first 8 characters of the input for keys longer than 8 characters. For SHA-256 hex strings (64 chars), 8 hex chars is 32 bits of the hash -- enough to be a unique identifier in small deployments but could theoretically be brute-forced to correlate with known API keys. The comment acknowledges this is a "short non-reversible identifier."

**Fix:** Consider reducing to 4 hex characters (16 bits) for the fingerprint, which is sufficient for log correlation without providing meaningful correlation attack surface. Or use a separate HMAC for fingerprinting.

---

_Reviewed: 2026-05-08T12:00:00Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_