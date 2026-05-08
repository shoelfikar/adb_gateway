//go:build soak

package session

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoak1000Cycles(t *testing.T) {
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(HubOpts{Stream: "video", BufFrames: 60, MaxConsecutiveDrops: 120, Log: nopLog})
	h.SetCodecMeta([12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		_ = h.Run(ctx)
		close(runDone)
	}()

	// Drive a steady producer in the background so the Hub goroutine
	// is doing real work while subscriptions churn.
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		keyframe := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				keyframe = !keyframe
				h.Publish(&Frame{
					Header:   [12]byte{},
					Payload:  make([]byte, 256),
					KeyFrame: keyframe,
				})
			}
		}
	}()

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()
	t.Logf("baseline goroutines: %d", baseline)

	for i := 0; i < 1000; i++ {
		ch, unsub, err := h.Subscribe("v")
		require.NoError(t, err)
		// Drain a few frames then unsubscribe.
		drainCount := 0
		for drainCount < 3 {
			select {
			case _, ok := <-ch:
				if !ok {
					break
				}
				drainCount++
			case <-time.After(50 * time.Millisecond):
				drainCount = 3 // give up on this iteration
			}
		}
		unsub()
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	t.Logf("after soak goroutines: %d (baseline=%d, delta=%d)", after, baseline, after-baseline)
	assert.LessOrEqual(t, after-baseline, 10, "goroutine leak: baseline=%d after=%d", baseline, after)

	cancel()
	<-runDone
	<-prodDone
}