package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/scrcpy"
)

// flakyLauncher fails its first failBeforeOK calls then succeeds. Used to
// drive recovery-orchestrator tests.
type flakyLauncher struct {
	mu          sync.Mutex
	failBefore  int
	calls       int
	failAlways  bool
	makeResult  func() *scrcpy.LaunchResult
}

func (f *flakyLauncher) LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error) {
	f.mu.Lock()
	f.calls++
	c := f.calls
	failAlways := f.failAlways
	failBefore := f.failBefore
	f.mu.Unlock()
	if failAlways {
		return nil, errors.New("flaky: launch failed")
	}
	if c <= failBefore {
		return nil, errors.New("flaky: still failing")
	}
	return f.makeResult(), nil
}

func (f *flakyLauncher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// makeFlakyResult builds a minimal LaunchResult sufficient for the recovery
// orchestrator to mark active again (no real connections needed since the
// recovery test does not run video readers).
func makeFlakyResult() *scrcpy.LaunchResult {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	return &scrcpy.LaunchResult{
		VideoLn:    ln,
		DeviceName: "test-device",
		CodecMeta:  [12]byte{'h', '2', '6', '4'},
		SCID:       "deadbeef",
		Cleanup:    func() { ln.Close() },
	}
}

// fastBackoff returns a backoff suitable for tests: 1ms initial, no caps.
func fastBackoff() backoff.BackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 1 * time.Millisecond
	bo.MaxInterval = 5 * time.Millisecond
	bo.MaxElapsedTime = 0
	return bo
}

// newRecoveryTestSession builds a DeviceSession in StateActive with a
// recorded LaunchOptions so recovery has something to re-launch with.
func newRecoveryTestSession(serial string, launcher Launcher) *DeviceSession {
	sess := NewDeviceSession(serial, nil, launcher, DefaultSessionOpts())
	sess.state = StateActive
	sess.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return sess
}

// TestRecoverySucceedsAfterTwoFailures: with maxAttempts=3 and the launcher
// failing twice then succeeding, recovery must end in StateActive.
func TestRecoverySucceedsAfterTwoFailures(t *testing.T) {
	flaky := &flakyLauncher{failBefore: 2, makeResult: makeFlakyResult}
	sess := newRecoveryTestSession("rec-A", flaky)

	rec := NewRecovery(RecoveryOpts{
		Launcher:    flaky,
		MaxAttempts: 3,
		Backoff:     fastBackoff(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := rec.Run(ctx, sess)
	require.NoError(t, err, "recovery must succeed within max attempts")
	assert.Equal(t, StateActive, sess.State())
	assert.Equal(t, 3, flaky.Calls(), "launcher should be called 3 times (2 fail + 1 success)")
}

// TestRecoveryGoesFailedAfterMaxAttempts: with maxAttempts=3 and launcher
// always failing, recovery must end in StateFailed (sticky) and the
// launcher must be called exactly maxAttempts times.
func TestRecoveryGoesFailedAfterMaxAttempts(t *testing.T) {
	flaky := &flakyLauncher{failAlways: true, makeResult: makeFlakyResult}
	sess := newRecoveryTestSession("rec-B", flaky)

	rec := NewRecovery(RecoveryOpts{
		Launcher:    flaky,
		MaxAttempts: 3,
		Backoff:     fastBackoff(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := rec.Run(ctx, sess)
	require.Error(t, err, "recovery must report exhaustion error")
	assert.Equal(t, StateFailed, sess.State())
	assert.Equal(t, 3, flaky.Calls(), "launcher should be called exactly maxAttempts times")
}

// TestRecoveryAbortsOnContextCancel: cancelling the context mid-recovery
// must terminate the loop and transition to StateFailed (no further attempts).
func TestRecoveryAbortsOnContextCancel(t *testing.T) {
	// Launcher blocks until ctx cancel.
	blockingLauncher := &blockingLauncher{
		started: make(chan struct{}, 1),
	}
	sess := newRecoveryTestSession("rec-C", blockingLauncher)

	rec := NewRecovery(RecoveryOpts{
		Launcher:    blockingLauncher,
		MaxAttempts: 3,
		Backoff:     fastBackoff(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- rec.Run(ctx, sess) }()

	// Wait for the launcher to actually be invoked, then cancel.
	select {
	case <-blockingLauncher.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("launcher was never invoked")
	}
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not exit after ctx cancel")
	}
	assert.Equal(t, StateFailed, sess.State())
}

type blockingLauncher struct {
	started chan struct{}
	mu      sync.Mutex
	n       atomic.Int32
}

func (b *blockingLauncher) LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error) {
	b.n.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
