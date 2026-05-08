---
phase: 02-multi-client-control
plan: 05
subsystem: session-lifecycle
tags: [audio-reader, device-message, launcher-extend, errgroup, goroutine-wiring, data-race]

# Dependency graph
requires:
  - phase: 02-01
    provides: config keys (Stream.BufFrames, Stream.MaxConsecutiveDrops), domain errors, metrics
  - phase: 02-02
    provides: Hub fan-out with late-joiner cache (NewHub, Run, Publish, Subscribe, SetCodecMeta)
  - phase: 02-03
    provides: ControlWriter (NewControlWriter, Run, In)
  - phase: 02-04
    provides: LeaseManager (NewLeaseManager, Acquire, Extend, Release, IsHeldBy, ReleaseChanFor)
provides:
  - AudioCodecOPUS/AAC/FLAC/RAW constants, ReadAudioCodecID, ReadAudioFrame (SCR-04)
  - DeviceMessage types (CLIPBOARD, ACK_CLIPBOARD, UHID_OUTPUT), ReadDeviceMessage (SCR-06)
  - LaunchOptions{AudioEnabled, ControlEnabled}, LaunchResult{AudioConn, ControlConn, AudioAvailable, AudioCodec}
  - LaunchWithOptions method on Launcher (one listener, one reverse tunnel, three Accepts in order)
  - DeviceEntry.LeaseManager *LeaseManager + AudioAvailable bool + accessor methods
  - DeviceSession.Run with 4-6 goroutines under errgroup (video, audio, control writer, device message reader)
  - DeviceSession accessor methods: VideoHub, AudioHub, ControlWriter, DeviceMessages, AudioAvailable, AudioCodec
  - SessionOpts{BufFrames, MaxConsecDrops, AudioEnabled} for Phase 2 configuration
affects: [02-06]

# Tech tracking
tech-stack:
  added: []
patterns: [one-listener-three-accepts v3.x scrcpy protocol, local-conn-capture to avoid data race, audio-gated-by-probe]

key-files:
  created:
    - internal/scrcpy/audio_reader.go
    - internal/scrcpy/audio_reader_test.go
    - internal/scrcpy/device_message.go
    - internal/scrcpy/device_message_test.go
  modified:
    - internal/scrcpy/launcher.go
    - internal/scrcpy/launcher_test.go
    - internal/session/registry.go
    - internal/session/supervisor.go
    - internal/session/supervisor_test.go
    - internal/api/handlers_devices.go
    - internal/api/handlers_devices_test.go

key-decisions:
  - "v3.x scrcpy uses ONE listener and ONE reverse tunnel; server connects sequentially video-audio-control (verified against scrcpy source, Pitfall 8)"
  - "Audio probe reads 4-byte codec ID after audio Accept; 0x00000000 or immediate EOF treated as unavailable (defensive parse, A1)"
  - "DeviceMessage reader exits on ErrUnknownDeviceMessage because protocol is length-prefix-stateful"
  - "Conn pointers captured into local variables in Run() to avoid data race with closer goroutine's cleanupResources nil-assignments"
  - "Launcher interface migrated to LaunchWithOptions; Phase 1 Launch() wraps with DefaultLaunchOptions()"
  - "audioReaderLoop and deviceMessageReaderLoop receive conn as parameter instead of reading s.audioConn/s.controlConn"

patterns-established:
  - "Local-conn-capture pattern: in Run(), capture s.audioConn and s.controlConn into locals before spawning goroutines to avoid write-read race with cleanupResources"
  - "Audio-gated-by-probe: audioHub + audioReader goroutines only spawned when AudioAvailable==true && audioConn!=nil (Pitfall 4)"
  - "Single-writer + single-reader on control socket: ControlWriter goroutine for writes, deviceMessageReaderLoop goroutine for reads (Pitfall 5)"

requirements-completed: [SCR-04, SCR-06]

# Metrics
duration: 30min
completed: 2026-05-08
---

# Phase 2 Plan 05: Audio Reader + Device Message + Session Lifecycle Summary

**Audio reader, device message reader, launcher three-socket accept, and DeviceSession errgroup wiring with race-free goroutine lifecycle**

## Performance

- **Duration:** 30 min
- **Started:** 2026-05-08T03:30:00Z
- **Completed:** 2026-05-08T04:04:00Z
- **Tasks:** 3 (all pre-committed; this execution fixed a data race bug)
- **Files modified:** 1 (supervisor.go race fix)

## Accomplishments

### Task 1: scrcpy audio reader + DeviceMessage reader (SCR-04, SCR-06)
- `ReadAudioCodecID`: 4-byte big-endian codec ID read, defensive parse for 0x00000000 and EOF
- `ReadAudioFrame`: delegates to `ReadVideoFrame` (identical 12-byte header + payload layout post-codec-ID)
- `AudioCodec` constants: OPUS (0x6f707573), AAC (0x00616163), FLAC (0x666c6163), RAW (0x00726177)
- `ReadDeviceMessage`: parses CLIPBOARD (0x00), ACK_CLIPBOARD (0x01), UHID_OUTPUT (0x02) with byte-exact wire layout
- `ErrUnknownDeviceMessage`: returned for unknown type byte, causing session restart per protocol design
- `ErrDeviceMessageOversize`: clipboard text capped at 262144 bytes
- 13 tests passing under -race (8 audio, 5 device message)

### Task 2: Launcher extension with LaunchWithOptions (Pitfall 8)
- `LaunchResult` gains AudioConn, AudioLn, ControlConn, ControlLn, AudioAvailable, AudioCodec, ReverseMaps
- `LaunchOptions{AudioEnabled, ControlEnabled}` configures stream types; `DefaultLaunchOptions` preserves Phase 1
- `LaunchWithOptions` uses ONE listener + ONE reverse tunnel (v3.x scrcpy protocol); three sequential Accepts: video, audio, control
- Audio codec probe: `ReadAudioCodecID` after audio Accept, graceful fallback on 0/EOF
- `acceptWithTimeout` helper extracted for reuse across three Accept calls
- Phase 1 `Launch()` wraps `LaunchWithOptions(ctx, serial, DefaultLaunchOptions())` for back-compat
- 8 new launcher tests + all Phase 1 tests passing

### Task 3: DeviceSession wiring under errgroup
- `DeviceEntry` gains `LeaseManager *LeaseManager` and `AudioAvailable bool` with thread-safe accessors
- `Registry` gains `NewRegistryWithOpts` for configurable lease TTL; `GetOrCreate` allocates LeaseManager on creation
- `DeviceSession.Run` spawns 4-6 goroutines under errgroup: videoHub, videoReader, audioHub+audioReader (gated), controlWriter, deviceMessageReader, closer
- `SessionOpts{BufFrames, MaxConsecDrops, AudioEnabled}` for Phase 2 config from main.go
- 6 accessor methods: VideoHub, AudioHub, ControlWriter, DeviceMessages, AudioAvailable, AudioCodec
- `Launcher` interface migrated to `LaunchWithOptions`; mock updated
- Context cancellation tears down all goroutines; errgroup.Wait returns first error

### Data Race Fix (Deviation)
- **Found:** Closer goroutine in `Run()` writes nil to `s.controlConn`, `s.audioConn` via `cleanupResources()` while the main goroutine reads those fields for nil-checks
- **Fix:** Captured conn pointers into local variables before starting errgroup goroutines; passed them explicitly to `audioReaderLoop(ctx, conn)` and `deviceMessageReaderLoop(ctx, conn)`; protected `cleanupResources` nil-assignments with session mutex

## Task Commits

Each task was committed atomically (3 original + 1 race fix):

1. **Task 1 (SCR-04, SCR-06):** `fd34779` - scrcpy audio reader and device message reader
2. **Task 2 (Pitfall 8):** `f4f9b7b` - launcher extended with LaunchWithOptions
3. **Task 3 (errgroup wiring):** `493a996` - DeviceEntry, DeviceSession, Hub/ControlWriter under errgroup
4. **Race fix:** `c9f09f1` - resolve data race in DeviceSession.Run closer goroutine

## Decisions Made

- v3.x scrcpy uses ONE listener and ONE reverse tunnel; server connects sequentially to same localabstract socket (verified against scrcpy source, not three separate sockets)
- Audio probe reads 4-byte codec ID after audio Accept; 0x00000000 or immediate EOF means unavailable (A1 defensive parse)
- DeviceMessage reader exits on ErrUnknownDeviceMessage because the protocol is length-prefix-stateful: unknown type means we cannot determine how many bytes to skip
- Conn pointers captured into local variables in Run() to avoid data race with closer goroutine's cleanupResources nil-assignments
- Launcher interface migrated from `Launch(ctx, serial)` to `LaunchWithOptions(ctx, serial, opts)`; Phase 1 callers use `DefaultLaunchOptions()`

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Data race in DeviceSession.Run closer goroutine**
- **Found during:** Post-commit verification (`-race` test run)
- **Issue:** The closer goroutine (spawned by `Run`) writes `s.controlConn = nil` and `s.audioConn = nil` via `cleanupResources()`, while the main goroutine still in `Run()` reads those fields for nil-checks before spawning goroutines. The race detector flagged this as a DATA RACE.
- **Fix:** (1) Capture `s.audioConn` and `s.controlConn` into local variables before starting the errgroup. (2) Pass the local `audioConn` and `controlConn` values explicitly to `audioReaderLoop(ctx, conn)` and `deviceMessageReaderLoop(ctx, conn)`. (3) Protect `cleanupResources` nil-assignments with `s.mu.Lock()`/`s.mu.Unlock()`.
- **Files modified:** `internal/session/supervisor.go`
- **Commit:** `c9f09f1`

## Known Stubs

None. All interfaces are fully implemented; no placeholder text or hardcoded empty values.

## Threat Flags

No new threat surface beyond what the plan's threat model documented. All mitigations are in place:
- T-02-05-01: Control socket read+write race mitigated by separate goroutines (ControlWriter writes, deviceMessageReaderLoop reads)
- T-02-05-02: Audio Hub gated on `s.audioAvailable && audioConn != nil` (local capture, not field read)
- T-02-05-03: All goroutines under errgroup; ctx cancellation propagates; cleanupResources nil-assignments protected by mutex
- T-02-05-04: DeviceMessage parser reads fixed-prefix + bounded-payload per type; unknown type returns ErrUnknownDeviceMessage
- T-02-05-05: Audio codec value logged at INFO (non-secret ops debug info)
- T-02-05-06: LeaseManager allocated inside sync.Map.LoadOrStore (atomic); independent mutex from DeviceEntry.mu

## Next Phase Readiness

- `DeviceSession.VideoHub()`, `.AudioHub()`, `.ControlWriter()`, `.DeviceMessages()` accessor methods ready for WS handlers in plan 02-06
- `DeviceEntry.GetLeaseManager()` ready for REST reservation endpoints in plan 02-06
- `DeviceEntry.GetAudioAvailable()` ready for audio stream availability check in plan 02-06
- `LaunchWithOptions` with `AudioEnabled`/`ControlEnabled` ready for config-driven session startup

## Self-Check: PASSED

- `internal/scrcpy/audio_reader.go` FOUND
- `internal/scrcpy/audio_reader_test.go` FOUND
- `internal/scrcpy/device_message.go` FOUND
- `internal/scrcpy/device_message_test.go` FOUND
- `internal/scrcpy/launcher.go` FOUND
- `internal/session/registry.go` FOUND
- `internal/session/supervisor.go` FOUND
- Commit `fd34779` FOUND
- Commit `f4f9b7b` FOUND
- Commit `493a996` FOUND
- Commit `c9f09f1` FOUND
- All tests PASS under `-race`
- `go build ./...` clean
- `go vet ./...` clean

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-08*