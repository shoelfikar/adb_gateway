---
phase: 02-multi-client-control
plan: 03
subsystem: scrcpy-protocol
tags: [scrcpy, binary-protocol, control-messages, marshal, single-writer, goroutine]

# Dependency graph
requires:
  - phase: 02-01
    provides: config keys, error envelope, metrics collectors
provides:
  - ControlMsg type with 18 type constants (0x00..0x11)
  - Marshal(ControlMsg) ([]byte, error) byte-exact encoder
  - ControlWriter type with Run(ctx) and In() channel
  - ErrUnknownControlType, ErrControlPayloadTooLarge, ErrControlPayloadInvalid sentinels
  - MaxInjectTextBytes=300, MaxSetClipboardBytes=262144 constants
  - Golden fixture bytes for 5 most-used control types
affects: [02-05, 02-06, 03-streaming-hardening]

# Tech tracking
tech-stack:
  added: []
  patterns: [single-writer goroutine per device, big-endian binary marshal table, buffered channel drain, fail-closed unknown type rejection]

key-files:
  created:
    - internal/scrcpy/control_writer.go
    - internal/scrcpy/control_writer_test.go
    - internal/scrcpy/testdata/control_inject_keycode.bin
    - internal/scrcpy/testdata/control_inject_text.bin
    - internal/scrcpy/testdata/control_inject_touch_event.bin
    - internal/scrcpy/testdata/control_inject_scroll_event.bin
    - internal/scrcpy/testdata/control_set_clipboard.bin
    - internal/scrcpy/testdata/README.md
  modified: []

key-decisions:
  - "Touch event size is 32 bytes per scrcpy v3.3.4 ControlMessageReader.java (1+1+8+4+4+2+2+2+4+4 = 32), not 36 as some older sources claim"
  - "Signed int32 coordinates (X, Y, HScroll, VScroll) use uint32 reinterpret cast via binary.BigEndian.PutUint32, matching Java DataOutputStream.writeInt two's-complement behavior"
  - "ControlWriter.Run logs marshal errors but does NOT abort the writer; only conn.Write errors abort (T-02-03-04)"
  - "ControlWriter does NOT own the net.Conn lifecycle; the supervisor (plan 02-05) owns it"

patterns-established:
  - "scrcpy binary protocol: all multi-byte fields big-endian; type byte first; length-prefixed variable fields use 4-byte (text) or 2-byte (UHID) or 1-byte (name) prefixes"
  - "Fail-closed validation: unknown types rejected before any wire bytes (D-15); ErrUnknownControlType returned from Marshal default branch"
  - "Single-writer goroutine: ControlWriter.in channel is the sole funnel; conn.Write called from Run goroutine only (D-14)"

requirements-completed: [SCR-05]

# Metrics
duration: 10min
completed: 2026-05-08
---

# Phase 2 Plan 03: Control Writer Summary

**18-type scrcpy v3.3.4 control message marshal table with byte-exact big-endian encoding, length validation, and single-writer goroutine with concurrent-producer race safety**

## Performance

- **Duration:** 10 min
- **Started:** 2026-05-08T03:09:54Z
- **Completed:** 2026-05-08T03:19:40Z
- **Tasks:** 2
- **Files modified:** 2 new files, 5 golden fixtures, 1 README

## Accomplishments
- All 18 scrcpy v3.3.4 control message types (0x00..0x11) marshal to byte-exact wire format per Pattern 3
- Single-writer discipline holds under `-race` with 8 concurrent producers and 96 messages (no torn frames)
- D-15 fail-closed: unknown types (0x99) rejected before any wire bytes leave Marshal
- Length limits enforced at gateway boundary: INJECT_TEXT > 300 bytes, SET_CLIPBOARD > 262144 bytes, UHID_CREATE name > 255 bytes, UHID_CREATE descriptor > 65535 bytes, UHID_INPUT data > 65535 bytes, START_APP name > 255 bytes
- ControlWriter survives bad messages (logs and continues); exits cleanly on ctx cancel
- ControlWriter exits on conn.Write errors (EOF, closed pipe, etc.)

## Task Commits

Each task was committed atomically:

1. **Task 1: Define ControlMsg type, 18 constants, and Marshal function** - `58f2c4a` (feat)
2. **Task 2: Byte-exact tests, golden fixtures, and single-writer race tests** - `44c8073` (test)

## Files Created/Modified
- `internal/scrcpy/control_writer.go` - 18 ControlType constants, ControlMsg struct with 13 field types, Marshal function, ControlWriter with Run/In, domain error sentinels
- `internal/scrcpy/control_writer_test.go` - 12 test functions: AllTypes (18 subcases), GoldenFixtures (5 fixtures), UnknownType, LengthLimits (6 subcases), MissingFields (12 subcases), EdgeCases (5 subcases), Serializes, BadMsgDoesNotKill, CtxCancel, DefaultBufferSize, CustomBufferSize, ControlTypeConstants
- `internal/scrcpy/testdata/control_inject_keycode.bin` - 14-byte golden fixture
- `internal/scrcpy/testdata/control_inject_text.bin` - 10-byte golden fixture
- `internal/scrcpy/testdata/control_inject_touch_event.bin` - 32-byte golden fixture (verifies A2: touch is 32 bytes in v3.3.4)
- `internal/scrcpy/testdata/control_inject_scroll_event.bin` - 21-byte golden fixture
- `internal/scrcpy/testdata/control_set_clipboard.bin` - 16-byte golden fixture
- `internal/scrcpy/testdata/README.md` - Fixture documentation and regeneration instructions

## Decisions Made
- Touch event size is 32 bytes per scrcpy v3.3.4 layout (1+1+8+4+4+2+2+2+4+4 = 32), not 36 as some older sources cite. Verified by byte-exact unit test matching the 32-byte total.
- Signed int32 coordinates (X, Y in touch; X, Y, HScroll, VScroll in scroll) use uint32 reinterpret cast via `binary.BigEndian.PutUint32`, preserving two's-complement bit pattern matching Java's `DataOutputStream.writeInt(int)`.
- ControlWriter logs marshal errors with type byte and error class but never the payload content (T-02-03-05: no SET_CLIPBOARD or InjectText text in logs).
- ControlWriter does NOT own the net.Conn lifecycle; the supervisor (plan 02-05) will create and close the conn.

## Deviations from Plan

None - plan executed exactly as written.

## Known Stubs

None. All interfaces are fully implemented; no placeholder text or hardcoded empty values.

## Threat Flags

No new threat surface beyond what the plan's threat model documented. All mitigations verified:
- T-02-03-01: Unknown type bytes rejected by Marshal default branch (ErrUnknownControlType)
- T-02-03-02: Length limits enforced at Marshal layer (MaxInjectTextBytes=300, MaxSetClipboardBytes=262144)
- T-02-03-03: Single-writer race test passes under `-race` with 8 producers
- T-02-03-04: Bad messages logged and dropped; only conn.Write errors abort Run
- T-02-03-05: No payload text logged (verified: grep returns 0 matches for clipboard/text in log messages)

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- ControlWriter is ready for wiring in Plan 02-05 (supervisor integration)
- WS handler in Plan 02-06 will call `Marshal(msg)` to translate JSON control messages to binary, then send via `ControlWriter.In()`
- Golden fixtures committed under testdata for regression detection against protocol drift

## Self-Check: PASSED

- `internal/scrcpy/control_writer.go` FOUND
- `internal/scrcpy/control_writer_test.go` FOUND
- `internal/scrcpy/testdata/control_inject_keycode.bin` FOUND
- `internal/scrcpy/testdata/control_inject_text.bin` FOUND
- `internal/scrcpy/testdata/control_inject_touch_event.bin` FOUND
- `internal/scrcpy/testdata/control_inject_scroll_event.bin` FOUND
- `internal/scrcpy/testdata/control_set_clipboard.bin` FOUND
- Commit `58f2c4a` FOUND
- Commit `44c8073` FOUND
- All `TestControl*` tests PASS with `-race`

---
*Phase: 02-multi-client-control*
*Completed: 2026-05-08*