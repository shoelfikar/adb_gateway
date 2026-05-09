// Package session manages device registry and session lifecycle state.
// hub.go implements the per-device fan-out Hub: a single producer
// (video or audio reader) -> N WebSocket viewer consumers with
// bounded per-viewer buffers and a late-joiner cache (codec metadata
// + most-recent IDR keyframe).
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/pelni/adb-gateway/internal/obs"
)

// Frame is a self-contained scrcpy frame ready for fan-out. The header
// is the raw 12 bytes (PTS + size + flags) preserving frame boundaries
// for browser WebCodecs consumption.
type Frame struct {
	Header   [12]byte
	Payload  []byte
	KeyFrame bool
}

// wireBytes returns header || payload as a single allocated slice.
// The Hub builds this once per frame and shares the slice across all
// viewers (read-only after construction).
func (f *Frame) wireBytes() []byte {
	out := make([]byte, 12+len(f.Payload))
	copy(out[:12], f.Header[:])
	copy(out[12:], f.Payload)
	return out
}

// viewer is a registered Hub subscriber. Created in Subscribe, destroyed
// when its send channel is closed by the Hub goroutine (slow-consumer
// eviction or Hub shutdown). Unsubscribe removes the viewer from the
// broadcast set but does NOT close the channel — the Hub goroutine is
// the sole closer.
type viewer struct {
	id               string
	send             chan []byte // buffered = StreamConfig.ViewerBufferFrames
	consecutiveDrops int         // Hub-goroutine-only; no mutex needed
	evicted          bool        // Hub-goroutine-only; idempotent close guard
}

// ErrHubClosed is returned by Subscribe after the Hub's Run has exited.
var ErrHubClosed = errors.New("hub: closed")

// Hub fans frames from a single producer to N viewer channels. There
// is one Hub per stream per device (e.g. videoHub, audioHub).
//
// Lifecycle: NewHub -> SetCodecMeta (once, before first Publish) ->
//
//	go Run(ctx) -> N x Subscribe / Publish ... -> ctx cancel.
type Hub struct {
	stream              string // "video" or "audio" (Prometheus label)
	bufFrames           int    // D-04 (StreamConfig.ViewerBufferFrames)
	maxConsecutiveDrops int    // D-05 (StreamConfig.MaxConsecutiveDrops)

	// frameCount is incremented on EVERY Publish call, regardless of whether
	// the frame was enqueued or dropped at the producer-side boundary. The
	// Plan 03-02 stall watchdog reads this lock-free to detect frame-flow
	// flatlines. Placed before viewers so it is visible at struct head.
	frameCount atomic.Uint64

	in chan *Frame // buffered ~16; supervisor produces, Hub consumes

	// metaCache and keyframeCache feed late joiners.
	// metaCache is set once before fan-out begins; keyframeCache is
	// updated by the Hub goroutine only (single writer => atomic.Pointer
	// is safe; readers in Subscribe load atomically).
	metaCache     atomic.Pointer[[12]byte]
	keyframeCache atomic.Pointer[Frame]

	// viewers map is owned by the Hub goroutine for writes
	// (registration via in-band Subscribe message); RWMutex allows
	// safe concurrent Subscribe/Unsubscribe while Run iterates a snapshot.
	mu      sync.RWMutex
	viewers map[string]*viewer

	log *slog.Logger
}

// HubOpts configures a new Hub. All fields are required.
type HubOpts struct {
	Stream              string // "video" | "audio"
	BufFrames           int    // = cfg.Stream.ViewerBufferFrames
	MaxConsecutiveDrops int    // = cfg.Stream.MaxConsecutiveDrops
	Log                 *slog.Logger
}

// NewHub allocates a Hub. The hub is inert until Run is started.
func NewHub(opts HubOpts) *Hub {
	if opts.Stream != "video" && opts.Stream != "audio" {
		panic("hub: stream must be \"video\" or \"audio\"")
	}
	if opts.BufFrames <= 0 || opts.MaxConsecutiveDrops <= 0 {
		panic("hub: BufFrames and MaxConsecutiveDrops must be > 0")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Hub{
		stream:              opts.Stream,
		bufFrames:           opts.BufFrames,
		maxConsecutiveDrops: opts.MaxConsecutiveDrops,
		in:                  make(chan *Frame, 16), // small buffer; Hub fans out fast
		viewers:             make(map[string]*viewer),
		log:                 opts.Log.With("stream", opts.Stream),
	}
}

// SetCodecMeta records the 12-byte codec metadata. Must be called BEFORE
// any Subscribe (otherwise late joiners get an empty metadata frame).
// Safe to call multiple times; only the latest sticks.
func (h *Hub) SetCodecMeta(meta [12]byte) {
	h.metaCache.Store(&meta)
}

// Publish enqueues a frame for fan-out. Non-blocking: if the Hub's
// input channel is full (every viewer is already saturated), the frame
// is dropped and the dropped counter is incremented under stream label.
// Returns false if the frame was dropped at the producer-side boundary.
//
// Plan 03-02: frameCount is incremented BEFORE the enqueue/drop branch so
// the stall watchdog observes producer activity even when all viewers are
// saturated. Reading the counter is lock-free (atomic.Load).
func (h *Hub) Publish(f *Frame) bool {
	h.frameCount.Add(1)
	select {
	case h.in <- f:
		return true
	default:
		obs.FramesDroppedTotal.WithLabelValues(h.stream).Inc()
		return false
	}
}

// FrameCount returns the cumulative number of Publish calls since the Hub
// was created. Plan 03-02 watchdog uses this to detect frame-flow flatlines:
// the value increases monotonically while the producer is alive.
func (h *Hub) FrameCount() uint64 {
	return h.frameCount.Load()
}

// Subscribe registers a new viewer and atomically pre-loads metadata
// and the cached keyframe (if any) into the viewer's send channel
// BEFORE adding it to the broadcast set. Returns the viewer's read-only
// channel and an unsubscribe function.
//
// Per STR-07 ordering: late joiners receive (metadata, lastKeyframe?,
// live tail). Implemented by writing to the freshly-allocated send
// channel before any Hub goroutine can fan out to it.
func (h *Hub) Subscribe(viewerID string) (<-chan []byte, func(), error) {
	v := &viewer{
		id:   viewerID,
		send: make(chan []byte, h.bufFrames),
	}

	// 1. Preload metadata (always, even on empty cache).
	if meta := h.metaCache.Load(); meta != nil {
		metaCopy := make([]byte, 12)
		copy(metaCopy, meta[:])
		v.send <- metaCopy
	}
	// 2. Preload last keyframe if any.
	if kf := h.keyframeCache.Load(); kf != nil {
		v.send <- kf.wireBytes()
	}

	// 3. Add to broadcast set (under write lock).
	h.mu.Lock()
	h.viewers[viewerID] = v
	h.mu.Unlock()
	h.log.Debug("viewer subscribed", "viewer_id", viewerID, "preloaded_keyframe", h.keyframeCache.Load() != nil)

	unsub := func() {
		h.mu.Lock()
		existing, ok := h.viewers[viewerID]
		if ok && existing == v {
			delete(h.viewers, viewerID)
		}
		h.mu.Unlock()
		// Do NOT close v.send here — only the Hub goroutine closes
		// channels (via evict or shutdown). Closing from the caller
		// side would race with Hub goroutine sends. The caller's
		// read loop exits via context cancellation.
	}
	return v.send, unsub, nil
}

// Run executes the fan-out loop until ctx is cancelled. Returns ctx.Err()
// on cancellation. After Run returns, all viewer channels are closed.
func (h *Hub) Run(ctx context.Context) error {
	defer h.shutdown()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f := <-h.in:
			if f == nil {
				continue
			}
			// 1. Update keyframe cache atomically (Hub is the single writer).
			if f.KeyFrame {
				h.keyframeCache.Store(f)
			}
			// 2. Build wire bytes once; share across all viewers.
			msg := f.wireBytes()

			// 3. Snapshot viewers under read lock for safe iteration.
			h.mu.RLock()
			snap := make([]*viewer, 0, len(h.viewers))
			for _, v := range h.viewers {
				snap = append(snap, v)
			}
			h.mu.RUnlock()

			// 4. Non-blocking fan-out with drop accounting.
			for _, v := range snap {
				if v.evicted {
					continue
				}
				select {
				case v.send <- msg:
					v.consecutiveDrops = 0
					obs.FramesEmittedTotal.WithLabelValues(h.stream).Inc()
				default:
					v.consecutiveDrops++
					obs.FramesDroppedTotal.WithLabelValues(h.stream).Inc()
					if v.consecutiveDrops >= h.maxConsecutiveDrops {
						h.evict(v, "slow_consumer")
					}
				}
			}
		}
	}
}

// evict marks a viewer as removed, deletes it from the broadcast set, and
// closes its send channel. Called only from the Hub goroutine.
func (h *Hub) evict(v *viewer, reason string) {
	if v.evicted {
		return
	}
	v.evicted = true
	h.mu.Lock()
	if existing, ok := h.viewers[v.id]; ok && existing == v {
		delete(h.viewers, v.id)
	}
	h.mu.Unlock()
	close(v.send)
	h.log.Info("viewer evicted",
		"viewer_id", v.id,
		"reason", reason,
		"consecutive_drops", v.consecutiveDrops,
	)
}

// shutdown closes all remaining viewer channels on Hub exit. Idempotent.
func (h *Hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, v := range h.viewers {
		if !v.evicted {
			v.evicted = true
			close(v.send)
		}
		delete(h.viewers, id)
	}
}

// ViewerCountForTest returns the number of registered viewers for test
// assertions. Only for use within the same package (hub_test.go).
func (h *Hub) ViewerCountForTest() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.viewers)
}