package scrcpy

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameHeader represents the parsed 12-byte scrcpy video frame header.
// The raw bytes are preserved for zero-copy WebSocket relay.
//
// Frame header layout (big-endian):
//
//	Bytes 0-7: PTS with top 2 bits repurposed
//	  Bit 63: config packet flag
//	  Bit 62: keyframe flag
//	  Bits 0-61: presentation timestamp
//	Bytes 8-11: packet size (uint32, big-endian)
type FrameHeader struct {
	ConfigPacket bool
	KeyFrame     bool
	PTS          uint64
	Size         uint32
	rawHeader    [12]byte
}

// RawHeader returns the original 12 bytes of the frame header.
// This enables zero-copy WebSocket relay without re-encoding.
func (h FrameHeader) RawHeader() [12]byte {
	return h.rawHeader
}

// ReadCodecMeta reads the 12-byte codec metadata from the start of a scrcpy
// video stream. The format is:
//   - bytes 0-3: codec ID as 4-byte string (e.g. "h264")
//   - bytes 4-7: video width (uint32, big-endian)
//   - bytes 8-11: video height (uint32, big-endian)
//
// CRITICAL: Always use io.ReadFull, never conn.Read (TCP is a byte stream).
func ReadCodecMeta(r io.Reader) (codecID string, width, height uint32, err error) {
	var buf [12]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return "", 0, 0, fmt.Errorf("read codec meta: %w", err)
	}
	codecID = string(buf[:4])
	width = binary.BigEndian.Uint32(buf[4:8])
	height = binary.BigEndian.Uint32(buf[8:12])
	return codecID, width, height, nil
}

// ReadFrameHeader reads a 12-byte scrcpy video frame header.
// CRITICAL: Always use io.ReadFull, never conn.Read (TCP is a byte stream).
func ReadFrameHeader(r io.Reader) (FrameHeader, error) {
	var buf [12]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return FrameHeader{}, fmt.Errorf("read frame header: %w", err)
	}

	rawPTS := binary.BigEndian.Uint64(buf[:8])
	size := binary.BigEndian.Uint32(buf[8:12])

	return FrameHeader{
		ConfigPacket: rawPTS&(1<<63) != 0,
		KeyFrame:     rawPTS&(1<<62) != 0,
		PTS:          rawPTS &^ (3 << 62), // clear top 2 bits
		Size:         size,
		rawHeader:    buf,
	}, nil
}

// ReadVideoFrame reads a complete video frame (header + payload) from a scrcpy
// stream. It reads the 12-byte header first, then reads exactly hdr.Size bytes
// of payload.
//
// CRITICAL: Always uses io.ReadFull for frame boundaries. Never use conn.Read
// directly -- TCP is a byte stream and partial reads corrupt frame boundaries.
func ReadVideoFrame(r io.Reader) (FrameHeader, []byte, error) {
	hdr, err := ReadFrameHeader(r)
	if err != nil {
		return hdr, nil, err
	}

	if hdr.Size == 0 {
		return hdr, nil, nil
	}

	payload := make([]byte, hdr.Size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return hdr, nil, fmt.Errorf("read frame payload (%d bytes): %w", hdr.Size, err)
	}

	return hdr, payload, nil
}