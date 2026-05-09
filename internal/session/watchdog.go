// Package session — watchdog.go implements the per-device stall watchdog
// introduced in Plan 03-02. It polls a lock-free counter (Hub.FrameCount)
// on a fixed interval; if the counter does not advance for `threshold`
// consecutive ticks, it invokes onStall once and resets misses to 0.
//
// Design notes:
//
//   - First-frame gate (Pitfall 2): the watchdog does NOT count misses
//     while the counter has never advanced past 0. Otherwise a slow scrcpy
//     startup would flap the device into reconnecting before the very
//     first frame arrives. The `started` flag flips on the first non-zero
//     observation and stays sticky.
//
//   - Single fire per stall: once onStall is invoked, misses are reset to
//     0 so the watchdog does not re-fire while the same stall persists.
//     The recovery orchestrator owns re-arming (it returns the device to
//     active or failed; the watchdog goroutine continues observing).
//
//   - Tick injection: production callers pass nil for tickCh, which
//     allocates a real time.Ticker at the configured interval. Tests pass
//     a synthetic channel so they can control timing deterministically.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// StallWatchdogOpts configures a StallWatchdog. Counter is required.
type StallWatchdogOpts struct {
	// Counter returns the current cumulative frame count. Must be cheap
	// and lock-free (typically Hub.FrameCount).
	Counter func() uint64
	// Interval is how often to poll the counter. Default 5s if zero.
	Interval time.Duration
	// Threshold is the number of consecutive misses (no advance) before
	// onStall fires. Default 5 (~25s with the default interval) if zero.
	Threshold int
	// OnStall is invoked from the watchdog goroutine when the counter
	// fails to advance for `threshold` ticks in a row. Must NOT block.
	OnStall func()
	// Log is used for debug/info messages about state changes.
	Log *slog.Logger

	// tickCh is an unexported test seam. When non-nil, the watchdog reads
	// ticks from this channel instead of allocating a time.Ticker. The
	// production constructor never sets it; only tests do.
	tickCh <-chan time.Time
}

// StallWatchdog observes a frame counter on a fixed cadence and signals
// onStall when the counter flatlines.
type StallWatchdog struct {
	counter   func() uint64
	interval  time.Duration
	threshold int
	onStall   func()
	log       *slog.Logger
	tickCh    <-chan time.Time
}

// NewStallWatchdog creates a watchdog. The watchdog is inert until Run is
// called.
func NewStallWatchdog(opts StallWatchdogOpts) *StallWatchdog {
	if opts.Counter == nil {
		panic("session: StallWatchdog requires a Counter function")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = 5
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	onStall := opts.OnStall
	if onStall == nil {
		onStall = func() {} // no-op; valid for benchmarks
	}
	return &StallWatchdog{
		counter:   opts.Counter,
		interval:  interval,
		threshold: threshold,
		onStall:   onStall,
		log:       log,
		tickCh:    opts.tickCh,
	}
}

// Run executes the watchdog loop until ctx is cancelled. Returns ctx.Err().
//
// Loop semantics on each tick:
//
//  1. Read the counter.
//  2. If `started` is false:
//     - if counter == 0 -> still in startup, do not count miss
//     - else -> set started=true, last=cur, misses=0
//  3. Else (started==true):
//     - if cur > last -> last=cur, misses=0
//     - else -> misses++; if misses >= threshold -> onStall(); misses=0
func (w *StallWatchdog) Run(ctx context.Context) error {
	var (
		last    uint64
		misses  int
		started bool
		fired   bool // sticky: blocks refire until counter advances again
	)

	tickCh := w.tickCh
	if tickCh == nil {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		tickCh = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tickCh:
			cur := w.counter()
			if !started {
				if cur == 0 {
					// Pitfall 2 first-frame gate: still waiting for the
					// producer to emit anything. Do not count this as a miss.
					continue
				}
				started = true
				last = cur
				misses = 0
				w.log.Debug("watchdog: first frame observed", "count", cur)
				continue
			}
			if cur > last {
				// Counter advanced — clear the stall episode entirely
				// (re-arms the watchdog for the NEXT stall, which is what
				// recovery success or transient slowdowns look like).
				last = cur
				misses = 0
				fired = false
				continue
			}
			// cur == last (or, defensively, < last on a counter wrap).
			if fired {
				// Stall episode is still in progress; don't refire.
				continue
			}
			misses++
			if misses >= w.threshold {
				w.log.Warn("watchdog: frame stall detected",
					"misses", misses,
					"threshold", w.threshold,
					"last_count", last,
				)
				w.onStall()
				misses = 0
				fired = true // sticky: blocks refire until counter advances
			}
		}
	}
}

// String returns a debug-friendly description of the watchdog config.
func (w *StallWatchdog) String() string {
	return fmt.Sprintf("StallWatchdog{interval=%s, threshold=%d}", w.interval, w.threshold)
}
