# Phase 1: Single-Device Streaming Foundation - Context

**Gathered:** 2026-05-06
**Status:** Ready for planning

<domain>
## Phase Boundary

One device, one viewer, video frames flowing through WebSocket end-to-end. Locks the ADB transport foundation, scrcpy version contract, API-key auth scaffolding, and systemd deployment — all later phases depend on this.

This phase delivers: ADB client with reverse:forward helper, pinned scrcpy server.jar embedded in the binary, per-device session lifecycle (push → tunnel → launch → stream → teardown), single-viewer video WebSocket, REST device/session CRUD, domain error codes, API-key auth, structured logging, healthz endpoint, and systemd unit file.

</domain>

<decisions>
## Implementation Decisions

### scrcpy Version Pin
- **D-01:** Pin scrcpy **v3.3.4** (2025-12-17) — latest stable with all Android 14/15/16 compatibility fixes. Record SHA-256 alongside the embed. Test codec readers against fixture bytes from this exact build.
- **D-02:** Embed as `internal/scrcpy/assets/scrcpy-server-v3.3.4` via `//go:embed`. Push to device as `/data/local/tmp/scrcpy-server-gateway.jar` (gateway-specific filename avoids stomping system scrcpy).
- **D-03:** Phase 1 only implements video codec readers (12-byte codec metadata + 12-byte frame header). The expanded control message surface (~16 types in v3.3.4) is added incrementally in later phases.

### Session Startup Sequence
- **D-04:** Strictly sequential startup: push jar → set up reverse tunnels → listen on local ports → launch app_process. Matches scrcpy's documented protocol — no parallelization of tunnels with launch (protocol violation risk).
- **D-05:** On startup failure at step N, clean up steps 1..N-1 (remove reverse forwards, optionally delete pushed jar). State machine transition: `idle → starting → (active | failed)`.
- **D-06:** No jar caching in v1 — always push. Acceptable latency (~2-4s per device) for long-lived sessions. Revisit hybrid (cache-check + conditional push) if adbd-restart recovery shows this on the critical path.

### Error Surface Design
- **D-07:** Domain-specific error codes in a JSON envelope: `{"error": {"code": "DEVICE_OFFLINE", "message": "..."}}`. Codes map to fixed HTTP statuses (503, 404, 409, etc.).
- **D-08:** Initial code set for Phase 1: `ADB_UNAVAILABLE` (503), `DEVICE_OFFLINE` (404), `DEVICE_NOT_FOUND` (404), `PUSH_FAILED` (502), `REVERSE_FORWARD_FAILED` (502), `SCRCPY_LAUNCH_FAILED` (502), `SESSION_CONFLICT` (409), `SESSION_NOT_FOUND` (404), `UNAUTHORIZED` (401).
- **D-09:** Full causal chains stay in slog (structured fields: device serial, session ID, ADB operation, error chain). HTTP response carries only the top-level domain code + human-readable message. No internal ADB error text leaks to the API consumer.

### Reconciliation Strategy
- **D-10:** Marker-based cleanup on startup: kill only `app_process` instances matching gateway's jar CLASSPATH (`/data/local/tmp/scrcpy-server-gateway.jar`), remove only reverse mappings on gateway port range (27183-27185). Safe for coexisting ADB tools.
- **D-11:** Reconciliation steps on startup: (1) enumerate devices, (2) `shell:ps -A -o PID,ARGS | grep scrcpy-server-gateway.jar` per device, (3) `shell:kill <pid>` for each match, (4) `reverse:list-forward` per device, (5) `reverse:remove` for gateway-owned ports, (6) log all actions at INFO level with device serial.

### Claude's Discretion
- Exact Go package structure under `internal/` (e.g., `internal/adb/`, `internal/scrcpy/`, `internal/session/`) — planner decides based on dependency graph.
- Whether per-device mutex uses `sync.Mutex` or `errgroup.Group` — planner decides based on goroutine tree shape.
- Test infrastructure (fake ADB listener, fixture byte generation) — researcher/planner designs.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### ADB Protocol
- `.planning/research/SUMMARY.md` — research synthesis with HIGH confidence ratings
- `.planning/research/ARCHITECTURE.md` — architecture research (per-device supervisor, goroutine model)
- `.planning/research/PITFALLS.md` — operational hazards and remedies
- `android-monitoring-architecture.md` — original architecture sketch (high-level flow, concurrency model, API sketch)
- [Android ADB SERVICES.TXT](https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/SERVICES.TXT) — wire format for host:devices, transport:, reverse:forward, reverse:list-forward, reverse:remove

### scrcpy Protocol
- [scrcpy/doc/develop.md](https://github.com/Genymobile/scrcpy/blob/master/doc/develop.md) — server protocol: dummy byte, codec metadata, frame header, options
- [scrcpy/server/.../Server.java](https://github.com/Genymobile/scrcpy/blob/master/server/src/main/java/com/genymobile/scrcpy/Server.java) — invocation: `CLASSPATH=… app_process / com.genymobile.scrcpy.Server`

### Project Context
- `.planning/PROJECT.md` — vision, constraints, key decisions
- `.planning/REQUIREMENTS.md` — 68 v1 requirements with traceability (29 in Phase 1)
- `.planning/ROADMAP.md` — 4 phases, success criteria, cross-phase dependencies
- `CLAUDE.md` — technology stack decisions, library choices, version pins, alternatives considered

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- No existing Go code in this repo — greenfield project. All patterns established in Phase 1 become conventions for later phases.

### Established Patterns
- Architecture sketch (`android-monitoring-architecture.md`) defines: 1 device = 1 supervisor goroutine, video/audio/control as separate goroutines under supervisor, `select`-based backpressure on client channels.
- CLAUDE.md locks: `prife/goadb` for ADB, `go-chi/chi/v5` for routing, `coder/websocket` for WS, `koanf/v2` for config, `log/slog` for logging, `prometheus/client_golang` for metrics.

### Integration Points
- `pelni_server` (Laravel) is the sole API consumer — REST + WS proxy to browser frontend.
- ADB server on `localhost:5037` — local only, USB-attached devices.
- systemd unit file for deployment (Type=simple, LimitNOFILE=65536).

</code_context>

<specifics>
## Specific Ideas

- Gateway-specific jar filename (`scrcpy-server-gateway.jar`) serves double duty: avoids stomping system scrcpy AND acts as reconciliation marker for orphan process cleanup.
- Phase 1 success criterion #4 (adbd kill mid-session, reconnect within 10s) is the hardest acceptance test — startup sequence and reverse-forward re-issuance must be robust.
- API-key auth with constant-time compare from day one (not a "fix later" item) — per ROADMAP key risks section.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 1-Single-Device Streaming Foundation*
*Context gathered: 2026-05-06*