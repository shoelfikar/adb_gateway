package session

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
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

// mkConfigFrame creates a test Frame that represents a config packet (SPS/PPS).
// Uses a distinct header byte pattern to differentiate from data frames.
func mkConfigFrame(seq byte) *Frame {
	return &Frame{
		Header:       [12]byte{0x80, seq, seq, seq, 0, 0, 0, 0, 0, 0, 0, 8},
		Payload:      bytes.Repeat([]byte{seq}, 8),
		ConfigPacket: true,
	}
}

// TestHubMultiViewer verifies STR-04: two subscribers receive the same frame
// bytes from a single producer (1:N fan-out).
func TestHubMultiViewer(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	v1ch, _, err := h.Subscribe("viewer-1")
	require.NoError(t, err)

	v2ch, _, err := h.Subscribe("viewer-2")
	require.NoError(t, err)

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

	_, _, err := h.Subscribe("viewer-1")
	require.NoError(t, err)

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish 65 P-frames without draining the viewer's channel.
	// Buffer capacity is 60. After 1 metadata pre-load, there's room for 59
	// more frames. The remaining 6 frames are dropped.
	for i := 0; i < 65; i++ {
		h.Publish(mkFrame(byte(i+1), false))
	}

	// Give the Hub goroutine time to process.
	time.Sleep(100 * time.Millisecond)

	cancel()

	// Verify drops occurred (at least some frames were dropped).
	droppedAfter := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))
	droppedDelta := droppedAfter - droppedBefore
	assert.GreaterOrEqual(t, int(droppedDelta), 4, "should have dropped at least 4 frames")
}

// TestHubSlowDisconnect verifies STR-06: after MaxConsecutiveDrops consecutive
// drops, the viewer is evicted with reason "slow_consumer" and its send channel
// is closed. Uses small thresholds (bufFrames=3, maxDrops=5) to keep the test
// fast and deterministic — the eviction logic is the same regardless of thresholds.
func TestHubSlowDisconnect(t *testing.T) {
	h := NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           3,
		MaxConsecutiveDrops: 5,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.SetCodecMeta([12]byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	droppedBefore := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))

	v1ch, _, err := h.Subscribe("viewer-1")
	require.NoError(t, err)

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Give the Hub goroutine a moment to start processing.
	time.Sleep(10 * time.Millisecond)

	// The viewer's channel has capacity 3 with 1 metadata pre-loaded,
	// leaving room for 2 more frames. After filling, each further frame
	// is dropped. 5 consecutive drops triggers eviction.
	// We need at least 2 (buffer fill) + 5 (consecutive drops) = 7
	// frames to make it through h.in to the Hub goroutine.
	// Publish plenty of frames with periodic yields so the Hub goroutine
	// can process them.
	for i := 0; i < 40; i++ {
		h.Publish(mkFrame(byte(i%256), false))
		if i%10 == 9 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Wait for the Hub goroutine to process all frames and evict the viewer.
	assert.Eventually(t, func() bool {
		return h.ViewerCountForTest() == 0
	}, 2*time.Second, 10*time.Millisecond, "viewer should be evicted after 5 consecutive drops")

	// The viewer's channel should be closed by eviction.
	// Drain remaining messages and verify closure.
	drainAndVerifyClosed(t, v1ch, "viewer-1")

	// Verify that drops were recorded (at least maxConsecutiveDrops).
	droppedAfter := testutil.ToFloat64(obs.FramesDroppedTotal.WithLabelValues("video"))
	droppedDelta := droppedAfter - droppedBefore
	assert.GreaterOrEqual(t, int(droppedDelta), 5, "should have at least 5 drops")

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
	lateCh, _, err := h.Subscribe("late")
	require.NoError(t, err)

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
// on CONSECUTIVE drops, not cumulative. A viewer that catches up after some drops
// gets a clean slate, and is NOT evicted even after cumulative drops exceed the
// threshold.
//
// Strategy: use BufFrames=10 and MaxConsecutiveDrops=20. Fill the buffer (10 slots),
// accumulate 10 drops (< 20), drain one to reset, then accumulate 11 more drops.
// Cumulative = 10+11 = 21 > 20, but consecutive never reaches 20.
func TestHubDropCounterResets(t *testing.T) {
	h := NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           10,
		MaxConsecutiveDrops: 20,
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.SetCodecMeta([12]byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	v1ch, _, err := h.Subscribe("viewer-1")
	require.NoError(t, err)

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Give the Hub goroutine a moment to start.
	time.Sleep(10 * time.Millisecond)

	// Phase 1: Fill the viewer's buffer (capacity 10, 1 metadata pre-loaded = 9 slots left).
	// Publish 20 frames: 9 succeed (fill buffer), 11 dropped (consecutiveDrops=11 < 20).
	for i := 0; i < 20; i++ {
		h.Publish(mkFrame(byte(i), false))
	}
	time.Sleep(50 * time.Millisecond)

	// Verify viewer is still registered (11 < 20 consecutive drops).
	assert.Equal(t, 1, h.ViewerCountForTest(), "viewer should still be registered after 11 drops")

	// Phase 2: Drain ONE message to free a slot, then publish 1 frame that succeeds.
	// This resets consecutiveDrops to 0.
	<-v1ch // drain metadata
	h.Publish(mkFrame(0xAA, false))
	time.Sleep(30 * time.Millisecond)

	// Phase 3: Publish 18 more frames. The freed slot is now full again.
	// All 18 are dropped (consecutiveDrops=18, cumulative=11+18=29 > 20).
	for i := 0; i < 18; i++ {
		h.Publish(mkFrame(byte(i+30), false))
		if i%6 == 5 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)

	// Verify viewer was NOT evicted — cumulative drops (29) exceed maxConsecutiveDrops (20)
	// but consecutive (18) never reached 20 because of the reset.
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
	lateCh, _, err := h.Subscribe("late")
	require.NoError(t, err)

	// First message: metadata.
	expectedMeta := []byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00}
	msg1 := readChan(t, lateCh, 500*time.Millisecond, "late joiner metadata")
	assert.Equal(t, expectedMeta, msg1, "first message must be metadata")

	// Second message must be K2 (not K1).
	expectedK2 := mkFrame(0x02, true).wireBytes()
	msg2 := readChan(t, lateCh, 500*time.Millisecond, "late joiner keyframe")
	assert.Equal(t, expectedK2, msg2, "second message must be K2 (most recent keyframe), not K1")
}

// TestHubConfigPacketCached verifies that config packets (SPS/PPS) are cached
// and preloaded for late-joining viewers. The preload order must be:
// metadata → config packet → keyframe → live tail.
func TestHubConfigPacketCached(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Publish: config packet → keyframe → P-frame (typical scrcpy sequence).
	h.Publish(mkConfigFrame(0xCC))  // config (SPS/PPS)
	h.Publish(mkFrame(0x01, true))   // K1
	h.Publish(mkFrame(0x02, false))   // P1

	// Give Hub time to process and cache.
	time.Sleep(50 * time.Millisecond)

	// Late joiner subscribes after all frames were published.
	lateCh, _, err := h.Subscribe("late")
	require.NoError(t, err)

	// 1. Metadata.
	expectedMeta := []byte{0xAA, 0xBB, 0xCC, 0x44, 0x33, 0x22, 0x11, 0x00, 0x10, 0x00, 0x00, 0x00}
	msg1 := readChan(t, lateCh, 500*time.Millisecond, "late joiner metadata")
	assert.Equal(t, expectedMeta, msg1, "first message must be metadata")

	// 2. Config packet (SPS/PPS).
	expectedCfg := mkConfigFrame(0xCC).wireBytes()
	msg2 := readChan(t, lateCh, 500*time.Millisecond, "late joiner config")
	assert.Equal(t, expectedCfg, msg2, "second message must be cached config packet")

	// 3. Keyframe.
	expectedK1 := mkFrame(0x01, true).wireBytes()
	msg3 := readChan(t, lateCh, 500*time.Millisecond, "late joiner keyframe")
	assert.Equal(t, expectedK1, msg3, "third message must be cached keyframe")

	// 4. Live P-frame.
	h.Publish(mkFrame(0x03, false))
	expectedP2 := mkFrame(0x03, false).wireBytes()
	msg4 := readChan(t, lateCh, 500*time.Millisecond, "late joiner live P2")
	assert.Equal(t, expectedP2, msg4, "fourth message must be live P2")
}

// TestHubConfigPacketUpdatedOnReconfig verifies that when a new config packet
// arrives (e.g. resolution change), the cache is updated so late joiners get
// the latest SPS/PPS before the matching keyframe.
func TestHubConfigPacketUpdatedOnReconfig(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := h.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("hub run: %v", err)
		}
	}()

	// Initial stream: config1 → K1.
	h.Publish(mkConfigFrame(0xAA))
	h.Publish(mkFrame(0x01, true))

	// Resolution change: config2 → K2.
	h.Publish(mkConfigFrame(0xBB))
	h.Publish(mkFrame(0x02, true))

	time.Sleep(50 * time.Millisecond)

	lateCh, _, err := h.Subscribe("late")
	require.NoError(t, err)

	// Skip metadata.
	readChan(t, lateCh, 500*time.Millisecond, "metadata")

	// Config must be config2 (0xBB), not config1 (0xAA).
	expectedCfg := mkConfigFrame(0xBB).wireBytes()
	msg := readChan(t, lateCh, 500*time.Millisecond, "config packet")
	assert.Equal(t, expectedCfg, msg, "config cache must contain latest config packet")

	// Keyframe must be K2 (0x02), not K1.
	expectedK2 := mkFrame(0x02, true).wireBytes()
	msg2 := readChan(t, lateCh, 500*time.Millisecond, "keyframe")
	assert.Equal(t, expectedK2, msg2, "keyframe cache must contain K2")
}

// TestHubRunCancel verifies that cancelling the context closes all viewer
// channels and Run returns context.Canceled.
func TestHubRunCancel(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())

	v1ch, _, err := h.Subscribe("viewer-1")
	require.NoError(t, err)

	v2ch, _, err := h.Subscribe("viewer-2")
	require.NoError(t, err)

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

	// Both viewer channels should be closed (by shutdown).
	// They may contain data (metadata), so drain first then verify closed.
	drainAndVerifyClosed(t, v1ch, "viewer-1")
	drainAndVerifyClosed(t, v2ch, "viewer-2")
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

// TestHubFrameCount verifies Plan 03-02: Hub.Publish increments an atomic
// frame counter that the watchdog reads lock-free.
func TestHubFrameCount(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		h := newTestHub(t)
		assert.Equal(t, uint64(0), h.FrameCount())
		for i := 0; i < 100; i++ {
			h.Publish(mkFrame(byte(i), false))
		}
		assert.Equal(t, uint64(100), h.FrameCount(),
			"FrameCount must increment on every Publish, even when in is full")
	})

	t.Run("concurrent", func(t *testing.T) {
		h := newTestHub(t)
		var wg sync.WaitGroup
		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					h.Publish(mkFrame(byte(i), false))
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, uint64(1000), h.FrameCount(),
			"FrameCount must be race-free under concurrent Publish")
	})
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

// drainAndVerifyClosed drains all messages from a channel and then verifies
// that the channel is closed (ok=false on read).
func drainAndVerifyClosed(t *testing.T, ch <-chan []byte, name string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // Channel is closed as expected.
			}
			// More data in channel, keep draining.
		case <-timeout:
			t.Fatalf("%s: timed out waiting for channel to close", name)
		}
	}
}