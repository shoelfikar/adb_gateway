package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// DeviceMessageType byte values per scrcpy v3.3.4 DeviceMessage stream
// (return path on the control socket). Layout is documented in
// 02-RESEARCH.md Pattern 4 (A3 LOW confidence — verify against real device fixtures during execution).
type DeviceMessageType byte

const (
	DeviceMsgClipboard    DeviceMessageType = 0x00
	DeviceMsgAckClipboard DeviceMessageType = 0x01
	DeviceMsgUhidOutput   DeviceMessageType = 0x02
)

// DeviceMessage is the typed envelope for any scrcpy DeviceMessage.
// Exactly one per-type field pointer is set, matching Type.
type DeviceMessage struct {
	Type         DeviceMessageType
	Clipboard    *ClipboardFields
	AckClipboard *AckClipboardFields
	UhidOutput   *UhidOutputFields
}

type ClipboardFields struct{ Text string }
type AckClipboardFields struct{ Sequence uint64 }
type UhidOutputFields struct {
	ID   uint16
	Data []byte
}

// Errors.
var (
	ErrUnknownDeviceMessage  = errors.New("scrcpy: unknown DeviceMessage type")
	ErrDeviceMessageOversize = errors.New("scrcpy: DeviceMessage payload exceeds expected limit")
)

// ReadDeviceMessage reads one DeviceMessage from r. CRITICAL: io.ReadFull only.
// Returns io.EOF / io.ErrUnexpectedEOF cleanly so the caller can detect socket close.
func ReadDeviceMessage(r io.Reader) (DeviceMessage, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return DeviceMessage{}, err
	}
	switch DeviceMessageType(t[0]) {

	case DeviceMsgClipboard:
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return DeviceMessage{}, fmt.Errorf("read clipboard len: %w", err)
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n > 262144 {
			return DeviceMessage{}, fmt.Errorf("%w: clipboard %d > 262144", ErrDeviceMessageOversize, n)
		}
		payload := make([]byte, n)
		if n > 0 {
			if _, err := io.ReadFull(r, payload); err != nil {
				return DeviceMessage{}, fmt.Errorf("read clipboard text: %w", err)
			}
		}
		return DeviceMessage{
			Type:      DeviceMsgClipboard,
			Clipboard: &ClipboardFields{Text: string(payload)},
		}, nil

	case DeviceMsgAckClipboard:
		var seqBuf [8]byte
		if _, err := io.ReadFull(r, seqBuf[:]); err != nil {
			return DeviceMessage{}, fmt.Errorf("read ack seq: %w", err)
		}
		return DeviceMessage{
			Type:         DeviceMsgAckClipboard,
			AckClipboard: &AckClipboardFields{Sequence: binary.BigEndian.Uint64(seqBuf[:])},
		}, nil

	case DeviceMsgUhidOutput:
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return DeviceMessage{}, fmt.Errorf("read uhid header: %w", err)
		}
		id := binary.BigEndian.Uint16(hdr[0:2])
		dataLen := binary.BigEndian.Uint16(hdr[2:4])
		data := make([]byte, dataLen)
		if dataLen > 0 {
			if _, err := io.ReadFull(r, data); err != nil {
				return DeviceMessage{}, fmt.Errorf("read uhid data: %w", err)
			}
		}
		return DeviceMessage{
			Type:       DeviceMsgUhidOutput,
			UhidOutput: &UhidOutputFields{ID: id, Data: data},
		}, nil

	default:
		return DeviceMessage{}, fmt.Errorf("%w: 0x%02x", ErrUnknownDeviceMessage, t[0])
	}
}