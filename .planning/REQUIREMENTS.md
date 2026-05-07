# Requirements: ADB Gateway

**Defined:** 2026-05-06
**Core Value:** Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

## v1 Requirements

Requirements for initial release. Each maps to exactly one roadmap phase.

### Foundation

- [ ] **FND-01**: Service starts as a single Go binary, runs under systemd, supports graceful shutdown on SIGTERM
- [ ] **FND-02**: Service loads config (listen addr, API key(s), ADB host:port, log level, scrcpy options) from file + env via `koanf`
- [ ] **FND-03**: Service reports its version, pinned scrcpy version, and build SHA on `--version` and a `/healthz` endpoint
- [ ] **FND-04**: Service emits structured JSON logs via `log/slog` (level configurable)
- [ ] **FND-05**: Service ships a `THIRD_PARTY_NOTICES` file satisfying scrcpy's Apache-2.0 attribution

### ADB Transport

- [x] **ADB-01**: Service connects to a local ADB server on `localhost:5037` (configurable), with reconnect on drop
- [x] **ADB-02**: Service speaks ADB host services (`host:devices`, `host:track-devices`, `host:transport:<serial>`) via the wire protocol
- [x] **ADB-03**: Service implements an in-house `reverse:forward` helper (allocate ephemeral local port, install reverse, tear down on session end)
- [x] **ADB-04**: Service can push files to a device via the ADB sync service (used to push `server.jar`)
- [x] **ADB-05**: Service can run shell commands on a device via `shell:v2` (used to launch `app_process`)
- [ ] **ADB-06**: After any ADB-server reconnect, service re-issues all expected reverse forwards and audits via `reverse:list-forward`
- [x] **ADB-07**: Every ADB call is bounded by a context with timeout; per-device mutex prevents global deadlock from one stuck device
- [ ] **ADB-08**: On startup, service reconciles stale state — enumerates `reverse --list`, removes gateway-owned forwards from prior runs, kills lingering `app_process` instances on each device

### Device Lifecycle

- [x] **DEV-01**: Service tracks connected devices in an in-memory registry, fed by `host:track-devices` long-poll
- [ ] **DEV-02**: REST `GET /devices` returns the current list of devices with serial, status, and (if active) session info
- [ ] **DEV-03**: REST `POST /devices/{serial}/sessions` creates a session for a device (idempotent: returns existing session if active)
- [ ] **DEV-04**: REST `DELETE /devices/{serial}/sessions/{id}` ends a session, tearing down reverse tunnels and stopping `app_process`
- [ ] **DEV-05**: A device session has a state machine (`idle → starting → active → stopping → idle/failed`); transitions are observable via REST and metrics
- [ ] **DEV-06**: Device IDs are stable serials; service never identifies a device by USB path

### scrcpy Integration

- [ ] **SCR-01**: Service vendors a single pinned `server.jar` version, embedded via `//go:embed`; version constant exposed at runtime
- [ ] **SCR-02**: On session start, service pushes `server.jar`, sets up reverse tunnels for video/audio/control, then launches `app_process` with the pinned scrcpy version arg and an SCID
- [ ] **SCR-03**: Service reads scrcpy's video stream (codec meta + 12-byte frame header + payload) using `io.ReadFull` to preserve frame boundaries — server NEVER decodes frames
- [ ] **SCR-04**: Service reads scrcpy's audio stream (when enabled) using the same framing discipline; audio is opt-in in Phase 1, default-on Phase 2+
- [ ] **SCR-05**: Service writes scrcpy's binary control protocol (touch, key, text, scroll, etc.) to the control socket from a single-writer goroutine per device
- [ ] **SCR-06**: Service reads scrcpy's device-message stream (clipboard, ACKs) and exposes events via control WS or metrics
- [ ] **SCR-07**: Session config exposes scrcpy parameters: codec, max_size, bit_rate, max_fps, audio_codec, audio_source

### Streaming API

- [ ] **STR-01**: WebSocket `GET /devices/{serial}/video` streams H.264/H.265 frames (binary messages), with codec metadata sent on first frame
- [ ] **STR-02**: WebSocket `GET /devices/{serial}/audio` streams OPUS/AAC frames (binary)
- [ ] **STR-03**: WebSocket `GET /devices/{serial}/control` accepts client→server control messages (binary, scrcpy format)
- [ ] **STR-04**: Multiple read-only viewers can attach to the same device's video/audio simultaneously (1 controller + N observers)
- [ ] **STR-05**: Per-client send buffer is bounded; on slow client, frames are dropped (counter incremented), not buffered indefinitely
- [ ] **STR-06**: After N consecutive drops, slow client is disconnected with a structured close code
- [ ] **STR-07**: Late-joining viewer receives the cached codec metadata + most recent keyframe before live frames, so a fresh decoder can start
- [ ] **STR-08**: Service sends app-layer pings every 20–30s on every WS connection; idle-disconnect after configurable timeout
- [ ] **STR-09**: WS `SetReadLimit` is configured to ≥4 MiB to accommodate scrcpy frame sizes

### Control & Reservation

- [ ] **CTL-01**: Only one client at a time may hold the controller role on a device; others are observers
- [ ] **CTL-02**: REST `POST /devices/{serial}/reservation` acquires a TTL lease (default 60s); REST `DELETE` releases; `PATCH` extends
- [ ] **CTL-03**: Reservation must be held for control input to be accepted on the control WS (controller role is gated by lease)
- [ ] **CTL-04**: Reservation auto-releases when its TTL expires; observers may then claim
- [ ] **CTL-05**: Forced release (admin endpoint or device disappearance) emits an event the lease holder can observe

### Auth

- [ ] **AUTH-01**: All REST and WS endpoints require an API key; key is supplied via `X-API-Key` header (or query param for WS clients that can't set headers)
- [ ] **AUTH-02**: API keys are compared in constant time; rotated by accepting a primary + secondary key from config
- [ ] **AUTH-03**: API keys never appear in logs (redacted at the slog handler level)
- [ ] **AUTH-04**: Failed auth returns 401 with no information about which key matched
- [ ] **AUTH-05**: Service exposes a way (config reload signal or restart) to rotate keys without dropping in-flight sessions on the surviving primary

### Multi-Device Operations

- [ ] **OPS-01**: Service supports ≥30 concurrent device sessions on a single host (target hardware: bare-metal/VM Linux with USB-attached devices)
- [ ] **OPS-02**: A health monitor per device watches frame-flow liveness; flat counter for >30s triggers auto-recovery (restart `app_process`, then re-attach reverse tunnels)
- [ ] **OPS-03**: Per-device reaper goroutine cleans up resources on session end / device disconnect (no goroutine or FD leaks)
- [ ] **OPS-04**: Each session uses ephemerally-allocated reverse-forward ports (not fixed 27183/4/5), so multiple sessions on different devices don't collide
- [ ] **OPS-05**: REST `GET /devices/{serial}/logcat` returns a streaming logcat (chunked or WS) with a configurable per-device retention buffer
- [ ] **OPS-06**: REST `POST /devices/{serial}/screenshot` returns a single PNG via the screencap shell command
- [ ] **OPS-07**: REST `POST /devices/{serial}/apks` installs an APK via `adb install` (sync push + `pm install`); returns success/failure with stderr
- [ ] **OPS-08**: REST `POST/GET/DELETE /devices/{serial}/files` push, pull, delete files via the ADB sync service
- [ ] **OPS-09**: REST `POST /devices/{serial}/recordings` starts a recording; the gateway tees frames to disk (mp4/mkv) without re-encoding; `DELETE` stops; `GET` lists/downloads
- [ ] **OPS-10**: Per-device performance metrics (CPU%, mem MB, FPS observed) are sampled at a configurable interval (default 5s) and exposed via Prometheus

### Observability

- [ ] **OBS-01**: Service exposes `GET /metrics` in Prometheus format
- [ ] **OBS-02**: Metrics cover: device count by status, session count by state, frames/sec per device, drop counter per client, ADB call latency, reverse-tunnel reconcile counter
- [ ] **OBS-03**: Logs include device serial + session ID as structured fields on every relevant entry
- [ ] **OBS-04**: A startup log line records pinned scrcpy version, build SHA, and effective config (with secrets redacted)

### Deployment

- [ ] **DPL-01**: Project ships a systemd unit file (`Type=simple`, `Restart=on-failure`, `LimitNOFILE=65536`, `TimeoutStopSec=30s`)
- [ ] **DPL-02**: Project ships a udev rules file disabling USB autosuspend for known Android vendor IDs
- [ ] **DPL-03**: Project documents minimum hub BoM (self-powered, ≥2 A/port) for the target fleet density

### Multi-Instance Scaling

- [ ] **SCL-01**: When Redis coordination is enabled in config, service registers itself + each owned device serial in Redis with a TTL lease
- [ ] **SCL-02**: Service refreshes Redis leases via heartbeat (default 10s); on instance death, leases expire and another instance can reclaim
- [ ] **SCL-03**: REST/WS requests for a device this instance does NOT own return a redirect (or in-process WS proxy fallback) to the owning instance
- [ ] **SCL-04**: Service tolerates a chaos test: `kill -9` of an instance results in another instance assuming its devices within the heartbeat window
- [ ] **SCL-05**: Project ships a reference load-balancer config (HAProxy or NGINX) demonstrating sticky-by-serial routing (`hash $arg_serial consistent`)
- [ ] **SCL-06**: Recordings have a configurable retention policy (max age + max disk); a janitor goroutine cleans up expired recordings

## v2 Requirements

Deferred to future releases. Acknowledged but not in current roadmap.

### Transport

- **WRTC-01**: WebRTC transport for sub-100ms latency use cases (only if measured WebSocket latency budget is missed)

### Auth & Multi-tenancy

- **AUTH2-01**: Per-tenant or per-device API keys (current: single shared key with primary/secondary rotation)
- **AUTH2-02**: OIDC/JWT mode — accept short-lived tokens issued by `pelni_server` on each request

### Observability

- **OBS2-01**: OpenTelemetry tracing across REST → ADB → device boundaries
- **OBS2-02**: Live device CPU/memory dashboards in the gateway UI (currently raw Prometheus only)

## Out of Scope

Explicitly excluded. Documented to prevent scope creep.

| Feature | Reason |
|---------|--------|
| Browser-side viewer / WebCodecs decoder UI | `pelni_server`'s frontend team owns the client; gateway is backend-only |
| Server-side video decode or transcode | CPU-prohibitive; defeats the 20–30 device per-host density target |
| Custom Android server.jar (replacing scrcpy) | Reusing scrcpy avoids reimplementing MediaCodec capture; vendor pinned upstream version |
| iOS / non-Android device support | ADB is Android-specific |
| Multi-tenant SaaS (sign-up, billing, per-tenant isolation) | Embedded backend for one customer (Pelni); out of scope |
| End-user authentication / RBAC | `pelni_server` handles user auth; gateway only validates API key from parent |
| WebRTC transport in v1 | WebSocket sufficient for proxy model; revisit if latency demands |
| Remote ADB over TCP | Devices are USB-attached to gateway host; no remote-ADB requirement |
| Server-side keyframe forcing | scrcpy's public protocol has no force-keyframe message; cache-and-replay is sufficient |

## Traceability

Each v1 requirement is mapped to exactly one phase. See `.planning/ROADMAP.md` for phase goals and success criteria.

| Requirement | Phase | Status |
|-------------|-------|--------|
| **_Foundation_** | | |
| FND-01 | Phase 1 | Pending |
| FND-02 | Phase 1 | Pending |
| FND-03 | Phase 1 | Pending |
| FND-04 | Phase 1 | Pending |
| FND-05 | Phase 1 | Pending |
| **_ADB Transport_** | | |
| ADB-01 | Phase 1 | Shipped (01-02) |
| ADB-02 | Phase 1 | Shipped (01-02) |
| ADB-03 | Phase 1 | Shipped (01-02) |
| ADB-04 | Phase 1 | Shipped (01-02) |
| ADB-05 | Phase 1 | Shipped (01-02) |
| ADB-06 | Phase 1 | Pending |
| ADB-07 | Phase 1 | Shipped (01-02) |
| ADB-08 | Phase 1 | Pending |
| **_Device Lifecycle_** | | |
| DEV-01 | Phase 1 | Shipped (01-03) |
| DEV-02 | Phase 1 | Pending |
| DEV-03 | Phase 1 | Pending |
| DEV-04 | Phase 1 | Pending |
| DEV-05 | Phase 3 | Pending |
| DEV-06 | Phase 3 | Pending |
| **_scrcpy Integration_** | | |
| SCR-01 | Phase 1 | Pending |
| SCR-02 | Phase 1 | Pending |
| SCR-03 | Phase 1 | Pending |
| SCR-04 | Phase 2 | Pending |
| SCR-05 | Phase 2 | Pending |
| SCR-06 | Phase 2 | Pending |
| SCR-07 | Phase 3 | Pending |
| **_Streaming API_** | | |
| STR-01 | Phase 1 | Pending |
| STR-02 | Phase 2 | Pending |
| STR-03 | Phase 2 | Pending |
| STR-04 | Phase 2 | Pending |
| STR-05 | Phase 2 | Pending |
| STR-06 | Phase 2 | Pending |
| STR-07 | Phase 2 | Pending |
| STR-08 | Phase 2 | Pending |
| STR-09 | Phase 2 | Pending |
| **_Control & Reservation_** | | |
| CTL-01 | Phase 2 | Pending |
| CTL-02 | Phase 2 | Pending |
| CTL-03 | Phase 2 | Pending |
| CTL-04 | Phase 2 | Pending |
| CTL-05 | Phase 2 | Pending |
| **_Auth_** | | |
| AUTH-01 | Phase 1 | Pending |
| AUTH-02 | Phase 1 | Pending |
| AUTH-03 | Phase 1 | Pending |
| AUTH-04 | Phase 1 | Pending |
| AUTH-05 | Phase 1 | Pending |
| **_Multi-Device Operations_** | | |
| OPS-01 | Phase 3 | Pending |
| OPS-02 | Phase 3 | Pending |
| OPS-03 | Phase 3 | Pending |
| OPS-04 | Phase 3 | Pending |
| OPS-05 | Phase 3 | Pending |
| OPS-06 | Phase 3 | Pending |
| OPS-07 | Phase 3 | Pending |
| OPS-08 | Phase 3 | Pending |
| OPS-09 | Phase 3 | Pending |
| OPS-10 | Phase 3 | Pending |
| **_Observability_** | | |
| OBS-01 | Phase 2 | Pending |
| OBS-02 | Phase 2 | Pending |
| OBS-03 | Phase 1 | Pending |
| OBS-04 | Phase 1 | Pending |
| **_Deployment_** | | |
| DPL-01 | Phase 1 | Pending |
| DPL-02 | Phase 3 | Pending |
| DPL-03 | Phase 3 | Pending |
| **_Multi-Instance Scaling_** | | |
| SCL-01 | Phase 4 | Pending |
| SCL-02 | Phase 4 | Pending |
| SCL-03 | Phase 4 | Pending |
| SCL-04 | Phase 4 | Pending |
| SCL-05 | Phase 4 | Pending |
| SCL-06 | Phase 4 | Pending |

**Coverage:**
- v1 requirements: 68 total
- Mapped to phases: 68 (100%) ✓
- Unmapped: 0
- Phase 1: 29 requirements
- Phase 2: 18 requirements
- Phase 3: 15 requirements
- Phase 4: 6 requirements

---
*Requirements defined: 2026-05-06*
*Last updated: 2026-05-06 — traceability populated by `/gsd-roadmap` (4 phases, 68/68 mapped)*
