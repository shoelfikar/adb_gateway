package scrcpy

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hexBytes converts a hex string (spaces allowed) to []byte for readable test cases.
func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	require.NoError(t, err, "hexBytes: invalid hex %q", s)
	return b
}

// goldenMsgFor returns the ControlMsg used to generate the named golden fixture.
func goldenMsgFor(t *testing.T, name string) ControlMsg {
	t.Helper()
	switch name {
	case "control_inject_keycode":
		return ControlMsg{
			Type: TypeInjectKeycode,
			InjectKeycode: &InjectKeycodeFields{
				Action:    0x00,
				Keycode:   0x00000042, // KEYCODE_ENTER
				Repeat:    0x00000000,
				MetaState: 0x00000001, // META_ALT_ON
			},
		}
	case "control_inject_text":
		return ControlMsg{
			Type:      TypeInjectText,
			InjectText: &InjectTextFields{Text: "hello"},
		}
	case "control_inject_touch_event":
		return ControlMsg{
			Type: TypeInjectTouchEvent,
			InjectTouchEvent: &InjectTouchEventFields{
				Action:       0x00, // ACTION_DOWN
				PointerID:    0xFFFFFFFFFFFFFFFF,
				X:            100,
				Y:            200,
				Width:        1080,
				Height:       1920,
				Pressure:     0xFFFF, // 1.0
				ActionButton: 0,
				Buttons:      0,
			},
		}
	case "control_inject_scroll_event":
		return ControlMsg{
			Type: TypeInjectScrollEvent,
			InjectScrollEvent: &InjectScrollEventFields{
				X:       100,
				Y:       200,
				Width:   1080,
				Height:  1920,
				HScroll: 0,
				VScroll: -1,
				Buttons: 0,
			},
		}
	case "control_set_clipboard":
		return ControlMsg{
			Type: TypeSetClipboard,
			SetClipboard: &SetClipboardFields{
				Sequence: 0x42,
				Paste:    true,
				Text:     "hi",
			},
		}
	default:
		t.Fatalf("unknown golden fixture name: %s", name)
		return ControlMsg{}
	}
}

func TestControlMarshal_AllTypes(t *testing.T) {
	cases := []struct {
		name     string
		msg      ControlMsg
		expected []byte
	}{
		{
			name: "inject_keycode",
			msg: ControlMsg{
				Type: TypeInjectKeycode,
				InjectKeycode: &InjectKeycodeFields{
					Action:    0x00,
					Keycode:   0x00000042, // KEYCODE_ENTER
					Repeat:    0x00000000,
					MetaState: 0x00000001, // META_ALT_ON
				},
			},
			// type(1) action(1) keycode(4 BE) repeat(4 BE) metaState(4 BE) = 14
			expected: hexBytes(t, "00 00 00000042 00000000 00000001"),
		},
		{
			name: "inject_text_hello",
			msg: ControlMsg{
				Type:      TypeInjectText,
				InjectText: &InjectTextFields{Text: "hello"},
			},
			// type(1) textLen(4 BE = 5) "hello"
			expected: hexBytes(t, "01 00000005 68 65 6c 6c 6f"),
		},
		{
			name: "inject_touch_event_down_at_100_200",
			msg: ControlMsg{
				Type: TypeInjectTouchEvent,
				InjectTouchEvent: &InjectTouchEventFields{
					Action:       0x00, // ACTION_DOWN
					PointerID:    0xFFFFFFFFFFFFFFFF,
					X:            100,
					Y:            200,
					Width:        1080,
					Height:       1920,
					Pressure:     0xFFFF, // 1.0
					ActionButton: 0,
					Buttons:      0,
				},
			},
			// 32 bytes total
			expected: hexBytes(t, "02 00 FFFFFFFFFFFFFFFF 00000064 000000C8 0438 0780 FFFF 00000000 00000000"),
		},
		{
			name: "inject_scroll_event_down",
			msg: ControlMsg{
				Type: TypeInjectScrollEvent,
				InjectScrollEvent: &InjectScrollEventFields{
					X:       100,
					Y:       200,
					Width:   1080,
					Height:  1920,
					HScroll: 0,
					VScroll: -1,
					Buttons: 0,
				},
			},
			// 21 bytes
			expected: hexBytes(t, "03 00000064 000000C8 0438 0780 0000 FFFF 00000000"),
		},
		{
			name:     "back_or_screen_on",
			msg:      ControlMsg{Type: TypeBackOrScreenOn, BackOrScreenOn: &BackOrScreenOnFields{Action: 0}},
			expected: hexBytes(t, "04 00"),
		},
		{
			name:     "expand_notification_panel",
			msg:      ControlMsg{Type: TypeExpandNotificationPanel},
			expected: hexBytes(t, "05"),
		},
		{
			name:     "expand_settings_panel",
			msg:      ControlMsg{Type: TypeExpandSettingsPanel},
			expected: hexBytes(t, "06"),
		},
		{
			name:     "collapse_panels",
			msg:      ControlMsg{Type: TypeCollapsePanels},
			expected: hexBytes(t, "07"),
		},
		{
			name:     "get_clipboard",
			msg:      ControlMsg{Type: TypeGetClipboard, GetClipboard: &GetClipboardFields{CopyKey: 1}},
			expected: hexBytes(t, "08 01"),
		},
		{
			name: "set_clipboard_paste_hi",
			msg: ControlMsg{
				Type: TypeSetClipboard,
				SetClipboard: &SetClipboardFields{Sequence: 0x42, Paste: true, Text: "hi"},
			},
			// type(1) seq(8) paste(1) txtLen(4) "hi" = 16
			expected: hexBytes(t, "09 0000000000000042 01 00000002 68 69"),
		},
		{
			name:     "set_display_power_on",
			msg:      ControlMsg{Type: TypeSetDisplayPower, SetDisplayPower: &SetDisplayPowerFields{Mode: 1}},
			expected: hexBytes(t, "0a 01"),
		},
		{
			name:     "rotate_device",
			msg:      ControlMsg{Type: TypeRotateDevice},
			expected: hexBytes(t, "0b"),
		},
		{
			name: "uhid_create_no_descriptor",
			msg: ControlMsg{
				Type:       TypeUhidCreate,
				UhidCreate: &UhidCreateFields{ID: 1, VendorID: 0x1234, ProductID: 0x5678, Name: "kb", ReportDescriptor: nil},
			},
			// type(1) id(2) vendor(2) product(2) nameLen(1) name(2) rdLen(2) = 12
			expected: hexBytes(t, "0c 0001 1234 5678 02 6b 62 0000"),
		},
		{
			name: "uhid_input",
			msg: ControlMsg{
				Type:     TypeUhidInput,
				UhidInput: &UhidInputFields{ID: 1, Data: []byte{0xAA, 0xBB}},
			},
			// type(1) id(2) dataLen(2) data(2) = 7
			expected: hexBytes(t, "0d 0001 0002 AA BB"),
		},
		{
			name:     "uhid_destroy",
			msg:      ControlMsg{Type: TypeUhidDestroy, UhidDestroy: &UhidDestroyFields{ID: 1}},
			expected: hexBytes(t, "0e 0001"),
		},
		{
			name:     "open_hard_keyboard_settings",
			msg:      ControlMsg{Type: TypeOpenHardKeyboardSettings},
			expected: hexBytes(t, "0f"),
		},
		{
			name: "start_app",
			msg: ControlMsg{
				Type:     TypeStartApp,
				StartApp: &StartAppFields{Name: "com.android.settings"},
			},
			// type(1) nameLen(1=20) name(20) = 22
			expected: hexBytes(t, "10 14 636f6d2e616e64726f69642e73657474696e6773"),
		},
		{
			name:     "reset_video",
			msg:      ControlMsg{Type: TypeResetVideo},
			expected: hexBytes(t, "11"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.msg)
			require.NoError(t, err, "Marshal(%s) returned error", tc.name)
			assert.Equal(t, tc.expected, got, "wire bytes for %s mismatch", tc.name)
			assert.Equal(t, len(tc.expected), len(got), "wire length for %s mismatch", tc.name)
		})
	}
}

func TestControlMarshal_GoldenFixtures(t *testing.T) {
	for _, name := range []string{
		"control_inject_keycode",
		"control_inject_text",
		"control_inject_touch_event",
		"control_inject_scroll_event",
		"control_set_clipboard",
	} {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", name+".bin"))
			require.NoError(t, err, "read golden fixture %s", name)
			msg := goldenMsgFor(t, name)
			got, err := Marshal(msg)
			require.NoError(t, err, "Marshal for golden %s", name)
			assert.Equal(t, want, got, "golden fixture mismatch for %s", name)
		})
	}
}

func TestControlMarshal_UnknownType(t *testing.T) {
	_, err := Marshal(ControlMsg{Type: 0x99})
	require.ErrorIs(t, err, ErrUnknownControlType)
}

func TestControlMarshal_LengthLimits(t *testing.T) {
	t.Run("inject_text_exceeds_300", func(t *testing.T) {
		longText := strings.Repeat("x", 301)
		_, err := Marshal(ControlMsg{Type: TypeInjectText, InjectText: &InjectTextFields{Text: longText}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})

	t.Run("set_clipboard_exceeds_262144", func(t *testing.T) {
		bigClip := strings.Repeat("y", 262145)
		_, err := Marshal(ControlMsg{Type: TypeSetClipboard, SetClipboard: &SetClipboardFields{Text: bigClip}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})

	t.Run("uhid_create_name_exceeds_255", func(t *testing.T) {
		longName := strings.Repeat("z", 256)
		_, err := Marshal(ControlMsg{Type: TypeUhidCreate, UhidCreate: &UhidCreateFields{Name: longName}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})

	t.Run("uhid_create_descriptor_exceeds_65535", func(t *testing.T) {
		bigDescriptor := make([]byte, 65536)
		_, err := Marshal(ControlMsg{Type: TypeUhidCreate, UhidCreate: &UhidCreateFields{
			ID:               1,
			VendorID:         1,
			ProductID:        1,
			Name:             "dev",
			ReportDescriptor: bigDescriptor,
		}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})

	t.Run("uhid_input_data_exceeds_65535", func(t *testing.T) {
		bigData := make([]byte, 65536)
		_, err := Marshal(ControlMsg{Type: TypeUhidInput, UhidInput: &UhidInputFields{ID: 1, Data: bigData}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})

	t.Run("start_app_name_exceeds_255", func(t *testing.T) {
		longName := strings.Repeat("a", 256)
		_, err := Marshal(ControlMsg{Type: TypeStartApp, StartApp: &StartAppFields{Name: longName}})
		require.ErrorIs(t, err, ErrControlPayloadTooLarge)
	})
}

func TestControlMarshal_MissingFields(t *testing.T) {
	// All types that require a non-nil field pointer should return ErrControlPayloadInvalid
	// when the field is nil.
	nilFieldCases := []struct {
		name string
		msg  ControlMsg
	}{
		{"inject_keycode_nil", ControlMsg{Type: TypeInjectKeycode}},
		{"inject_text_nil", ControlMsg{Type: TypeInjectText}},
		{"inject_touch_event_nil", ControlMsg{Type: TypeInjectTouchEvent}},
		{"inject_scroll_event_nil", ControlMsg{Type: TypeInjectScrollEvent}},
		{"back_or_screen_on_nil", ControlMsg{Type: TypeBackOrScreenOn}},
		{"get_clipboard_nil", ControlMsg{Type: TypeGetClipboard}},
		{"set_clipboard_nil", ControlMsg{Type: TypeSetClipboard}},
		{"set_display_power_nil", ControlMsg{Type: TypeSetDisplayPower}},
		{"uhid_create_nil", ControlMsg{Type: TypeUhidCreate}},
		{"uhid_input_nil", ControlMsg{Type: TypeUhidInput}},
		{"uhid_destroy_nil", ControlMsg{Type: TypeUhidDestroy}},
		{"start_app_nil", ControlMsg{Type: TypeStartApp}},
	}

	for _, tc := range nilFieldCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Marshal(tc.msg)
			require.ErrorIs(t, err, ErrControlPayloadInvalid, "expected ErrControlPayloadInvalid for %s, got: %v", tc.name, err)
		})
	}
}

func TestControlMarshal_EdgeCases(t *testing.T) {
	t.Run("inject_text_empty_string", func(t *testing.T) {
		got, err := Marshal(ControlMsg{Type: TypeInjectText, InjectText: &InjectTextFields{Text: ""}})
		require.NoError(t, err)
		// type(1) + textLen(4 BE = 0) = 5 bytes
		assert.Equal(t, hexBytes(t, "01 00000000"), got)
	})

	t.Run("set_clipboard_paste_false", func(t *testing.T) {
		got, err := Marshal(ControlMsg{
			Type:         TypeSetClipboard,
			SetClipboard: &SetClipboardFields{Sequence: 0, Paste: false, Text: ""},
		})
		require.NoError(t, err)
		// type(1) + seq(8=0) + paste(1=0) + textLen(4=0) = 14 bytes
		expected := make([]byte, 14)
		expected[0] = 0x09 // TypeSetClipboard
		// rest is all zeros (seq=0, paste=0, textLen=0)
		assert.Equal(t, expected, got)
	})

	t.Run("inject_text_exactly_300_bytes", func(t *testing.T) {
		// At the boundary — should succeed
		text := strings.Repeat("a", 300)
		_, err := Marshal(ControlMsg{Type: TypeInjectText, InjectText: &InjectTextFields{Text: text}})
		require.NoError(t, err)
	})

	t.Run("set_clipboard_exactly_262144_bytes", func(t *testing.T) {
		// At the boundary — should succeed
		text := strings.Repeat("b", 262144)
		_, err := Marshal(ControlMsg{Type: TypeSetClipboard, SetClipboard: &SetClipboardFields{Text: text}})
		require.NoError(t, err)
	})

	t.Run("touch_event_signed_coordinates", func(t *testing.T) {
		// Negative int32 values — verify they marshal correctly as uint32 reinterpret
		msg := ControlMsg{
			Type: TypeInjectTouchEvent,
			InjectTouchEvent: &InjectTouchEventFields{
				Action:    0x01, // ACTION_UP
				PointerID: 0,
				X:        -100, // negative x
				Y:        -200, // negative y
				Width:    1080,
				Height:   1920,
				Pressure: 0x7FFF,
			},
		}
		got, err := Marshal(msg)
		require.NoError(t, err)
		// Verify the output is 32 bytes and the signed values are properly reinterpreted
		assert.Equal(t, 32, len(got))
		// -100 as int32 -> 0xFFFFFF9C as uint32
		assert.Equal(t, byte(0xFF), got[10])
		assert.Equal(t, byte(0xFF), got[11])
		assert.Equal(t, byte(0xFF), got[12])
		assert.Equal(t, byte(0x9C), got[13])
	})
}

func TestControlWriterRun_Serializes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	cw := NewControlWriter(ControlWriterOpts{
		Conn: server,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cw.Run(ctx)
		close(done)
	}()

	// Reader: read all bytes until EOF
	readDone := make(chan []byte, 1)
	go func() {
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				all = append(all, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		readDone <- all
	}()

	// 8 producers concurrently enqueue 12 messages each
	var wg sync.WaitGroup
	wg.Add(8)
	for p := 0; p < 8; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				cw.In() <- ControlMsg{Type: TypeBackOrScreenOn, BackOrScreenOn: &BackOrScreenOnFields{Action: 0}}
			}
		}()
	}
	wg.Wait()

	// Allow drain
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	client.Close()
	all := <-readDone

	// Each BackOrScreenOn marshals to 2 bytes [0x04, 0x00]. We expect 8*12 = 96 messages = 192 bytes.
	require.Equal(t, 192, len(all), "expected 192 bytes (96 messages * 2 bytes each)")
	for i := 0; i < len(all); i += 2 {
		require.Equal(t, byte(0x04), all[i], "torn frame at byte %d: expected type 0x04", i)
		require.Equal(t, byte(0x00), all[i+1], "torn payload at byte %d", i+1)
	}
}

func TestControlWriterRun_BadMsgDoesNotKill(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	cw := NewControlWriter(ControlWriterOpts{
		Conn: server,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cw.Run(ctx)
		close(done)
	}()

	// Reader goroutine
	readDone := make(chan []byte, 1)
	go func() {
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				all = append(all, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		readDone <- all
	}()

	// Enqueue an unknown-type message (will be dropped by Marshal)
	// then a valid BackOrScreenOn message (should arrive on the wire)
	cw.In() <- ControlMsg{Type: 0x99}
	cw.In() <- ControlMsg{Type: TypeBackOrScreenOn, BackOrScreenOn: &BackOrScreenOnFields{Action: 0}}

	// Allow drain
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	client.Close()
	all := <-readDone

	// Only the valid BackOrScreenOn (2 bytes) should have been written.
	// The unknown type 0x99 should have been logged and dropped.
	require.Equal(t, 2, len(all), "expected 2 bytes from valid message only (bad message dropped)")
	require.Equal(t, byte(0x04), all[0])
	require.Equal(t, byte(0x00), all[1])
}

func TestControlWriterRun_CtxCancel(t *testing.T) {
	server, _ := net.Pipe()
	defer server.Close()

	cw := NewControlWriter(ControlWriterOpts{
		Conn: server,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cw.Run(ctx)
	}()

	// Cancel the context
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled, "ControlWriter.Run should return context.Canceled on ctx cancel")
}

func TestControlWriter_DefaultBufferSize(t *testing.T) {
	cw := NewControlWriter(ControlWriterOpts{
		Conn: nil, // nil conn is fine for this test
		Log:  slog.Default(),
	})
	// Default buffer size should be 64
	require.Equal(t, 64, cap(cw.in), "default buffer size should be 64")
}

func TestControlWriter_CustomBufferSize(t *testing.T) {
	cw := NewControlWriter(ControlWriterOpts{
		Conn:       nil,
		Log:        slog.Default(),
		BufferSize: 128,
	})
	require.Equal(t, 128, cap(cw.in), "custom buffer size should be 128")
}

func TestControlTypeConstants(t *testing.T) {
	// Verify all 18 constants exist and have correct byte values
	expectedTypes := []struct {
		name  string
		value ControlType
		byte  byte
	}{
		{"TypeInjectKeycode", TypeInjectKeycode, 0x00},
		{"TypeInjectText", TypeInjectText, 0x01},
		{"TypeInjectTouchEvent", TypeInjectTouchEvent, 0x02},
		{"TypeInjectScrollEvent", TypeInjectScrollEvent, 0x03},
		{"TypeBackOrScreenOn", TypeBackOrScreenOn, 0x04},
		{"TypeExpandNotificationPanel", TypeExpandNotificationPanel, 0x05},
		{"TypeExpandSettingsPanel", TypeExpandSettingsPanel, 0x06},
		{"TypeCollapsePanels", TypeCollapsePanels, 0x07},
		{"TypeGetClipboard", TypeGetClipboard, 0x08},
		{"TypeSetClipboard", TypeSetClipboard, 0x09},
		{"TypeSetDisplayPower", TypeSetDisplayPower, 0x0a},
		{"TypeRotateDevice", TypeRotateDevice, 0x0b},
		{"TypeUhidCreate", TypeUhidCreate, 0x0c},
		{"TypeUhidInput", TypeUhidInput, 0x0d},
		{"TypeUhidDestroy", TypeUhidDestroy, 0x0e},
		{"TypeOpenHardKeyboardSettings", TypeOpenHardKeyboardSettings, 0x0f},
		{"TypeStartApp", TypeStartApp, 0x10},
		{"TypeResetVideo", TypeResetVideo, 0x11},
	}
	require.Equal(t, 18, len(expectedTypes), "expected 18 control types")
	for _, tc := range expectedTypes {
		assert.Equal(t, tc.byte, byte(tc.value), "%s byte value mismatch", tc.name)
	}
}

func TestMaxConstantValues(t *testing.T) {
	assert.Equal(t, 300, MaxInjectTextBytes, "MaxInjectTextBytes should be 300")
	assert.Equal(t, 262144, MaxSetClipboardBytes, "MaxSetClipboardBytes should be 262144")
}