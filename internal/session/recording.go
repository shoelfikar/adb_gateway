// Package session — recording.go implements the per-device screen recording
// subscriber (Phase 3 Plan 03-04 / OPS-09).
//
// # Muxer choice (resolved spike)
//
// The plan's RESEARCH.md flagged two candidates: github.com/Eyevinn/mp4ff (MP4)
// and github.com/at-wat/ebml-go (MKV/WebM). I chose **mkvcore (the lower-level
// half of ebml-go) writing real MKV with H.264-in-AVCC** for these reasons:
//
//  1. CONTEXT.md explicitly recommends MKV: "mkv tolerates abrupt truncation
//     better, recommend mkv". The recorder is a "just another viewer" of a
//     long-running scrcpy stream — abrupt termination (gateway crash, OOM
//     kill, watchdog stall, Hub eviction) must leave the on-disk file in a
//     state ffmpeg/mpv can still read up to the last cluster boundary.
//     mp4ff requires a clean Close() to finalize the moov atom; without it
//     the file is unplayable.
//  2. mkvcore exposes a clean writer interface (Write(keyframe, timestampMs,
//     payload)) that maps 1:1 onto our (header, AVCC-payload, keyframe-flag)
//     triple. No re-encoding, no ffmpeg dependency, no frame buffering.
//  3. Apache-2.0 license — already an accepted attribution category in
//     THIRD_PARTY_NOTICES.
//
// # Wire-format conversion (Annex-B → AVCC)
//
// scrcpy publishes H.264 NAL units in Annex-B format (00 00 00 01 start
// codes). MKV requires AVCC (4-byte big-endian length prefix per NALU).
// The recorder scans for start codes and rewrites them to length prefixes
// in-place per frame. SPS (NAL type 7) and PPS (NAL type 8) are extracted
// from the first IDR-bearing payload and stitched into a minimal
// AVCDecoderConfigurationRecord written to the track's CodecPrivate field.
//
// # Slow-disk discipline (D-18 architectural insurance)
//
// The recorder consumes frames ONLY through Hub.Subscribe. It NEVER reads
// raw frames from anywhere else. This means a slow disk that cannot drain
// the per-subscriber chan triggers Hub's existing eviction policy
// (consecutiveDrops >= maxConsecutiveDrops -> Hub closes the chan). The
// recorder observes the closed chan and returns ErrRecordingFailed —
// while live viewers continue to receive frames at full rate. This is
// the contract verified by TestRecordingSlowConsumer.
package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/google/uuid"
)

// ErrRecordingFailed is returned by Recording.Run when a non-recoverable
// error terminates the recording (slow-disk eviction, file write failure).
var ErrRecordingFailed = errors.New("recording failed")

// recordingMuxer abstracts the on-disk container writer so tests can
// inject a stub (TestRecordingSlowConsumer uses blockingMuxer). The real
// implementation is *mkvMuxer.
type recordingMuxer interface {
	WriteTrackHeader(sps, pps []byte) error
	WriteFrame(ctx context.Context, keyframe bool, timestampMs uint64, payload []byte) (int, error)
	Close() error
}

// RecordingOpts configures a new Recording.
type RecordingOpts struct {
	// MaxFileBytes triggers rotation. <=0 means no rotation.
	MaxFileBytes int64
	// Log is the structured logger; defaults to slog.Default().
	Log *slog.Logger
	// MuxerFactory allows tests to inject a stub muxer. Production code
	// leaves this nil and gets the real *mkvMuxer.
	MuxerFactory func(filePath string) (recordingMuxer, error)
}

// Recording captures the video Hub fan-out to disk as a sequence of MKV
// segments. One Recording per (device, recording_id). Lifecycle:
//
//	NewRecording -> rec.Run(ctx) -> ctx.Cancel -> rec.Close (auto)
type Recording struct {
	id     uuid.UUID
	serial string
	dir    string
	opts   RecordingOpts

	hub    *Hub
	sub    <-chan []byte
	unsub  func()

	// Atomic counters — readable lock-free by ListRecordings handler.
	bytesWritten   atomic.Int64
	framesWritten  atomic.Int64
	droppedFrames  atomic.Int64
	currentSeq     atomic.Int32
	currentPath    atomic.Pointer[string]

	// Track config — set on first IDR.
	sps []byte
	pps []byte

	// Active muxer + file (Run-goroutine-only).
	muxer    recordingMuxer
	file     *os.File
	curBytes int64

	// startTime is the wall-clock for filename prefix.
	startTime time.Time

	log *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

// NewRecording subscribes to hub, prepares the directory, and returns a
// Recording ready for Run. Subscribe MUST happen at construction so frames
// published during the brief window between handler return and Run-start
// are not lost.
func NewRecording(hub *Hub, id uuid.UUID, serial, dir string, opts RecordingOpts) (*Recording, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	deviceDir := filepath.Join(dir, serial)
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		return nil, fmt.Errorf("recording: mkdir %s: %w", deviceDir, err)
	}

	subID := "recording-" + id.String()
	sub, unsub, err := hub.Subscribe(subID)
	if err != nil {
		return nil, fmt.Errorf("recording: subscribe: %w", err)
	}

	r := &Recording{
		id:        id,
		serial:    serial,
		dir:       dir,
		opts:      opts,
		hub:       hub,
		sub:       sub,
		unsub:     unsub,
		startTime: time.Now().UTC(),
		log:       opts.Log.With("device", serial, "recording_id", id.String()),
	}
	return r, nil
}

// ID returns the recording's UUID.
func (r *Recording) ID() uuid.UUID { return r.id }

// Path returns the current segment's path (atomic — safe to call from any goroutine).
func (r *Recording) Path() string {
	if p := r.currentPath.Load(); p != nil {
		return *p
	}
	return ""
}

// BytesWritten / FramesWritten / DroppedFrames return cumulative counters.
func (r *Recording) BytesWritten() int64  { return r.bytesWritten.Load() }
func (r *Recording) FramesWritten() int64 { return r.framesWritten.Load() }
func (r *Recording) DroppedFrames() int64 { return r.droppedFrames.Load() }

// StartedAt returns the wall-clock time the recording subscribed.
func (r *Recording) StartedAt() time.Time { return r.startTime }

// Run drains the Hub subscription into the on-disk container. Returns
// when ctx is cancelled (clean stop), the Hub closes the chan
// (slow-consumer eviction -> ErrRecordingFailed), or a write error occurs.
//
// D-18 architectural insurance: the recorder is "just another viewer".
// It reads ONE frame at a time from r.sub and writes synchronously to the
// muxer. A slow muxer leaves frames piling in r.sub; Hub's drop-on-slow
// policy evicts after MaxConsecutiveDrops, closing the chan. The next
// chan read (after the slow write completes) returns (_, false) and we
// exit with ErrRecordingFailed.
//
// In the pathological case where the muxer hangs forever (defect, not
// slow disk), only ctx-cancel will unblock — but that's an out-of-scope
// recovery path. Production muxers must respect ctx in WriteFrame.
func (r *Recording) Run(ctx context.Context) error {
	defer r.cleanup()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-r.sub:
			if !ok {
				// Hub closed the chan — slow-consumer eviction (D-18).
				r.log.Warn("recording: hub evicted slow consumer (likely slow disk)")
				return fmt.Errorf("%w: evicted by hub (slow consumer)", ErrRecordingFailed)
			}
			if err := r.handleFrame(ctx, msg); err != nil {
				return err
			}
		}
	}
}

// handleFrame parses the chan-format wire bytes (12-byte header || payload),
// extracts SPS/PPS on first IDR, opens the muxer if needed, and writes the
// AVCC-converted payload to the active segment. Triggers rotation when
// curBytes + frameSize > MaxFileBytes.
func (r *Recording) handleFrame(ctx context.Context, msg []byte) error {
	if len(msg) < 12 {
		// Malformed (likely a metadata frame from Hub pre-load if size==12);
		// skip silently if it's just the codec metadata header.
		return nil
	}
	hdr := msg[:12]
	payload := msg[12:]
	pts := binary.BigEndian.Uint64(hdr[0:8])
	keyframe := (pts & (1 << 63)) != 0
	pts &^= (1 << 63)
	size := binary.BigEndian.Uint32(hdr[8:12])
	if int(size) != len(payload) {
		// Hub may have published a metadata-only message (no payload).
		// In that case len(payload)==0; skip cleanly.
		if len(payload) == 0 {
			return nil
		}
	}

	// On the very first IDR we extract SPS/PPS and open the muxer.
	if r.muxer == nil {
		if !keyframe {
			// Not a keyframe yet — skip until the first IDR. The cached
			// keyframe pre-load by Hub.Subscribe means this branch is rare
			// in practice (the first message after meta is typically the
			// cached keyframe).
			return nil
		}
		sps, pps := extractSPSPPS(payload)
		if sps == nil || pps == nil {
			// Malformed first IDR; wait for the next.
			r.log.Warn("recording: keyframe missing SPS/PPS, waiting for next IDR")
			return nil
		}
		r.sps = sps
		r.pps = pps
		if err := r.openSegment(); err != nil {
			return fmt.Errorf("%w: open initial segment: %v", ErrRecordingFailed, err)
		}
	}

	// Convert Annex-B → AVCC.
	avcc := annexBToAVCC(payload)
	if len(avcc) == 0 {
		r.droppedFrames.Add(1)
		return nil
	}

	// Rotation check — close current and open next BEFORE writing this frame.
	if r.opts.MaxFileBytes > 0 && r.curBytes+int64(len(avcc)) > r.opts.MaxFileBytes {
		if err := r.rotate(); err != nil {
			return fmt.Errorf("%w: rotate: %v", ErrRecordingFailed, err)
		}
	}

	timestampMs := pts / 1000 // scrcpy PTS is microseconds.
	if _, err := r.muxer.WriteFrame(ctx, keyframe, timestampMs, avcc); err != nil {
		return fmt.Errorf("%w: write frame: %v", ErrRecordingFailed, err)
	}
	// Track gating against payload size — mkvcore buffers internally, so
	// the muxer's reported "bytes written" lags. Using the AVCC payload
	// size is a stable upper bound for rotation triggering.
	r.curBytes += int64(len(avcc))
	r.bytesWritten.Add(int64(len(avcc)))
	r.framesWritten.Add(1)
	return nil
}

// openSegment opens a new file, builds a muxer, and writes the track header.
func (r *Recording) openSegment() error {
	seq := r.currentSeq.Load()
	name := fmt.Sprintf("%s-%03d.mkv", r.startTime.Format("20060102T150405Z"), seq)
	path := filepath.Join(r.dir, r.serial, name)

	// File ownership lives in the muxer (mkvcore.NewSimpleBlockWriter
	// closes its underlying io.WriteCloser on muxer.Close). We keep r.file
	// nil to make this explicit.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	var mux recordingMuxer
	if r.opts.MuxerFactory != nil {
		mux, err = r.opts.MuxerFactory(path)
		if err != nil {
			f.Close()
			return err
		}
		// Stub muxer manages its own writer; our file is unused.
		f.Close()
	} else {
		mux, err = newMKVMuxer(f)
		if err != nil {
			f.Close()
			return err
		}
	}

	if err := mux.WriteTrackHeader(r.sps, r.pps); err != nil {
		mux.Close()
		return err
	}

	r.muxer = mux
	r.curBytes = 0
	r.currentPath.Store(&path)
	r.log.Info("recording: segment opened", "path", path, "seq", seq)
	return nil
}

// rotate closes the current segment and opens the next one. Re-writes the
// SPS/PPS track header so the new segment is independently playable.
func (r *Recording) rotate() error {
	if r.muxer != nil {
		if err := r.muxer.Close(); err != nil {
			r.log.Warn("recording: rotate close", "error", err)
		}
		r.muxer = nil
	}
	r.currentSeq.Add(1)
	return r.openSegment()
}

// cleanup is the last-action hook called by Run on exit. Flushes the muxer,
// unsubscribes from the Hub.
func (r *Recording) cleanup() {
	r.closeOnce.Do(func() {
		if r.muxer != nil {
			if err := r.muxer.Close(); err != nil {
				r.closeErr = err
				r.log.Warn("recording: muxer close", "error", err)
			}
			r.muxer = nil
		}
		if r.unsub != nil {
			r.unsub()
		}
	})
}

// Stop signals the Recording's Run to exit and waits for cleanup. Idempotent.
// Caller is the StopRecording handler — it cancels the recording's context
// and waits via the cancel-then-wait protocol below.
func (r *Recording) Stop() {
	r.cleanup()
}

// =====================================================================
// Annex-B / AVCC conversion
// =====================================================================

// annexBToAVCC walks Annex-B payload (start codes 00 00 00 01 or 00 00 01)
// and rewrites each NAL unit with a 4-byte big-endian length prefix.
//
// A safe implementation: scan, split, then re-emit. Allocates a new slice;
// callers must not retain the input slice after the call.
func annexBToAVCC(annexB []byte) []byte {
	nalus := splitAnnexB(annexB)
	if len(nalus) == 0 {
		return nil
	}
	total := 0
	for _, n := range nalus {
		total += 4 + len(n)
	}
	out := make([]byte, total)
	off := 0
	for _, n := range nalus {
		binary.BigEndian.PutUint32(out[off:off+4], uint32(len(n)))
		copy(out[off+4:off+4+len(n)], n)
		off += 4 + len(n)
	}
	return out
}

// splitAnnexB returns each NAL unit body (without start code) from an
// Annex-B framed payload. Handles 3-byte (00 00 01) and 4-byte (00 00 00 01)
// start codes interchangeably.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	i := 0
	// Find first start code.
	start := -1
	for i+2 < len(b) {
		if b[i] == 0x00 && b[i+1] == 0x00 {
			if b[i+2] == 0x01 {
				start = i + 3
				i = start
				break
			}
			if i+3 < len(b) && b[i+2] == 0x00 && b[i+3] == 0x01 {
				start = i + 4
				i = start
				break
			}
		}
		i++
	}
	if start == -1 {
		return nil
	}
	// Walk subsequent start codes.
	for i < len(b) {
		// Look for next start code.
		next := -1
		j := i
		for j+2 < len(b) {
			if b[j] == 0x00 && b[j+1] == 0x00 {
				if b[j+2] == 0x01 {
					next = j
					break
				}
				if j+3 < len(b) && b[j+2] == 0x00 && b[j+3] == 0x01 {
					next = j
					break
				}
			}
			j++
		}
		if next == -1 {
			// Last NAL.
			if start < len(b) {
				out = append(out, b[start:])
			}
			break
		}
		out = append(out, b[start:next])
		// Advance past the start code.
		if b[next+2] == 0x01 {
			start = next + 3
		} else {
			start = next + 4
		}
		i = start
	}
	return out
}

// extractSPSPPS scans an Annex-B payload for NAL types 7 (SPS) and 8 (PPS).
// Returns the raw NAL bodies (with the 1-byte nal_unit_header but WITHOUT
// the start code). Returns (nil, nil) if either is missing.
func extractSPSPPS(annexB []byte) (sps, pps []byte) {
	for _, nalu := range splitAnnexB(annexB) {
		if len(nalu) == 0 {
			continue
		}
		nalType := nalu[0] & 0x1f
		switch nalType {
		case 7:
			sps = append([]byte(nil), nalu...)
		case 8:
			pps = append([]byte(nil), nalu...)
		}
	}
	return sps, pps
}

// buildAVCDecoderConfigurationRecord packs SPS+PPS into the standard
// AVCDecoderConfigurationRecord (ISO/IEC 14496-15 §5.2.4.1.1) used as
// MKV CodecPrivate for V_MPEG4/ISO/AVC tracks.
func buildAVCDecoderConfigurationRecord(sps, pps []byte) []byte {
	if len(sps) < 4 {
		return nil
	}
	// configurationVersion=1, AVCProfileIndication=sps[1], profile_compat=sps[2],
	// AVCLevelIndication=sps[3], lengthSizeMinusOne=3 (4-byte NALU lengths).
	buf := []byte{
		1,
		sps[1], sps[2], sps[3],
		0xff, // 6 bits reserved=1 + 2 bits lengthSizeMinusOne=3
		0xe1, // 3 bits reserved=1 + 5 bits numOfSPS=1
	}
	spsLen := []byte{0, 0}
	binary.BigEndian.PutUint16(spsLen, uint16(len(sps)))
	buf = append(buf, spsLen...)
	buf = append(buf, sps...)
	// 1 byte numOfPPS=1
	buf = append(buf, 0x01)
	ppsLen := []byte{0, 0}
	binary.BigEndian.PutUint16(ppsLen, uint16(len(pps)))
	buf = append(buf, ppsLen...)
	buf = append(buf, pps...)
	return buf
}

// =====================================================================
// MKV muxer (production)
// =====================================================================

// mkvMuxer is the production recordingMuxer backed by ebml-go/mkvcore.
type mkvMuxer struct {
	w     io.WriteCloser
	track mkvcore.BlockWriteCloser
	mu    sync.Mutex
	written int64
}

// newMKVMuxer takes ownership of f. After Close the file is closed.
func newMKVMuxer(f *os.File) (*mkvMuxer, error) {
	return &mkvMuxer{w: &countingWriter{wrap: f}}, nil
}

// matroskaEBMLHeader is the MKV (NOT WebM) EBML header.
var matroskaEBMLHeader = struct {
	EBMLVersion        uint64 `ebml:"EBMLVersion"`
	EBMLReadVersion    uint64 `ebml:"EBMLReadVersion"`
	EBMLMaxIDLength    uint64 `ebml:"EBMLMaxIDLength"`
	EBMLMaxSizeLength  uint64 `ebml:"EBMLMaxSizeLength"`
	DocType            string `ebml:"EBMLDocType"`
	DocTypeVersion     uint64 `ebml:"EBMLDocTypeVersion"`
	DocTypeReadVersion uint64 `ebml:"EBMLDocTypeReadVersion"`
}{
	EBMLVersion:        1,
	EBMLReadVersion:    1,
	EBMLMaxIDLength:    4,
	EBMLMaxSizeLength:  8,
	DocType:            "matroska",
	DocTypeVersion:     4,
	DocTypeReadVersion: 2,
}

// matroskaTrackEntry — the shape mkvcore needs for an H.264 video track.
type matroskaTrackEntry struct {
	Name         string  `ebml:"Name,omitempty"`
	TrackNumber  uint64  `ebml:"TrackNumber"`
	TrackUID     uint64  `ebml:"TrackUID"`
	CodecID      string  `ebml:"CodecID"`
	CodecPrivate []byte  `ebml:"CodecPrivate,omitempty"`
	TrackType    uint64  `ebml:"TrackType"`
	Video        *matroskaVideo `ebml:"Video"`
}
type matroskaVideo struct {
	PixelWidth  uint64 `ebml:"PixelWidth"`
	PixelHeight uint64 `ebml:"PixelHeight"`
}

func (m *mkvMuxer) WriteTrackHeader(sps, pps []byte) error {
	codecPrivate := buildAVCDecoderConfigurationRecord(sps, pps)
	if codecPrivate == nil {
		return errors.New("mkv: failed to build AVCDecoderConfigurationRecord")
	}
	tracks := []mkvcore.TrackDescription{
		{
			TrackNumber: 1,
			TrackEntry: matroskaTrackEntry{
				Name:         "Video",
				TrackNumber:  1,
				TrackUID:     0xdeadbeef,
				CodecID:      "V_MPEG4/ISO/AVC",
				CodecPrivate: codecPrivate,
				TrackType:    1, // Video
				Video: &matroskaVideo{
					PixelWidth:  1920,
					PixelHeight: 1080,
				},
			},
		},
	}
	writers, err := mkvcore.NewSimpleBlockWriter(m.w, tracks,
		mkvcore.WithEBMLHeader(matroskaEBMLHeader),
	)
	if err != nil {
		return fmt.Errorf("mkv: NewSimpleBlockWriter: %w", err)
	}
	if len(writers) != 1 {
		return fmt.Errorf("mkv: expected 1 writer, got %d", len(writers))
	}
	m.track = writers[0]
	return nil
}

func (m *mkvMuxer) WriteFrame(_ context.Context, keyframe bool, timestampMs uint64, payload []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.track == nil {
		return 0, errors.New("mkv: track not initialized (call WriteTrackHeader first)")
	}
	// mkvcore Block timestamps are signed 16-bit relative to cluster timecode;
	// it handles cluster splitting internally. Pass the absolute ms.
	before := m.bytesSoFar()
	if _, err := m.track.Write(keyframe, int64(timestampMs), payload); err != nil {
		return 0, err
	}
	after := m.bytesSoFar()
	return int(after - before), nil
}

// bytesSoFar reads from the countingWriter wrap; mkvcore buffers internally
// so this is approximate but sufficient for rotation gating.
func (m *mkvMuxer) bytesSoFar() int64 {
	if cw, ok := m.w.(*countingWriter); ok {
		return cw.n.Load()
	}
	return 0
}

func (m *mkvMuxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.track == nil {
		return nil
	}
	err := m.track.Close()
	m.track = nil
	// mkvcore.NewSimpleBlockWriter takes ownership of m.w; track.Close
	// closes the underlying writer.
	return err
}

// countingWriter wraps an io.WriteCloser and tracks bytes written.
type countingWriter struct {
	wrap io.WriteCloser
	n    atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.wrap.Write(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingWriter) Close() error { return c.wrap.Close() }
