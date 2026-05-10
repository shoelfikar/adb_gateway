// Package session — logcat_buffer.go implements a per-device retroactive
// logcat ring buffer with pub-sub fan-out (Plan 03-03 OPS-05).
//
// Design notes (mirror hub.go where the semantics are the same):
//
//   - Ring buffer of N strings. Default N=10000 (configurable). Wrapping is
//     handled in Snapshot() so callers always see chronological order.
//
//   - Subscribers receive a snapshot of the ring AT subscribe time PLUS a
//     buffered chan (default 256) carrying every Append after subscription.
//     Subscribe takes both under a single write lock so no lines are missed
//     or duplicated between the snapshot and the live tail.
//
//   - Drop-on-slow with eviction (mirrors Hub D-04/D-05). When a subscriber
//     channel is full, Append increments the per-subscriber consecutiveDrops
//     counter; on the (Nth+1) consecutive drop the buffer goroutine evicts
//     the subscriber by closing its channel. This is identical to hub.go's
//     evict path, including the `evicted` flag for idempotent close.
//
//   - Channel close is owned by the buffer goroutine ONLY (mirrors decision
//     #28). Unsubscribe does map-removal but never closes a channel, so
//     concurrent Append/close cannot race.
package session

import (
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// LogcatBufferOpts configures a LogcatBuffer.
type LogcatBufferOpts struct {
	// Capacity is the ring-buffer size in lines. Default 10000.
	Capacity int
	// SubscriberChanSize is the bounded send-channel size per subscriber.
	// Default 256 (Plan 03-03 hard-coded; Hub uses 60-frame analog).
	SubscriberChanSize int
	// EvictionThreshold is the consecutive-drop count that triggers
	// subscriber eviction. Default 120 (mirrors decision #30).
	EvictionThreshold int
	// Log is used for eviction info messages.
	Log *slog.Logger
}

const (
	defaultLogcatCapacity        = 10000
	defaultLogcatSubChanSize     = 256
	defaultLogcatEvictionThresh  = 120
)

// logcatSub is a registered LogcatBuffer subscriber. Created in Subscribe,
// destroyed when its send channel is closed by the buffer goroutine.
type logcatSub struct {
	id               uuid.UUID
	send             chan string
	consecutiveDrops int  // buffer-goroutine-only; no mutex needed
	evicted          bool // buffer-goroutine-only; idempotent close guard
}

// LogcatBuffer is a per-device retroactive ring buffer with pub-sub fan-out.
type LogcatBuffer struct {
	mu sync.RWMutex

	// Ring storage.
	lines []string
	head  int
	full  bool

	// Subscribers map. Owned by Append (writes under mu); Subscribe
	// adds; Unsubscribe / evict / shutdown delete.
	subs map[uuid.UUID]*logcatSub

	chanSize    int
	evictThresh int

	closed bool

	log *slog.Logger
}

// NewLogcatBuffer creates a LogcatBuffer ready to accept Append calls and
// Subscribe registrations. Inert until used.
func NewLogcatBuffer(opts LogcatBufferOpts) *LogcatBuffer {
	cap := opts.Capacity
	if cap <= 0 {
		cap = defaultLogcatCapacity
	}
	chanSize := opts.SubscriberChanSize
	if chanSize <= 0 {
		chanSize = defaultLogcatSubChanSize
	}
	thresh := opts.EvictionThreshold
	if thresh <= 0 {
		thresh = defaultLogcatEvictionThresh
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &LogcatBuffer{
		lines:       make([]string, cap),
		subs:        make(map[uuid.UUID]*logcatSub),
		chanSize:    chanSize,
		evictThresh: thresh,
		log:         log,
	}
}

// Append writes a line into the ring and broadcasts it to every active
// subscriber. Slow subscribers drop (per-subscriber drop counter); after
// `evictThresh` consecutive drops a subscriber is evicted.
func (b *LogcatBuffer) Append(line string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	// Write to ring.
	b.lines[b.head] = line
	b.head++
	if b.head >= len(b.lines) {
		b.head = 0
		b.full = true
	}

	// Snapshot subs for non-blocking fan-out OUTSIDE the lock.
	snap := make([]*logcatSub, 0, len(b.subs))
	for _, s := range b.subs {
		snap = append(snap, s)
	}

	// Hold the lock through fan-out to avoid races with shutdown.
	// (We use Lock not RLock because we mutate per-sub fields and may evict.)
	for _, s := range snap {
		if s.evicted {
			continue
		}
		select {
		case s.send <- line:
			s.consecutiveDrops = 0
		default:
			s.consecutiveDrops++
			if s.consecutiveDrops >= b.evictThresh {
				b.evictLocked(s, "slow_consumer")
			}
		}
	}
	b.mu.Unlock()
}

// Snapshot returns a copy of the ring contents in chronological order
// (oldest first). At most `Capacity` lines.
func (b *LogcatBuffer) Snapshot() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotLocked()
}

// snapshotLocked is the unlocked variant. Caller must hold b.mu (read or write).
func (b *LogcatBuffer) snapshotLocked() []string {
	if !b.full {
		out := make([]string, b.head)
		copy(out, b.lines[:b.head])
		return out
	}
	out := make([]string, 0, len(b.lines))
	out = append(out, b.lines[b.head:]...)
	out = append(out, b.lines[:b.head]...)
	return out
}

// Subscribe atomically takes a snapshot AND registers the new subscriber so
// no lines are missed or duplicated between the snapshot and the live tail.
//
// Returns:
//   - snapshot: a chronologically-ordered copy of the ring at subscribe time.
//   - ch: read-only channel that receives every line appended AFTER the
//     subscribe call (until eviction or Shutdown).
//   - unsub: removes the subscriber from the map (does NOT close ch — only
//     the buffer goroutine closes channels).
func (b *LogcatBuffer) Subscribe(id uuid.UUID) ([]string, <-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := b.snapshotLocked()
	sub := &logcatSub{
		id:   id,
		send: make(chan string, b.chanSize),
	}
	if !b.closed {
		b.subs[id] = sub
	} else {
		// Buffer is shut down; close the new channel immediately so the
		// caller's range loop exits cleanly.
		close(sub.send)
	}
	unsub := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok && existing == sub {
			delete(b.subs, id)
		}
		// Channel close is owned by the buffer goroutine only (Append
		// evictLocked / shutdown). Caller's reader exits via ctx
		// cancellation in production; or by reading until close on shutdown.
	}
	return snap, sub.send, unsub
}

// evictLocked marks the subscriber evicted, removes it from the map, and
// closes its channel. Caller MUST hold b.mu.
func (b *LogcatBuffer) evictLocked(s *logcatSub, reason string) {
	if s.evicted {
		return
	}
	s.evicted = true
	if existing, ok := b.subs[s.id]; ok && existing == s {
		delete(b.subs, s.id)
	}
	close(s.send)
	b.log.Info("logcat subscriber evicted",
		"viewer_id", s.id.String(),
		"reason", reason,
		"consecutive_drops", s.consecutiveDrops,
	)
}

// Shutdown closes every subscriber channel and marks the buffer closed.
// Idempotent. After Shutdown, Append is a no-op and Subscribe returns a
// pre-closed channel.
func (b *LogcatBuffer) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		if !s.evicted {
			s.evicted = true
			close(s.send)
		}
		delete(b.subs, id)
	}
}

// SubscriberCountForTest returns the number of registered subscribers.
// Only for use within the same package and by tests.
func (b *LogcatBuffer) SubscriberCountForTest() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
