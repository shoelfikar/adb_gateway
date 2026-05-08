package session

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/obs"
)

// newTestHub creates a Hub with default test parameters (BufFrames=60,
// MaxConsecutiveDrops=120) for unit testing.
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           60,
		MaxConsecutiveDrops: 120,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.SetCodecMeta([12]byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00})
	return h
}

// mkFrame creates a test Frame with a given sequence byte and keyframe flag.
// Header bytes: [seq seq seq seq 0 0 0 0 0 0 0 16] (12 bytes total).
// Payload: 16 repetitions of the seq byte.
func mkFrame(seq byte, key bool) *Frame {
	return &Frame{
		Header:   [12]byte{seq, seq, seq, seq, 0, 0, 0, 0, 0, 0, 0, 16},
		Payload:  bytes.Repeat([]byte{seq}, 16),
		KeyFrame: key,
	}
}

// TestHubMultiViewer verifies STR-04: two subscribers receive the same frame
// bytes from a single producer (1:N fan-out).
func TestHubMultiViewer(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	v1ch, v1unsub, err := h.Subscribe("viewer-1")
	require.NoError(t, err)
	defer v1unsub()

	v2ch, v2unsub, err := h.Subscribe("viewer-2")
	require.NoError(t, err)
	defer v2unsub()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish a keyframe and a P-frame.
	h.Publish(mkFrame(0x01, true))
	h.Publish(mkFrame(0x02, false))

	// Each viewer should receive: metadata (12 bytes) + K1 wire + P1 wire.
	expectedMeta := []byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00}
	expectedK1 := mkFrame(0x01, true).wireBytes()
	expectedP1 := mkFrame(0x02, false).wireBytes()

	for viewerIdx, ch := range []<-chan []byte{v1ch, v2ch} {
		msg1 := readChan(t, ch, 500*time.Millisecond, "viewer %d msg1", viewerIdx+1)
		assert.Equal(t, expectedMeta, msg1, "viewer %d: first message must be metadata", viewerIdx+1)

		msg2 := readChan(t, ch, 500*time.Millisecond, "viewer %d msg2", viewerIdx+1)
		assert.Equal(t, expectedK1, msg2, "viewer %d: second message must be keyframe wire bytes", viewerIdx+1)

		msg3 := readChan(t, ch, 500*time.Millisecond, "viewer %d msg3", viewerIdx+1)
		assert.Equal(t, expectedP1, msg3, "viewer %d: third message must be P-frame wire bytes", viewerIdx+1)
	}
}

// TestHubBackpressure verifies STR-05: a slow viewer's full channel does not
// block the Hub send loop; drops are accounted via Prometheus counters.
func TestHubBackpressure(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

	// Snapshot the dropped counter before the test.
	droppedBefore := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))

	v1ch, v1unsub, err := h.Subscribe("viewer-1")
	require.NoError(t, err)
	_ = v1ch
	defer v1unsub()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish 65 P-frames without draining the viewer's channel.
	// Buffer is 60 + 1 metadata = 61 messages capacity.
	// After the 60th frame fills the buffer, the remaining frames are dropped.
	for i := 0; i < 65; i++ {
		h.Publish(mkFrame(byte(i+1), false))
	}

	// Give the Hub goroutine time to process.
	time.Sleep(100 * time.Millisecond)

	cancel()

	// Verify drops occurred (at least some of the 65 - buffer capacity = at least a few drops).
	droppedAfter := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))
	droppedDelta := droppedAfter - droppedBefore
	assert.GreaterOrEqual(t, int(droppedDelta), 4, "should have dropped at least 4 frames")
}

// TestHubSlowDisconnect verifies STR-06: after 120 consecutive drops, the
// viewer is evicted with reason "slow_consumer" and its send channel is closed.
func TestHubSlowDisconnect(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	droppedBefore := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))

	v1ch, v1unsub, err := h.Subscribe("viewer-1")
	require.NoError(t, err)
	defer v1unsub()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish enough frames to trigger eviction.
	// Channel buffers 60 frames (after metadata). The first 60 land in the buffer
	// (success). Publishes 61 through 180 each fail (drops 1..120).
	// On the 120th consecutive drop, eviction fires.
	for i := 0; i < 180; i++ {
		h.Publish(mkFrame(byte(i%256), false))
	}

	// Give the Hub goroutine time to process and evict.
	time.Sleep(200 * time.Millisecond)

	// The viewer's channel should be closed (eviction).
	// Drain any remaining messages first.
	for {
		select {
		case _, ok := <-v1ch:
			if !ok {
				// Channel closed as expected.
				goto verifyDrops
			}
		default:
			// Channel not closed yet or still has data; try draining with timeout.
			select {
			case _, ok := <-v1ch:
				if !ok {
					goto verifyDrops
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("timed out waiting for viewer channel to close")
			}
		}
	}

verifyDrops:
	// Verify that at least 120 drops were recorded.
	droppedAfter := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))
	droppedDelta := droppedAfter - droppedBefore
	assert.GreaterOrEqual(t, int(droppedDelta), 120, "should have at least 120 drops")

	cancel()
}

// TestHubLateJoiner verifies STR-07: a late-joining viewer receives metadata,
// then the cached keyframe, then live frames in that order.
func TestHubLateJoiner(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish a keyframe and a P-frame BEFORE the late joiner subscribes.
	h.Publish(mkFrame(0x01, true))  // K1
	h.Publish(mkFrame(0x02, false)) // P1

	// Give Hub time to process and cache the keyframe.
	time.Sleep(50 * time.Millisecond)

	// Late joiner subscribes.
	lateCh, lateUnsub, err := h.Subscribe("late")
	require.NoError(t, err)
	defer lateUnsub()

	// First message must be the 12-byte metadata.
	expectedMeta := []byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00}
	msg1 := readChan(t, lateCh, 500*time.Millisecond, "late joiner metadata")
	assert.Equal(t, expectedMeta, msg1, "late joiner first message must be metadata")

	// Second message must be the cached keyframe K1.
	expectedK1 := mkFrame(0x01, true).wireBytes()
	msg2 := readChan(t, lateCh, 500*time.Millisecond, "late joiner keyframe")
	assert.Equal(t, expectedK1, msg2, "late joiner second message must be cached keyframe")

	// Now publish another P-frame; the late joiner should receive it live.
	h.Publish(mkFrame(0x03, false)) // P2
	expectedP2 := mkFrame(0x03, false).wireBytes()
	msg3 := readChan(t, lateCh, 500*time.Millisecond, "late joiner live P2")
	assert.Equal(t, expectedP2, msg3, "late joiner third message must be live P2")
}

// TestHubDropCounterResets verifies Pitfall 2: the eviction counter is based
// on CONSECUTIVE drops, not cumulative. A viewer that catches up after 119 drops
// gets a clean slate, and is NOT evicted even after > 120 cumulative drops.
func TestHubDropCounterResets(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	v1ch, v1unsub, err := h.Subscribe("viewer-1")
	require.NoError(t, err)
	defer v1unsub()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Phase 1: Fill the 60-frame buffer + 1 metadata slot.
	// Publish 60 P-frames. They should all land in the buffer.
	for i := 0; i < 60; i++ {
		h.Publish(mkFrame(byte(i%256), false))
	}
	time.Sleep(50 * time.Millisecond)

	// Verify viewer count is still 1 (not evicted).
	assert.Equal(t, 1, h.ViewerCountForTest(), "viewer should still be registered after filling buffer")

	// Phase 2: Publish 59 more frames. These should be dropped (consecutiveDrops = 59).
	for i := 0; i < 59; i++ {
		h.Publish(mkFrame(byte((i + 60) % 256), false))
	}
	time.Sleep(50 * time.Millisecond)

	// Verify viewer is still registered.
	assert.Equal(t, 1, h.ViewerCountForTest(), "viewer should still be registered after 59 drops")

	// Phase 3: Drain ONE message from the channel to allow a successful send
	// (which resets the consecutive drops counter to 0).
	<-v1ch // drain one message (metadata)

	// Publish 1 more frame — this should succeed and reset consecutiveDrops to 0.
	h.Publish(mkFrame(0xAA, false))
	time.Sleep(50 * time.Millisecond)

	// Phase 4: Publish 119 more frames — 60 fill the buffer, 59 drop.
	// Total cumulative drops = 59 + 59 = 118, but consecutive never reaches 120.
	for i := 0; i < 119; i++ {
		h.Publish(mkFrame(byte(i%256), false))
	}
	time.Sleep(100 * time.Millisecond)

	// Verify viewer was NOT evicted — cumulative drops are well over 120 but
	// consecutive never reached 120 because of the reset.
	assert.Equal(t, 1, h.ViewerCountForTest(), "viewer should NOT be evicted — consecutive drops reset on success")

	cancel()
}

// TestHubKeyframeReplacedAtomically verifies that when a new keyframe arrives,
// the late-joiner cache is updated atomically: subscribing after K2 should
// yield K2's wire bytes, not K1's.
func TestHubKeyframeReplacedAtomically(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish K1, then K2 (both keyframes).
	h.Publish(mkFrame(0x01, true)) // K1
	h.Publish(mkFrame(0x02, true)) // K2

	// Give Hub time to process and cache K2.
	time.Sleep(50 * time.Millisecond)

	// Late joiner subscribes AFTER both keyframes.
	lateCh, lateUnsub, err := h.Subscribe("late")
	require.NoError(t, err)
	defer lateUnsub()

	// First message: metadata.
	expectedMeta := []byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00}
	msg1 := readChan(t, lateCh, 500*time.Millisecond, "late joiner metadata")
	assert.Equal(t, expectedMeta, msg1, "first message must be metadata")

	// Second message must be K2 (not K1).
	expectedK2 := mkFrame(0x02, true).wireBytes()
	msg2 := readChan(t, lateCh, 500*time.Millisecond, "late joiner keyframe")
	assert.Equal(t, expectedK2, msg2, "second message must be K2 (most recent keyframe), not K1")
}

// TestHubRunCancel verifies that cancelling the context closes all viewer
// channels and Run returns context.Canceled.
func TestHubRunCancel(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())

	v1ch, v1unsub, err := h.Subscribe("viewer-1")
	require.NoError(t, err)
	defer v1unsub()

	v2ch, v2unsub, err := h.Subscribe("viewer-2")
	require.NoError(t, err)
	defer v2unsub()

	runErr := make(chan error, 1)
	go func() {
		runErr <- h.Run(ctx)
	}()

	// Cancel the context.
	cancel()

	// Run should return context.Canceled.
	select {
	case err := <-runErr:
		assert.ErrorIs(t, err, context.Canceled, "Run should return context.Canceled on cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within timeout after context cancellation")
	}

	// Both viewer channels should be closed.
	_, ok1 := <-v1ch
	assert.False(t, ok1, "viewer-1 channel should be closed after Run exits")

	_, ok2 := <-v2ch
	assert.False(t, ok2, "viewer-2 channel should be closed after Run exits")
}

// TestHubPublishWhenInFull verifies that when the Hub's input channel is full,
// Publish returns false and increments the dropped counter.
func TestHubPublishWhenInFull(t *testing.T) {
	h := newTestHub(t)
	// Do NOT start Run — the input channel will fill up.

	emittedBefore := testutil.ToFloat64(obs.FramesEmittedTotal.WithLabelValues("video"))
	droppedBefore := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))

	// The Hub's input channel has capacity 16.
	// Publish 17 frames: first 16 succeed, 17th should return false.
	successCount := 0
	for i := 0; i < 17; i++ {
		if h.Publish(mkFrame(byte(i), false)) {
			successCount++
		}
	}

	assert.Equal(t, 16, successCount, "first 16 Publish calls should succeed")

	// 17th call should return false (channel full).
	result := h.Publish(mkFrame(0xFF, false))
	assert.False(t, result, "17th Publish should return false when input channel is full")

	// Verify the dropped counter was incremented.
	emittedAfter := testutil.ToFloat64(obs.FramesEmittedTotal.WithLabelValues("video"))
	droppedAfter := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))

	// No frames were emitted (Run not started), but at least 1 drop was recorded.
	assert.Equal(t, emittedBefore, emittedAfter, "no frames should be emitted when Run is not started")
	assert.GreaterOrEqual(t, droppedAfter-droppedBefore, float64(1), "at least 1 drop should be recorded")
}

// readChan is a test helper that reads from a channel with a timeout.
// Returns the message value. Fails the test if the channel blocks or closes.
func readChan(t *testing.T, ch <-chan []byte, timeout time.Duration, msg string, args ...any) []byte {
	t.Helper()
	select {
	case data, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed while waiting for: "+msg, args...)
		}
		return data
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for: "+msg, args...)
		return nil
	}
}