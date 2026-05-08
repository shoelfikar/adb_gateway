package session

import (
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nopLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLeaseExclusive(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	var success int32
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if _, err := m.Acquire("user-x"); err == nil {
				atomic.AddInt32(&success, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), success)
}

func TestLeaseExpiry(t *testing.T) {
	m := NewLeaseManager(50*time.Millisecond, nopLog())
	l, err := m.Acquire("user-x")
	require.NoError(t, err)
	ch := m.ReleaseChanFor(l.ID)
	require.NotNil(t, ch)

	select {
	case reason, ok := <-ch:
		require.True(t, ok || reason == ReasonExpired) // chan closes after send
		assert.Equal(t, ReasonExpired, reason)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expiry signal not received")
	}
	// Channel should be closed.
	_, ok := <-ch
	assert.False(t, ok)
	_, held := m.Snapshot()
	assert.False(t, held)
}

func TestLeaseExtendResetsTTL(t *testing.T) {
	m := NewLeaseManager(100*time.Millisecond, nopLog())
	l, err := m.Acquire("user-x")
	require.NoError(t, err)

	time.Sleep(60 * time.Millisecond)
	l2, err := m.Extend(l.ID)
	require.NoError(t, err)
	assert.Equal(t, l.ID, l2.ID, "Extend must keep same ID")
	assert.True(t, l2.ExpiresAt.After(l.ExpiresAt))

	time.Sleep(60 * time.Millisecond)
	// Original TTL would have fired by now; lease should still be held.
	_, held := m.Snapshot()
	assert.True(t, held)

	time.Sleep(60 * time.Millisecond)
	_, held = m.Snapshot()
	assert.False(t, held)
}

func TestLeaseRelease(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	l, _ := m.Acquire("u")
	ch := m.ReleaseChanFor(l.ID)
	require.NoError(t, m.Release(l.ID))

	select {
	case reason := <-ch:
		assert.Equal(t, ReasonClientReleased, reason)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("release signal not received")
	}
}

func TestLeaseGrace(t *testing.T) {
	m := newLeaseManagerForTest(60*time.Second, 100*time.Millisecond, nopLog())
	l, _ := m.Acquire("u")
	ch := m.ReleaseChanFor(l.ID)
	require.NoError(t, m.BeginGrace(l.ID))

	// During grace, other clients cannot acquire.
	_, err := m.Acquire("v")
	require.ErrorIs(t, err, ErrLeaseHeldByOther)

	// Wait for grace timer to fire.
	select {
	case reason := <-ch:
		assert.Equal(t, ReasonExpired, reason)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("grace expiry not received")
	}

	// After grace fires, new acquire succeeds.
	_, err = m.Acquire("v")
	assert.NoError(t, err)
}

func TestLeasePatchDuringGrace(t *testing.T) {
	m := newLeaseManagerForTest(60*time.Second, 100*time.Millisecond, nopLog())
	l, _ := m.Acquire("u")
	require.NoError(t, m.BeginGrace(l.ID))

	// Patch (Extend) during grace should re-anchor.
	l2, err := m.Extend(l.ID)
	require.NoError(t, err)
	assert.Equal(t, l.ID, l2.ID)

	// Wait past the original grace window.
	time.Sleep(150 * time.Millisecond)
	// Lease must STILL be held (re-anchor cancelled grace).
	_, held := m.Snapshot()
	assert.True(t, held)
}

func TestLeaseForceReleaseAdmin(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	l, _ := m.Acquire("u")
	ch := m.ReleaseChanFor(l.ID)
	m.ForceRelease(ReasonAdminRevoked)

	select {
	case reason := <-ch:
		assert.Equal(t, ReasonAdminRevoked, reason)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("force-release signal not received")
	}
}

func TestLeaseForceReleaseDeviceGone(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	l, _ := m.Acquire("u")
	ch := m.ReleaseChanFor(l.ID)
	m.ForceRelease(ReasonDeviceGone)
	assert.Equal(t, ReasonDeviceGone, <-ch)
}

func TestLeaseConstantTimeCompare(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	l, _ := m.Acquire("u")
	assert.True(t, m.IsHeldBy(l.ID))
	assert.False(t, m.IsHeldBy("a-different-uuid-of-similar-length"))
	assert.False(t, m.IsHeldBy("short"))
	assert.False(t, m.IsHeldBy(""))
}

func TestLeaseExtendWrongID(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	_, _ = m.Acquire("u")
	_, err := m.Extend("not-the-id")
	assert.ErrorIs(t, err, ErrLeaseMismatch)
}

func TestLeaseReleaseChanForeRapidReacquire(t *testing.T) {
	m := NewLeaseManager(60*time.Second, nopLog())
	l1, _ := m.Acquire("u")
	require.NoError(t, m.Release(l1.ID))
	l2, err := m.Acquire("v")
	require.NoError(t, err)
	require.NotEqual(t, l1.ID, l2.ID)

	// Old chan was closed by Release; ReleaseChanFor(l1.ID) should now return nil.
	assert.Nil(t, m.ReleaseChanFor(l1.ID))
	// New chan exists.
	assert.NotNil(t, m.ReleaseChanFor(l2.ID))
}

func TestLeaseNoTimerLeak(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	m := NewLeaseManager(50*time.Millisecond, nopLog())
	for i := 0; i < 1000; i++ {
		l, err := m.Acquire("u")
		require.NoError(t, err)
		require.NoError(t, m.Release(l.ID))
	}

	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	assert.LessOrEqualf(t, after-before, 5, "goroutine leak: before=%d after=%d", before, after)
}