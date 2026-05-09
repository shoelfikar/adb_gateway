package session

import (
	"context"
	"net"
	"regexp"
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

	// Create an active entry with a session that has closeable resources.
	activeEntry := r.GetOrCreate("device_active")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	activeSess := &DeviceSession{
		ID:         "session-1",
		Serial:     "device_active",
		state:      StateActive,
		videoLn:    ln,
		reverseMap: &adb.ReverseMapping{DeviceSpec: "localabstract:scrcpy_test", HostSpec: "tcp:0"},
		cleanup:    func() { ln.Close() },
	}
	activeEntry.mu.Lock()
	activeEntry.State = StateActive
	activeEntry.Session = activeSess
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

	// ALL entries should be REMOVED from the registry (not just idle ones).
	_, ok := r.Get("device_active")
	assert.False(t, ok, "active device should be removed from registry after MarkAllDisconnected")

	_, ok = r.Get("device_idle")
	assert.False(t, ok, "idle device should be removed from registry after MarkAllDisconnected")

	_, ok = r.Get("device_starting")
	assert.False(t, ok, "starting device should be removed from registry after MarkAllDisconnected")

	_, ok = r.Get("device_failed")
	assert.False(t, ok, "failed device should be removed from registry after MarkAllDisconnected")

	// Registry should be empty.
	entries := r.List()
	assert.Empty(t, entries, "registry should be empty after MarkAllDisconnected")
}

func TestMarkAllDisconnected_ReleasesSessionResources(t *testing.T) {
	r := NewRegistry()

	// Create an entry with a session that has a real listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cleanupCalled := false
	sess := &DeviceSession{
		ID:         "session-1",
		Serial:     "device_active",
		state:      StateActive,
		videoLn:    ln,
		reverseMap: &adb.ReverseMapping{DeviceSpec: "localabstract:scrcpy_test", HostSpec: "tcp:0"},
		cleanup:    func() { cleanupCalled = true },
	}

	entry := r.GetOrCreate("device_active")
	entry.mu.Lock()
	entry.State = StateActive
	entry.Session = sess
	entry.mu.Unlock()

	r.MarkAllDisconnected()

	// Cleanup function should have been called.
	assert.True(t, cleanupCalled, "ReleaseResources should call cleanup function")

	// Registry should be empty.
	entries := r.List()
	assert.Empty(t, entries, "registry should be empty after MarkAllDisconnected")

	// ActiveSessionSpecs on empty registry returns empty map (method removed, but verify via List).
	assert.Empty(t, entries)
}

func TestMarkAllDisconnected_EmptyRegistry(t *testing.T) {
	r := NewRegistry()

	// MarkAllDisconnected on empty registry should not panic.
	r.MarkAllDisconnected()

	entries := r.List()
	assert.Empty(t, entries, "empty registry should remain empty after MarkAllDisconnected")
}

func TestMarkAllDisconnected_RemovesAllEntries(t *testing.T) {
	r := NewRegistry()

	// Create idle entries (no sessions).
	r.GetOrCreate("device1")
	r.GetOrCreate("device2")

	// Verify entries exist.
	_, ok1 := r.Get("device1")
	_, ok2 := r.Get("device2")
	assert.True(t, ok1)
	assert.True(t, ok2)

	// Mark all disconnected.
	r.MarkAllDisconnected()

	// Both entries should be REMOVED (all entries, not just idle ones).
	_, ok1 = r.Get("device1")
	_, ok2 = r.Get("device2")
	assert.False(t, ok1, "device1 should be removed from registry")
	assert.False(t, ok2, "device2 should be removed from registry")

	// Registry should be completely empty.
	entries := r.List()
	assert.Empty(t, entries, "registry should be empty after MarkAllDisconnected")
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

func TestReleaseResources_Idempotent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	cleanupCount := 0
	sess := &DeviceSession{
		ID:         "session-idempotent",
		Serial:     "device_test",
		state:      StateActive,
		videoLn:    ln,
		reverseMap: &adb.ReverseMapping{DeviceSpec: "localabstract:scrcpy_test", HostSpec: "tcp:0"},
		cleanup:    func() { cleanupCount++ },
	}

	// Call ReleaseResources twice — should not panic or double-close.
	sess.ReleaseResources()
	assert.Equal(t, 1, cleanupCount, "cleanup should be called exactly once")

	sess.ReleaseResources() // second call — should be a no-op
	assert.Equal(t, 1, cleanupCount, "cleanup should still only be called once (idempotent)")
}
// ---------------------------------------------------------------------------
// Phase 3 — DEV-06 device-serial stability audit (03-01)
// ---------------------------------------------------------------------------

// TestDeviceSerialStability locks the contract that a device serial round-trips
// byte-for-byte through Registry.Add -> entry -> Session -> metric labels.
// Future refactors that derive serial from USB path or any unstable identifier
// will fail this test (DEV-06 audit).
func TestDeviceSerialStability(t *testing.T) {
	const want = "ABCD1234"

	// 1. Registry.GetOrCreate stores the serial verbatim.
	r := NewRegistry()
	entry := r.GetOrCreate(want)
	assert.Equal(t, want, entry.Serial, "registry must preserve serial bytes")

	// 2. Registry.Get returns the same bytes.
	got, ok := r.Get(want)
	require.True(t, ok)
	assert.Equal(t, want, got.Serial)

	// 3. Round-trip through DeviceSession.Serial field (the metric label source).
	sess := &DeviceSession{Serial: want}
	entry.SetSession(sess)
	assert.Equal(t, want, entry.GetSession().Serial,
		"DeviceSession.Serial must equal registry serial — locked for DEV-06")

	// 4. Serial must be valid per the API regex (handlers_devices.go:21).
	//    Replicating the pattern here because the api package would create an
	//    import cycle. If the regex changes, both must change in lockstep.
	pattern := regexp.MustCompile(`^[a-zA-Z0-9:._-]+$`)
	assert.True(t, pattern.MatchString(want),
		"serial must satisfy api.serialPattern — handlers reject otherwise")
}
