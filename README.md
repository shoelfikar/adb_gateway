# ADB Gateway

A Go service that turns a fleet of USB-attached Android devices into a clean
HTTP/WebSocket API. It speaks the ADB wire protocol on one side and the
[scrcpy](https://github.com/Genymobile/scrcpy) server protocol on the other,
so upstream applications can stream video/audio and send input to many
devices at once without ever touching ADB or scrcpy themselves.

The gateway is designed to run on a Linux host with `adbd` available
locally, expose a REST + WebSocket surface guarded by an API key, and scale
to **20–30 concurrent device sessions per instance** (horizontal scaling via
Redis is planned in a future phase).

---

## Highlights

- **Single binary**, embedded scrcpy `server.jar` — no separate runtime to
  deploy.
- **REST + WebSocket** API, multiplexable from any frontend.
- **Per-device supervisor** with bounded contexts, drop-on-slow fan-out,
  and graceful recovery from `adbd` restarts.
- **Reservation/lease model** so multiple viewers can watch one device but
  only the lease holder sends input.
- **Hardened systemd unit** and installer ship in every release tarball.
- **Prometheus `/metrics`** exposed out of the box.

---

## Features

### Streaming
- **Video stream** (`/devices/{serial}/video`) — raw H.264 / H.265 / AV1
  bytes relayed from scrcpy; no server-side decode.
- **Audio stream** (`/devices/{serial}/audio`) — Opus / AAC / raw / FLAC
  output, mic, or playback capture.
- **Control channel** (`/devices/{serial}/control`) — tap, swipe, text,
  hardware keys, clipboard, rotation.
- **Combined session stream** (`/devices/{serial}/session`) — single
  WebSocket carrying video + audio + control.

### Device & session management
- List devices with their session state (`GET /devices/`).
- Create / delete a scrcpy session per device, with per-request codec / bit
  rate / FPS / size overrides.
- Restart a stuck (sticky-Failed) session via `POST /devices/{serial}/restart`.
- Connect / disconnect TCP/IP-attached devices
  (`POST /devices/connect`, `DELETE /devices/connect/{serial}`).
- Reboot or power off a device (`POST /devices/{serial}/reboot`,
  `POST /devices/{serial}/shutdown`).

### Reservations (multi-viewer control)
- Create, extend, release a reservation lease for a device. Only the lease
  holder may send control input; other clients still receive video/audio.

### File browser
- List, stat, mkdir, upload (file + folder), download (file + folder),
  rename, recursive delete — all bounded by allow-listed device paths and
  rate-limited per API key.

### App manager
- List installed apps, fetch package details, export the APK, launch,
  backup, and uninstall.

### Diagnostics & capture
- Live **logcat** stream (`GET /devices/{serial}/logcat`).
- On-demand **screenshot** (`POST /devices/{serial}/screenshot`).
- **Screen recording** start / list / stop
  (`POST|GET|DELETE /devices/{serial}/recordings`).
- **APK install** from upload (`POST /devices/{serial}/apks`).

### Observability & ops
- `GET /healthz` — version + build SHA, no auth required.
- `GET /metrics` — Prometheus exposition (sessions, fan-out drops,
  adb-reconnect counters, etc.), no auth required.
- Structured JSON logs via `log/slog`.
- `--version` and `--licenses` flags for build inspection and third-party
  attribution.

---

## Architecture at a glance

```
┌──────────────┐    HTTP/WS    ┌──────────────┐   ADB wire    ┌────────┐
│  Frontend /  │ ◄──────────►  │ adb-gateway  │ ◄──────────►  │  adbd  │
│   upstream   │   (REST + WS) │   (this app) │  localhost    │        │
└──────────────┘               └──────┬───────┘                └────┬───┘
                                      │ scrcpy proto                │ USB
                                      ▼                             ▼
                                ┌──────────────┐              ┌──────────┐
                                │ scrcpy server│              │ Android  │
                                │   (on dev)   │              │  device  │
                                └──────────────┘              └──────────┘
```

- **chi/v5** for routing, **coder/websocket** for WebSockets,
  **prife/goadb** for the bulk of the ADB protocol, an in-house
  `reverse:forward` helper for the parts goadb doesn't cover, and
  **scrcpy server.jar** embedded via `//go:embed`.
- One supervisor goroutine tree per device, per-device mutex (never global),
  drop-on-slow fan-out hubs with bounded buffers.

---

## Install

### Production: pre-built release (Ubuntu / Debian, systemd)

Every tagged release publishes archives for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, and `windows/amd64`. The Linux/macOS
archives bundle the binary, a systemd unit, and an installer script.

```bash
# 1. Install adb on the host (the gateway talks to a local ADB server).
sudo apt update && sudo apt install -y adb
adb start-server

# 2. Download and extract the latest release for your arch.
VERSION=v0.1.0
ARCH=linux_amd64        # or linux_arm64
curl -LO https://github.com/<owner>/adb-gateway/releases/download/$VERSION/adb-gateway_${VERSION}_${ARCH}.tar.gz
tar xzf adb-gateway_${VERSION}_${ARCH}.tar.gz
cd adb-gateway_${VERSION}_${ARCH}

# 3. Install and enable the service.
sudo ./install.sh

# 4. Set your API key, then restart.
sudo "$EDITOR" /etc/adb-gateway/config.yaml          # set api_key_primary
sudo systemctl restart adb-gateway

# 5. Watch it run.
systemctl status adb-gateway
journalctl -u adb-gateway -f
```

`install.sh` is idempotent. It will:

- Create the `adb-gateway` system user and group.
- Install the binary to `/usr/local/bin/adb-gateway`.
- Drop the systemd unit at `/etc/systemd/system/adb-gateway.service`.
- Seed `/etc/adb-gateway/config.yaml` **only if it does not already exist**
  (otherwise it ships the new release's example side-by-side as
  `config.yaml.example` so you can diff).
- Prepare `/var/lib/adb-gateway` for runtime state (recordings, etc.).
- `daemon-reload`, enable, and start/restart the service.

To uninstall:

```bash
sudo ./uninstall.sh           # keeps /etc/adb-gateway and the user
sudo ./uninstall.sh --purge   # also removes config, state, user, and group
```

### From source

Requires Go 1.24+ (the project is currently on Go 1.25).

```bash
git clone https://github.com/<owner>/adb-gateway.git
cd adb-gateway
go build -o adb-gateway ./cmd/gateway
./adb-gateway --version

# Run with a config file.
./adb-gateway --config ./config.yaml
```

Build with version info embedded:

```bash
go build \
  -ldflags "-X main.buildVersion=$(git describe --tags) -X main.buildSHA=$(git rev-parse --short HEAD)" \
  -o adb-gateway ./cmd/gateway
```

### Cutting a release (maintainers)

Releases are produced by [GoReleaser](https://goreleaser.com) on tag push:

```bash
git tag v0.1.0
git push origin v0.1.0
# GitHub Actions runs `goreleaser release --clean` and publishes the archives.
```

For a local dry run:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish
```

---

## Configuration

The gateway reads `config.yaml` (path overridable via `--config`). Selected
fields:

| Key                  | Purpose                                           |
|----------------------|---------------------------------------------------|
| `listen_addr`        | HTTP bind address (default `127.0.0.1:8000`).     |
| `adb_addr`           | Local ADB server (default `localhost:5037`).      |
| `log_level`          | `debug` / `info` / `warn` / `error`.              |
| `api_key_primary`    | **Required.** Authenticates API callers.          |
| `api_key_secondary`  | Optional second key for zero-downtime rotation.   |
| `scrcpy.*`           | Default codec / bit rate / FPS / size / audio.    |
| `stream.*`           | Per-viewer buffer size, drop thresholds.          |
| `files.*`            | File-browser allow-listed roots and limits.       |

Any setting can also be supplied via environment variable
(`ADB_GW_API_KEY_PRIMARY=…`, etc.) — useful for systemd
`EnvironmentFile=/etc/adb-gateway/adb-gateway.env`.

### Authentication

Every request to `/devices/*` requires one of:

```
Authorization: Bearer <api_key>
```

`/healthz` and `/metrics` are intentionally unauthenticated so external
monitoring can scrape them; restrict network access at the bind address or
behind a reverse proxy if that is not acceptable.

---

## Operations cheat sheet

```bash
# List devices
curl -H "Authorization: Bearer $KEY" http://localhost:8000/devices/

# Connect a TCP/IP device
curl -H "Authorization: Bearer $KEY" -X POST http://localhost:8000/devices/connect \
     -d '{"host":"192.168.1.10","port":5555}'

# Start a session
curl -H "Authorization: Bearer $KEY" -X POST \
     http://localhost:8000/devices/$SERIAL/sessions \
     -d '{"max_fps":30,"bit_rate":4000000}'

# Tail logs
journalctl -u adb-gateway -f

# Scrape metrics
curl http://localhost:8000/metrics | grep adb_gateway
```

---

## Compatibility & constraints

- **Linux host with systemd**, dedicated to this gateway and its
  USB-attached fleet.
- **Local ADB only by default** — gateway expects `adbd` reachable on
  `localhost:5037`. (A TCP/IP `host:connect` endpoint exists but is opt-in.)
- **No server-side media decode** — the gateway relays raw codec bytes;
  decoding is the browser/client's responsibility.
- **Pinned scrcpy `server.jar`** version per release; mixing versions is the
  most common breakage source.

---

## License & third-party notices

The gateway embeds an Apache-2.0 licensed scrcpy `server.jar`. Run
`adb-gateway --licenses` (or read [`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES))
for the full attribution.
