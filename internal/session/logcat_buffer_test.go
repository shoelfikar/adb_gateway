package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogcatBufferAppendSnapshot verifies the ring buffer holds the most
// recent N lines in chronological order and overwrites oldest first.
func TestLogcatBufferAppendSnapshot(t *testing.T) {
	buf := NewLogcatBuffer(LogcatBufferOpts{Capacity: 10000})

	// Under-fill case: 5 lines, snapshot returns them in order.
	for i := 0; i < 5; i++ {
		buf.Append("line-" + itoa(i))
	}
	snap := buf.Snapshot()
	require.Len(t, snap, 5)
	for i, line := range snap {
		assert.Equal(t, "line-"+itoa(i), line)
	}

	// Wrap case: append until well past capacity (cap=100 for speed).
	buf2 := NewLogcatBuffer(LogcatBufferOpts{Capacity: 100})
	for i := 0; i < 250; i++ {
		buf2.Append("L" + itoa(i))
	}
	snap2 := buf2.Snapshot()
	require.Len(t, snap2, 100, "snapshot should be capped at capacity")
	// Oldest visible line should be L150 (250 produced, 100 retained).
	assert.Equal(t, "L150", snap2[0])
	assert.Equal(t, "L249", snap2[99])
	// Verify chronological order through the wrap point.
	for i := 0; i < 100; i++ {
		assert.Equal(t, "L"+itoa(150+i), snap2[i])
	}
}

// TestLogcatBufferRestart simulates a logcat-process restart: the buffer
// itself is unaffected by a producer cycle; lines from before and after a
// "restart" coexist in chronological order.
func TestLogcatBufferRestart(t *testing.T) {
	buf := NewLogcatBuffer(LogcatBufferOpts{Capacity: 10000})

	for i := 0; i < 100; i++ {
		buf.Append("pre-" + itoa(i))
	}
	// Simulate logcat producer EOF + restart: just keep appending.
	for i := 0; i < 100; i++ {
		buf.Append("post-" + itoa(i))
	}

	snap := buf.Snapshot()
	require.Len(t, snap, 200)
	for i := 0; i < 100; i++ {
		assert.Equal(t, "pre-"+itoa(i), snap[i])
	}
	for i := 0; i < 100; i++ {
		assert.Equal(t, "post-"+itoa(i), snap[100+i])
	}
}

// TestLogcatBufferConcurrent stress-tests the buffer with multiple
// producers and subscribers under -race. Subscribers may drop, but no
// data races should occur.
func TestLogcatBufferConcurrent(t *testing.T) {
	buf := NewLogcatBuffer(LogcatBufferOpts{Capacity: 1000})

	var wg sync.WaitGroup

	// 10 subscribers; each drains lazily.
	const subCount = 10
	type subTotals struct {
		received atomic.Uint64
		closed   atomic.Bool
	}
	subs := make([]*subTotals, subCount)
	for i := 0; i < subCount; i++ {
		subs[i] = &subTotals{}
		_, ch, unsub := buf.Subscribe(uuid.New())
		wg.Add(1)
		go func(idx int, ch <-chan string, unsub func()) {
			defer wg.Done()
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						subs[idx].closed.Store(true)
						return
					}
					subs[idx].received.Add(1)
				case <-time.After(2 * time.Second):
					unsub()
					return
				}
			}
		}(i, ch, unsub)
	}

	// 4 producers × 1000 appends each.
	const producers = 4
	const perProducer = 1000
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				buf.Append("p" + itoa(idx) + "-" + itoa(i))
			}
		}(p)
	}

	// Wait for producers to finish, give subs a moment, then signal shutdown.
	time.Sleep(200 * time.Millisecond)
	buf.Shutdown()
	wg.Wait()

	// We can't assert exact counts (slow consumers may drop), but:
	// - No goroutine should still be running.
	// - All subscribers should observe channel close.
	for i := 0; i < subCount; i++ {
		assert.True(t, subs[i].closed.Load(), "subscriber %d should observe channel close", i)
	}
}

// TestLogcatBufferSlowConsumerEviction verifies that a subscriber whose
// channel is full for `EvictionThreshold` consecutive appends gets evicted
// (channel closed by the buffer goroutine).
func TestLogcatBufferSlowConsumerEviction(t *testing.T) {
	buf := NewLogcatBuffer(LogcatBufferOpts{
		Capacity:           100,
		SubscriberChanSize: 4,    // small chan to force quick fill
		EvictionThreshold:  10,   // evict after 10 consecutive drops
	})

	_, ch, _ := buf.Subscribe(uuid.New())

	// Fill the chan (4) plus enough drops to trigger eviction (≥10 more).
	for i := 0; i < 50; i++ {
		buf.Append("L" + itoa(i))
	}

	// Channel must be closed (eviction).
	closed := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("subscriber chan was not closed within deadline")
		}
		if closed {
			break
		}
	}
	assert.True(t, closed, "evicted subscriber chan should be closed")

	buf.Shutdown()
}

// TestLogcatBufferSubscribeAtomicSnapshot verifies that Subscribe returns a
// snapshot AND a live-tail channel such that no lines are missed or
// duplicated between the snapshot and the live tail.
func TestLogcatBufferSubscribeAtomicSnapshot(t *testing.T) {
	buf := NewLogcatBuffer(LogcatBufferOpts{Capacity: 10000})

	// Pre-populate.
	for i := 0; i < 50; i++ {
		buf.Append("pre-" + itoa(i))
	}

	snapshot, ch, unsub := buf.Subscribe(uuid.New())
	defer unsub()

	require.Len(t, snapshot, 50)
	// Snapshot last entry is "pre-49".
	assert.Equal(t, "pre-49", snapshot[49])

	// Now append more lines; they must appear on the live channel and NOT
	// duplicate any snapshot entry.
	for i := 0; i < 5; i++ {
		buf.Append("live-" + itoa(i))
	}

	got := make([]string, 0, 5)
	deadline := time.After(2 * time.Second)
	for len(got) < 5 {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatal("chan closed unexpectedly")
			}
			got = append(got, line)
		case <-deadline:
			t.Fatalf("only got %d lines: %v", len(got), got)
		}
	}
	for i, line := range got {
		assert.Equal(t, "live-"+itoa(i), line)
	}

	buf.Shutdown()
}

// itoa is a tiny formatter to avoid importing strconv in test helpers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
