package scrcpy

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAudioCodecOPUS(t *testing.T) {
	// 0x6f707573 = "opus"
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(AudioCodecOPUS))
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, AudioCodecOPUS, codec)
}

func TestAudioCodecAAC(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(AudioCodecAAC))
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, AudioCodecAAC, codec)
}

func TestAudioCodecFLAC(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(AudioCodecFLAC))
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, AudioCodecFLAC, codec)
}

func TestAudioCodecRAW(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(AudioCodecRAW))
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, AudioCodecRAW, codec)
}

func TestAudioCodecZeroSentinel(t *testing.T) {
	// 0x00000000 → available=false, err=nil (defensive parse per A1)
	buf := make([]byte, 4) // all zeros
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.False(t, available)
	assert.Equal(t, AudioCodec(0), codec)
}

func TestAudioCodecImmediateEOF(t *testing.T) {
	// Empty reader → available=false, err=nil
	codec, available, err := ReadAudioCodecID(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.False(t, available)
	assert.Equal(t, AudioCodec(0), codec)
}

func TestAudioCodecUnknownValue(t *testing.T) {
	// 0xDEADBEEF → codec=0xDEADBEEF, available=false, err=nil (logged unknown)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, 0xDEADBEEF)
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.False(t, available)
	assert.Equal(t, AudioCodec(0xDEADBEEF), codec)
}

func TestAudioFrameRoundtrip(t *testing.T) {
	// After codec ID, audio frame layout is 12-byte header + payload (identical to video).
	// Build a header: PTS=100, keyframe=1, size=8
	var hdr [12]byte
	rawPTS := uint64(100) | (1 << 62) // keyframe flag
	binary.BigEndian.PutUint64(hdr[0:8], rawPTS)
	binary.BigEndian.PutUint32(hdr[8:12], 8) // 8 bytes of payload
	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}

	data := append(hdr[:], payload...)
	gotHdr, gotPayload, err := ReadAudioFrame(bytes.NewReader(data))
	require.NoError(t, err)
	assert.True(t, gotHdr.KeyFrame)
	assert.Equal(t, uint32(8), gotHdr.Size)
	assert.Equal(t, payload, gotPayload)
}