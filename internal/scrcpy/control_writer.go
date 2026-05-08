// Package scrcpy implements the scrcpy v3.3.4 wire protocol bindings.
// control_writer.go marshals scrcpy ControlMessages (client -> device)
// per scrcpy/server/.../control/ControlMessageReader.java byte layouts.
//
// CRITICAL: Single-writer discipline (D-14) — only one goroutine writes
// to the device control socket; multiple WS handlers funnel through
// ControlWriter.in (a buffered chan) which the writer goroutine drains.
// Concurrent writes from multiple goroutines would corrupt frame
// boundaries (scrcpy's reader is length-prefix-stateful per type).
package scrcpy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
)

// Control message type byte values per scrcpy v3.3.4
// (server/src/main/java/com/genymobile/scrcpy/control/ControlMessage.java).
type ControlType byte

const (
	TypeInjectKeycode            ControlType = 0x00
	TypeInjectText               ControlType = 0x01
	TypeInjectTouchEvent         ControlType = 0x02
	TypeInjectScrollEvent        ControlType = 0x03
	TypeBackOrScreenOn           ControlType = 0x04
	TypeExpandNotificationPanel  ControlType = 0x05
	TypeExpandSettingsPanel      ControlType = 0x06
	TypeCollapsePanels           ControlType = 0x07
	TypeGetClipboard             ControlType = 0x08
	TypeSetClipboard             ControlType = 0x09
	TypeSetDisplayPower          ControlType = 0x0a
	TypeRotateDevice             ControlType = 0x0b
	TypeUhidCreate               ControlType = 0x0c
	TypeUhidInput                ControlType = 0x0d
	TypeUhidDestroy              ControlType = 0x0e
	TypeOpenHardKeyboardSettings ControlType = 0x0f
	TypeStartApp                 ControlType = 0x10
	TypeResetVideo               ControlType = 0x11
)

// scrcpy server-enforced length limits (Open Question 3 fail-fast).
const (
	MaxInjectTextBytes   = 300
	MaxSetClipboardBytes = 262144
)

// Domain errors used by Marshal and the WS handler in plan 02-06.
var (
	ErrUnknownControlType     = errors.New("scrcpy: unknown control type")
	ErrControlPayloadTooLarge = errors.New("scrcpy: control payload exceeds scrcpy server limit")
	ErrControlPayloadInvalid  = errors.New("scrcpy: control payload invalid")
)

// ControlMsg is the typed envelope for any scrcpy control message.
// Exactly one of the per-type field pointers is set, matching Type.
type ControlMsg struct {
	Type ControlType

	InjectKeycode    *InjectKeycodeFields
	InjectText       *InjectTextFields
	InjectTouchEvent *InjectTouchEventFields
	InjectScrollEvent *InjectScrollEventFields
	BackOrScreenOn   *BackOrScreenOnFields
	GetClipboard     *GetClipboardFields
	SetClipboard     *SetClipboardFields
	SetDisplayPower  *SetDisplayPowerFields
	UhidCreate       *UhidCreateFields
	UhidInput        *UhidInputFields
	UhidDestroy      *UhidDestroyFields
	StartApp         *StartAppFields
	// No-payload types (ExpandNotificationPanel, ExpandSettingsPanel,
	// CollapsePanels, RotateDevice, OpenHardKeyboardSettings, ResetVideo)
	// require no fields beyond Type.
}

type InjectKeycodeFields struct {
	Action    byte   // AKeyEventAction
	Keycode   uint32 // AKeycode (Android)
	Repeat    uint32
	MetaState uint32
}

type InjectTextFields struct {
	Text string // UTF-8, max 300 bytes
}

type InjectTouchEventFields struct {
	Action       byte   // AMotionEventAction
	PointerID    uint64
	X            int32  // signed; serialized as 4-byte BE (int32 reinterpret)
	Y            int32
	Width        uint16
	Height       uint16
	Pressure     uint16 // 16-bit fixed-point (0xFFFF == 1.0)
	ActionButton uint32 // AMotionEventButtonState
	Buttons      uint32
}

type InjectScrollEventFields struct {
	X       int32
	Y       int32
	Width   uint16
	Height  uint16
	HScroll int16
	VScroll int16
	Buttons uint32
}

type BackOrScreenOnFields struct {
	Action byte
}

type GetClipboardFields struct {
	CopyKey byte
}

type SetClipboardFields struct {
	Sequence uint64
	Paste    bool
	Text     string // UTF-8, max 262144 bytes
}

type SetDisplayPowerFields struct {
	Mode byte
}

type UhidCreateFields struct {
	ID               uint16
	VendorID         uint16
	ProductID        uint16
	Name             string // length-prefixed by 1 byte (max 255)
	ReportDescriptor []byte // length-prefixed by 2 bytes BE
}

type UhidInputFields struct {
	ID   uint16
	Data []byte // length-prefixed by 2 bytes BE
}

type UhidDestroyFields struct {
	ID uint16
}

type StartAppFields struct {
	Name string // length-prefixed by 1 byte (max 255)
}

// Marshal encodes the ControlMsg to scrcpy v3.3.4 wire bytes.
// Returns ErrUnknownControlType for unrecognized type bytes,
// ErrControlPayloadTooLarge for length-violating payloads,
// ErrControlPayloadInvalid for missing required field pointers.
//
// CRITICAL: All multi-byte numeric fields are big-endian per scrcpy
// protocol. Touch coordinates are signed int32 reinterpreted to 4 BE bytes.
func Marshal(msg ControlMsg) ([]byte, error) {
	switch msg.Type {

	case TypeInjectKeycode:
		if msg.InjectKeycode == nil {
			return nil, fmt.Errorf("%w: InjectKeycode required", ErrControlPayloadInvalid)
		}
		f := msg.InjectKeycode
		buf := make([]byte, 14)
		buf[0] = byte(TypeInjectKeycode)
		buf[1] = f.Action
		binary.BigEndian.PutUint32(buf[2:6], f.Keycode)
		binary.BigEndian.PutUint32(buf[6:10], f.Repeat)
		binary.BigEndian.PutUint32(buf[10:14], f.MetaState)
		return buf, nil

	case TypeInjectText:
		if msg.InjectText == nil {
			return nil, fmt.Errorf("%w: InjectText required", ErrControlPayloadInvalid)
		}
		txt := []byte(msg.InjectText.Text)
		if len(txt) > MaxInjectTextBytes {
			return nil, fmt.Errorf("%w: inject_text %d > %d", ErrControlPayloadTooLarge, len(txt), MaxInjectTextBytes)
		}
		buf := make([]byte, 5+len(txt))
		buf[0] = byte(TypeInjectText)
		binary.BigEndian.PutUint32(buf[1:5], uint32(len(txt)))
		copy(buf[5:], txt)
		return buf, nil

	case TypeInjectTouchEvent:
		if msg.InjectTouchEvent == nil {
			return nil, fmt.Errorf("%w: InjectTouchEvent required", ErrControlPayloadInvalid)
		}
		f := msg.InjectTouchEvent
		// Layout (32 bytes total): type(1) + action(1) + pointerId(8) +
		//                          x(4) + y(4) + width(2) + height(2) +
		//                          pressure(2) + actionButton(4) + buttons(4)
		buf := make([]byte, 32)
		buf[0] = byte(TypeInjectTouchEvent)
		buf[1] = f.Action
		binary.BigEndian.PutUint64(buf[2:10], f.PointerID)
		binary.BigEndian.PutUint32(buf[10:14], uint32(f.X))
		binary.BigEndian.PutUint32(buf[14:18], uint32(f.Y))
		binary.BigEndian.PutUint16(buf[18:20], f.Width)
		binary.BigEndian.PutUint16(buf[20:22], f.Height)
		binary.BigEndian.PutUint16(buf[22:24], f.Pressure)
		binary.BigEndian.PutUint32(buf[24:28], f.ActionButton)
		binary.BigEndian.PutUint32(buf[28:32], f.Buttons)
		return buf, nil

	case TypeInjectScrollEvent:
		if msg.InjectScrollEvent == nil {
			return nil, fmt.Errorf("%w: InjectScrollEvent required", ErrControlPayloadInvalid)
		}
		f := msg.InjectScrollEvent
		buf := make([]byte, 21)
		buf[0] = byte(TypeInjectScrollEvent)
		binary.BigEndian.PutUint32(buf[1:5], uint32(f.X))
		binary.BigEndian.PutUint32(buf[5:9], uint32(f.Y))
		binary.BigEndian.PutUint16(buf[9:11], f.Width)
		binary.BigEndian.PutUint16(buf[11:13], f.Height)
		binary.BigEndian.PutUint16(buf[13:15], uint16(f.HScroll))
		binary.BigEndian.PutUint16(buf[15:17], uint16(f.VScroll))
		binary.BigEndian.PutUint32(buf[17:21], f.Buttons)
		return buf, nil

	case TypeBackOrScreenOn:
		if msg.BackOrScreenOn == nil {
			return nil, fmt.Errorf("%w: BackOrScreenOn required", ErrControlPayloadInvalid)
		}
		return []byte{byte(TypeBackOrScreenOn), msg.BackOrScreenOn.Action}, nil

	case TypeExpandNotificationPanel:
		return []byte{byte(TypeExpandNotificationPanel)}, nil

	case TypeExpandSettingsPanel:
		return []byte{byte(TypeExpandSettingsPanel)}, nil

	case TypeCollapsePanels:
		return []byte{byte(TypeCollapsePanels)}, nil

	case TypeGetClipboard:
		if msg.GetClipboard == nil {
			return nil, fmt.Errorf("%w: GetClipboard required", ErrControlPayloadInvalid)
		}
		return []byte{byte(TypeGetClipboard), msg.GetClipboard.CopyKey}, nil

	case TypeSetClipboard:
		if msg.SetClipboard == nil {
			return nil, fmt.Errorf("%w: SetClipboard required", ErrControlPayloadInvalid)
		}
		f := msg.SetClipboard
		txt := []byte(f.Text)
		if len(txt) > MaxSetClipboardBytes {
			return nil, fmt.Errorf("%w: set_clipboard %d > %d", ErrControlPayloadTooLarge, len(txt), MaxSetClipboardBytes)
		}
		buf := make([]byte, 14+len(txt))
		buf[0] = byte(TypeSetClipboard)
		binary.BigEndian.PutUint64(buf[1:9], f.Sequence)
		if f.Paste {
			buf[9] = 1
		}
		binary.BigEndian.PutUint32(buf[10:14], uint32(len(txt)))
		copy(buf[14:], txt)
		return buf, nil

	case TypeSetDisplayPower:
		if msg.SetDisplayPower == nil {
			return nil, fmt.Errorf("%w: SetDisplayPower required", ErrControlPayloadInvalid)
		}
		return []byte{byte(TypeSetDisplayPower), msg.SetDisplayPower.Mode}, nil

	case TypeRotateDevice:
		return []byte{byte(TypeRotateDevice)}, nil

	case TypeUhidCreate:
		if msg.UhidCreate == nil {
			return nil, fmt.Errorf("%w: UhidCreate required", ErrControlPayloadInvalid)
		}
		f := msg.UhidCreate
		name := []byte(f.Name)
		if len(name) > 255 {
			return nil, fmt.Errorf("%w: uhid_create name %d > 255", ErrControlPayloadTooLarge, len(name))
		}
		if len(f.ReportDescriptor) > 0xFFFF {
			return nil, fmt.Errorf("%w: uhid_create report descriptor %d > 65535", ErrControlPayloadTooLarge, len(f.ReportDescriptor))
		}
		// type(1) + id(2) + vendorId(2) + productId(2) + nameLen(1) + name + rdLen(2) + rd
		sz := 1 + 2 + 2 + 2 + 1 + len(name) + 2 + len(f.ReportDescriptor)
		buf := make([]byte, sz)
		i := 0
		buf[i] = byte(TypeUhidCreate); i++
		binary.BigEndian.PutUint16(buf[i:i+2], f.ID); i += 2
		binary.BigEndian.PutUint16(buf[i:i+2], f.VendorID); i += 2
		binary.BigEndian.PutUint16(buf[i:i+2], f.ProductID); i += 2
		buf[i] = byte(len(name)); i++
		copy(buf[i:i+len(name)], name); i += len(name)
		binary.BigEndian.PutUint16(buf[i:i+2], uint16(len(f.ReportDescriptor))); i += 2
		copy(buf[i:i+len(f.ReportDescriptor)], f.ReportDescriptor)
		return buf, nil

	case TypeUhidInput:
		if msg.UhidInput == nil {
			return nil, fmt.Errorf("%w: UhidInput required", ErrControlPayloadInvalid)
		}
		f := msg.UhidInput
		if len(f.Data) > 0xFFFF {
			return nil, fmt.Errorf("%w: uhid_input %d > 65535", ErrControlPayloadTooLarge, len(f.Data))
		}
		// type(1) + id(2) + dataLen(2) + data
		buf := make([]byte, 5+len(f.Data))
		buf[0] = byte(TypeUhidInput)
		binary.BigEndian.PutUint16(buf[1:3], f.ID)
		binary.BigEndian.PutUint16(buf[3:5], uint16(len(f.Data)))
		copy(buf[5:], f.Data)
		return buf, nil

	case TypeUhidDestroy:
		if msg.UhidDestroy == nil {
			return nil, fmt.Errorf("%w: UhidDestroy required", ErrControlPayloadInvalid)
		}
		buf := make([]byte, 3)
		buf[0] = byte(TypeUhidDestroy)
		binary.BigEndian.PutUint16(buf[1:3], msg.UhidDestroy.ID)
		return buf, nil

	case TypeOpenHardKeyboardSettings:
		return []byte{byte(TypeOpenHardKeyboardSettings)}, nil

	case TypeStartApp:
		if msg.StartApp == nil {
			return nil, fmt.Errorf("%w: StartApp required", ErrControlPayloadInvalid)
		}
		name := []byte(msg.StartApp.Name)
		if len(name) > 255 {
			return nil, fmt.Errorf("%w: start_app name %d > 255", ErrControlPayloadTooLarge, len(name))
		}
		buf := make([]byte, 2+len(name))
		buf[0] = byte(TypeStartApp)
		buf[1] = byte(len(name))
		copy(buf[2:], name)
		return buf, nil

	case TypeResetVideo:
		return []byte{byte(TypeResetVideo)}, nil

	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownControlType, byte(msg.Type))
	}
}

// ControlWriter is the single-writer goroutine that drains in and writes
// marshalled control messages to the device control socket. Per D-14
// there is exactly one ControlWriter per device session.
type ControlWriter struct {
	in   chan ControlMsg
	conn net.Conn
	log  *slog.Logger
}

// ControlWriterOpts configures NewControlWriter.
type ControlWriterOpts struct {
	Conn        net.Conn    // device control socket (from launcher)
	Log         *slog.Logger
	BufferSize  int         // in-channel capacity; default 64 if 0
}

// NewControlWriter allocates a ControlWriter; call Run in an errgroup goroutine.
func NewControlWriter(opts ControlWriterOpts) *ControlWriter {
	size := opts.BufferSize
	if size <= 0 {
		size = 64
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &ControlWriter{
		in:   make(chan ControlMsg, size),
		conn: opts.Conn,
		log:  log,
	}
}

// In returns the send-only channel for enqueueing messages. The WS
// handler in plan 02-06 sends here only after JSON validation +
// lease check pass. Sends are non-blocking from the caller's POV
// (send blocks if buffer full, but caller is the per-WS handler
// goroutine which can tolerate brief back-pressure since one human's
// input rate is bounded).
func (cw *ControlWriter) In() chan<- ControlMsg {
	return cw.in
}

// Run drains cw.in, marshals each message, and writes to cw.conn.
// Returns ctx.Err() on cancellation, or io.ErrClosedPipe / similar
// on conn write failure. Marshal errors are logged but DO NOT abort
// the writer (single bad message must not kill the session).
//
// Single-writer discipline: cw.conn.Write is called from this goroutine
// ONLY. The supervisor's controlReader goroutine reads from the same
// net.Conn but Go's net.Conn permits concurrent Read+Write provided
// each direction has a single owner.
func (cw *ControlWriter) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-cw.in:
			if !ok {
				return nil
			}
			wire, err := Marshal(msg)
			if err != nil {
				cw.log.Warn("invalid control message dropped",
					"type", fmt.Sprintf("0x%02x", byte(msg.Type)),
					"error", err,
				)
				continue
			}
			if _, err := cw.conn.Write(wire); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					return err
				}
				cw.log.Error("control write failed",
					"type", fmt.Sprintf("0x%02x", byte(msg.Type)),
					"bytes", len(wire),
					"error", err,
				)
				return fmt.Errorf("control write: %w", err)
			}
		}
	}
}