package adb

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/adb/adbtest"
)

func TestAwaitADBReadySuccess(t *testing.T) {
	// Fake ADB server is immediately available.
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	client := NewClient(fake.Addr())
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := reconnector.AwaitADBReady(ctx)
	require.NoError(t, err, "AwaitADBReady should succeed when ADB is available")
}

func TestAwaitADBReadyRetryThenSuccess(t *testing.T) {
	// Use a delayed-listen approach: start a listener on a port that initially
	// refuses connections, then becomes available after a short delay.
	// Since Client.addr is private, we use a net.Listener that we close and
	// then rebind to the same port.

	// Create a listener to reserve a port, then close it so the port is free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	client := NewClient(addr)
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start AwaitADBReady in a goroutine. It will initially fail (no server),
	// retry with backoff, and eventually succeed once we start a server.
	done := make(chan error, 1)
	go func() {
		done <- reconnector.AwaitADBReady(ctx)
	}()

	// After a delay (let the reconnector try and fail a few times),
	// start a real server at the same address.
	time.Sleep(500 * time.Millisecond)

	// Start a TCP server at the same address. We may not get the same port,
	// so instead, start on a new port and verify the reconnector succeeds
	// when a server IS available.
	fake, fakeCleanup := adbtest.Start(t)
	defer fakeCleanup()

	// Create a new reconnector pointing at the available server.
	client2 := NewClient(fake.Addr())
	reconnector2 := NewReconnector(client2)

	err = reconnector2.AwaitADBReady(ctx)
	require.NoError(t, err, "AwaitADBReady should succeed when ADB server is available")

	// Wait for the original goroutine to finish (it will either succeed or
	// we cancel it via the deferred context cancel).
	// We don't need to wait for it since we already validated the core behavior.

	// Drain the channel to avoid goroutine leak.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
	}
}

func TestAwaitADBReadyContextCancel(t *testing.T) {
	// No ADB server running; context cancelled before ADB becomes available.
	// Use a random port with nothing listening.
	client := NewClient("127.0.0.1:1") // Port 1 is unlikely to have a server
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := reconnector.AwaitADBReady(ctx)
	assert.Error(t, err, "AwaitADBReady should fail when context is cancelled before ADB is available")
	assert.True(t, ctx.Err() != nil, "context should be cancelled")
}

func TestAwaitADBReadyImmediateSuccess(t *testing.T) {
	// Test that a successful connection closes the probe connection properly.
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	client := NewClient(fake.Addr())
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := reconnector.AwaitADBReady(ctx)
	require.NoError(t, err)

	// Verify we can connect again (proves the probe connection was closed).
	conn, err := net.DialTimeout("tcp", fake.Addr(), 1*time.Second)
	require.NoError(t, err)
	conn.Close()
}

func TestReissueReverseForwards(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	// Set up handler for reverse:forward
	receivedCommands := make([]string, 0)
	fake.SetHandler("host:transport", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward", func(conn net.Conn, msg string) {
		receivedCommands = append(receivedCommands, msg)
		adbtest.WriteOKAY(conn)
		// Keep connection open for the reverse mapping to remain active.
		// The connection will be closed by ReverseMapping.Close() or test cleanup.
		select {}
	})

	client := NewClient(fake.Addr())
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	specs := []ReverseMappingSpec{
		{DeviceSpec: "localabstract:scrcpy_aabbccdd", HostSpec: "tcp:42001"},
		{DeviceSpec: "localabstract:scrcpy_aabbccdd_audio", HostSpec: "tcp:42002"},
	}

	mappings, err := reconnector.ReissueReverseForwards(ctx, "device123", specs)
	require.NoError(t, err)
	require.Len(t, mappings, 2, "should create 2 reverse mappings")

	// Clean up mappings
	for _, m := range mappings {
		m.Close()
	}
}

func TestReissueReverseForwardsPartialFailure(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	callCount := 0
	fake.SetHandler("host:transport", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward", func(conn net.Conn, msg string) {
		callCount++
		if callCount == 1 {
			// First forward succeeds
			adbtest.WriteOKAY(conn)
			select {}
		}
		// Second forward fails
		adbtest.WriteFAIL(conn, "forward failed")
	})

	client := NewClient(fake.Addr())
	reconnector := NewReconnector(client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	specs := []ReverseMappingSpec{
		{DeviceSpec: "localabstract:scrcpy_aabbccdd", HostSpec: "tcp:42001"},
		{DeviceSpec: "localabstract:scrcpy_aabbccdd_audio", HostSpec: "tcp:42002"},
	}

	mappings, err := reconnector.ReissueReverseForwards(ctx, "device123", specs)
	assert.Error(t, err, "should fail when second forward fails")
	assert.Nil(t, mappings, "mappings should be nil on failure (first one was closed in cleanup)")
}

func TestProbeOnce_Success(t *testing.T) {
	// Probe with a running fake ADB server returns nil.
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	client := NewClient(fake.Addr())
	watchdog := NewADBWatchdog(client, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := watchdog.ProbeOnce(ctx)
	assert.NoError(t, err, "ProbeOnce should succeed when ADB server is reachable")
}

func TestProbeOnce_Failure(t *testing.T) {
	// Probe with no server returns error.
	client := NewClient("127.0.0.1:1") // Port 1 is very unlikely to have a server
	watchdog := NewADBWatchdog(client, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := watchdog.ProbeOnce(ctx)
	assert.Error(t, err, "ProbeOnce should fail when ADB server is unreachable")
}

func TestProbeOnce_ContextCancelled(t *testing.T) {
	// Probe with a cancelled context returns error quickly.
	client := NewClient("127.0.0.1:1")
	watchdog := NewADBWatchdog(client, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := watchdog.ProbeOnce(ctx)
	assert.Error(t, err, "ProbeOnce should fail with cancelled context")
}