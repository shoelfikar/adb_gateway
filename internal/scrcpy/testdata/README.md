# Control message golden fixtures

Binary files containing the expected wire bytes for scrcpy v3.3.4 control
messages, used by `TestControlMarshal_GoldenFixtures` in
`control_writer_test.go`.

| File | Type | Size | Description |
|------|------|------|-------------|
| `control_inject_keycode.bin` | 0x00 | 14 | KEYCODE_ENTER, meta=ALT_ON |
| `control_inject_text.bin` | 0x01 | 10 | Text "hello" |
| `control_inject_touch_event.bin` | 0x02 | 32 | ACTION_DOWN at (100,200) |
| `control_inject_scroll_event.bin` | 0x03 | 21 | Scroll at (100,200), v=-1 |
| `control_set_clipboard.bin` | 0x09 | 16 | Sequence=0x42, paste=true, text="hi" |

**Regeneration:** If the scrcpy protocol version changes, update the Marshal
function and re-generate fixtures by running the golden fixture test. The test
compares Marshal output against these committed bytes, so any protocol drift
will be caught automatically.