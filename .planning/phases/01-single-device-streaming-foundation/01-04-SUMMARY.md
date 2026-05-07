---
phase: 01-single-device-streaming-foundation
plan: 04
subsystem: scrcpy-protocol
tags: [scrcpy, embed, video-reader, launcher, frame-header, codec-metadata, reverse-forward]

# Dependency graph
requires:
  - phase: 01
    provides: ADB client with ReverseForward, HostServices.PushFile, HostServices.RunShellCommand
provides:
  - Embedded scrcpy v3.3.4 server.jar via //go:embed
  - SCRCPYVersion, ServerJarPath, ServerJarSHA256 constants
  - BuildSCID() for random session ID generation
  - Launcher with 8-step sequential startup (push, SCID, listen, reverse, launch, accept, device meta, codec meta)
  - FrameHeader, ReadCodecMeta, ReadFrameHeader, ReadVideoFrame for video stream parsing
  - Cleanup-on-failure pattern for launcher resource management
affects: [session-supervisor, ws-video-relay]

# Tech tracking
tech-stack:
  added: [crypto/rand for SCID generation]
  patterns: [io.ReadFull for all frame boundary reads, //go:embed for binary assets, cleanup-on-failure slice pattern, rawHeader preservation for zero-copy WS relay]

key-files:
  created:
    - internal/scrcpy/embed.go
    - internal/scrcpy/version.go
    - internal/scrcpy/launcher.go
    - internal/scrcpy/video_reader.go
    - internal/scrcpy/embed_test.go
    - internal/scrcpy/video_reader_test.go
    - internal/scrcpy/assets/scrcpy-server-v3.3.4
    - internal/scrcpy/testdata/codec_meta.bin
    - internal/scrcpy/testdata/frame_h264_keyframe.bin
    - internal/scrcpy/testdata/frame_config_packet_header.bin

key-decisions:
  - "Used crypto/rand for SCID generation instead of math/rand for stronger randomness"
  - "ReadCodecMeta reads raw 12 bytes first then parses, preserving raw bytes in LaunchResult.CodecMeta"
  - "Launcher treats RunShellCommand errors as non-fatal since scrcpy server starts in background"
  - "Shell command timeout of 15s; server process continues after timeout"

patterns-established:
  - "io.ReadFull for all frame boundary reads -- NEVER conn.Read (TCP byte stream pitfall)"
  - "Cleanup-on-failure: append cleanup funcs to slice, execute in reverse on error"
  - "Raw header preservation: FrameHeader.rawHeader [12]byte for zero-copy WS relay"
  - "Embed binary assets as []byte (not embed.FS) for single-blob push"
  - "SHA-256 of embedded assets recorded as const for integrity verification"

requirements-completed: [SCR-01, SCR-02, SCR-03]

# Metrics
duration: 12min
completed: 2026-05-07
---

# Phase 1 Plan 04: Scrcpy Protocol Layer Summary

**Embedded scrcpy v3.3.4 server.jar with 8-step launcher and video frame reader using io.ReadFull for frame boundaries**

## Performance

- **Duration:** 12 min
- **Started:** 2026-05-07T04:21:52Z
- **Completed:** 2026-05-07T04:34:11Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- Pinned scrcpy v3.3.4 server.jar embedded in Go binary via //go:embed
- 8-step sequential launcher with cleanup-on-failure (push, SCID, listen, reverse, launch, accept, device meta, codec meta)
- Video frame reader with io.ReadFull discipline, raw header preservation for zero-copy WS relay
- BuildSCID using crypto/rand for 31-bit random session IDs

## Task Commits

Each task was committed atomically:

1. **Task 1: Server.jar embed + version constants + SCID generation** - `6b13ea5` (feat)
2. **Task 2: Launcher (8-step startup) + video frame reader** - `0b6d432` (feat)

## Files Created/Modified
- `internal/scrcpy/embed.go` - //go:embed directive for scrcpy-server-v3.3.4 as []byte
- `internal/scrcpy/version.go` - SCRCPYVersion "3.3.4", ServerJarPath "/data/local/tmp/scrcpy-server-gateway.jar", ServerJarSHA256, BuildSCID()
- `internal/scrcpy/launcher.go` - Launcher struct, LaunchResult struct, 8-step Launch() method with cleanup-on-failure
- `internal/scrcpy/video_reader.go` - FrameHeader struct with RawHeader(), ReadCodecMeta, ReadFrameHeader, ReadVideoFrame (all using io.ReadFull)
- `internal/scrcpy/embed_test.go` - Tests for ServerJar embedding and BuildSCID format
- `internal/scrcpy/video_reader_test.go` - Tests for codec meta, frame header, video frame, truncated streams, raw byte preservation
- `internal/scrcpy/assets/scrcpy-server-v3.3.4` - Pinned scrcpy v3.3.4 server binary (90KB)
- `internal/scrcpy/testdata/codec_meta.bin` - 12-byte H.264 codec metadata fixture
- `internal/scrcpy/testdata/frame_h264_keyframe.bin` - 76-byte keyframe with header + payload
- `internal/scrcpy/testdata/frame_config_packet_header.bin` - 12-byte config packet header fixture

## Decisions Made
- Used `crypto/rand` instead of `math/rand` for SCID generation for stronger randomness per session
- ReadCodecMeta returns parsed values (codecID, width, height) while Launch function reads raw 12 bytes to preserve them in LaunchResult.CodecMeta
- Launcher treats RunShellCommand errors as non-fatal warnings since scrcpy server process starts in background and may not produce output before Accept is needed
- Shell command timeout of 15s is generous; actual Accept timeout of 10s catches server launch failures

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all tests pass, all acceptance criteria verified.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Scrcpy protocol layer complete and ready for session supervisor integration (Plan 05)
- Video reader ready for WebSocket relay (Plan 05)
- Launcher ready to be called from session FSM starting state
- All ADB, config, auth, registry, FSM, and scrcpy protocol layers are in place for session orchestration

---
*Phase: 01-single-device-streaming-foundation*
*Completed: 2026-05-07*