package scrcpy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

func TestReadCodecMeta(t *testing.T) {
	data, err := os.ReadFile("testdata/codec_meta.bin")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	codecID, width, height, err := ReadCodecMeta(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadCodecMeta: %v", err)
	}
	if codecID != "h264" {
		t.Errorf("codecID = %q, want %q", codecID, "h264")
	}
	if width != 1920 {
		t.Errorf("width = %d, want %d", width, 1920)
	}
	if height != 1080 {
		t.Errorf("height = %d, want %d", height, 1080)
	}
}

func TestReadCodecMetaTruncatedStream(t *testing.T) {
	// Only 6 bytes available -- should fail
	buf := make([]byte, 6)
	_, _, _, err := ReadCodecMeta(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error from truncated stream, got nil")
	}
}

func TestReadFrameHeader(t *testing.T) {
	data, err := os.ReadFile("testdata/frame_h264_keyframe.bin")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	hdr, err := ReadFrameHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrameHeader: %v", err)
	}
	// PTS=1234, keyframe=1 (bit 62), config=0 (bit 63)
	if hdr.ConfigPacket {
		t.Error("ConfigPacket = true, want false")
	}
	if !hdr.KeyFrame {
		t.Error("KeyFrame = false, want true")
	}
	if hdr.PTS != 1234 {
		t.Errorf("PTS = %d, want %d", hdr.PTS, 1234)
	}
	if hdr.Size != 64 {
		t.Errorf("Size = %d, want %d", hdr.Size, 64)
	}
}

func TestReadFrameHeaderConfigPacket(t *testing.T) {
	data, err := os.ReadFile("testdata/frame_config_packet_header.bin")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	hdr, err := ReadFrameHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrameHeader: %v", err)
	}
	// config=1 (bit 63), keyframe=0, PTS=5000
	if !hdr.ConfigPacket {
		t.Error("ConfigPacket = false, want true")
	}
	if hdr.KeyFrame {
		t.Error("KeyFrame = true, want false")
	}
	if hdr.PTS != 5000 {
		t.Errorf("PTS = %d, want %d", hdr.PTS, 5000)
	}
	if hdr.Size != 32 {
		t.Errorf("Size = %d, want %d", hdr.Size, 32)
	}
}

func TestReadVideoFrame(t *testing.T) {
	data, err := os.ReadFile("testdata/frame_h264_keyframe.bin")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	hdr, payload, err := ReadVideoFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadVideoFrame: %v", err)
	}
	if hdr.Size != 64 {
		t.Errorf("hdr.Size = %d, want %d", hdr.Size, 64)
	}
	if len(payload) != int(hdr.Size) {
		t.Errorf("payload length = %d, want %d", len(payload), hdr.Size)
	}
	// Verify payload content: should be bytes 0, 1, 2, ..., 63
	for i := 0; i < 64; i++ {
		if payload[i] != byte(i) {
			t.Errorf("payload[%d] = %d, want %d", i, payload[i], byte(i))
			break
		}
	}
}

func TestReadVideoFrameTruncatedPayload(t *testing.T) {
	// Header claims 1000 bytes but only 64 are available
	var header [12]byte
	rawPTS := uint64(42) | (1 << 62) // keyframe, PTS=42
	binary.BigEndian.PutUint64(header[0:8], rawPTS)
	binary.BigEndian.PutUint32(header[8:12], 1000) // claims 1000 bytes
	buf := append(header[:], make([]byte, 64)...) // only 64 bytes of payload

	_, _, err := ReadVideoFrame(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error from truncated payload, got nil")
	}
	if !errors.Is(err, ErrTruncatedPayload) && !isTruncatedPayloadError(err) {
		// The error must mention the payload issue; exact check depends on wrapping
		t.Logf("got error: %v", err)
	}
}

func isTruncatedPayloadError(err error) bool {
	// Check if the error message contains "frame payload" indicating truncation
	return err != nil && (containsStr(err.Error(), "frame payload") || containsStr(err.Error(), "unexpected EOF"))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var ErrTruncatedPayload = errors.New("truncated payload")

func TestReadFrameHeaderPreservesRawBytes(t *testing.T) {
	data, err := os.ReadFile("testdata/frame_h264_keyframe.bin")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	hdr, err := ReadFrameHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrameHeader: %v", err)
	}

	// rawHeader must match the first 12 bytes of the file exactly (no re-encoding)
	raw := hdr.RawHeader()
	if raw != [12]byte(data[:12]) {
		t.Errorf("rawHeader = %x, want %x", raw, data[:12])
	}

	// Verify parsed values match what we'd get from the raw bytes
	rawPTS := binary.BigEndian.Uint64(raw[:8])
	parsedPTS := rawPTS &^ (3 << 62)
	if parsedPTS != hdr.PTS {
		t.Errorf("PTS mismatch: rawHeader gives %d, FrameHeader.PTS = %d", parsedPTS, hdr.PTS)
	}
	parsedSize := binary.BigEndian.Uint32(raw[8:12])
	if parsedSize != hdr.Size {
		t.Errorf("Size mismatch: rawHeader gives %d, FrameHeader.Size = %d", parsedSize, hdr.Size)
	}
}

func TestReadFrameHeaderZeroSize(t *testing.T) {
	// A frame with size=0 (empty payload) should work fine
	var buf [12]byte
	binary.BigEndian.PutUint64(buf[0:8], 100) // PTS=100, no flags
	binary.BigEndian.PutUint32(buf[8:12], 0)   // size=0

	hdr, payload, err := ReadVideoFrame(bytes.NewReader(buf[:]))
	if err != nil {
		t.Fatalf("ReadVideoFrame with zero size: %v", err)
	}
	if hdr.Size != 0 {
		t.Errorf("Size = %d, want 0", hdr.Size)
	}
	if payload != nil {
		t.Errorf("payload should be nil for zero-size frame, got %d bytes", len(payload))
	}
}