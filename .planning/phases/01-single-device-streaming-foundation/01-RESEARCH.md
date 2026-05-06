# Phase 1: Single-Device Streaming Foundation - Research

**Researched:** 2026-05-06
**Domain:** ADB wire protocol + scrcpy server protocol + Go relay service
**Confidence:** HIGH

## Summary

Phase 1 establishes the foundational ADB transport layer, scrcpy version contract, API-key auth, and the end-to-end video relay path for one device and one viewer. Every later phase depends on the correctness of this phase's byte-level protocol implementations -- particularly the in-house `reverse:forward` helper (no Go ADB library implements it) and the scrcpy frame-header parser.

**Critical correction from research:** The architecture sketch and CONTEXT.md assumed scrcpy reverse tunnels use `tcp:27183` on the device side. In reality, scrcpy v3.3.4 uses **`localabstract:scrcpy_<SCID>`** Unix domain sockets on the device side. The host side still uses `tcp:<PORT>`. The correct `reverse:forward` command is `reverse:forward:localabstract:scrcpy_<SCID>;tcp:<host_port>` (semicolon separator, not colon). This changes the reverse-forward helper implementation but not its complexity.

**Primary recommendation:** Implement the reverse-forward helper against the verified ADB wire format, pin scrcpy v3.3.4 with the `localabstract:` socket model, and use `io.ReadFull` for frame boundaries from day one. The ~150 LOC estimate for the reverse-forward helper is validated, though the actual format differs from initial assumptions.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **D-01:** Pin scrcpy v3.3.4 (2025-12-17) -- latest stable with Android 14/15/16 compatibility fixes. Record SHA-256 alongside the embed. Test codec readers against fixture bytes from this exact build.
- **D-02:** Embed as `internal/scrcpy/assets/scrcpy-server-v3.3.4` via `//go:embed`. Push to device as `/data/local/tmp/scrcpy-server-gateway.jar` (gateway-specific filename avoids stomping system scrcpy).
- **D-03:** Phase 1 only implements video codec readers (12-byte codec metadata + 12-byte frame header). The expanded control message surface (~16 types in v3.3.4) is added incrementally in later phases.
- **D-04:** Strictly sequential startup: push jar -> set up reverse tunnels -> listen on local ports -> launch app_process. Matches scrcpy's documented protocol -- no parallelization of tunnels with launch (protocol violation risk).
- **D-05:** On startup failure at step N, clean up steps 1..N-1 (remove reverse forwards, optionally delete pushed jar). State machine transition: `idle -> starting -> (active | failed)`.
- **D-06:** No jar caching in v1 -- always push. Acceptable latency (~2-4s per device) for long-lived sessions.
- **D-07:** Domain-specific error codes in a JSON envelope: `{"error": {"code": "DEVICE_OFFLINE", "message": "..."}}`. Codes map to fixed HTTP statuses (503, 404, 409, etc.).
- **D-08:** Initial code set for Phase 1: `ADB_UNAVAILABLE` (503), `DEVICE_OFFLINE` (404), `DEVICE_NOT_FOUND` (404), `PUSH_FAILED` (502), `REVERSE_FORWARD_FAILED` (502), `SCRCPY_LAUNCH_FAILED` (502), `SESSION_CONFLICT` (409), `SESSION_NOT_FOUND` (404), `UNAUTHORIZED` (401).
- **D-09:** Full causal chains stay in slog (structured fields: device serial, session ID, ADB operation, error chain). HTTP response carries only the top-level domain code + human-readable message. No internal ADB error text leaks to the API consumer.
- **D-10:** Marker-based cleanup on startup: kill only `app_process` instances matching gateway's jar CLASSPATH (`/data/local/tmp/scrcpy-server-gateway.jar`), remove only reverse mappings on gateway port range. Safe for coexisting ADB tools.
- **D-11:** Reconciliation steps on startup: (1) enumerate devices, (2) `shell:ps -A -o PID,ARGS | grep scrcpy-server-gateway.jar` per device, (3) `shell:kill <pid>` for each match, (4) `reverse:list-forward` per device, (5) `reverse:remove` for gateway-owned ports, (6) log all actions at INFO level with device serial.

### Claude's Discretion
- Exact Go package structure under `internal/` (e.g., `internal/adb/`, `internal/scrcpy/`, `internal/session/`) -- planner decides based on dependency graph.
- Whether per-device mutex uses `sync.Mutex` or `errgroup.Group` -- planner decides based on goroutine tree shape.
- Test infrastructure (fake ADB listener, fixture byte generation) -- researcher/planner designs.

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope.
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|------------------|
| FND-01 | Service starts as single Go binary, runs under systemd, supports graceful shutdown on SIGTERM | systemd unit with Type=simple, signal handler with 30s drain |
| FND-02 | Service loads config from file + env via koanf | koanf/v2 with file+env+posflag providers |
| FND-03 | Service reports version, pinned scrcpy version, build SHA on --version and /healthz | Build-time ldflags for SHA, runtime const for scrcpy version |
| FND-04 | Service emits structured JSON logs via log/slog | slog JSON handler with configurable level |
| FND-05 | Service ships THIRD_PARTY_NOTICES for scrcpy Apache-2.0 | scrcpy is Apache-2.0 (confirmed); ship LICENSE verbatim + NOTICE if upstream provides one |
| ADB-01 | Service connects to local ADB server on localhost:5037 (configurable), with reconnect on drop | prife/goadb provides dial + reconnect; add backoff via cenkalti/backoff/v4 |
| ADB-02 | Service speaks ADB host services (host:devices, host:track-devices, host:transport) via wire protocol | prife/goadb covers all three; NewDeviceWatcher for track-devices |
| ADB-03 | Service implements in-house reverse:forward helper | CRITICAL: No Go library implements this. Wire format verified against AOSP SERVICES.TXT. Uses localabstract:scrcpy_<SCID> on device side |
| ADB-04 | Service can push files via ADB sync service | prife/goadb provides PushFile/PushFileCtx |
| ADB-05 | Service can run shell commands via shell:v2 | prife/goadb provides RunShellCommand(true, ...) and Session API |
| ADB-06 | After ADB reconnect, re-issue all reverse forwards and audit | reverse:list-forward after reconnect; re-issue any missing mappings |
| ADB-07 | Every ADB call bounded by context with timeout; per-device mutex | context.WithTimeout on every call; sync.Mutex per device serial |
| ADB-08 | On startup, reconcile stale state | D-10/D-11 define exact reconciliation sequence |
| DEV-01 | Track devices in-memory, fed by host:track-devices | prife/goadb NewDeviceWatcher provides streaming device state changes |
| DEV-02 | REST GET /devices returns current list | chi handler over in-memory device registry |
| DEV-03 | REST POST /devices/{serial}/sessions creates session (idempotent) | Session FSM: idle->starting->active; return existing if active |
| DEV-04 | REST DELETE /devices/{serial}/sessions/{id} ends session | Teardown: cancel context, remove reverse forwards, kill app_process |
| SCR-01 | Vendor pinned server.jar, embedded via //go:embed | D-01/D-02: v3.3.4, embedded as internal/scrcpy/assets/scrcpy-server-v3.3.4 |
| SCR-02 | Push jar, set up reverse tunnels, launch app_process | Sequential per D-04; localabstract socket model with SCID |
| SCR-03 | Read scrcpy video stream (codec meta + 12-byte frame header + payload) using io.ReadFull | Frame header format verified: 8 bytes (config+keyframe+PTS) + 4 bytes (size) |
| STR-01 | WebSocket GET /devices/{serial}/video streams H.264/H.265 frames with codec metadata on first frame | coder/websocket binary messages; send codec meta + frame-boundary-preserved packets |
| AUTH-01 | All REST+WS endpoints require API key via X-API-Key header (or query param for WS) | chi middleware; extract from header or query param for WS upgrade |
| AUTH-02 | API keys compared in constant time; primary+secondary rotation | crypto/subtle.ConstantTimeCompare; SHA-256 hash before compare |
| AUTH-03 | API keys never appear in logs | Custom slog handler or middleware redaction |
| AUTH-04 | Failed auth returns 401 with no key-match info | Generic "unauthorized" response, no timing or content leak |
| AUTH-05 | Key rotation without dropping in-flight sessions | Primary+secondary keys; reload on SIGHUP or config change |
| OBS-03 | Logs include device serial + session ID as structured fields | slog.With("device", serial, "session", id) per-device subloggers |
| OBS-04 | Startup log line records pinned scrcpy version, build SHA, effective config (secrets redacted) | Single structured log at startup with version info + config dump |
| DPL-01 | systemd unit file (Type=simple, Restart=on-failure, LimitNOFILE=65536, TimeoutStopSec=30s) | Ship deploy/adb-gateway.service |
</phase_requirements>

## Architectural Responsibility Map

| Capability | Primary Tier | Secondary Tier | Rationale |
|-----------|-------------|-----------------|-----------|
| ADB wire protocol communication | API/Backend | -- | All ADB calls are server-side; no browser or CDN involvement |
| Device discovery + tracking | API/Backend | -- | host:track-devices is a server-to-adbd stream |
| Reverse tunnel management | API/Backend | -- | reverse:forward commands go to adbd; host-side TCP listeners are server-side |
| scrcpy server launch orchestration | API/Backend | -- | Push jar, set up tunnels, invoke app_process -- all server-side |
| Video frame reading + parsing | API/Backend | -- | io.ReadFull on ADB socket; frame boundary preservation is server's job |
| WebSocket video relay | API/Backend | Browser (consumer) | Server produces binary WS messages; browser consumes via WebCodecs |
| API key authentication | API/Backend | -- | Middleware validates before any handler runs |
| Session lifecycle FSM | API/Backend | -- | State machine lives in the session supervisor |
| Configuration loading | API/Backend | -- | koanf reads from file+env+flags on server startup |
| Structured logging | API/Backend | -- | slog JSON handler writes to stderr/journald |
| Graceful shutdown + drain | API/Backend | -- | SIGTERM handler with 30s timeout |

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.24+ (1.26.2 latest stable) | Runtime | Required by constraints; goroutine-per-stream fits relay model [VERIFIED: go.dev] |
| `github.com/prife/goadb` | v0.4.8 (latest on proxy; v0.4.4 also valid) | ADB client -- host/transport/shell/sync | Best-maintained Go ADB lib; covers host:devices, track-devices, transport, shell:v2, sync push [VERIFIED: Go module proxy] |
| In-house `reverse:forward` helper | -- | Reverse-tunnel setup | **Mandatory.** No Go ADB library implements reverse:forward [VERIFIED: pkg.go.dev prife/goadb, electricbubble/gadb] |
| `github.com/go-chi/chi/v5` | v5.2.5 | HTTP router | net/http-compatible, composable middleware [VERIFIED: Go module proxy] |
| `github.com/coder/websocket` | v1.8.14 | WebSocket server | Zero-alloc Read/Write, context.Context-first [VERIFIED: Go module proxy] |
| `github.com/knadh/koanf/v2` | v2.3.4 | Config | Modular, no global state, no key-lowercasing [VERIFIED: Go module proxy] |
| `log/slog` | stdlib (Go 1.21+) | Structured logging | No dep, JSON handler production-grade |
| `github.com/prometheus/client_golang` | v1.23.2 | Metrics | Canonical Prometheus exposition [VERIFIED: Go module proxy] |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/stretchr/testify` | v1.10+ | Assertions, mocks | Default test assertions; prefer fakes over mocks |
| `github.com/google/uuid` | v1.6.x | Session/client IDs | Per-session IDs for log correlation |
| `github.com/cenkalti/backoff/v4` | v4.x | Reconnect/retry on adbd | ADB server can restart; exponential backoff avoids tight loops |
| `golang.org/x/sync/errgroup` | latest | Bounded goroutine groups | Per-device session lifecycle |
| `github.com/spf13/pflag` | latest | POSIX-style CLI flags | Pair with koanf via posflag provider |
| `github.com/coreos/go-systemd/v22` | v22.5.0 | sd_notify ready/watchdog | If Type=notify adopted in systemd unit |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| prife/goadb + custom reverse helper | Shell out to adb binary | Only if 100% feature parity needed fast; subprocess management of adb is brittle for long-lived services |
| chi | stdlib net/http (Go 1.22 ServeMux) | If middleware count is 3 or fewer and zero deps is preferred |
| coder/websocket | gorilla/websocket | If gorilla-only feature needed (legacy compression); coder is better for new code |
| koanf | viper | If team has heavy viper investment; new code should not start with viper |

**Installation:**
```bash
go mod init github.com/pelni/adb-gateway

# Core
go get github.com/prife/goadb@v0.4.8
go get github.com/go-chi/chi/v5@v5.2.5
go get github.com/coder/websocket@v1.8.14
go get github.com/knadh/koanf/v2@v2.3.4
go get github.com/knadh/koanf/providers/file
go get github.com/knadh/koanf/providers/env
go get github.com/knadh/koanf/providers/posflag
go get github.com/knadh/koanf/parsers/yaml
go get github.com/prometheus/client_golang@v1.23.2
go get github.com/coreos/go-systemd/v22@v22.5.0

# Supporting
go get github.com/google/uuid
go get github.com/cenkalti/backoff/v4
go get golang.org/x/sync
go get github.com/spf13/pflag

# Test
go get github.com/stretchr/testify@v1.10.0
```

**Version verification (via Go module proxy, 2026-05-06):**
- `prife/goadb`: v0.4.8 (latest); CLAUDE.md specifies v0.4.4 which is also available
- `go-chi/chi/v5`: v5.2.5 (confirmed latest)
- `coder/websocket`: v1.8.14 (confirmed latest)
- `knadh/koanf/v2`: v2.3.4 (confirmed latest)
- `prometheus/client_golang`: v1.23.2 (confirmed latest stable)
- `go-systemd/v22`: v22.5.0 available
- Go stable: 1.26.2 (project targets 1.24+)

## Architecture Patterns

### System Architecture Diagram

```
pelni_server (Laravel)
    |
    | HTTP/WS + X-API-Key
    v
+----------------------------------------------------------+
|                   GATEWAY (Go binary)                     |
|                                                           |
|  chi.Router                                               |
|  |-- APIKeyAuth middleware                                |
|  |-- GET  /healthz                                       |
|  |-- GET  /devices                                        |
|  |-- POST /devices/{serial}/sessions                      |
|  |-- DELETE /devices/{serial}/sessions/{id}              |
|  |-- WS   /devices/{serial}/video                        |
|  |                                                        |
|  +-- DeviceRegistry (sync.Map: serial -> *DeviceEntry)   |
|       |                                                   |
|       +-- DeviceSession (per-device supervisor)          |
|            |-- errgroup.Group                              |
|            |-- videoReader goroutine                      |
|            |-- wsVideoWriter goroutine (single viewer)     |
|            |-- healthMonitor goroutine                     |
|            |-- closer goroutine (context cancel -> close)  |
|                                                           |
|  internal/adb/Client                                      |
|  |-- Dial() -> net.Conn to localhost:5037                 |
|  |-- ListDevices() / NewDeviceWatcher()                   |
|  |-- Device(serial).Transport()                          |
|  |-- Device(serial).PushFileCtx()                        |
|  |-- Device(serial).RunShellCommand()                    |
|  |-- ReverseForward(serial, local, remote)  [IN-HOUSE]   |
|  |-- ReverseListForward(serial)              [IN-HOUSE]   |
|  |-- ReverseRemove(serial, spec)             [IN-HOUSE]   |
|                                                           |
|  internal/scrcpy/                                         |
|  |-- embedded server.jar (//go:embed)                     |
|  |-- Launcher: push + reverse + app_process              |
|  |-- VideoReader: 12-byte codec meta + frame parsing      |
|  |-- SCRCPY_VERSION const = "3.3.4"                      |
+----------------------------------------------------------+
    |                           |
    | TCP :5037                 | TCP :<ephemeral_ports>
    v                           v
  adbd (system)          host-side listeners
    |                    (net.Listen("tcp","127.0.0.1:0"))
    | USB                       |
    v                           v (device connects back via reverse tunnel)
  Android Device           scrcpy server (app_process)
    |                    connects to localabstract:scrcpy_<SCID>
    |
    +-- MediaCodec -> scrcpy server -> localabstract socket
```

### Recommended Project Structure
```
adb_gateway/
  cmd/
    gateway/
      main.go              # entry point, signal handling, wire config->modules
  internal/
    adb/                   # ADB client (wraps prife/goadb + in-house reverse)
      client.go            # Dial localhost:5037, New(), NewWithConfig()
      host_services.go     # ListDevices, NewDeviceWatcher, ServerVersion
      reverse.go           # ReverseForward, ReverseListForward, ReverseRemove
      reconnect.go         # Backoff reconnect on adbd restart
    scrcpy/
      embed.go             # //go:embed assets/scrcpy-server-v3.3.4
      launcher.go          # PushJar, SetupTunnels, LaunchAppProcess
      video_reader.go      # ReadCodecMeta, ReadFrame (io.ReadFull discipline)
      version.go           # SCRCPY_VERSION const, BuildSCID()
    session/
      supervisor.go        # DeviceSession with errgroup, context lifecycle
      registry.go          # sync.Map serial -> *DeviceEntry
      fsm.go               # SessionState: idle/starting/active/stopping/failed
    api/
      router.go            # chi.Router with middleware stack
      handlers_devices.go  # GET /devices, POST/DELETE sessions
      handlers_healthz.go  # GET /healthz
      ws_video.go           # WS /devices/{serial}/video (Phase 1: single viewer)
      auth.go              # API key middleware (constant-time compare)
      errors.go            # Domain error codes + JSON envelope
    config/
      config.go            # koanf loading, struct binding, validation
    obs/
      logging.go           # slog setup, key redaction
  internal/scrcpy/assets/
    scrcpy-server-v3.3.4   # vendored, pinned server.jar
  deploy/
    adb-gateway.service    # systemd unit file
    config.example.yaml    # example config
  THIRD_PARTY_NOTICES      # scrcpy Apache-2.0 attribution
  go.mod
  go.sum
  .golangci.yml
```

### Pattern 1: ADB Reverse-Forward Helper

**What:** Open a TCP connection to `localhost:5037`, switch to a device transport, then send the `reverse:forward` service command in the ADB smart-sockets wire format.

**When to use:** Every session startup (video/audio/control tunnels) and every adbd reconnect.

**Critical wire format details (verified against AOSP SERVICES.TXT):**

The reverse-forward command is sent **after** `host:transport:<serial>` binds the connection to a device. The format uses a **semicolon** (`;`) as the separator between device-side and host-side socket specs:

| Command | Wire Format (after transport) | Response |
|---------|-------------------------------|----------|
| `reverse:forward` | `reverse:forward:<device-socket>;<host-socket>` | OKAY on success |
| `reverse:forward:norebind` | `reverse:forward:norebind:<device-socket>;<host-socket>` | FAIL if already bound |
| `reverse:list-forward` | `reverse:list-forward` | OKAY + hex-length + text listing |
| `reverse:killforward` | `reverse:killforward:<device-socket>` | OKAY |
| `reverse:killforward-all` | `reverse:killforward-all` | OKAY |

**Important:** For scrcpy, the device-side socket is `localabstract:scrcpy_<SCID>`, NOT `tcp:27183`. The host-side socket is `tcp:<PORT>`.

The full wire protocol sequence for reverse:forward:

1. Dial `localhost:5037`
2. Send length-prefixed `host:transport:<serial>` -> read 4-byte OKAY
3. Send length-prefixed `reverse:forward:localabstract:scrcpy_<SCID>;tcp:<host_port>` -> read 4-byte OKAY
4. **Keep the connection open** -- the reverse mapping is active as long as the connection to `:5037` stays open

The 4-byte hex length prefix format: `fmt.Sprintf("%04x%s", len(msg), msg)` [CITED: AOSP SERVICES.TXT, android.googlesource.com]

**Example:**
```go
func (c *Client) ReverseForward(ctx context.Context, serial, deviceSocket, hostSocket string) error {
    conn, err := c.dial(ctx)
    if err != nil {
        return fmt.Errorf("dial: %w", err)
    }
    defer conn.Close()

    // Step 1: Bind to device transport
    if err := c.sendService(conn, "host:transport:"+serial); err != nil {
        return fmt.Errorf("transport: %w", err)
    }

    // Step 2: Send reverse:forward command
    cmd := "reverse:forward:" + deviceSocket + ";" + hostSocket
    if err := c.sendService(conn, cmd); err != nil {
        return fmt.Errorf("reverse:forward: %w", err)
    }

    // Connection must stay open for the reverse to remain active.
    // Caller is responsible for keeping conn alive.
    return nil
}

func (c *Client) sendService(conn net.Conn, service string) error {
    msg := fmt.Sprintf("%04x%s", len(service), service)
    if _, err := conn.Write([]byte(msg)); err != nil {
        return err
    }
    buf := make([]byte, 4)
    if _, err := io.ReadFull(conn, buf); err != nil {
        return err
    }
    if string(buf) != "OKAY" {
        // Read error message: 4-byte hex length + message
        // ...
        return fmt.Errorf("ADB replied %s", string(buf))
    }
    return nil
}
```

**LOC estimate:** 120-180 LOC for the reverse package (ReverseForward, ReverseListForward, ReverseRemove, ReverseKillforwardAll), plus ~80 LOC for the ADB smart-sockets codec (sendService, readResponse). Total: ~200-260 LOC with proper error handling. The ~150 LOC estimate was close but slightly low.

### Pattern 2: scrcpy Session Startup Sequence

**What:** Strictly sequential: push jar -> listen on host ports -> install reverse tunnels -> launch app_process -> accept connections -> read codec metadata.

**When to use:** Every session creation.

**Exact sequence (verified against scrcpy server.c, Server.java, develop.md):**

```
1. Push server.jar
   adb push internal/scrcpy/assets/scrcpy-server-v3.3.4 -> /data/local/tmp/scrcpy-server-gateway.jar

2. Generate SCID (31-bit random)
   scid := rand.Int31()
   deviceSocketName := fmt.Sprintf("scrcpy_%08x", scid)

3. Allocate host-side TCP listeners (ephemeral ports)
   videoLn, _ := net.Listen("tcp", "127.0.0.1:0")
   audioLn, _ := net.Listen("tcp", "127.0.0.1:0")  // Phase 1: optional
   controlLn, _ := net.Listen("tcp", "127.0.0.1:0") // Phase 1: not used yet

4. Install reverse tunnels (one per enabled stream)
   reverse:forward:localabstract:scrcpy_<SCID>;tcp:<videoPort>
   [Phase 1: only video tunnel is needed if audio=false, control=false]

5. Launch app_process (via shell:v2)
   CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar \
     app_process / com.genymobile.scrcpy.Server 3.3.4 \
     scid=<SCID> log_level=info \
     video=true audio=false control=false \
     video_codec=h264 max_size=0 video_bit_rate=0 \
     tunnel_forward=false cleanup=true \
     send_device_meta=true send_frame_meta=true \
     send_dummy_byte=false send_codec_meta=true \
     raw_stream=false

6. Accept connections on host listeners (order: video, audio, control)
   videoConn, _ := videoLn.Accept()

7. Read video codec metadata (12 bytes)
   var codecMeta [12]byte
   io.ReadFull(videoConn, codecMeta[:])
   codecID := codecMeta[0:4]   // e.g. "h264"
   width := binary.BigEndian.Uint32(codecMeta[4:8])
   height := binary.BigEndian.Uint32(codecMeta[8:12])

8. Session is now active -- spawn video reader goroutine
```

**Key server args for Phase 1 (verified against scrcpy develop.md):**

| Arg | Value | Rationale |
|-----|-------|-----------|
| `<version>` | `3.3.4` | Must match exactly or server exits |
| `scid=<hex>` | Random 31-bit | Disambiguates multiple instances |
| `tunnel_forward` | `false` (default, omit) | We use reverse tunnels |
| `video` | `true` | Phase 1 streams video |
| `audio` | `false` | Audio deferred to Phase 2 |
| `control` | `false` | Control deferred to Phase 2 |
| `send_device_meta` | `true` (default) | Read device name on first socket |
| `send_frame_meta` | `true` (default) | 12-byte frame headers (required) |
| `send_codec_meta` | `true` (default) | Codec metadata on stream start |
| `send_dummy_byte` | `false` (default for reverse) | No dummy byte in reverse mode |
| `cleanup` | `true` (default) | Server cleans up on exit |
| `max_size` | `0` (default=original) | No downscaling unless configured |
| `video_bit_rate` | `0` (default) | Use device default unless configured |
| `log_level` | `info` | Reasonable for production |

### Pattern 3: scrcpy Video Frame Reading

**What:** Read 12-byte header + payload for each frame, preserving frame boundaries for WebSocket binary messages.

**When to use:** Every video frame in the hot path.

**Frame header layout (verified against scrcpy develop.md, unchanged in v3.3.4):**

```
Byte 7   Byte 6   Byte 5   Byte 4   Byte 3   Byte 2   Byte 1   Byte 0
CK...... xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx
^^                                                                       +
||<----------------------------PTS (62 bits)---------------------------->+
| `- keyframe flag (bit 62)
 `-- config packet flag (bit 63)

Byte 8   Byte 9   Byte 10  Byte 11
<----------packet_size (u32 BE)--------->
```

**Example:**
```go
type FrameHeader struct {
    ConfigPacket bool
    KeyFrame     bool
    PTS          uint64
    Size         uint32
}

func ReadFrameHeader(r io.Reader) (FrameHeader, error) {
    var buf [12]byte
    if _, err := io.ReadFull(r, buf[:]); err != nil {
        return FrameHeader{}, fmt.Errorf("read frame header: %w", err)
    }
    rawPTS := binary.BigEndian.Uint64(buf[:8])
    size := binary.BigEndian.Uint32(buf[8:12])
    return FrameHeader{
        ConfigPacket: rawPTS&(1<<63) != 0,
        KeyFrame:     rawPTS&(1<<62) != 0,
        PTS:          rawPTS &^ (3 << 62), // clear top 2 bits
        Size:         size,
    }, nil
}

func ReadVideoFrame(r io.Reader) (FrameHeader, []byte, error) {
    hdr, err := ReadFrameHeader(r)
    if err != nil {
        return hdr, nil, err
    }
    payload := make([]byte, hdr.Size)
    if _, err := io.ReadFull(r, payload); err != nil {
        return hdr, nil, fmt.Errorf("read frame payload (%d bytes): %w", hdr.Size, err)
    }
    return hdr, payload, nil
}
```

**Critical:** Always use `io.ReadFull`. Never assume a single `conn.Read()` returns a complete frame. TCP is a byte stream; frames may split across reads or be concatenated in one read. [CITED: scrcpy develop.md, PITFALLS.md Pitfall 6]

### Pattern 4: Single-Viewer WebSocket Video Relay

**What:** For Phase 1, exactly one WebSocket client per device. The video reader goroutine reads frames and writes them as binary WS messages.

**When to use:** Phase 1 only (Phase 2 adds the Hub for N viewers).

**Example:**
```go
func (s *DeviceSession) relayVideo(ctx context.Context, conn net.Conn, ws *websocket.Conn) error {
    // Read codec metadata first
    var codecMeta [12]byte
    if _, err := io.ReadFull(conn, codecMeta[:]); err != nil {
        return fmt.Errorf("read codec meta: %w", err)
    }

    // Send codec metadata as first WS message
    if err := ws.Write(ctx, websocket.MessageBinary, codecMeta[:]); err != nil {
        return fmt.Errorf("write codec meta: %w", err)
    }

    // Read and relay frames
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }

        hdr, payload, err := ReadVideoFrame(conn)
        if err != nil {
            return fmt.Errorf("read frame: %w", err)
        }

        // Forward raw: 12-byte header + payload as single WS message
        msg := make([]byte, 12+len(payload))
        copy(msg[:12], hdr.rawHeader[:]) // preserve original 12 bytes
        copy(msg[12:], payload)
        if err := ws.Write(ctx, websocket.MessageBinary, msg); err != nil {
            return fmt.Errorf("write frame: %w", err)
        }
    }
}
```

**Buffer sizing:** Average H.264 frame at 720p/4Mbps ~50KB, keyframes ~500KB. `coder/websocket` handles messages up to `SetReadLimit` (default 32KB for reads; writes have no hard limit). For writes, frames are fine -- even a 500KB keyframe is a single `Write` call.

### Pattern 5: API Key Authentication Middleware

**What:** chi middleware that validates `X-API-Key` header (or query param for WS) using constant-time comparison with SHA-256 hashing.

**When to use:** All REST and WS endpoints.

**Example:**
```go
func APIKeyAuth(primary, secondary []byte) func(http.Handler) http.Handler {
    // Pre-compute SHA-256 hashes at middleware creation time
    primaryHash := sha256.Sum256(primary)
    secondaryHash := sha256.Sum256(secondary)

    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            key := r.Header.Get("X-API-Key")
            if key == "" {
                key = r.URL.Query().Get("api_key") // WS clients that cannot set headers
            }
            if key == "" {
                writeError(w, ErrUnauthorized)
                return
            }

            keyHash := sha256.Sum256([]byte(key))
            matchPrimary := subtle.ConstantTimeCompare(keyHash[:], primaryHash[:]) == 1
            matchSecondary := subtle.ConstantTimeCompare(keyHash[:], secondaryHash[:]) == 1

            if !matchPrimary && !matchSecondary {
                writeError(w, ErrUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

**Key points:**
- SHA-256 hash before compare prevents timing leaks from `ConstantTimeCompare`'s length-dependent early return [CITED: caiorcferreira.github.io, crypto/subtle docs]
- Pre-hash at middleware creation (not per-request) for primary/secondary
- Two-key scheme allows rotation: deploy new secondary -> swap primary -> remove old
- For WS upgrade, the `X-API-Key` header is available during the HTTP upgrade request
- Query param fallback for WS clients that cannot set headers (less secure; log a warning)

### Anti-Patterns to Avoid
- **Using `tcp:27183` for device-side reverse tunnel spec:** scrcpy uses `localabstract:scrcpy_<SCID>`, not TCP ports on the device side. Using `tcp:27183` will not work. [VERIFIED: scrcpy adb.c source, develop.md]
- **Reading frames with `conn.Read(buf)` instead of `io.ReadFull`:** TCP is a byte stream; partial reads corrupt frame boundaries. Always use `io.ReadFull` for both the 12-byte header and the payload. [VERIFIED: PITFALLS.md Pitfall 6]
- **Parallelizing tunnel setup with server launch:** The scrcpy server connects back the moment it boots. Tunnels must be established and listeners ready BEFORE `app_process` is invoked. [VERIFIED: CONTEXT.md D-04]
- **Sharing a single ADB connection across goroutines:** ADB connections are sticky after `host:transport`. One connection per logical operation. [VERIFIED: ADB protocol, PITFALLS.md]
- **String comparison (`==`) for API keys:** Timing oracle attack. Use `crypto/subtle.ConstantTimeCompare`. [VERIFIED: PITFALLS.md, Go stdlib docs]
- **Logging API key values:** Redact at the slog handler level. [VERIFIED: CONTEXT.md D-09, PITFALLS.md]

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| ADB host services + transport + shell + sync | Custom ADB client from scratch | prife/goadb | Sync protocol is fiddly; prife covers shell:v2, push, devices, transport with contexts |
| HTTP routing + middleware | Custom router | go-chi/chi/v5 | Middleware composition (auth, logging, recovery) needs chi; stdlib is tedious at 5+ middleware |
| WebSocket server | Custom WS framing | coder/websocket | Zero-alloc, context-aware, concurrent-write safe; avoids gorilla's single-writer constraint |
| Config loading from file+env+flags | Custom config parser | koanf/v2 | Modular providers; no viper key-lowercasing or global state |
| Constant-time key comparison | `==` on strings | crypto/subtle.ConstantTimeCompare | Timing oracle prevents brute-force optimization |
| Exponential backoff on adbd reconnect | Custom sleep loop | cenkalti/backoff/v4 | Proper exponential backoff with jitter and max elapsed |
| Structured logging | log.Printf or logrus | log/slog | stdlib since Go 1.21; JSON handler; no dep drift |

**Key insight:** The only thing we must hand-roll is the `reverse:forward` helper (~200-260 LOC). Everything else has a proven library.

## Common Pitfalls

### Pitfall 1: Wrong Device-Side Socket Format for Reverse Tunnels
**What goes wrong:** Using `reverse:forward:tcp:27183;tcp:<host_port>` instead of `reverse:forward:localabstract:scrcpy_<SCID>;tcp:<host_port>`.
**Why it happens:** Early documentation and the architecture sketch used `tcp:27183` based on scrcpy's default port numbers. The actual scrcpy server uses abstract Unix domain sockets.
**How to avoid:** Always use `localabstract:scrcpy_<SCID>` as the device-side socket spec. Generate a random SCID for each session.
**Warning signs:** `reverse:forward` returns OKAY but the scrcpy server cannot connect back; accept() never fires.

### Pitfall 2: Frame Boundary Loss on TCP Partial Reads
**What goes wrong:** Using `conn.Read(buf)` which may return partial frames or concatenated frames. Browser-side decoder receives corrupted data.
**Why it happens:** TCP is a byte stream, not a message stream. The 12-byte header may be split across two reads.
**How to avoid:** Always use `io.ReadFull` for the 12-byte header and for the payload. Unit test with a synthetic byte stream that splits headers.
**Warning signs:** Browser WebCodecs `EncodingError`; `ffprobe` reports corrupted stream.

### Pitfall 3: Reverse Tunnels Lost on adbd Restart
**What goes wrong:** `adb kill-server` or adbd crash evaporates all `reverse:forward` mappings. The scrcpy server tries to reconnect and fails.
**Why it happens:** Reverse forwards live in adbd's in-memory state. No push notification when they disappear.
**How to avoid:** After any `:5037` reconnection, re-issue every `reverse:forward` and verify via `reverse:list-forward`. Treat reverse forwards as soft state that must be reconciled.
**Warning signs:** Sessions go silent (no frames) but device still listed in `adb devices`.

### Pitfall 4: scrcpy Version Mismatch
**What goes wrong:** Pushed `server.jar` version does not match the version string passed as the first `app_process` argument. Server immediately exits with `IllegalArgumentException`.
**Why it happens:** The embedded `server.jar` and the `SCRCPY_VERSION` constant diverge after a scrcpy upgrade.
**How to avoid:** Pin `SCRCPY_VERSION` as a Go const next to the embed directive. Bump both in the same commit. CI test: push jar + parse 5 seconds of frames.
**Warning signs:** "server version does not match" in adbd logs; session immediately fails after `starting`.

### Pitfall 5: ADB Hangs on Dead Device Blocking Gateway
**What goes wrong:** A USB device half-disconnects and adbd marks it `offline`. Any `host:transport` + shell command blocks indefinitely.
**Why it happens:** adbd has no response deadline for offline devices.
**How to avoid:** Every ADB command gets `context.WithTimeout`. Per-device mutexes (never global). If timeout fires, cancel the context.
**Warning signs:** HTTP request latency spikes correlated with one device serial.

### Pitfall 6: API Key Timing Leak
**What goes wrong:** Using `==` or `strings.EqualFold` for key comparison leaks key content via microsecond-level timing differences.
**Why it happens:** String comparison short-circuits on first mismatched byte.
**How to avoid:** `crypto/subtle.ConstantTimeCompare` on SHA-256 hashes. Hash before compare to prevent `ConstantTimeCompare`'s length-dependent early return.
**Warning signs:** Lint rule `gosec` flags `==` on secrets; timing analysis shows variable response times.

### Pitfall 7: Stale Reverse Tunnels + app_process on Gateway Restart
**What goes wrong:** After `kill -9`, `reverse:list-forward` shows stale mappings and `app_process` instances persist on devices.
**Why it happens:** `defer` cleanup does not run on SIGKILL or panic.
**How to avoid:** Startup reconciliation pass (D-10/D-11): enumerate devices, kill orphan `app_process`, remove stale reverse forwards.
**Warning signs:** `adb reverse --list` keeps growing across restarts; new session fails with "address already in use".

### Pitfall 8: Not Reading Device Metadata Before Frames
**What goes wrong:** Forgetting to read the 64-byte device name metadata from the first socket before reading codec metadata, causing the first 64 bytes of the stream to be consumed as if they were codec metadata.
**Why it happens:** scrcpy sends device metadata on the first socket when `send_device_meta=true` (default).
**How to avoid:** Read 64 bytes of device name from the first (video) socket before reading the 12-byte codec metadata.
**Warning signs:** Codec metadata parsed as garbage (wrong width/height); stream never decodes.

## Code Examples

### ADB Smart-Sockets Codec (send/receive)
```go
// Source: AOSP SERVICES.TXT wire format
// https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/SERVICES.TXT

func sendMessage(conn net.Conn, msg string) error {
    payload := fmt.Sprintf("%04x%s", len(msg), msg)
    _, err := conn.Write([]byte(payload))
    return err
}

func readResponse(conn net.Conn) (string, error) {
    status := make([]byte, 4)
    if _, err := io.ReadFull(conn, status); err != nil {
        return "", err
    }
    if string(status) == "OKAY" {
        return "OKAY", nil
    }
    if string(status) == "FAIL" {
        lenBuf := make([]byte, 4)
        if _, err := io.ReadFull(conn, lenBuf); err != nil {
            return "", err
        }
        msgLen, _ := strconv.ParseInt(string(lenBuf), 16, 32)
        errMsg := make([]byte, msgLen)
        if _, err := io.ReadFull(conn, errMsg); err != nil {
            return "", err
        }
        return "", fmt.Errorf("FAIL: %s", string(errMsg))
    }
    return "", fmt.Errorf("unexpected response: %s", string(status))
}
```

### Reverse Forward with Connection Preservation
```go
// The connection to :5037 MUST stay open for the reverse mapping to remain active.
// Do NOT defer conn.Close() immediately.

type ReverseMapping struct {
    conn       net.Conn    // kept alive for the mapping duration
    DeviceSpec string      // e.g. "localabstract:scrcpy_00001234"
    HostSpec   string      // e.g. "tcp:42001"
}

func (c *Client) ReverseForward(ctx context.Context, serial, deviceSpec, hostSpec string) (*ReverseMapping, error) {
    dialer := net.Dialer{Timeout: 5 * time.Second}
    conn, err := dialer.DialContext(ctx, "tcp", c.addr) // localhost:5037
    if err != nil {
        return nil, fmt.Errorf("dial adb: %w", err)
    }

    // Set deadline for the handshake, then clear it
    conn.SetDeadline(time.Now().Add(10 * time.Second))
    defer conn.SetDeadline(time.Time{}) // clear after handshake

    // Bind to device transport
    if err := sendMessage(conn, "host:transport:"+serial); err != nil {
        conn.Close()
        return nil, err
    }
    if _, err := readResponse(conn); err != nil {
        conn.Close()
        return nil, err
    }

    // Send reverse:forward
    cmd := "reverse:forward:" + deviceSpec + ";" + hostSpec
    if err := sendMessage(conn, cmd); err != nil {
        conn.Close()
        return nil, err
    }
    if _, err := readResponse(conn); err != nil {
        conn.Close()
        return nil, err
    }

    return &ReverseMapping{conn: conn, DeviceSpec: deviceSpec, HostSpec: hostSpec}, nil
}

func (rm *ReverseMapping) Close() error {
    return rm.conn.Close() // closing the :5037 connection removes the reverse mapping
}
```

### Reverse List-Forward
```go
func (c *Client) ReverseListForward(ctx context.Context, serial string) ([]ForwardEntry, error) {
    conn, err := c.dial(ctx)
    if err != nil {
        return nil, err
    }
    defer conn.Close()

    // Bind to device transport
    sendMessage(conn, "host:transport:"+serial)
    if _, err := readResponse(conn); err != nil {
        return nil, err
    }

    // Send reverse:list-forward
    sendMessage(conn, "reverse:list-forward")
    if _, err := readResponse(conn); err != nil {
        return nil, err
    }

    // Read listing: 4-byte hex length + text
    lenBuf := make([]byte, 4)
    io.ReadFull(conn, lenBuf)
    msgLen, _ := strconv.ParseInt(string(lenBuf), 16, 32)
    data := make([]byte, msgLen)
    io.ReadFull(conn, data)

    // Parse: each line is "serial local remote\n" (space-separated)
    // For reverse:list-forward, serial is always "(not used)" or empty
    var entries []ForwardEntry
    for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
        if line == "" {
            continue
        }
        parts := strings.SplitN(line, " ", 3)
        if len(parts) >= 2 {
            entries = append(entries, ForwardEntry{
                Local:  parts[len(parts)-2],
                Remote: parts[len(parts)-1],
            })
        }
    }
    return entries, nil
}
```

### Session Lifecycle State Machine
```go
type SessionState int

const (
    StateIdle     SessionState = iota
    StateStarting              // push jar, tunnels, launch
    StateActive                // streaming
    StateStopping              // cleanup
    StateFailed                // terminal state
)

var validTransitions = map[SessionState][]SessionState{
    StateIdle:     {StateStarting},
    StateStarting: {StateActive, StateFailed, StateStopping},
    StateActive:   {StateStopping, StateFailed},
    StateStopping: {StateIdle, StateFailed},
    StateFailed:   {StateIdle}, // retry
}

func (s *DeviceSession) transitionTo(target SessionState) error {
    for _, valid := range validTransitions[s.state] {
        if valid == target {
            s.state = target
            s.log.Info("session state transition", "from", s.state, "to", target)
            return nil
        }
    }
    return fmt.Errorf("invalid transition %s -> %s", s.state, target)
}
```

### Graceful Shutdown with 30s Drain
```go
func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // ... wire up registry, api server, etc.

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

    go func() {
        sig := <-sigCh
        slog.Info("received signal, starting graceful shutdown", "signal", sig)
        cancel() // cancel root context -> tears down all sessions
    }()

    // Wait for all sessions to drain or timeout
    drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer drainCancel()
    registry.CloseAllSessions(drainCtx)

    slog.Info("shutdown complete")
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|-------------|-----------------|-------------|--------|
| scrcpy `tcp:27183` for device-side sockets | `localabstract:scrcpy_<SCID>` Unix domain sockets | scrcpy v2.0 (2022) | Allows multiple instances per device; abstract sockets avoid port conflicts |
| gorilla/websocket | coder/websocket (nhooyr successor) | 2024 (Coder took over) | Context-aware, zero-alloc, concurrent-write safe |
| viper for config | koanf for new Go services | ~2023 | No key lowercasing, no global state, modular providers |
| `==` for API key compare | `crypto/subtle.ConstantTimeCompare` + SHA-256 | Security best practice | Prevents timing oracle attacks |
| `host:devices` polling | `host:track-devices` long-poll | Always available | Push-based, lower latency, less adbd load |
| Fixed reverse-tunnel ports | Ephemeral `net.Listen("127.0.0.1:0")` | scrcpy v2.0+ | No port conflicts across sessions |
| scrcpy v2.x frame header | Same 12-byte format in v3.x | Unchanged since v2.0 | Frame header is stable across v3.3.x |

**Deprecated/outdated:**
- `zach-klippenstein/goadb`: Dormant since 2018; superseded by `prife/goadb`
- `gorilla/websocket` original repo: Archived then un-archived but development slowed
- `sirupsen/logrus`: Maintenance mode for years
- `spf13/viper`: Key lowercasing footgun, global state, large dep graph
- `gofiber/fiber`: Incompatible with stdlib `http.Handler` and `coder/websocket`

## Assumptions Log

| # | Claim | Section | Risk if Wrong |
|---|-------|---------|---------------|
| A1 | scrcpy v3.3.4 uses `localabstract:scrcpy_<SCID>` for device-side reverse tunnel sockets (not `tcp:27183`) | Architecture Patterns, Pitfall 1 | Server cannot connect back; accept() never fires |
| A2 | Go 1.24 is sufficient; currently installed Go 1.22.4 needs upgrade | Environment Availability | Build fails if Go version is too old |
| A3 | prife/goadb v0.4.4 (or v0.4.8) shell:v2 works on Android 14/15/16 devices | Standard Stack | app_process launch fails on some devices |
| A4 | scrcpy v3.3.4 server.jar is available from GitHub releases with a predictable filename | Standard Stack | Cannot download the jar for embedding |
| A5 | `reverse:list-forward` output format is `<serial> <local> <remote>\n` with spaces (not semicolons) | Code Examples | Parsing fails, reconciliation breaks |
| A6 | Keeping the `:5037` connection open is sufficient for the reverse mapping to persist | Code Examples | Reverse mappings disappear unexpectedly |
| A7 | The gateway-specific jar filename (`scrcpy-server-gateway.jar`) is sufficient to distinguish gateway processes from system scrcpy | Pitfall 7 | Reconciliation kills the wrong processes |

## Open Questions

1. **prife/goadb shell:v2 on real Android 14/15 devices**
   - What we know: prife/goadb v0.4.0 added shell-v2 transport, which fixes some Android 14+ cases
   - What's unclear: Whether `CLASSPATH=... app_process / com.genymobile.scrcpy.Server 3.3.4` via goadb's shell:v2 works correctly on Android 14+ with recent security restrictions
   - Recommendation: Test on at least one real Android 14+ device during Phase 1 execution; add integration test if possible

2. **scrcpy v3.3.4 server.jar download and SHA-256**
   - What we know: scrcpy v3.3.4 was released 2025-12-17, bug-fix only, no protocol changes
   - What's unclear: Exact download URL and SHA-256 for the server.jar asset
   - Recommendation: Download from GitHub releases during Phase 1 task 1; verify SHA-256 against upstream

3. **WebSocket authentication for clients that cannot set headers**
   - What we know: CONTEXT.md and REQUIREMENTS.md specify `X-API-Key` header or query param for WS
   - What's unclear: Whether `pelni_server`'s WS proxy can inject the `X-API-Key` header during the upgrade request, eliminating the need for query-param fallback
   - Recommendation: Support both header and query param; prefer header; document that query param is less secure (appears in proxy logs)

4. **`send_device_meta` handling for the 64-byte device name**
   - What we know: scrcpy sends 64 bytes of device name on the first socket when `send_device_meta=true`
   - What's unclear: Whether `send_device_meta=false` is more appropriate for a headless gateway (the device name is not useful for the API consumer)
   - Recommendation: Set `send_device_meta=true` in Phase 1 (matches scrcpy defaults); consider `false` in Phase 2+ if the 64 bytes cause issues

## Environment Availability

| Dependency | Required By | Available | Version | Fallback |
|------------|------------|-----------|---------|----------|
| Go 1.24+ | Build/runtime | **No** (1.22.4 installed) | 1.22.4 (needs upgrade) | None -- must upgrade |
| ADB (`adb` binary) | Testing, device verification | Yes | 35.0.2 | -- |
| Docker | Integration testing (optional) | Yes | 27.4.0 | -- |
| golangci-lint | Static analysis | No | -- | Install as part of project setup |
| gotestsum | Test runner UX | No | -- | Use `go test` directly |
| goreleaser | Build+release | No | -- | Manual `go build` for Phase 1 |
| Android device | End-to-end testing | **Unknown** | -- | Fake ADB listener for unit tests |

**Missing dependencies with no fallback:**
- Go 1.24+: Must upgrade from 1.22.4 before any code can be built. This is a hard prerequisite.
- golangci-lint: Should be installed as part of project bootstrap.

**Missing dependencies with fallback:**
- gotestsum: Use `go test ./...` directly; gotestsum is a convenience tool.
- Android device: Use the fake ADB listener pattern for unit/integration tests; real device testing is manual.
- goreleaser: Manual `go build` is fine for Phase 1; goreleaser becomes important at release time.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | stdlib `testing` + stretchr/testify |
| Config file | None (Go convention) |
| Quick run command | `go test ./internal/adb/... ./internal/scrcpy/... ./internal/api/... -short -count=1` |
| Full suite command | `go test ./... -count=1 -race` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| ADB-01 | Connect to ADB server, reconnect on drop | unit + integration | `go test ./internal/adb/... -run TestDial -count=1` | Wave 0 |
| ADB-02 | host:devices, host:track-devices, host:transport | unit | `go test ./internal/adb/... -run TestHostServices -count=1` | Wave 0 |
| ADB-03 | reverse:forward, reverse:list-forward, reverse:remove | unit | `go test ./internal/adb/... -run TestReverse -count=1` | Wave 0 |
| ADB-04 | sync push (server.jar) | unit | `go test ./internal/adb/... -run TestPush -count=1` | Wave 0 |
| ADB-05 | shell:v2 execution | unit | `go test ./internal/adb/... -run TestShell -count=1` | Wave 0 |
| ADB-06 | Re-issue reverse forwards on reconnect | unit | `go test ./internal/adb/... -run TestReconnect -count=1` | Wave 0 |
| ADB-07 | Context timeout on ADB calls | unit | `go test ./internal/adb/... -run TestTimeout -count=1` | Wave 0 |
| ADB-08 | Startup reconciliation | integration | `go test ./internal/adb/... -run TestReconciliation -count=1` | Wave 0 |
| SCR-01 | Embedded server.jar via //go:embed | unit | `go test ./internal/scrcpy/... -run TestEmbed -count=1` | Wave 0 |
| SCR-02 | Push + reverse + launch sequence | integration | `go test ./internal/scrcpy/... -run TestLaunch -count=1` | Wave 0 |
| SCR-03 | Video frame reading (12-byte header + payload) | unit | `go test ./internal/scrcpy/... -run TestVideoReader -count=1` | Wave 0 |
| STR-01 | WebSocket video relay end-to-end | integration | `go test ./internal/api/... -run TestWSVideo -count=1` | Wave 0 |
| AUTH-01 | API key required on all endpoints | unit | `go test ./internal/api/... -run TestAuthRequired -count=1` | Wave 0 |
| AUTH-02 | Constant-time compare, two-key rotation | unit | `go test ./internal/api/... -run TestAuthCompare -count=1` | Wave 0 |
| AUTH-03 | API keys redacted in logs | unit | `go test ./internal/obs/... -run TestKeyRedaction -count=1` | Wave 0 |
| AUTH-04 | 401 with no info leak | unit | `go test ./internal/api/... -run TestAuthNoLeak -count=1` | Wave 0 |
| AUTH-05 | Key rotation without session drop | integration | `go test ./internal/api/... -run TestKeyRotation -count=1` | Wave 0 |
| FND-01 | Graceful shutdown on SIGTERM | integration | `go test ./... -run TestGracefulShutdown -count=1` | Wave 0 |
| FND-02 | Config loading via koanf | unit | `go test ./internal/config/... -run TestConfig -count=1` | Wave 0 |
| FND-03 | /healthz endpoint | unit | `go test ./internal/api/... -run TestHealthz -count=1` | Wave 0 |
| FND-04 | Structured JSON logs | unit | `go test ./internal/obs/... -run TestLogging -count=1` | Wave 0 |
| FND-05 | THIRD_PARTY_NOTICES | manual | Check file exists in deploy artifact | N/A |
| DEV-01 | Device tracking via track-devices | unit | `go test ./internal/session/... -run TestDeviceTracking -count=1` | Wave 0 |
| DEV-02 | GET /devices | unit | `go test ./internal/api/... -run TestGetDevices -count=1` | Wave 0 |
| DEV-03 | POST /devices/{serial}/sessions | unit | `go test ./internal/api/... -run TestCreateSession -count=1` | Wave 0 |
| DEV-04 | DELETE /devices/{serial}/sessions/{id} | unit | `go test ./internal/api/... -run TestDeleteSession -count=1` | Wave 0 |
| OBS-03 | Device serial + session ID in log fields | unit | `go test ./internal/obs/... -run TestLogFields -count=1` | Wave 0 |
| OBS-04 | Startup log with version + config | unit | `go test ./internal/obs/... -run TestStartupLog -count=1` | Wave 0 |
| DPL-01 | systemd unit file | manual | Check file exists + valid syntax | N/A |

### Sampling Rate
- **Per task commit:** `go test ./internal/adb/... ./internal/scrcpy/... -short -count=1`
- **Per wave merge:** `go test ./... -count=1 -race`
- **Phase gate:** Full suite green + manual tests (THIRD_PARTY_NOTICES, systemd unit, real device)

### Wave 0 Gaps
- [ ] `internal/adb/reverse_test.go` -- covers ADB-03 (reverse:forward wire format against fake ADB listener)
- [ ] `internal/adb/fake_adb_test.go` -- shared fake ADB net.Listener for all ADB tests
- [ ] `internal/scrcpy/video_reader_test.go` -- covers SCR-03 (frame header parsing with fixture bytes)
- [ ] `internal/scrcpy/testdata/` -- fixture bytes from scrcpy v3.3.4 codec metadata + frame headers
- [ ] `internal/api/auth_test.go` -- covers AUTH-01 through AUTH-05
- [ ] `internal/obs/logging_test.go` -- covers OBS-03, AUTH-03 (key redaction)
- [ ] Go 1.24+ install: upgrade from 1.22.4 before any code can be compiled

## Security Domain

### Applicable ASVS Categories

| ASVS Category | Applies | Standard Control |
|---------------|---------|-----------------|
| V2 Authentication | yes | API-key auth via `X-API-Key` header; constant-time compare |
| V3 Session Management | no | No user sessions; gateway uses per-device session lifecycle, not auth sessions |
| V4 Access Control | yes | All endpoints require valid API key; no role differentiation in Phase 1 |
| V5 Input Validation | yes | Serial validation (alphanumeric only); session ID validation (UUID format) |
| V6 Cryptography | yes | SHA-256 hashing for key comparison; `crypto/subtle.ConstantTimeCompare` |

### Known Threat Patterns for Go ADB Gateway

| Pattern | STRIDE | Standard Mitigation |
|---------|--------|---------------------|
| Timing oracle on API key comparison | Information Disclosure | `crypto/subtle.ConstantTimeCompare` on SHA-256 hashes |
| API key in URL query string (logged by proxies) | Information Disclosure | Prefer `X-API-Key` header; document query param risk |
| API key in logs | Information Disclosure | Custom slog handler redacts keys; unit test scans logs for key literals |
| Unauthenticated endpoint access | Spoofing | All routes require API key middleware; WS upgrade validates key before Accept() |
| ADB shell injection via session args | Tampering | Whitelist scrcpy server args; never pass user-supplied strings to `adb shell` |
| scrcpy version mismatch leading to undefined protocol behavior | Tampering | Pin version; embed jar; CI test pushes jar and parses frames |
| Stale reverse tunnel from previous run | Elevation of Privilege | Startup reconciliation removes gateway-owned mappings |
| Gateway bound to 0.0.0.0 | Information Disclosure | Bind to 127.0.0.1 or private interface only |

## Sources

### Primary (HIGH confidence)
- [AOSP ADB SERVICES.TXT](https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/SERVICES.TXT) -- reverse:forward wire format, semicolon separator, command syntax [VERIFIED]
- [scrcpy develop.md](https://github.com/Genymobile/scrcpy/blob/master/doc/develop.md) -- server protocol, frame header, codec metadata, socket order, dummy byte, SCID [VERIFIED]
- [scrcpy server.c](https://github.com/Genymobile/scrcpy/blob/master/app/src/server.c) -- push + app_process invocation, tunnel setup, socket name generation [VERIFIED]
- [scrcpy adb.c](https://github.com/Genymobile/scrcpy/blob/master/app/src/adb/adb.c) -- exact reverse/forward command construction: `localabstract:NAME` + `tcp:PORT` [VERIFIED]
- [scrcpy adb_tunnel.c](https://github.com/Genymobile/scrcpy/blob/master/app/src/adb/adb_tunnel.c) -- reverse tunnel enable/listen/accept sequence [VERIFIED]
- [prife/goadb on Go module proxy](https://proxy.golang.org/github.com/prife/goadb/) -- versions v0.4.2 through v0.4.8 available [VERIFIED]
- [Go module proxy version checks](https://proxy.golang.org/) -- chi v5.2.5, coder/websocket v1.8.14, koanf v2.3.4, prometheus v1.23.2 all confirmed latest [VERIFIED]
- [scrcpy v3.3.4 release](https://github.com/Genymobile/scrcpy/releases/tag/v3.3.4) -- bug-fix only, no protocol changes from v3.3.x [VERIFIED]
- [Tango ADB Development Guide](https://docs.tangoapp.dev/scrcpy/connect-server) -- localabstract socket details, multiplexing warning [VERIFIED]

### Secondary (MEDIUM confidence)
- [electricbubble/gadb](https://github.com/electricbubble/gadb) -- forward/reverse wire protocol example in Go (forward only; confirms wire format)
- [codeskyblue/go-adbkit](https://github.com/codeskyblue/go-adbkit) -- Go ADB client with both forward and reverse support (reference for wire format)
- [Caio Ferreira -- Secure API Key Middleware in Go](https://caiorcferreira.github.io/post/golang-secure-api-key-middleware/) -- SHA-256 + ConstantTimeCompare pattern
- [crypto/subtle package docs](https://pkg.go.dev/crypto/subtle) -- ConstantTimeCompare API, length-dependent early return

### Tertiary (LOW confidence -- needs validation)
- prife/goadb shell:v2 on Android 14/15/16 -- needs real device testing
- Exact reverse:list-forward output format for device-bound connections -- needs testing against real adbd
- scrcpy v3.3.4 server.jar exact SHA-256 -- needs download and verification

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - versions verified against Go module proxy and upstream repos
- Architecture: HIGH - scrcpy protocol details verified against develop.md and source code; ADB wire format verified against AOSP SERVICES.TXT
- Pitfalls: HIGH - scrcpy and ADB pitfalls validated against official sources and community reports
- Reverse-forward implementation: HIGH - wire format verified against AOSP SERVICES.TXT and scrcpy source; localabstract socket model confirmed
- scrcpy v3.3.4 specifics: MEDIUM - protocol unchanged from v3.3.x confirmed; exact jar download and SHA-256 need verification

**Research date:** 2026-05-06
**Valid until:** 2026-06-06 (30 days; stable domain but watch for Go/scrcpy minor releases)