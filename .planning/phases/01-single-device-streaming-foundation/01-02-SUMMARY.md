---
phase: 01-single-device-streaming-foundation
plan: 02
subsystem: internal/adb
tags: [adb, wire-protocol, reverse-forward, scrcpy, host-services]
requires: [01-01]
provides: [adb-client, host-services, reverse-forward, fake-adb]
affects: [internal/adb/client.go, internal/adb/host_services.go, internal/adb/reverse.go, internal/adb/adbtest/fake.go]
tech-stack:
  added:
    - prife/goadb v0.4.8 (ADB client library for well-supported operations)
    - ADB smart-sockets wire protocol codec (in-house)
    - In-house reverse:forward helper (no Go library implements it)
  patterns:
    - net.Listener-based fake ADB server for test isolation
    - Context-timeout-bounded ADB operations per ADB-07
    - ReverseMapping with persistent connection for mapping preservation
key-files:
  created:
    - internal/adb/client.go
    - internal/adb/host_services.go
    - internal/adb/reverse.go
    - internal/adb/adbtest/fake.go
    - internal/adb/client_test.go
    - internal/adb/host_services_test.go
    - internal/adb/reverse_test.go
decisions:
  - Used prife/goadb v0.4.8 for well-supported ADB operations (devices, push, shell)
  - In-house reverse:forward helper implemented against AOSP SERVICES.TXT wire format
  - Semicolon separator verified for reverse:forward command format
  - localabstract:scrcpy_<SCID> used for device-side socket (NOT tcp:27183)
  - ReverseMapping.conn kept open to preserve reverse mapping (not deferred closed)
  - FakeADB uses configurable handler map for test isolation
  - HostServices.PushFile uses goadb sync API with SyncFileWriter for in-memory data
metrics:
  duration: 21m
  completed: 2026-05-07T03:54:23Z
  tasks: 2
  files: 7
  tests: 24
---

# Phase 1 Plan 02: ADB Transport Layer Summary

In-house reverse:forward helper works against ADB wire protocol, using localabstract:scrcpy_<SCID> (not tcp:27183) for device-side sockets with semicolon separator. Host services wrap prife/goadb for devices, push, and shell operations. All ADB calls bounded by context with timeout. Fake ADB listener enables isolated testing.

## Tasks Completed

### Task 1: ADB client + smart-sockets codec + host services + test infrastructure

**Commit:** c46258b - `feat(01-02): ADB client, smart-sockets codec, host services, and test infrastructure`

**Files created:**
- `internal/adb/client.go` - Client struct with dial, sendMessage, readResponse, readStringResponse
- `internal/adb/host_services.go` - HostServices wrapping prife/goadb for ListDevices, NewDeviceWatcher, ServerVersion, PushFile, RunShellCommand
- `internal/adb/adbtest/fake.go` - FakeADB net.Listener with configurable handler map for test isolation
- `internal/adb/client_test.go` - Tests for dial, send/receive, string response, format validation
- `internal/adb/host_services_test.go` - Tests for ServerVersion, context timeout, HostServices construction

**Key decisions:**
- Used prife/goadb v0.4.8 for well-supported operations (ListDevices, NewDeviceWatcher, PushFile, RunShellCommand)
- HostServices.NewHostServices parses the client address into host/port for goadb's ServerConfig
- ServerVersion uses raw protocol (our sendMessage/readResponse) since goadb's version returns an int
- PushFile uses goadb's sync API (NewSyncConn + Send + SyncFileWriter.Write + CopyDone) for in-memory data push
- RunShellCommand uses goadb's shell:v2 (first arg `true`) for Android 14+ compatibility

### Task 2: Reverse:forward helper + reverse:list-forward + reverse:remove

**Commit:** 06cf628 - `feat(01-02): reverse:forward helper, reverse:list-forward, and reverse:remove`

**Files created:**
- `internal/adb/reverse.go` - ReverseMapping struct, ReverseForward, ReverseListForward, ReverseRemove, ReverseKillforwardAll
- `internal/adb/reverse_test.go` - 14 tests covering wire format, connection preservation, semicolon separator, failure handling, parsing

**Critical implementation details:**
- `reverse:forward:<deviceSpec>;<hostSpec>` uses SEMICOLON separator (NOT colon) - verified by TestSemicolonSeparatorFormat
- Device-side socket uses `localabstract:scrcpy_<SCID>` (NOT `tcp:27183`) - verified by TestDeviceSocketFormat
- ReverseMapping.conn stays open for the mapping to persist - verified by TestReverseMappingConnectionPreserved
- Only `ReverseMapping.Close()` closes the connection, removing the mapping
- ReverseListForward parses `<serial> <local> <remote>\n` format
- All reverse methods use `context.WithTimeout` with 10-second deadlines

## Verification Results

- `go test ./internal/adb/... -count=1` - PASS (24 tests)
- `go vet ./internal/adb/...` - PASS (no warnings)
- `go test ./... -count=1` - PASS (all packages)
- Reverse:forward uses `localabstract:scrcpy_<SCID>;tcp:<port>` format (semicolon, not colon)
- ReverseMapping.conn stays open after ReverseForward returns
- All ADB operations bounded by context with timeout

## Deviations from Plan

None - plan executed exactly as written.

## Known Stubs

No stubs identified. All functionality is wired and tested.

## Threat Flags

No new threat surfaces beyond those identified in the plan's threat_model.

## Self-Check: PASSED

- All 7 created files verified as present on disk
- Both commit hashes (c46258b, 06cf628) verified in git log
- `go test ./internal/adb/... -count=1` passes (24 tests)
- `go vet ./internal/adb/...` passes (no warnings)
- `go test ./... -count=1` passes (all packages)