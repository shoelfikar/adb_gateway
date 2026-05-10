// Package session — recording_test.go covers the per-device Recording
// subscriber: MKV mux, file rotation, slow-disk eviction, lifecycle.
package session

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeFrameWire builds a chan-format frame: 12-byte scrcpy header || payload.
// Layout (per Phase 1 RESEARCH.md): 8 bytes PTS big-endian (top bit = keyframe
// flag), 4 bytes payload size big-endian.
func makeFrameWire(pts uint64, keyframe bool, payload []byte) []byte {
	buf := make([]byte, 12+len(payload))
	if keyframe {
		pts |= 1 << 63
	}
	binary.BigEndian.PutUint64(buf[0:8], pts)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(payload)))
	copy(buf[12:], payload)
	return buf
}

// makeAnnexBNALU builds an Annex-B framed NAL unit (00 00 00 01 || nal_byte || body).
// nalType is the lower-5-bit nal_unit_type; nal_byte is forbidden_zero_bit(0) +
// nal_ref_idc(3) + nal_unit_type(5).
func makeAnnexBNALU(nalType byte, body []byte) []byte {
	out := []byte{0x00, 0x00, 0x00, 0x01, nalType}
	out = append(out, body...)
	return out
}

// minimalSPS / minimalPPS are syntactically marked nal-units. They are NOT
// fully compliant SPS/PPS — but they have the right NAL types and our
// extractor only inspects the NAL type byte. Real-device validation
// (ffprobe-gated tests) is deferred per the plan.
func minimalSPS() []byte {
	// nal_unit_type=7 (SPS), nal_ref_idc=3 -> nal_byte = 0x67.
	return makeAnnexBNALU(0x67, []byte{0x42, 0x00, 0x1e, 0x95, 0xa0, 0xb0, 0x40})
}
func minimalPPS() []byte {
	// nal_unit_type=8 (PPS), nal_ref_idc=3 -> nal_byte = 0x68.
	return makeAnnexBNALU(0x68, []byte{0xce, 0x3c, 0x80})
}
func minimalIDR() []byte {
	return makeAnnexBNALU(0x65, []byte{0x88, 0x84, 0x00, 0x10, 0x00})
}
func minimalPFrame(seq byte) []byte {
	return makeAnnexBNALU(0x41, []byte{0x9a, seq, seq, seq, seq})
}

func TestRecordingHappyPath(t *testing.T) {
	dir := t.TempDir()
	hub := newTestHub(t, "video")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	id := uuid.New()
	rec, err := NewRecording(hub, id, "ABC123", dir, RecordingOpts{
		MaxFileBytes: 0, // unlimited (no rotation in this test)
		Log:          slog.Default(),
	})
	require.NoError(t, err)

	recCtx, recCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- rec.Run(recCtx) }()

	// Build first frame: SPS+PPS+IDR concatenated.
	idrPayload := append(append(minimalSPS(), minimalPPS()...), minimalIDR()...)
	hub.Publish(parseAndPackFrame(t, makeFrameWire(1000, true, idrPayload)))
	for i := byte(2); i <= 5; i++ {
		hub.Publish(parseAndPackFrame(t, makeFrameWire(uint64(i)*1000, false, minimalPFrame(i))))
	}

	time.Sleep(200 * time.Millisecond)
	recCancel()

	select {
	case err := <-runDone:
		// On clean shutdown we expect either nil or ctx.Canceled.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recording did not stop after ctx cancel")
	}

	// Verify file exists and is non-empty.
	files, err := filepath.Glob(filepath.Join(dir, "ABC123", "*.mkv"))
	require.NoError(t, err)
	require.Len(t, files, 1, "exactly one rotation segment expected")
	stat, err := os.Stat(files[0])
	require.NoError(t, err)
	assert.Greater(t, stat.Size(), int64(0), "recording file must be non-empty")
	assert.Greater(t, rec.FramesWritten(), int64(0), "frames counter must advance")
}

// parseAndPackFrame unwraps a chan-format wire byte slice (12-byte header ||
// payload) into a *Frame the way the producer side does it.
func parseAndPackFrame(t *testing.T, wire []byte) *Frame {
	t.Helper()
	require.GreaterOrEqual(t, len(wire), 12)
	var hdr [12]byte
	copy(hdr[:], wire[:12])
	pts := binary.BigEndian.Uint64(hdr[0:8])
	keyframe := (pts & (1 << 63)) != 0
	return &Frame{
		Header:   hdr,
		Payload:  append([]byte(nil), wire[12:]...),
		KeyFrame: keyframe,
	}
}

// newTestHub builds a hub with sane defaults for tests. The hub's Run goroutine
// is started by the caller.
func newTestHub(t *testing.T, stream string) *Hub {
	t.Helper()
	hub := NewHub(HubOpts{
		Stream:              stream,
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.Default(),
	})
	hub.SetCodecMeta([12]byte{})
	return hub
}

func TestRecordingSlowConsumer(t *testing.T) {
	// Build a Hub with very small buffer + low drop threshold so the
	// recorder is evicted quickly.
	hub := NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           2,
		MaxConsecutiveDrops: 3,
		Log:                 slog.Default(),
	})
	hub.SetCodecMeta([12]byte{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	dir := t.TempDir()
	id := uuid.New()

	// Inject a stub muxer that BLOCKS on Write so the recorder cannot drain
	// its sub channel.
	blockMux := newBlockingMuxer()
	rec, err := NewRecording(hub, id, "ABC123", dir, RecordingOpts{
		MaxFileBytes: 0,
		Log:          slog.Default(),
		MuxerFactory: func(_ string) (recordingMuxer, error) { return blockMux, nil },
	})
	require.NoError(t, err)

	recCtx, recCancel := context.WithCancel(context.Background())
	defer recCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- rec.Run(recCtx) }()

	// Also subscribe a healthy viewer — it MUST keep receiving frames after
	// the recorder is evicted (D-18 architectural insurance).
	viewerCh, unsub, err := hub.Subscribe("healthy-viewer")
	require.NoError(t, err)
	defer unsub()

	// Drain healthy viewer in background to keep it healthy.
	healthyDone := make(chan int, 1)
	go func() {
		count := 0
		for range viewerCh {
			count++
			if count >= 10 {
				healthyDone <- count
				return
			}
		}
		healthyDone <- count
	}()

	// Hammer 100 frames into the Hub.
	idrPayload := append(append(minimalSPS(), minimalPPS()...), minimalIDR()...)
	for i := 0; i < 100; i++ {
		hub.Publish(&Frame{
			Header:   buildHeader(uint64(i+1)*1000, len(idrPayload), i%5 == 0),
			Payload:  idrPayload,
			KeyFrame: i%5 == 0,
		})
		time.Sleep(2 * time.Millisecond)
	}

	// Recording must terminate with ErrRecordingFailed (slow-disk eviction).
	select {
	case err := <-runDone:
		require.Error(t, err, "recording must return error on Hub eviction")
		assert.Contains(t, err.Error(), "evicted", "error must mention slow-consumer eviction")
	case <-time.After(3 * time.Second):
		t.Fatal("recording did not exit after Hub eviction")
	}

	// Healthy viewer should have received ≥10 frames despite slow recorder.
	select {
	case got := <-healthyDone:
		assert.GreaterOrEqual(t, got, 10, "healthy viewer must keep receiving frames while recorder is evicted")
	case <-time.After(2 * time.Second):
		t.Fatal("healthy viewer did not receive enough frames — recorder back-pressured the Hub (BUG)")
	}
}

func buildHeader(pts uint64, size int, keyframe bool) [12]byte {
	if keyframe {
		pts |= 1 << 63
	}
	var h [12]byte
	binary.BigEndian.PutUint64(h[0:8], pts)
	binary.BigEndian.PutUint32(h[8:12], uint32(size))
	return h
}

// blockingMuxer always blocks on WriteFrame until released. Used to
// simulate a slow disk in TestRecordingSlowConsumer.
type blockingMuxer struct {
	mu       sync.Mutex
	released chan struct{}
}

func newBlockingMuxer() *blockingMuxer {
	return &blockingMuxer{released: make(chan struct{})}
}

func (m *blockingMuxer) WriteFrame(_ context.Context, _ bool, _ uint64, _ []byte) (int, error) {
	<-m.released
	return 0, errors.New("blockingMuxer: never returns")
}

func (m *blockingMuxer) WriteTrackHeader(_ []byte, _ []byte) error { return nil }

func (m *blockingMuxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.released:
	default:
		close(m.released)
	}
	return nil
}

func TestRecordingRotation(t *testing.T) {
	dir := t.TempDir()
	hub := newTestHub(t, "video")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	id := uuid.New()
	// Use a tiny rotation cap so even a few frames trigger rotation.
	rec, err := NewRecording(hub, id, "ABC123", dir, RecordingOpts{
		MaxFileBytes: 256, // bytes
		Log:          slog.Default(),
	})
	require.NoError(t, err)

	recCtx, recCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- rec.Run(recCtx) }()

	// Big payload to trigger rotation quickly.
	bigPayload := make([]byte, 200)
	for i := range bigPayload {
		bigPayload[i] = 0x9a
	}
	idrPayload := append(append(minimalSPS(), minimalPPS()...), minimalIDR()...)
	hub.Publish(parseAndPackFrame(t, makeFrameWire(1000, true, idrPayload)))
	for i := uint64(2); i <= 10; i++ {
		hub.Publish(parseAndPackFrame(t, makeFrameWire(i*1000, false, bigPayload)))
	}

	time.Sleep(300 * time.Millisecond)
	recCancel()
	<-runDone

	files, err := filepath.Glob(filepath.Join(dir, "ABC123", "*.mkv"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(files), 2, "recording must rotate to ≥2 segments at MaxFileBytes")
}
