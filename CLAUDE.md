<!-- GSD:project-start source:PROJECT.md -->
## Project

**ADB Gateway**

A Go-based backend service that acts as a gateway between the `pelni_server` application and a fleet of USB-connected Android devices. It exposes a REST API for device management/control and WebSocket endpoints for real-time video, audio, and input streams — using the scrcpy Android server for capture and ADB as the device transport. Pelni server proxies these endpoints to its own browser frontend; this project does not ship a UI.

**Core Value:** Reliable, low-latency streaming and control of many physical Android devices, exposed as a clean API that `pelni_server` can embed without needing to understand ADB or scrcpy internals.

### Constraints

- **Tech stack**: Go for the gateway service — required for performance and concurrency, matches the prior architecture decision.
- **Streaming agent**: scrcpy `server.jar` (Apache-2.0) — must comply with attribution requirements (ship `THIRD_PARTY_NOTICES`); vendor a pinned version with the gateway and embed via `//go:embed`.
- **ADB transport**: Local ADB server only (no remote ADB) — devices are USB-attached.
- **No media decode**: Server must relay raw bytes; encoding/decoding is the device's and browser's job.
- **Auth**: API-key based, validated against a value provisioned by `pelni_server` — keep it simple in v1; revisit if/when multi-tenant requirements appear.
- **Deployment target**: Linux host with systemd, dedicated to this gateway and its USB-attached fleet.
- **Performance**: 1 instance ≈ 20–30 concurrent device sessions; horizontal scaling via Redis + load balancer beyond that.
- **Compatibility**: Must coexist with a system-installed `adb` (no port conflicts) and survive `adbd` restarts gracefully.
<!-- GSD:project-end -->

<!-- GSD:stack-start source:research/STACK.md -->
## Technology Stack

## TL;DR
| Concern | Pick | One-liner |
|---|---|---|
| ADB protocol library | **`github.com/prife/goadb` v0.4.4** + a small in-house `reverse:forward` helper | Best-maintained Apache-2.0 fork; covers host/transport/shell/sync. Reverse-forward is missing in *every* Go ADB library and we must add it (~150 LOC against `localhost:5037`). |
| HTTP router | **`github.com/go-chi/chi/v5` v5.2.5** | `net/http`-compatible, ~1k LOC, ideal for a relay-heavy service that mostly upgrades to WS. |
| WebSocket | **`github.com/coder/websocket` v1.8.14** | Successor to `nhooyr.io/websocket`; zero-alloc reads/writes, `context.Context`-first, actively maintained. |
| Config | **`github.com/knadh/koanf/v2` v2.3.4** | Modular, no global state, no key-lowercasing footgun (vs viper). |
| Logging | **`log/slog`** (stdlib, Go 1.21+) | Structured logs without an extra dep; pair with `slog-multi` if needed. |
| Metrics | **`github.com/prometheus/client_golang` v1.23.2** | Canonical. |
| Redis | **`github.com/redis/go-redis/v9` v9.19.0** | Officially recommended by Redis; built-in pubsub + pool. |
| Process / scrcpy server | Vendor **`scrcpy-server-v3.x.jar`** under `internal/scrcpy/assets/` and embed via `//go:embed` | Pin a specific version; ship one canonical build. |
| Testing | stdlib `testing` + **`github.com/stretchr/testify` v1.10+** + a fake-ADB `net.Listener` | Real devices in nightly only. |
| Go version | **Go 1.24** (current stable) | `slog`, `http.ServeMux` patterns, `context.AfterFunc` are all useful. |
## Core Technologies
| Technology | Version | Purpose | Why for this domain |
|---|---|---|---|
| Go | 1.24 (or current stable when starting) | Runtime / language | Required by constraints; goroutine-per-stream model is exactly what ADB relays need. |
| `prife/goadb` | v0.4.4 (2025-09-10) | ADB client — host/devices, transport, shell:, sync push/pull | Best-maintained Go ADB lib (Apache-2.0, fork of `zach-klippenstein/goadb` with shell-v2, contexts, push fixes). |
| In-house `reverse:forward` helper | — | Reverse-tunnel setup `tcp:27183 → tcp:27183`, etc. | **Mandatory.** No Go ADB library implements `reverse:forward` (verified against prife/goadb, gadb, zach-klippenstein/goadb). Wire format is documented in [SERVICES.TXT](https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/SERVICES.TXT); ~150 LOC over a `net.Conn` to `localhost:5037`. |
| `go-chi/chi/v5` | v5.2.5 (2026-02-05) | HTTP router | Composable middleware, `net/http`-compatible — keeps WS upgrade trivial; minimal allocations on hot paths. |
| `coder/websocket` | v1.8.14 (2025-09-06) | WebSocket server | Zero-alloc Read/Write, `context.Context` propagation, permessage-deflate; successor to nhooyr.io/websocket. |
| `redis/go-redis/v9` | v9.19.0 (2026-04-28) | Redis client | Connection pool, native pubsub, OTel hooks. |
| `prometheus/client_golang` | v1.23.2 (2025-09-05) | Metrics | Standard `/metrics` integration, native histograms. |
| `knadh/koanf/v2` | v2.3.4 (2026-03-21) | Config | Read YAML/env/flag without viper's global-state and case-folding pitfalls. |
| `log/slog` | stdlib (Go 1.21+) | Structured logging | Stdlib-blessed; no third-party dependency; JSON handler is production-grade. |
## Supporting Libraries
| Library | Version | Purpose | When to use |
|---|---|---|---|
| `github.com/stretchr/testify` | v1.10.x | Assertions, mocks, suites | Default test assertions; keep `mock` use minimal — prefer fakes. |
| `github.com/google/uuid` | v1.6.x | Session/client IDs | Per-session, per-client IDs for log correlation. |
| `github.com/cenkalti/backoff/v4` | v4.x | Reconnect/retry on `adbd` | ADB server can restart; backoff helps avoid tight loops. |
| `github.com/oklog/run` | v1.x | Goroutine lifecycle / actor pattern | Clean fan-in shutdown of per-device supervisor goroutines (video/audio/control/health). Optional but excellent fit. |
| `golang.org/x/sync/errgroup` | latest | Bounded goroutine groups | Per-device session lifecycle if you skip `oklog/run`. |
| `github.com/spf13/pflag` | latest | POSIX-style flags | Pair with koanf via `koanf/providers/posflag`. |
| `golang.org/x/time/rate` | latest | Rate limit per API key | Lightweight token bucket for control endpoint protection. |
| `github.com/prometheus/client_golang/prometheus/promhttp` | (bundled) | `/metrics` handler | Standard Prom exposition. |
| `github.com/coreos/go-systemd/v22` | v22.x | `sd_notify` ready/watchdog signals | If you adopt `Type=notify` in the unit file (recommended). |
## Development Tools
| Tool | Purpose | Notes |
|---|---|---|
| `golangci-lint` | Static analysis | Pin in `.golangci.yml`; enable `gosec`, `bodyclose`, `errcheck`, `revive`. |
| `gotestsum` | Test runner UX | Friendly CI output; JUnit export. |
| `goreleaser` | Build + release | Single Linux/amd64+arm64 tarball with embedded `server.jar`. |
| `air` | Live reload (dev only) | Optional; nice when iterating on REST handlers. |
| `pprof` (stdlib) | CPU/heap/goroutine profiling | Critical — relay services must profile under fan-out load. |
## ADB Library — Detailed Comparison
| Library | Latest | License | Maintenance | `host:devices` | `transport:` | `shell:` | `sync:` (push/pull) | `reverse:forward` | Verdict |
|---|---|---|---|---|---|---|---|---|---|
| **`prife/goadb`** | v0.4.4 (2025-09-10) | Apache-2.0 | **Active** (7 releases in 2025; ctx, shell-v2, sync rework) | Yes | Yes | Yes (v1+v2) | Yes (rewritten in v0.3) | **No** (forward only) | **Adopt** as base |
| `electricbubble/gadb` | (last commits ~2024) | MIT | Stale (~21 commits, no tagged releases) | Yes | Yes | Yes | Push/Pull | **No** | Skip — limited surface, low activity |
| `zach-klippenstein/goadb` | (no releases, 74 commits, ~2018) | Apache-2.0 | Dormant (16 open issues) | Yes | Yes | Yes | Push/Pull | **No** | Skip — superseded by prife fork |
| Roll our own | — | — | — | — | — | — | — | — | Don't — sync protocol is fiddly; reuse prife |
## HTTP Router — Why chi over alternatives
| Option | Verdict |
|---|---|
| `net/http` (stdlib, Go 1.22 ServeMux) | Workable, but middleware composition is tedious for this surface (auth, logging, request-id, metrics, rate-limit). |
| **`go-chi/chi/v5`** | **Pick.** Stdlib-compatible `http.Handler`, ~1k LOC core, mature middleware, no global state. Trivial to upgrade to coder/websocket. |
| `gin` | Heavier, custom context type, harder to compose with `http.Handler` middleware (e.g., promhttp). |
| `echo` | Same concerns as gin; opinionated context. |
| `fiber` | Built on `fasthttp` — incompatible with stdlib `net/http`, **breaks `coder/websocket`** (and most WS libs). Disqualified. |
## WebSocket — Why coder/websocket over gorilla
- `gorilla/websocket` v1.5.3 (2024-06) was un-archived in 2024, but development has slowed; API still requires manual buffer management and lacks `context.Context` propagation.
- `nhooyr.io/websocket` is now `coder/websocket` (Coder, Inc. took over maintenance from nhooyr in 2024). Same import-compatible API surface under the new path.
- For a fan-out relay where each device pushes to many clients, **zero-alloc writes** matter (`coder/websocket`'s `Writer` reuses buffers). Gorilla allocates per `WriteMessage`.
- `coder/websocket` integrates cleanly with `chi` via `websocket.Accept(w, r, opts)`.
## Config — koanf over viper
- Forces lowercase keys (breaks structured device-serial-keyed config).
- Pulls in many transitive deps (HCL, YAML, etcd client) regardless of what you use.
- Global state via `package var`.
## Logging — slog stdlib
- No dep, no version drift.
- JSON handler suitable for structured ingestion.
- `slog.With("device", serial)` works perfectly for per-device subloggers.
## scrcpy server.jar Management
| Concern | Decision |
|---|---|
| Sourcing | Download the official release artifact (e.g. `scrcpy-server-v3.0`) from [Genymobile/scrcpy/releases](https://github.com/Genymobile/scrcpy/releases). |
| Pinning | One pinned version per gateway release. Record SHA-256 in `go.sum`-adjacent `assets.sha256`. |
| Packaging | Embed via `//go:embed assets/scrcpy-server-v3.0`. Push to `/data/local/tmp/scrcpy-server.jar` with a per-deploy filename to avoid stomping system scrcpy. |
| Compatibility | Pin a *single* matching server.jar version. Mixing versions is the most common breakage source. |
| License | scrcpy is Apache-2.0 — record NOTICE in `THIRD_PARTY_LICENSES.md`. (The scope doc says "BSD-style" — that's incorrect; verify and update.) |
## Installation
# Bootstrap module
# Core
# Supporting
# Dev / test
## Alternatives Considered
| Recommended | Alternative | When the alternative wins |
|---|---|---|
| `prife/goadb` + custom reverse helper | Shell out to `adb` binary | Only if you need 100% feature parity with `adb` CLI fast (e.g., one-off scripts). For long-lived services, sub-process management of `adb` is brittle. |
| `prife/goadb` | `electricbubble/gadb` | If you need only push + shell and want the smallest dep — but then we're already pulling reverse logic in-house, prife costs nothing more. |
| `chi` | stdlib `net/http` (Go 1.22 patterns) | If middleware count is small (≤3) and you want zero deps. |
| `coder/websocket` | `gorilla/websocket` | If you depend on a gorilla-only feature (legacy compression options, specific subprotocol helpers) or have existing gorilla code to reuse. |
| `koanf` | `viper` | If your team already has heavy viper investment. New code should not start there. |
| `slog` | `zerolog` / `zap` | Only with profiler evidence that slog allocations are a hot path. Unlikely in a relay (network is the bottleneck). |
| `go-redis/v9` | `redigo` | Lower-level / hand-pipelined commands needed; we don't. |
| Embed `server.jar` | Download at runtime | Air-gapped deploys / CDN-controlled updates. Not our case. |
## What NOT to Use
| Avoid | Why | Use instead |
|---|---|---|
| `zach-klippenstein/goadb` | Dormant since ~2018, no releases, missing shell-v2 + ctx APIs. | `prife/goadb` (active fork). |
| `electricbubble/gadb` | Few commits, no tagged releases, narrow feature set, no reverse-forward. | `prife/goadb`. |
| `gofiber/fiber` | Built on `fasthttp` — incompatible with stdlib `http.Handler`, breaks every WS lib that expects `http.Hijacker`. | `chi` on `net/http`. |
| `gorilla/mux` | Even gorilla maintainers steer new projects toward chi/stdlib; mux is feature-frozen. | `chi`. |
| `gin-gonic/gin` for relay services | Custom context/handler type complicates middleware reuse with `promhttp` and `coder/websocket`. | `chi`. |
| `spf13/viper` (for new services) | Forces key lowercasing, large dep graph, global state. | `koanf/v2`. |
| `sirupsen/logrus` | Maintenance mode for years; no structured-first API. | `log/slog`. |
| `gomodule/redigo` | Lower-level, no built-in pool; community has consolidated on go-redis. | `go-redis/v9`. |
| Raw goroutines without supervisor | Per-device fan-out without lifecycle management leaks goroutines on `adbd` restart. | `oklog/run` or `errgroup` + structured shutdown. |
| Server-side video decode (any lib) | Out of scope per PROJECT.md; would blow the 20–30-device-per-instance budget. | Relay raw H.264 bytes only. |
| Mixed scrcpy server.jar versions across deploys | Top reported source of "screen black, no error" bugs. | Pin one server.jar version per gateway release. |
## Stack Patterns by Variant
- Skip Redis pubsub for streaming; use it only for cross-instance device-registry coordination.
- `Type=notify` systemd + `sd_notify` watchdog is sufficient — no orchestrator needed.
- Sticky session by `device_id` at the LB (HAProxy `balance source` or `hash`-based) — a WS for device X must always land on the instance that owns the USB.
- Redis stores `{device_id → instance_id}`; instances publish presence on `gateway:devices` channel.
- LB routes `/devices/{id}/stream` based on Redis lookup or sidecar router.
- Keep `coder/websocket` for control plane; introduce `pion/webrtc` v4 for media plane only.
- Don't try to do both transports in v1.
- Profile first (`pprof`). Likely culprits: JSON marshal in WS write loop, per-frame allocations, GC churn from buffered channels.
- Consider `bufio.Reader.WriteTo(net.Conn)` style zero-copy splice paths between ADB socket and WS writer.
## Version Compatibility
| Pair | Notes |
|---|---|
| Go 1.24 + all libs above | All compatible. `slog` requires Go ≥1.21. |
| `coder/websocket` + `chi` | Native — `websocket.Accept(w, r)` on a chi handler works directly. |
| `coder/websocket` + `fasthttp` | **Incompatible.** Reason to avoid `fiber`. |
| `prife/goadb` + Android 14/15 devices | Verified path: shell-v2 transport added in v0.4.0 fixes some Android 14+ cases. Re-test on real hardware. |
| `go-redis/v9` + Redis 6.x | Officially supported is Redis 8.0/8.2/8.4; v9 still works against Redis 6.2+ in practice but pin Redis 7+ for production. |
| `prometheus/client_golang` v1.23 + Prometheus server 2.50+ | Native histograms require server ≥2.40. |
| scrcpy `server.jar` ↔ Android API level | Server v3.x requires Android 5.0+ (API 21). Bundled `app_process` invocation matches scrcpy's documented protocol. |
## Sources
- [github.com/prife/goadb](https://github.com/prife/goadb) — confirmed v0.4.4 (2025-09-10), Apache-2.0, active.
- [pkg.go.dev/github.com/prife/goadb](https://pkg.go.dev/github.com/prife/goadb) — exposed methods (Forward*, Shell, Sync); no reverse-forward.
- [github.com/electricbubble/gadb](https://github.com/electricbubble/gadb) — MIT, low activity, no reverse-forward.
- [github.com/zach-klippenstein/goadb](https://github.com/zach-klippenstein/goadb) — Apache-2.0, dormant since ~2018.
- [github.com/coder/websocket](https://github.com/coder/websocket) — v1.8.14 (2025-09-06), successor to nhooyr.io/websocket.
- [github.com/gorilla/websocket](https://github.com/gorilla/websocket) — v1.5.3 (2024-06-14), un-archived but slow.
- [github.com/go-chi/chi](https://github.com/go-chi/chi) — v5.2.5 (2026-02-05).
- [github.com/redis/go-redis](https://github.com/redis/go-redis) — v9.19.0 (2026-04-28).
- [github.com/prometheus/client_golang](https://github.com/prometheus/client_golang) — v1.23.2 (2025-09-05).
- [github.com/knadh/koanf](https://github.com/knadh/koanf) — v2.3.4 (2026-03-21).
- [Android ADB SERVICES.TXT](https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/SERVICES.TXT) — host:devices, transport:, reverse:forward wire format.
- [scrcpy/doc/develop.md](https://github.com/Genymobile/scrcpy/blob/master/doc/develop.md) — server protocol: dummy byte, codec metadata (12 bytes), 12-byte frame header, options to disable each.
- [scrcpy/server/.../Server.java](https://github.com/Genymobile/scrcpy/blob/master/server/src/main/java/com/genymobile/scrcpy/Server.java) — invocation `CLASSPATH=… app_process / com.genymobile.scrcpy.Server`.
- [redis.uptrace.dev — go-redis vs redigo](https://redis.uptrace.dev/guide/go-redis-vs-redigo.html) — go-redis recommended by Redis itself.
- [Context7 `/coder/websocket`](https://context7.com) — coder/websocket benchmark score 89.5, gorilla 77.9.
- HIGH — versions, maintenance status, license, ADB-library API surface (verified against current source).
- HIGH — coder/websocket vs gorilla recommendation (multiple corroborating sources + Context7).
- MEDIUM — exact line count for in-house reverse-forward helper (estimate based on SERVICES.TXT inspection; refine in Phase 1 spike).
- HIGH — scrcpy protocol header structure (verified from upstream develop.md).
<!-- GSD:stack-end -->

<!-- GSD:conventions-start source:CONVENTIONS.md -->
## Conventions

Conventions not yet established. Will populate as patterns emerge during development.
<!-- GSD:conventions-end -->

<!-- GSD:architecture-start source:ARCHITECTURE.md -->
## Architecture

Architecture not yet mapped. Follow existing patterns found in the codebase.
<!-- GSD:architecture-end -->

<!-- GSD:skills-start source:skills/ -->
## Project Skills

No project skills found. Add skills to any of: `.claude/skills/`, `.agents/skills/`, `.cursor/skills/`, `.github/skills/`, or `.codex/skills/` with a `SKILL.md` index file.
<!-- GSD:skills-end -->

<!-- GSD:workflow-start source:GSD defaults -->
## GSD Workflow Enforcement

Before using Edit, Write, or other file-changing tools, start work through a GSD command so planning artifacts and execution context stay in sync.

Use these entry points:
- `/gsd-quick` for small fixes, doc updates, and ad-hoc tasks
- `/gsd-debug` for investigation and bug fixing
- `/gsd-execute-phase` for planned phase work

Do not make direct repo edits outside a GSD workflow unless the user explicitly asks to bypass it.
<!-- GSD:workflow-end -->



<!-- GSD:profile-start -->
## Developer Profile

> Profile not yet configured. Run `/gsd-profile-user` to generate your developer profile.
> This section is managed by `generate-claude-profile` -- do not edit manually.
<!-- GSD:profile-end -->
