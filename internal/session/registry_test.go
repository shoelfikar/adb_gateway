package session

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

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

	// Send a device connect event (goadb DeviceState.String() returns "StateOnline", etc.)
	events <- adb.DeviceEvent{Serial: "ABC123", State: "StateOnline"}

	// Give the goroutine time to process.
	// In tests with sync.Map, the update should be nearly instant.
	// We use a small assertion loop to avoid flaky timing.
	var entry *DeviceEntry
	assert.Eventually(t, func() bool {
		var ok bool
		entry, ok = r.Get("ABC123")
		return ok
	}, 1*time.Second, 10*time.Millisecond, "device should appear in registry after connect event")

	require.NotNil(t, entry)
	assert.Equal(t, "ABC123", entry.Serial)
	assert.Equal(t, StateIdle, entry.State)

	// Send another device connect event.
	events <- adb.DeviceEvent{Serial: "DEF456", State: "StateOnline"}
	assert.Eventually(t, func() bool {
		_, ok := r.Get("DEF456")
		return ok
	}, 1*time.Second, 10*time.Millisecond, "second device should appear in registry")

	// Send a disconnect event for ABC123 (StateDisconnected is goadb's string form).
	events <- adb.DeviceEvent{Serial: "ABC123", State: "StateDisconnected"}
	assert.Eventually(t, func() bool {
		_, ok := r.Get("ABC123")
		return !ok
	}, 1*time.Second, 10*time.Millisecond, "device should be removed from registry after disconnect event")

	// Verify DEF456 is still present.
	entry2, ok := r.Get("DEF456")
	assert.True(t, ok)
	assert.Equal(t, "DEF456", entry2.Serial)
}

func TestWatchDevices_ContextCancellation(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan bool, 1)
	go func() {
		result := r.WatchDevices(ctx, events)
		done <- result
	}()

	// Cancel the context.
	cancel()

	// WatchDevices should exit and return false (graceful shutdown).
	assert.Eventually(t, func() bool {
		select {
		case result := <-done:
			return !result // false = context cancelled
		default:
			return false
		}
	}, 1*time.Second, 10*time.Millisecond, "WatchDevices should exit after context cancellation and return false")
}

func TestWatchDevices_ChannelClosed(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan bool, 1)
	go func() {
		result := r.WatchDevices(ctx, events)
		done <- result
	}()

	// Close the event channel.
	close(events)

	// WatchDevices should exit and return true (ADB disconnect).
	assert.Eventually(t, func() bool {
		select {
		case result := <-done:
			return result // true = ADB disconnect
		default:
			return false
		}
	}, 1*time.Second, 10*time.Millisecond, "WatchDevices should exit when event channel is closed and return true")
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

func TestMarkAllDisconnected(t *testing.T) {
	r := NewRegistry()

	// Create an active entry with a session.
	activeEntry := r.GetOrCreate("device_active")
	activeEntry.mu.Lock()
	activeEntry.State = StateActive
	activeEntry.Session = &DeviceSession{ID: "session-1", Serial: "device_active"}
	activeEntry.mu.Unlock()

	// Create an idle entry with no session.
	idleEntry := r.GetOrCreate("device_idle")
	_ = idleEntry

	// Create a starting entry with a session.
	startingEntry := r.GetOrCreate("device_starting")
	startingEntry.mu.Lock()
	startingEntry.State = StateStarting
	startingEntry.Session = &DeviceSession{ID: "session-2", Serial: "device_starting"}
	startingEntry.mu.Unlock()

	// Create an already-failed entry.
	failedEntry := r.GetOrCreate("device_failed")
	failedEntry.mu.Lock()
	failedEntry.State = StateFailed
	failedEntry.Session = &DeviceSession{ID: "session-3", Serial: "device_failed"}
	failedEntry.mu.Unlock()

	// Call MarkAllDisconnected.
	r.MarkAllDisconnected()

	// Active entry should be transitioned to StateFailed and KEPT.
	activeEntry, ok := r.Get("device_active")
	assert.True(t, ok, "active device should still be in registry after MarkAllDisconnected")
	assert.Equal(t, StateFailed, activeEntry.GetState(), "active device should transition to StateFailed")

	// Idle entry should be REMOVED from the registry.
	_, ok = r.Get("device_idle")
	assert.False(t, ok, "idle device should be removed from registry after MarkAllDisconnected")

	// Starting entry should be transitioned to StateFailed and KEPT.
	startingEntry, ok = r.Get("device_starting")
	assert.True(t, ok, "starting device should still be in registry after MarkAllDisconnected")
	assert.Equal(t, StateFailed, startingEntry.GetState(), "starting device should transition to StateFailed")

	// Failed entry should remain in StateFailed.
	failedEntry, ok = r.Get("device_failed")
	assert.True(t, ok, "already-failed device should still be in registry")
	assert.Equal(t, StateFailed, failedEntry.GetState(), "already-failed device should remain StateFailed")
}

func TestMarkAllDisconnected_EmptyRegistry(t *testing.T) {
	r := NewRegistry()

	// MarkAllDisconnected on empty registry should not panic.
	r.MarkAllDisconnected()

	entries := r.List()
	assert.Empty(t, entries, "empty registry should remain empty after MarkAllDisconnected")
}

func TestMarkAllDisconnected_IdleEntryRemoved(t *testing.T) {
	r := NewRegistry()

	// Create idle entry with no session.
	r.GetOrCreate("device1")
	r.GetOrCreate("device2")

	// Verify entries exist.
	_, ok1 := r.Get("device1")
	_, ok2 := r.Get("device2")
	assert.True(t, ok1)
	assert.True(t, ok2)

	// Mark all disconnected.
	r.MarkAllDisconnected()

	// Both idle entries should be REMOVED (not transitioned to failed).
	_, ok1 = r.Get("device1")
	_, ok2 = r.Get("device2")
	assert.False(t, ok1, "idle device1 should be removed from registry")
	assert.False(t, ok2, "idle device2 should be removed from registry")
}

func TestWatchDevices_ReturnsTrueOnChannelClose(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan bool, 1)
	go func() {
		result := r.WatchDevices(ctx, events)
		done <- result
	}()

	// Close the events channel to simulate ADB disconnect.
	close(events)

	// WatchDevices should return true (ADB disconnect).
	select {
	case result := <-done:
		assert.True(t, result, "WatchDevices should return true when event channel is closed (ADB disconnect)")
	case <-time.After(2 * time.Second):
		t.Fatal("WatchDevices did not return within timeout")
	}
}

func TestWatchDevices_ReturnsFalseOnContextCancel(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan adb.DeviceEvent, 10)

	done := make(chan bool, 1)
	go func() {
		result := r.WatchDevices(ctx, events)
		done <- result
	}()

	// Cancel the context to simulate graceful shutdown.
	cancel()

	// WatchDevices should return false (graceful shutdown).
	select {
	case result := <-done:
		assert.False(t, result, "WatchDevices should return false when context is cancelled (graceful shutdown)")
	case <-time.After(2 * time.Second):
		t.Fatal("WatchDevices did not return within timeout")
	}
}

func TestWatchDevices_UpdatesFailedToIdle(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan adb.DeviceEvent, 10)

	go r.WatchDevices(ctx, events)

	// Create an entry in StateFailed.
	entry := r.GetOrCreate("device_recovery")
	entry.mu.Lock()
	entry.State = StateFailed
	entry.mu.Unlock()

	// Send a connect event for the same device.
	events <- adb.DeviceEvent{Serial: "device_recovery", State: "StateOnline"}

	// Wait for the entry to be updated.
	assert.Eventually(t, func() bool {
		return entry.GetState() == StateIdle
	}, 1*time.Second, 10*time.Millisecond, "failed device should recover to StateIdle on reconnect event")
}

func TestActiveSessionSpecs_EmptyRegistry(t *testing.T) {
	r := NewRegistry()

	specs := r.ActiveSessionSpecs()
	assert.Empty(t, specs, "empty registry should return empty specs")
}

func TestActiveSessionSpecs_WithActiveSession(t *testing.T) {
	r := NewRegistry()

	// Create a listener to get a real port (needed for session creation).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	// Create an active entry with a session that has a reverse mapping.
	entry := r.GetOrCreate("device1")
	rm := &adb.ReverseMapping{
		DeviceSpec: "localabstract:scrcpy_testscid",
		HostSpec:   "tcp:42001",
	}
	sess := &DeviceSession{
		ID:         "session-1",
		Serial:     "device1",
		state:      StateActive,
		videoLn:    ln,
		reverseMap: rm,
		scid:       "testscid",
	}
	entry.mu.Lock()
	entry.State = StateActive
	entry.Session = sess
	entry.mu.Unlock()

	specs := r.ActiveSessionSpecs()
	assert.Len(t, specs, 1, "should have specs for one device")
	assert.Contains(t, specs, "device1")

	deviceSpecs := specs["device1"]
	assert.Len(t, deviceSpecs, 1, "should have one spec per device")
	assert.Equal(t, "localabstract:scrcpy_testscid", deviceSpecs[0].DeviceSpec)
	assert.Equal(t, "tcp:42001", deviceSpecs[0].HostSpec)
}

func TestActiveSessionSpecs_IgnoresInactiveEntries(t *testing.T) {
	r := NewRegistry()

	// Create an idle entry (should be ignored).
	entry := r.GetOrCreate("device_idle")
	entry.mu.Lock()
	entry.State = StateIdle
	entry.Session = &DeviceSession{ID: "session-idle", Serial: "device_idle"}
	entry.mu.Unlock()

	// Create a failed entry (should be ignored).
	entry2 := r.GetOrCreate("device_failed")
	entry2.mu.Lock()
	entry2.State = StateFailed
	entry2.Session = &DeviceSession{ID: "session-failed", Serial: "device_failed"}
	entry2.mu.Unlock()

	specs := r.ActiveSessionSpecs()
	assert.Empty(t, specs, "should not return specs for idle or failed entries")
}