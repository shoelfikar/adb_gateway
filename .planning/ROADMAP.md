# Roadmap: ADB Gateway

**Project:** ADB Gateway (Go service embedded by `pelni_server`)
**Created:** 2026-05-06
**Granularity:** standard (4 phases)
**Mode:** sequential, YOLO
**v1 Requirements:** 68, all mapped (100% coverage)

## Core Value

Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

## Granularity Justification

`standard` (5-8 phases). Research (SUMMARY.md, ARCHITECTURE.md, PITFALLS.md) and PROJECT.md converge on a 4-phase structure with strong, falsifiable boundaries. We considered two splits and rejected both:

- **Phase 0 spike for `reverse:forward` + scrcpy launch.** Rejected: the spike *is* Phase 1's first slice, and a separate phase adds bookkeeping without changing the work order. We instead flag Phase 1 for `/gsd-research-phase` so planning can do a focused spike before the phase plan.
- **Splitting Phase 3 ops-hardening from ADB-shell features (logcat / screenshot / file push / APK / recording).** Rejected: these features all share the same per-device supervisor + reaper foundation, and PITFALLS.md concentrates the same operational hazards (USB power, autosuspend, watchdog, FSM) across them. Splitting would force a synthetic boundary mid-feature.

Four phases stay within the `standard` band and each has a verifiable end-state.

## Phases

- [ ] **Phase 1: Single-Device Streaming Foundation** — One device, one viewer, video frames flowing through WebSocket end-to-end. Locks the ADB foundation, scrcpy version contract, and auth scaffolding for all later phases.
- [ ] **Phase 2: Multi-Client + Control** — 1 controller + N observers per device, with audio, control input, reservation leases, and baseline metrics.
- [ ] **Phase 3: Multi-Device Fleet** — 20-30 concurrent devices on one host with health-driven auto-recovery, ADB-shell features (logcat, screenshot, APK, file push/pull, recording), and per-device performance metrics.
- [ ] **Phase 4: Horizontal Scaling** — N instances behind a load balancer, Redis-backed device registry, cross-node session handoff, recording retention, and production-grade observability.

## Phase Details

### Phase 1: Single-Device Streaming Foundation

**Goal**: An operator can connect a single Android device to the gateway and stream live video to a WebSocket client, authenticated via API key, with the service running under systemd as a hermetic single binary.

**Depends on**: Nothing (foundation phase -- blocks all subsequent phases).

**Requirements** (29): FND-01, FND-02, FND-03, FND-04, FND-05, ADB-01, ADB-02, ADB-03, ADB-04, ADB-05, ADB-06, ADB-07, ADB-08, DEV-01, DEV-02, DEV-03, DEV-04, SCR-01, SCR-02, SCR-03, STR-01, AUTH-01, AUTH-02, AUTH-03, AUTH-04, AUTH-05, OBS-03, OBS-04, DPL-01

**Success Criteria** (what must be TRUE):
  1. Operator can `systemctl start adb-gateway`, `curl -H 'X-API-Key: ...' http://localhost:8080/healthz` returns 200 with version + scrcpy version + build SHA, and `--version` prints the same; `SIGTERM` drains cleanly within 30s without leaving stale `reverse:forward` mappings or orphan `app_process` instances on the device.
  2. With one USB-attached Android device, `GET /devices` lists the device by serial and `POST /devices/{serial}/sessions` returns a session ID; the gateway pushes the embedded pinned `server.jar`, allocates ephemeral reverse-tunnel ports, launches `app_process` with the version-matched arg, and reaches `active` state.
  3. A test WebSocket client connecting to `/devices/{serial}/video` with a valid `X-API-Key` receives the 12-byte codec metadata followed by frame-boundary-preserved H.264 packets (verified via `ffprobe` on the captured stream); without a valid key the request returns 401 and never reaches handlers.
  4. Killing `adbd` mid-session causes the gateway to reconnect to `localhost:5037`, re-issue every `reverse:forward`, audit via `reverse:list-forward`, and resume the session within 10s -- without restarting the gateway process.
  5. Restarting the gateway after `kill -9` leaves no orphan `app_process` on the device and no stale gateway-owned reverse mappings (verified by startup reconciliation pass); `THIRD_PARTY_NOTICES` ships in the deploy artifact and is exposed via `--licenses`.

**Plans**: 6 plans

Plans:
- [x] 01-01-PLAN.md -- Foundation: project scaffold, config, logging, healthz, auth middleware, domain errors
- [x] 01-02-PLAN.md -- ADB transport: client, host services, reverse:forward helper
- [x] 01-03-PLAN.md -- Device registry: tracking with track-devices, session FSM definitions
- [x] 01-04-PLAN.md -- scrcpy integration: server.jar embed, launcher, video frame reader
- [x] 01-05-PLAN.md -- Session lifecycle: supervisor, REST endpoints, video WebSocket relay
- [x] 01-06-PLAN.md -- Hardening: ADB reconnect, startup reconciliation, graceful shutdown, deploy artifacts

**Research flag**: yes -- `/gsd-plan-phase` should run `/gsd-research-phase` first to spike the in-house `reverse:forward` helper against AOSP `SERVICES.TXT` (refine ~150 LOC estimate), validate `prife/goadb` shell-v2 against a real Android 14/15 device, and confirm the pinned scrcpy `server.jar` version + frame-header layout against fixture bytes.

### Phase 2: Multi-Client + Control

**Goal**: Multiple viewers can watch the same device simultaneously while exactly one holds a reservation lease and steers control input; audio streams alongside video, slow viewers degrade gracefully without harming others, and operators can observe baseline behavior via Prometheus.

**Depends on**: Phase 1 (per-device supervisor, ADB foundation, scrcpy codec readers, auth, REST/WS scaffolding).

**Requirements** (18): SCR-04, SCR-05, SCR-06, STR-02, STR-03, STR-04, STR-05, STR-06, STR-07, STR-08, STR-09, CTL-01, CTL-02, CTL-03, CTL-04, CTL-05, OBS-01, OBS-02

**Success Criteria** (what must be TRUE):
  1. Two WebSocket clients connecting to `/devices/{serial}/video` 5 s apart both decode video successfully; the late joiner receives the cached codec metadata + most recent keyframe before the live tail and shows a frame within < 1 s of connecting.
  2. Operator throttling one viewer to 100 KB/s does not affect the frame rate observed by other viewers (verified by per-client `frames_dropped_total` and `frames_emitted_per_device` metrics); after N consecutive drops the slow viewer is disconnected with a structured close code, and total in-flight bytes per process stay under the configured watermark.
  3. `POST /devices/{serial}/reservation` returns a 60s TTL lease; only the lease holder's control WS messages reach the device (touch, key, text verified end-to-end against scrcpy's binary control protocol with single-writer discipline); observers' control messages are rejected with a structured error; `PATCH` extends, `DELETE` releases, expired leases auto-release and emit an event.
  4. With audio enabled by default, `/devices/{serial}/audio` streams OPUS frames in parallel with video, preserving the same 12-byte frame-header discipline; clipboard updates and ack DeviceMessages are exposed via the control WS or metrics.
  5. After a 1000-cycle viewer connect/disconnect soak test, `runtime.NumGoroutine()` returns to within ~10 of baseline and FD count stays bounded; `/metrics` exposes device-state, session-state, frames-per-second, drop counters, ADB-call latency, and reverse-tunnel reconcile counters.

**Plans**: TBD

**Research flag**: yes -- `/gsd-plan-phase` should run `/gsd-research-phase` to validate late-joiner mechanics against a real WebCodecs decoder, decide the "force keyframe" strategy (no message exists in scrcpy's public protocol -- accept "wait for next natural keyframe" or plan a server.jar tweak), and confirm the actual proxy stack `pelni_server` will use (NGINX/HAProxy/Cloudflare timeouts and Upgrade-header handling).

### Phase 3: Multi-Device Fleet

**Goal**: A single host runs 20-30 concurrent device sessions with per-device health-driven auto-recovery, full session-lifecycle FSM, ADB-shell device-management features (logcat, screenshot, APK install, file push/pull, recording, performance metrics), and operationally hardened deployment (USB autosuspend overrides, hub BoM, raised FD limits).

**Depends on**: Phase 2 (Hub, control writer, reservation, base metrics) -- multi-device is mostly "remove device-specific globals" plus operational hardening.

**Requirements** (15): DEV-05, DEV-06, SCR-07, OPS-01, OPS-02, OPS-03, OPS-04, OPS-05, OPS-06, OPS-07, OPS-08, OPS-09, OPS-10, DPL-02, DPL-03

**Success Criteria** (what must be TRUE):
  1. Thirty USB-attached devices stream concurrently from one host; pulling any one device's USB cable causes its session FSM to transition `active -> reconnecting -> failed`/`idle` within 30 s without affecting other devices' frame rates or REST latency, and per-device mutexes prevent any single hung device from blocking gateway-wide requests.
  2. Stalling a device's frame flow (e.g. `SIGSTOP` the on-device `app_process`) flips `/health/devices` to `stalled` within 30 s and triggers auto-recovery: the supervisor restarts `app_process`, re-attaches reverse tunnels, and resumes streaming, with the FSM transitions observable via REST and metrics.
  3. Operators can `GET /devices/{serial}/logcat` (chunked/WS stream with retroactive ring buffer), `POST /devices/{serial}/screenshot` (returns PNG via `screencap`), `POST /devices/{serial}/apks` (installs APK via sync push + `pm install` with stderr on failure), `POST/GET/DELETE /devices/{serial}/files` (sync push/pull/delete), and `POST /devices/{serial}/recordings` (tees frames to mp4/mkv on disk without re-encoding); device IDs are stable serials, never USB paths.
  4. After 1000 session create/destroy cycles, no goroutine, FD, or reverse-tunnel mapping leaks (verified by per-session reaper + startup reconciliation); ephemeral reverse-tunnel ports are allocated per session via `net.Listen("127.0.0.1:0")` so simultaneous sessions on different devices never collide; `LimitNOFILE=65536` in the systemd unit is in effect.
  5. Per-device performance metrics (CPU%, memory MB, observed FPS) are sampled at the configured interval (default 5s) and exposed via Prometheus with `device_serial` labels; a udev rules file disabling USB autosuspend for known Android vendor IDs ships with the install, and a documented hub BoM (self-powered, >=2 A/port) is part of the deployment guide.

**Plans**: TBD

**Research flag**: no -- PITFALLS.md provides concrete remedies for every Phase 3 failure mode (USB autosuspend, hub BoM, FSM, watchdog, reaper, FD limits). Architecture stays the same as Phase 2; this phase is mostly hardening + ADB-shell feature surface, both well-trodden patterns.

### Phase 4: Horizontal Scaling

**Goal**: The gateway runs as N instances behind a load balancer with Redis-backed device-to-instance routing, surviving instance death within one heartbeat interval; recordings have a configurable retention policy and Prometheus dashboards aggregate cleanly across instances.

**Depends on**: Phase 3 (full single-host fleet semantics) -- coordination is opt-in and layered on top.

**Requirements** (6): SCL-01, SCL-02, SCL-03, SCL-04, SCL-05, SCL-06

**Success Criteria** (what must be TRUE):
  1. With Redis coordination enabled in config, each instance registers itself + every owned device serial in Redis with a TTL lease and refreshes via heartbeat (default 10s); `kill -9` of an instance causes its devices' leases to expire and another instance with the physical USB attachment reclaims them within the heartbeat window (verified by chaos test).
  2. A request for a device this instance does not own returns either an HTTP 307 redirect or transparently proxies the WebSocket in-process to the owning instance; LB serial-hash affinity (HAProxy `balance hdr`/`url_param` or NGINX `hash $arg_serial consistent`) routes new requests to the correct owner without double-relaying frames on the steady state path.
  3. The deploy artifact ships a reference HAProxy or NGINX config demonstrating sticky-by-serial routing; an integration test against the real proxy stack confirms WebSocket Upgrade headers, `proxy_read_timeout` >= 3600s, and `proxy_buffering off` work end-to-end through the LB.
  4. Recordings respect a configurable retention policy (max age + max disk); a janitor goroutine cleans up expired recordings on a configurable interval, and disk usage stays bounded under continuous recording load.
  5. After a multi-instance soak test (24h, mixed device churn, 2x rolling restart), no Redis claims older than 2x heartbeat interval persist, no duplicate `app_process` instances run on any device, and the API key surface remains constant-time-compared with two-key rotation surviving the rolling restart without dropping in-flight sessions on the surviving primary.

**Plans**: TBD

**Research flag**: yes -- `/gsd-plan-phase` should run `/gsd-research-phase` to verify the actual LB `pelni_server` will deploy supports URL-path/query-param hashing (HAProxy `balance hdr`/`url_param`, NGINX `hash`); if it only supports cookie stickiness, fall back to the in-process WS proxy variant and accept the bandwidth cost. Verify Redis topology (single node vs Sentinel vs Cluster) before designing key schemas.

## Cross-Phase Dependencies

| From | To | What flows | Why critical |
|------|----|-----------|---------|
| Phase 1 -> Phase 2 | `internal/adb/` (host services + reverse-forward + sync + shell-v2), `internal/scrcpy/` (codec readers + version pin + launcher), `internal/session/supervisor.go`, auth middleware, REST/WS scaffolding | Everything else needs the ADB foundation. Reverse-forward helper is the single hardest dependency -- scrcpy cannot start without it, and no Go ADB library implements it. |
| Phase 1 -> Phase 2 | scrcpy version pin + embedded `server.jar` + frame-header parser | Codec readers are tested against fixture bytes from a specific server.jar. Pinning later means re-testing readers on bump. |
| Phase 2 -> Phase 3 | `internal/session/hub.go` + keyframe cache, control single-writer goroutine, reservation lease semantics | Multi-device with a working Hub is mostly removing globals; multi-device without a Hub means rebuilding fan-out per device. |
| Phase 2 -> Phase 3 | Baseline Prometheus collectors + structured log fields | Per-device metrics in Phase 3 layer device-serial labels onto the same collectors. |
| Phase 3 -> Phase 4 | Per-device supervisor + FSM + reaper + ephemeral port allocation | Coordination layer registers devices in Redis by serial; the FSM states feed the cross-node "release / reclaim" signals. |
| Phase 3 -> Phase 4 | systemd unit + LimitNOFILE + graceful-drain SIGTERM handler | Multi-instance rolling restart inherits the Phase 3 drain behavior unchanged. |

**Hard blockers:**
- Phase 1 blocks all of Phase 2/3/4. The reverse-forward helper and scrcpy version pin are the keystones -- a wrong byte in either produces silent breakage that surfaces only at runtime.
- Phase 2's Hub + keyframe cache blocks Phase 3's multi-device -- without the cache, late-joining viewers on any device can never decode.
- Phase 3's per-device supervisor + ephemeral ports blocks Phase 4 -- coordination assumes one supervisor per (instance, device) pair, not a global pool.

## Key Risks & Decisions Carried Into Planning

- **scrcpy version is a vendored protocol, not a library** (Pitfall 1). Decide the pinned version at Phase 1 kickoff; embed the jar before writing codec readers; never auto-update; CI test pushes the jar and parses 5 s of frames on every dependency bump.
- **`reverse:forward` helper is bespoke** (~150 LOC, MEDIUM confidence on LOC). Phase 1 spike refines the estimate against AOSP `SERVICES.TXT` and a real adbd before locking schedule.
- **No "force keyframe" in scrcpy's public protocol** (open question). Phase 2 must decide between "wait for natural keyframe" (simpler, higher join latency) vs server.jar patch (faster joins, takes us off mainline). Default to the cached-keyframe approach; revisit only if measured.
- **LB sticky-by-serial requires URL-path or query-param hashing** (Phase 4 risk). Verify against `pelni_server`'s actual LB before designing the routing layer. In-process WS proxy fallback is the bandwidth-expensive plan B.
- **API-key auth is constant-time-compared with two-key rotation from day one** (Phase 1, audited Phase 4) -- not a "we'll fix it later" item. 10 lines of code; emergency rotation otherwise requires coordinated downtime.
- **Apache-2.0 attribution for embedded `server.jar`** is a Phase 1 deliverable (`THIRD_PARTY_NOTICES` + `/licenses` endpoint or `--licenses` flag). PROJECT.md's "BSD-style" was incorrect and has been corrected.
- **Per-device mutexes, never a global one** (Pitfall 9). One hung device must not freeze the others -- this is a Phase 1 design decision, not a Phase 3 fix.

## Progress

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Single-Device Streaming Foundation | 6/6 | complete | 01-01, 01-02, 01-03, 01-04, 01-05, 01-06 |
| 2. Multi-Client + Control | 0/0 | not started | -- |
| 3. Multi-Device Fleet | 0/0 | not started | -- |
| 4. Horizontal Scaling | 0/0 | not started | -- |

## Coverage Summary

- **v1 requirements:** 68
- **Mapped to phases:** 68 (100%)
- **Unmapped:** 0
- **Phase 1:** 29 requirements (foundation, ADB, single-device session lifecycle, single video stream, auth, base obs/deploy)
- **Phase 2:** 18 requirements (audio + control + multi-client + reservation + baseline metrics)
- **Phase 3:** 15 requirements (FSM + multi-device + ADB-shell features + recording + perf metrics + udev/hub deploy)
- **Phase 4:** 6 requirements (Redis registry + LB affinity + cross-node + retention)
- **Total:** 29 + 18 + 15 + 6 = 68 (matches v1 requirement count); each requirement is mapped to exactly one phase -- see REQUIREMENTS.md traceability table for the canonical mapping.

---
*Roadmap created: 2026-05-06. Next: `/gsd-plan-phase 1` (will trigger `/gsd-research-phase` first per Phase 1 research flag).*