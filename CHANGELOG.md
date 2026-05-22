# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Once releases start being tagged, entries graduate from **Unreleased** into a
versioned section. The release workflow ([.github/workflows/release.yml](.github/workflows/release.yml))
will also generate a per-release changelog from conventional commits — this
file is the curated, human-readable narrative on top of that.

---

## [Unreleased]

Initial preparation for the `v0.1.0` cut. Everything below has shipped on
`main` but has not yet been bundled into a tagged release.

### Added

#### Build, release, and ops
- **GoReleaser-based release pipeline** ([.goreleaser.yaml](.goreleaser.yaml),
  [.github/workflows/release.yml](.github/workflows/release.yml)). Push a
  `v*` tag and GitHub Actions builds for `linux/amd64`, `linux/arm64`,
  `darwin/amd64`, `darwin/arm64`, and `windows/amd64`, then publishes a
  release with checksums and a conventional-commits changelog.
- **Hardened systemd installer** ([scripts/install.sh](scripts/install.sh),
  [scripts/uninstall.sh](scripts/uninstall.sh),
  [scripts/adb-gateway.service](scripts/adb-gateway.service)) bundled in
  every Linux archive. Creates a system user, installs the binary, seeds
  config without overwriting, and enables the service.

#### API surface
- **TCP/IP device connect** (`POST /devices/connect`,
  `DELETE /devices/connect/{serial}`) so the gateway can issue
  `host:connect:<host>:<port>` against the local ADB server on behalf of
  upstream callers. Opt-in to the otherwise-USB-only constraint.
- **Unified session WebSocket** (`GET /devices/{serial}/session`) carrying
  video + audio + control over a single connection.
- **App manager** under `/devices/{serial}/apps/...`:
  list installed packages, fetch details, export APK (single & split),
  launch, backup, uninstall.
- **File browser** under `/devices/{serial}/files`: list, stat, mkdir,
  rename, recursive delete, upload (file & folder), download (file &
  folder, streamed as tar).
- **APK install** (`POST /devices/{serial}/apks`) with rate-limit and
  per-device concurrency guard.
- **Screen recording** (`POST|GET|DELETE /devices/{serial}/recordings`) —
  MKV via scrcpy's recording subscriber.
- **Logcat live stream** (`GET /devices/{serial}/logcat`).
- **On-demand screenshot** (`POST /devices/{serial}/screenshot`).
- **Device power** — `POST /devices/{serial}/reboot` and
  `POST /devices/{serial}/shutdown`.
- **Manual session restart** (`POST /devices/{serial}/restart`) to recover
  sticky-Failed devices.
- **Reservation leases** for multi-viewer control: only the lease holder
  can send input, all clients still receive media.
- **Audio stream** (`GET /devices/{serial}/audio`) with codec/source
  override per session.
- **Per-request scrcpy overrides** on session create — codec, bit rate,
  max FPS, max size, audio codec, audio source.

#### Platform
- **Per-device supervisor** (`errgroup`-managed) with the per-device mutex
  discipline that prevents global blocking.
- **Stall watchdog** + recovery orchestrator for stuck sessions.
- **`StateReconnecting`** in the device FSM so transient ADB disconnects
  no longer surface as terminal failures.
- **Hub fan-out** with bounded per-viewer buffers, late-joiner keyframe
  cache, and drop-on-slow eviction.
- **Single-writer control goroutine** with byte-exact marshal for all
  18 scrcpy v3.3.4 control message types.
- **Audio reader and device-message reader** plumbed into the session
  pipeline.
- **ADB reconnection loop** in `main.go` — gateway survives `adbd`
  restarts and re-establishes its goadb client cleanly.
- **In-house `reverse:forward` helper** against the AOSP `SERVICES.TXT`
  wire format (no Go ADB library implements it).
- **`prife/goadb`-backed host services**: list devices, track devices,
  shell v2, sync push, daemon command, server version.
- **Streaming ADB primitives** (`internal/adb/shell.go`) for handlers that
  need to consume long-running command output.
- **scrcpy v3.3.4 server.jar** vendored and embedded via `//go:embed`,
  with pinned SHA-256.

#### Observability
- **Prometheus collectors** for sessions, lease state, fan-out drops,
  ADB-reconnect counters, and frame counters — exposed at `/metrics`.
- **Structured `log/slog` JSON logs** with API-key redaction.
- **`GET /healthz`** with build version and SHA.
- **`--version` and `--licenses` CLI flags** for build inspection and
  third-party attribution.

#### Configuration
- **`koanf/v2`-based config** with YAML + env (`ADB_GW_*`) providers.
- **`scrcpy.*` keys** to set default codec / bit rate / FPS / size /
  audio.
- **`files.*` allow-list** restricting file-browser operations to a set
  of device-side roots.
- **`stream.*` knobs** for per-viewer buffer frames, max consecutive
  drops, audio toggle.

#### Security
- **API-key authentication** middleware with primary + secondary key
  rotation and constant-time comparison.
- **Per-key write rate-limiter** on file/app write endpoints (default
  30/min/key, configurable).
- **Path validator** that rejects device paths outside the allow-list,
  blocks traversal, and enforces canonical separators.
- **CORS middleware** with `Vary: Origin` for correct cache behaviour.
- **API responses scrubbed** of internal error details — only
  `DomainError` codes reach the client.

### Fixed

- File-browser rename now uses a bounded context for the underlying
  `ShellV2Stream`, so a stalled device can no longer leak the goroutine.
- `DownloadFolder` propagates the request context into every ADB call,
  making cancellations effective end-to-end.
- `io.ReadAll` errors during file ops are now logged instead of silently
  discarded.
- Peek-on-stream error paths cancel before returning, preventing a brief
  goroutine leak on backup/export.
- `mapError` no longer leaks internal error text to API consumers.
- `keyLimiter` evicts stale entries and caps memory, so a flood of unique
  API keys can no longer grow it unboundedly.
- CORS middleware emits `Vary: Origin` so caches do not serve a response
  computed for a different origin.
- `WatchDevices` now treats `device`, `recovery`, and `offline` as
  connect states, keeping offline devices visible to the session manager.
- WebSocket write-only handlers call `ws.CloseRead` to drain the read
  side; HTTP server timeouts adjusted so long-lived WS connections are
  not killed by `ReadTimeout`.
- Per-device launch lock is released **before** the long-running scrcpy
  launch, so concurrent requests for other devices are no longer
  serialized behind it.
- Data race in `DeviceSession.Run` closer goroutine resolved by tightened
  locking around `entry.Session`.
- `IsSessionActive` reads fields directly when the caller already holds
  the lock, preventing a re-entry deadlock.
- ADB reconnect resource leak: `ReleaseResources` + rewritten
  `MarkAllDisconnected` clear all registry entries instead of leaving
  Failed husks behind.
- env-provider key transformation now preserves underscores
  (`ADB_GW_LOG_LEVEL` → `log_level`, not `loglevel`).
- `crypto/rand` (not `math/rand`) used for SCID generation in reverse
  tunnel setup.

### Security

- The `/healthz` and `/metrics` endpoints are intentionally
  unauthenticated; bind to `127.0.0.1` or restrict via reverse proxy if
  your environment cannot expose them.
- `config.yaml` shipped in repo is a working example, **not a template** —
  rotate `api_key_primary` before deploying. The release archive ships it
  as `config.yaml.example`.

---

## Project milestones (pre-release narrative)

Until tagged releases begin, here is the phase-by-phase summary that the
repo grew through. Once a tag is cut, the corresponding rows fold into the
versioned sections above.

| Phase | Status | Highlights |
|-------|--------|-----------|
| **Phase 1 — Single-Device Streaming Foundation** | complete | Project scaffold, koanf config, structured logging, API-key auth, ADB client + smart-sockets codec, host services, in-house `reverse:forward`, device registry, session FSM, ADB reconnection loop. |
| **Phase 2 — Multi-Client Control** | complete | Prometheus collectors, Hub fan-out with late-joiner cache & slow-consumer eviction, scrcpy control message marshal table, single-writer control goroutine, reservation lease state machine, audio reader, device message reader, REST + WS wiring, soak tests, WS lifecycle fix, metrics gap closure. |
| **Phase 3 — Multi-Device Fleet** | complete | Path validator, Phase 3 error sentinels, streaming ADB primitives, stall watchdog + recovery orchestrator, `StateReconnecting`, manual `RestartSession`, logcat live stream, screenshot, basic file push/pull/delete, APK install, MKV screen recording. |
| **Phase 3.1 — File Browser + App Manager** | complete | Recursive file ops (mkdir/rename/recursive-delete), upload-folder + download-folder via tar streaming, app manager (list/details/launch/backup/uninstall/APK export), per-key write rate limiter, FilesDispatcher op-verb routing, full plan-by-plan code review with all WR-/CR- findings resolved. |
| **Phase 4 — Horizontal Scaling** | not started | Planned: Redis-backed device registry, sticky session load balancing, cross-instance coordination. |

---

[Unreleased]: https://github.com/shoelfikar/adb_gateway/compare/v0.1.0...HEAD
