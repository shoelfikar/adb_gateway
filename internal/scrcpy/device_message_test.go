package scrcpy

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeviceMessageClipboard(t *testing.T) {
	// type=0x00, len=5 (big-endian uint32), text="hello"
	var buf bytes.Buffer
	buf.WriteByte(0x00) // type: CLIPBOARD
	binary.Write(&buf, binary.BigEndian, uint32(5))
	buf.WriteString("hello")

	msg, err := ReadDeviceMessage(&buf)
	require.NoError(t, err)
	assert.Equal(t, DeviceMsgClipboard, msg.Type)
	require.NotNil(t, msg.Clipboard)
	assert.Equal(t, "hello", msg.Clipboard.Text)
}

func TestDeviceMessageAckClipboard(t *testing.T) {
	// type=0x01, sequence=42 (big-endian uint64)
	var buf bytes.Buffer
	buf.WriteByte(0x01) // type: ACK_CLIPBOARD
	binary.Write(&buf, binary.BigEndian, uint64(42))

	msg, err := ReadDeviceMessage(&buf)
	require.NoError(t, err)
	assert.Equal(t, DeviceMsgAckClipboard, msg.Type)
	require.NotNil(t, msg.AckClipboard)
	assert.Equal(t, uint64(42), msg.AckClipboard.Sequence)
}

func TestDeviceMessageUhidOutput(t *testing.T) {
	// type=0x02, id=1 (uint16 BE), dataLen=2 (uint16 BE), data=[0xAA, 0xBB]
	var buf bytes.Buffer
	buf.WriteByte(0x02) // type: UHID_OUTPUT
	binary.Write(&buf, binary.BigEndian, uint16(1))   // id
	binary.Write(&buf, binary.BigEndian, uint16(2))   // dataLen
	buf.Write([]byte{0xAA, 0xBB})                     // data

	msg, err := ReadDeviceMessage(&buf)
	require.NoError(t, err)
	assert.Equal(t, DeviceMsgUhidOutput, msg.Type)
	require.NotNil(t, msg.UhidOutput)
	assert.Equal(t, uint16(1), msg.UhidOutput.ID)
	assert.Equal(t, []byte{0xAA, 0xBB}, msg.UhidOutput.Data)
}

func TestDeviceMessageUnknownType(t *testing.T) {
	// type=0x99 → ErrUnknownDeviceMessage
	var buf bytes.Buffer
	buf.WriteByte(0x99)

	_, err := ReadDeviceMessage(&buf)
	assert.ErrorIs(t, err, ErrUnknownDeviceMessage)
}

func TestDeviceMessageEOF(t *testing.T) {
	// Empty reader → returns io.EOF directly
	_, err := ReadDeviceMessage(bytes.NewReader(nil))
	assert.ErrorIs(t, err, io.EOF)
}