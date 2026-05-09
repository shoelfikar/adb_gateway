package session

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestStallWatchdogStartupGate verifies Pitfall 2: while the producer has
// never published a frame (counter stays at 0), the watchdog must NOT count
// misses or fire onStall. Otherwise a slow scrcpy startup (no frames yet)
// would trigger an immediate flap into reconnecting.
func TestStallWatchdogStartupGate(t *testing.T) {
	var counter atomic.Uint64 // stays at 0 the entire test
	stalled := newStallProbe()
	tickCh := make(chan time.Time, 16)

	w := NewStallWatchdog(StallWatchdogOpts{
		Counter:   counter.Load,
		Threshold: 5,
		OnStall:   stalled.fire,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickCh:    tickCh,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchdog(t, w, ctx)

	for i := 0; i < 6; i++ {
		tickCh <- time.Now()
	}
	// Give the watchdog a chance to process.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, 0, stalled.count(),
		"watchdog must NOT fire while counter is still 0 (first-frame gate)")
}

// TestStallWatchdogFiresOnFlatline verifies the basic stall: counter
// reaches a non-zero plateau, stays there for `threshold` ticks, onStall
// fires exactly once.
func TestStallWatchdogFiresOnFlatline(t *testing.T) {
	var counter atomic.Uint64
	stalled := newStallProbe()
	tickCh := make(chan time.Time, 16)

	w := NewStallWatchdog(StallWatchdogOpts{
		Counter:   counter.Load,
		Threshold: 5,
		OnStall:   stalled.fire,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickCh:    tickCh,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchdog(t, w, ctx)

	// Counter sequence per plan: 0,0,5,5,5,5,5,5
	// Tick 1: counter=0   -> startup gate, no miss
	// Tick 2: counter=0   -> startup gate, no miss
	// Tick 3: counter=5   -> first non-zero, started=true, last=5, misses=0
	// Tick 4: counter=5   -> last==cur, misses=1
	// Tick 5: counter=5   -> misses=2
	// Tick 6: counter=5   -> misses=3
	// Tick 7: counter=5   -> misses=4
	// Tick 8: counter=5   -> misses=5 -> onStall fires (1x)
	steps := []uint64{0, 0, 5, 5, 5, 5, 5, 5}
	for _, v := range steps {
		counter.Store(v)
		tickCh <- time.Now()
		time.Sleep(2 * time.Millisecond) // let watchdog drain the tick
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, 1, stalled.count(),
		"watchdog must fire exactly once after %d consecutive misses", 5)
}

// TestStallWatchdogResetsOnFlow verifies that frames flowing through the
// threshold-1 boundary do NOT cause a fire — misses reset every time the
// counter advances.
func TestStallWatchdogResetsOnFlow(t *testing.T) {
	var counter atomic.Uint64
	stalled := newStallProbe()
	tickCh := make(chan time.Time, 16)

	w := NewStallWatchdog(StallWatchdogOpts{
		Counter:   counter.Load,
		Threshold: 5,
		OnStall:   stalled.fire,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickCh:    tickCh,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchdog(t, w, ctx)

	// Frames flow normally up to threshold-1 then stall starts (only 1 miss).
	steps := []uint64{0, 5, 10, 15, 20, 20, 20}
	for _, v := range steps {
		counter.Store(v)
		tickCh <- time.Now()
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, 0, stalled.count(),
		"watchdog must NOT fire when frames keep advancing through threshold-1 then stall briefly")
}

// TestStallWatchdogOnlyFiresOncePerStall verifies that after onStall fires,
// the watchdog resets misses to 0 so it doesn't refire while the same stall
// persists. Recovery owns re-arming.
func TestStallWatchdogOnlyFiresOncePerStall(t *testing.T) {
	var counter atomic.Uint64
	stalled := newStallProbe()
	tickCh := make(chan time.Time, 16)

	w := NewStallWatchdog(StallWatchdogOpts{
		Counter:   counter.Load,
		Threshold: 3,
		OnStall:   stalled.fire,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickCh:    tickCh,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchdog(t, w, ctx)

	// Counter: 1 (start), then plateau 1,1,1,1,1,1,1,1 — should fire once.
	counter.Store(1)
	tickCh <- time.Now()
	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 12; i++ { // way past threshold
		tickCh <- time.Now()
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, 1, stalled.count(),
		"after onStall fires, misses must reset; refire only on a new stall episode")
}

// stallProbe is a thread-safe counter for onStall invocations.
type stallProbe struct {
	mu sync.Mutex
	n  int
}

func newStallProbe() *stallProbe { return &stallProbe{} }
func (p *stallProbe) fire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
}
func (p *stallProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func runWatchdog(t *testing.T, w *StallWatchdog, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	return done
}
