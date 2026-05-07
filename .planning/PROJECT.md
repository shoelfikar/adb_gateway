# ADB Gateway

## What This Is

A Go-based backend service that acts as a gateway between the `pelni_server` application and a fleet of USB-connected Android devices. It exposes a REST API for device management/control and WebSocket endpoints for real-time video, audio, and input streams — using the scrcpy Android server for capture and ADB as the device transport. Pelni server proxies these endpoints to its own browser frontend; this project does not ship a UI.

## Core Value

Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

## Requirements

### Validated

<!-- Shipped and confirmed valuable. -->

(None yet — ship to validate)

### Active

<!-- Current scope. Building toward these. -->

- [ ] Custom (or library-backed) ADB client speaking the ADB wire protocol over `localhost:5037`
- [ ] Device discovery, connect, disconnect, and health checks
- [ ] Per-device session lifecycle (push scrcpy `server.jar`, set up reverse tunnels, start `app_process`)
- [ ] Video stream relay (Android → ADB → Go → WebSocket) without server-side decode
- [ ] Audio stream relay (optional per device)
- [ ] Control input forwarding (touch, key, text) from WebSocket → control socket
- [ ] Multi-client viewing of a single device (broadcast with backpressure / frame drop)
- [ ] Multi-device support on a single instance (target 20–30 devices)
- [ ] REST API for device CRUD and session control
- [ ] WebSocket endpoints suitable for proxying through `pelni_server`
- [ ] API-key authentication for all REST and WebSocket endpoints
- [ ] systemd-friendly deployment on a bare-metal/VM host with USB-attached devices
- [ ] Redis-backed coordination for multi-node scaling
- [ ] Load-balancer-friendly multi-instance deployment
- [ ] Prometheus metrics + structured logs for ops observability

### Out of Scope

<!-- Explicit boundaries. Includes reasoning to prevent re-adding. -->

- Browser-side viewer / WebCodecs decoder UI — `pelni_server`'s frontend team owns the client
- Server-side video decoding or transcoding — relay only, to keep CPU low
- Custom Android server.jar — reusing scrcpy's open-source server avoids reimplementing MediaCodec capture
- Multi-tenant SaaS features (sign-up, billing, per-tenant isolation) — embedded backend only
- End-user authentication / RBAC — `pelni_server` handles user auth; gateway only validates an API key from the parent
- iOS / non-Android device support — ADB is Android-specific
- WebRTC transport in v1 — WebSocket is sufficient for `pelni_server`'s proxy model (revisit later if latency demands it)

## Context

**Domain.** Remote viewing and control of physical Android devices over ADB, using scrcpy's open-source Android server as the on-device capture/control agent. The architecture sketch is captured in [android-monitoring-architecture.md](../android-monitoring-architecture.md) and is the starting point for this project.

**Parent system.** This service is one component of `pelni_server` (a Laravel-based product). Pelni server owns user-facing concerns (auth, UI, business logic) and treats this gateway as an internal microservice — calling its REST endpoints and proxying its WebSocket streams to the browser.

**Physical fleet.** Devices are USB-attached to the host running this gateway. ADB server (`adbd`) runs on the same host and is reached over `localhost:5037`. There is no remote-ADB-over-network requirement.

**Streaming model.** Video and audio frames flow Android → MediaCodec → scrcpy server → ADB socket → Go relay → WebSocket → browser. The Go service must never decode media; it only routes bytes. This keeps CPU low enough to fit 20–30 devices per instance.

**Reference protocol.** scrcpy's server uses three reverse-forwarded ports per device: `27183` (video), `27184` (audio), `27185` (control). We follow this convention.

## Constraints

- **Tech stack**: Go for the gateway service — required for performance and concurrency, matches the prior architecture decision.
- **Streaming agent**: scrcpy `server.jar` (Apache-2.0) — must comply with attribution requirements (ship `THIRD_PARTY_NOTICES`); vendor a pinned version with the gateway and embed via `//go:embed`.
- **ADB transport**: Local ADB server only (no remote ADB) — devices are USB-attached.
- **No media decode**: Server must relay raw bytes; encoding/decoding is the device's and browser's job.
- **Auth**: API-key based, validated against a value provisioned by `pelni_server` — keep it simple in v1; revisit if/when multi-tenant requirements appear.
- **Deployment target**: Linux host with systemd, dedicated to this gateway and its USB-attached fleet.
- **Performance**: 1 instance ≈ 20–30 concurrent device sessions; horizontal scaling via Redis + load balancer beyond that.
- **Compatibility**: Must coexist with a system-installed `adb` (no port conflicts) and survive `adbd` restarts gracefully.

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Embedded backend service for `pelni_server` (no public UI, no multi-tenancy) | Scope is internal Pelni operations; user auth/UX lives in the parent app | — Pending |
| Go for the gateway | High concurrency, low overhead, matches the architecture sketch | — Pending |
| Reuse scrcpy's open-source `server.jar` for capture/control | Avoid reimplementing MediaCodec capture and input injection | — Pending |
| Adopt an existing Go ADB library rather than writing one from scratch | Faster to ship; ADB protocol is well-trodden | — Pending |
| REST + WebSocket-proxy integration with `pelni_server` (not direct browser-to-gateway) | Keeps auth and routing in the parent; gateway stays a backend service | — Pending |
| Static API-key auth between `pelni_server` and gateway | Simplest mechanism that fits the trust boundary; revisit if scope expands | — Pending |
| systemd on bare-metal/VM (no Kubernetes for v1) | Devices are physically attached; one host owns one fleet | — Pending |
| Full 4-phase roadmap as v1 (single device → multi-client → multi-device → scaling) | Stated end-state needs Redis/multi-node from the start of planning | — Pending |
| Defer WebRTC; WebSocket only in v1 | Simpler, sufficient for the proxy model; can add WebRTC if latency demands | ⚠️ Revisit |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition** (via `/gsd-transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd-complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-05-06 after initialization*
