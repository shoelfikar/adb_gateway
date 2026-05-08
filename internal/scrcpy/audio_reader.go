package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Audio codec IDs per scrcpy v3.3.4 audio/AudioCodec.java [VERIFIED].
type AudioCodec uint32

const (
	AudioCodecOPUS AudioCodec = 0x6f707573 // "opus"
	AudioCodecAAC  AudioCodec = 0x00616163 // "\0aac"
	AudioCodecFLAC AudioCodec = 0x666c6163 // "flac"
	AudioCodecRAW  AudioCodec = 0x00726177 // "\0raw"
)

// ErrAudioUnavailable is returned when the audio socket signals "no audio
// for this device" — either by writing 0x00000000 as the codec ID, by
// closing immediately (EOF before codec ID arrives), or by writing an
// unknown codec value. Per Assumption A1 (LOW confidence) the gateway
// treats all three as "audio unavailable" and the supervisor sets
// DeviceEntry.AudioAvailable = false.
var ErrAudioUnavailable = errors.New("scrcpy: audio unavailable for this device")

// ReadAudioCodecID reads the 4-byte big-endian codec ID at the start of
// the audio stream. Returns:
//
//	codec    : the raw codec value (zero on EOF/disabled).
//	available: true iff codec is one of the four known IDs.
//	err      : non-nil only on read errors that aren't EOF.
//
// CRITICAL: Always use io.ReadFull, never conn.Read (TCP is a byte stream).
func ReadAudioCodecID(r io.Reader) (codec AudioCodec, available bool, err error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read audio codec id: %w", err)
	}
	codec = AudioCodec(binary.BigEndian.Uint32(buf[:]))
	switch codec {
	case AudioCodecOPUS, AudioCodecAAC, AudioCodecFLAC, AudioCodecRAW:
		return codec, true, nil
	case 0:
		// Sentinel for "audio disabled" per likely scrcpy convention (A1).
		return 0, false, nil
	default:
		// Unknown codec — treat as unavailable for safety.
		return codec, false, nil
	}
}

// ReadAudioFrame reads a single scrcpy audio frame: 12-byte header + payload.
// Reuses the video frame reader because the layout is identical post-codec-id (D-13).
func ReadAudioFrame(r io.Reader) (FrameHeader, []byte, error) {
	return ReadVideoFrame(r)
}