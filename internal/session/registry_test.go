package session

import (
	"context"
	"sync"
	"testing"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()
	assert.NotNil(t, r)
	// New registry should have zero devices.
	entries := r.List()
	assert.Empty(t, entries)
}

func TestGetOrCreate(t *testing.T) {
	r := NewRegistry()

	// Add a device; should return the new entry.
	entry := r.GetOrCreate("ABC123")
	assert.Equal(t, "ABC123", entry.Serial)
	assert.Equal(t, StateIdle, entry.State)

	// GetOrCreate same serial again; should return the same entry (idempotent).
	entry2 := r.GetOrCreate("ABC123")
	assert.Same(t, entry, entry2, "GetOrCreate should return the same entry for same serial")

	// Adding a different serial should create a new entry.
	entry3 := r.GetOrCreate("DEF456")
	assert.Equal(t, "DEF456", entry3.Serial)
	assert.Equal(t, StateIdle, entry3.State)
}

func TestGet(t *testing.T) {
	r := NewRegistry()

	// Get nonexistent serial returns nil, false.
	entry, ok := r.Get("NONEXISTENT")
	assert.Nil(t, entry)
	assert.False(t, ok)

	// Add a device, then get it.
	r.GetOrCreate("ABC123")
	entry, ok = r.Get("ABC123")
	assert.True(t, ok)
	assert.Equal(t, "ABC123", entry.Serial)
}

func TestList(t *testing.T) {
	r := NewRegistry()

	// Empty registry returns empty list.
	entries := r.List()
	assert.Empty(t, entries)

	// Add 3 devices.
	r.GetOrCreate("device1")
	r.GetOrCreate("device2")
	r.GetOrCreate("device3")

	entries = r.List()
	assert.Len(t, entries, 3)

	// Verify all serials are present (order may vary with sync.Map).
	serials := make(map[string]bool)
	for _, e := range entries {
		serials[e.Serial] = true
	}
	assert.True(t, serials["device1"])
	assert.True(t, serials["device2"])
	assert.True(t, serials["device3"])
}

func TestRemove(t *testing.T) {
	r := NewRegistry()

	// Add a device, then remove it.
	r.GetOrCreate("ABC123")
	_, ok := r.Get("ABC123")
	assert.True(t, ok)

	r.Remove("ABC123")

	// After removal, Get should return nil, false.
	entry, ok := r.Get("ABC123")
	assert.Nil(t, entry)
	assert.False(t, ok)

	// Removing a nonexistent serial should not panic.
	r.Remove("NONEXISTENT")
}

func TestWatchDevices(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan adb.DeviceEvent, 10)

	// Start watching in a goroutine.
	go r.WatchDevices(ctx, events)

	// Send a device connect event.
	events <- adb.DeviceEvent{Serial: "ABC123", State: "device"}

	// Give the goroutine time to process.
	// In tests with sync.Map, the update should be nearly instant.
	// We use a small assertion loop to avoid flaky timing.
	var entry *DeviceEntry
	assert.Eventually(t, func() bool {
		var ok bool
		entry, ok = r.Get("ABC123")
		return ok
	}, 1000000000, 10000000, "device should appear in registry after connect event")

	require.NotNil(t, entry)
	assert.Equal(t, "ABC123", entry.Serial)
	assert.Equal(t, StateIdle, entry.State)

	// Send another device connect event.
	events <- adb.DeviceEvent{Serial: "DEF456", State: "device"}
	assert.Eventually(t, func() bool {
		_, ok := r.Get("DEF456")
		return ok
	}, 1000000000, 10000000, "second device should appear in registry")

	// Send a disconnect event for ABC123.
	events <- adb.DeviceEvent{Serial: "ABC123", State: "disconnected"}
	assert.Eventually(t, func() bool {
		_, ok := r.Get("ABC123")
		return !ok
	}, 1000000000, 10000000, "device should be removed from registry after disconnect event")

	// Verify DEF456 is still present.
	entry2, ok := r.Get("DEF456")
	assert.True(t, ok)
	assert.Equal(t, "DEF456", entry2.Serial)
}

func TestWatchDevices_ContextCancellation(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan struct{})
	go func() {
		r.WatchDevices(ctx, events)
		close(done)
	}()

	// Cancel the context.
	cancel()

	// WatchDevices should exit.
	assert.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 1000000000, 10000000, "WatchDevices should exit after context cancellation")
}

func TestWatchDevices_ChannelClosed(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan struct{})
	go func() {
		r.WatchDevices(ctx, events)
		close(done)
	}()

	// Close the event channel.
	close(events)

	// WatchDevices should exit.
	assert.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 1000000000, 10000000, "WatchDevices should exit when event channel is closed")
}

func TestConcurrentAccess(t *testing.T) {
	r := NewRegistry()

	// Use multiple goroutines to concurrently GetOrCreate different serials.
	var wg sync.WaitGroup
	numDevices := 100
	serials := make([]string, numDevices)
	for i := 0; i < numDevices; i++ {
		serials[i] = "device_" + string(rune('A'+i%26)) + string(rune('0'+i%10))
	}

	for i := 0; i < numDevices; i++ {
		wg.Add(1)
		go func(serial string) {
			defer wg.Done()
			entry := r.GetOrCreate(serial)
			assert.Equal(t, serial, entry.Serial)
			assert.Equal(t, StateIdle, entry.State)
		}(serials[i])
	}
	wg.Wait()

	// Verify all devices were added.
	entries := r.List()
	assert.Len(t, entries, numDevices)
}

func TestCloseAllSessions(t *testing.T) {
	r := NewRegistry()

	// Create entries with mock sessions.
	entry1 := r.GetOrCreate("ABC123")
	entry1.mu.Lock()
	entry1.Session = &DeviceSession{ID: "session-1", Serial: "ABC123"}
	entry1.mu.Unlock()

	entry2 := r.GetOrCreate("DEF456")
	entry2.mu.Lock()
	entry2.Session = &DeviceSession{ID: "session-2", Serial: "DEF456"}
	entry2.mu.Unlock()

	// CloseAllSessions should iterate and call Close on each session.
	// Since DeviceSession.Close is a stub that returns an error,
	// we just verify it doesn't panic and processes all entries.
	ctx := context.Background()
	r.CloseAllSessions(ctx)

	// Verify entries still exist (CloseAllSessions doesn't remove entries).
	_, ok1 := r.Get("ABC123")
	_, ok2 := r.Get("DEF456")
	assert.True(t, ok1)
	assert.True(t, ok2)
}

func TestPerDeviceMutex(t *testing.T) {
	r := NewRegistry()

	// Verify that each DeviceEntry has its own mutex.
	entry1 := r.GetOrCreate("ABC123")
	entry2 := r.GetOrCreate("DEF456")

	// Locking one should not block the other.
	entry1.mu.Lock()
	// This should not deadlock since entry2 has its own mutex.
	entry2.mu.Lock()
	entry2.mu.Unlock()
	entry1.mu.Unlock()

	// Verify that the mutex is per-entry, not a global.
	// The functional proof is that locking entry1.mu does not block entry2.mu,
	// which the above lock/unlock sequence demonstrates. Pointer comparison
	// is unreliable for sync.Mutex, so we confirm by checking that each entry
	// is a distinct struct (already verified by GetOrCreate idempotency tests).
}