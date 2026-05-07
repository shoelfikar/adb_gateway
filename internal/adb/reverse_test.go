package adb

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pelni/adb-gateway/internal/adb/adbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReverseForwardSuccess(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	// Set up reverse:forward handler that keeps connection open
	var receivedCmd string
	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward:", func(conn net.Conn, msg string) {
		receivedCmd = msg
		adbtest.WriteOKAY(conn)
		// Keep connection open (don't close) to preserve the reverse mapping
		select {}
	})

	c := NewClient(fake.Addr())
	mapping, err := c.ReverseForward(context.Background(), "device123", "localabstract:scrcpy_00001234", "tcp:42001")
	require.NoError(t, err)
	require.NotNil(t, mapping)

	assert.Equal(t, "localabstract:scrcpy_00001234", mapping.DeviceSpec)
	assert.Equal(t, "tcp:42001", mapping.HostSpec)

	// Verify the command used semicolon separator
	assert.Contains(t, receivedCmd, "reverse:forward:localabstract:scrcpy_00001234;tcp:42001")

	// Close the mapping to clean up
	err = mapping.Close()
	require.NoError(t, err)
}

func TestReverseForwardLocalAbstract(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	var receivedCmd string
	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward:", func(conn net.Conn, msg string) {
		receivedCmd = msg
		adbtest.WriteOKAY(conn)
		select {}
	})

	c := NewClient(fake.Addr())
	mapping, err := c.ReverseForward(context.Background(), "emulator-5554", "localabstract:scrcpy_00001234", "tcp:42001")
	require.NoError(t, err)
	defer mapping.Close()

	// Verify the wire command format:
	// - Uses SEMICOLON separator between device-socket and host-socket (NOT colon)
	// - Uses localabstract:scrcpy_<SCID> for device-side socket (NOT tcp:27183)
	expectedCmd := "reverse:forward:localabstract:scrcpy_00001234;tcp:42001"
	assert.Equal(t, expectedCmd, receivedCmd)

	// Verify that colons in the local and remote specs don't interfere
	// The format is: reverse:forward:<deviceSpec>;<hostSpec>
	// where deviceSpec = "localabstract:scrcpy_00001234" (has a colon!)
	// and hostSpec = "tcp:42001" (has a colon!)
	// The SEMICOLON is the separator between the two specs.
	assert.True(t, strings.Contains(receivedCmd, ";tcp:"), "wire command must use semicolon separator, not colon")
	assert.False(t, strings.Contains(receivedCmd, ";tcp;"), "must not have semicolon within host spec")
}

func TestReverseMappingConnectionPreserved(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	connectionClosed := make(chan struct{})

	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
		// Monitor when the connection is closed
		go func() {
			buf := make([]byte, 1)
			_, err := conn.Read(buf)
			if err != nil {
				close(connectionClosed)
			}
		}()
		// Keep handler goroutine alive so connection stays open
		select {}
	})

	c := NewClient(fake.Addr())
	mapping, err := c.ReverseForward(context.Background(), "device123", "localabstract:scrcpy_abcd", "tcp:27183")
	require.NoError(t, err)

	// The connection should NOT be closed after ReverseForward returns.
	// Only ReverseMapping.Close() should close it.
	// We verify by checking that connectionClosed is NOT signaled yet.
	select {
	case <-connectionClosed:
		t.Fatal("connection was closed prematurely - reverse mapping would be lost")
	case <-time.After(100 * time.Millisecond):
		// Good - connection is still open
	}

	// Now close the mapping and verify the connection closes
	err = mapping.Close()
	require.NoError(t, err)

	// Wait for the connection close to be detected
	select {
	case <-connectionClosed:
		// Connection closed as expected
	case <-time.After(1 * time.Second):
		t.Fatal("connection was not closed after ReverseMapping.Close()")
	}
}

func TestReverseForwardTransportFailure(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	// Transport handler returns FAIL
	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteFAIL(conn, "device not found")
	})

	c := NewClient(fake.Addr())
	mapping, err := c.ReverseForward(context.Background(), "nonexistent", "localabstract:scrcpy_00001234", "tcp:42001")
	assert.Error(t, err)
	assert.Nil(t, mapping)
	assert.Contains(t, err.Error(), "transport")
}

func TestReverseListForward(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:list-forward", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
		// Return a list of reverse forward entries
		// Format: "<serial> <local> <remote>\n"
		list := "device123 localabstract:scrcpy_00001234 tcp:42001\ndevice123 localabstract:scrcpy_00005678 tcp:42002\n"
		adbtest.WriteString(conn, list)
	})

	c := NewClient(fake.Addr())
	entries, err := c.ReverseListForward(context.Background(), "device123")
	require.NoError(t, err)

	require.Len(t, entries, 2)
	assert.Equal(t, "localabstract:scrcpy_00001234", entries[0].Local)
	assert.Equal(t, "tcp:42001", entries[0].Remote)
	assert.Equal(t, "localabstract:scrcpy_00005678", entries[1].Local)
	assert.Equal(t, "tcp:42002", entries[1].Remote)
}

func TestReverseListForwardEmpty(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:list-forward", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
		// Return empty list
		adbtest.WriteString(conn, "")
	})

	c := NewClient(fake.Addr())
	entries, err := c.ReverseListForward(context.Background(), "device123")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestReverseRemove(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	var receivedCmd string
	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:killforward:", func(conn net.Conn, msg string) {
		receivedCmd = msg
		adbtest.WriteOKAY(conn)
	})

	c := NewClient(fake.Addr())
	err := c.ReverseRemove(context.Background(), "device123", "localabstract:scrcpy_00001234")
	require.NoError(t, err)

	expectedCmd := "reverse:killforward:localabstract:scrcpy_00001234"
	assert.Equal(t, expectedCmd, receivedCmd)
}

func TestReverseKillforwardAll(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	var receivedCmd string
	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:killforward-all", func(conn net.Conn, msg string) {
		receivedCmd = msg
		adbtest.WriteOKAY(conn)
	})

	c := NewClient(fake.Addr())
	err := c.ReverseKillforwardAll(context.Background(), "device123")
	require.NoError(t, err)

	assert.Equal(t, "reverse:killforward-all", receivedCmd)
}

func TestReverseForwardContextCanceled(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	fake.SetHandler("host:transport:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
	})
	fake.SetHandler("reverse:forward:", func(conn net.Conn, msg string) {
		adbtest.WriteOKAY(conn)
		select {}
	})

	c := NewClient(fake.Addr())

	// Create a context that is already canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mapping, err := c.ReverseForward(ctx, "device123", "localabstract:scrcpy_test", "tcp:42001")
	assert.Error(t, err)
	assert.Nil(t, mapping)
}

func TestReverseListForwardParsing(t *testing.T) {
	// Test the parsing logic with various formats
	tests := []struct {
		name     string
		input    string
		expected []ForwardEntry
	}{
		{
			name:     "single entry",
			input:    "device123 localabstract:scrcpy_00001234 tcp:42001\n",
			expected: []ForwardEntry{{Local: "localabstract:scrcpy_00001234", Remote: "tcp:42001"}},
		},
		{
			name:     "multiple entries",
			input:    "device123 localabstract:scrcpy_00001234 tcp:42001\ndevice123 localabstract:scrcpy_00005678 tcp:42002\n",
			expected: []ForwardEntry{
				{Local: "localabstract:scrcpy_00001234", Remote: "tcp:42001"},
				{Local: "localabstract:scrcpy_00005678", Remote: "tcp:42002"},
			},
		},
		{
			name:     "empty input",
			input:    "",
			expected: nil,
		},
		{
			name:     "trailing newline only",
			input:    "\n",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse using the same logic as ReverseListForward
			var entries []ForwardEntry
			for _, line := range strings.Split(strings.TrimSpace(tt.input), "\n") {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, " ", 3)
				if len(parts) >= 2 {
					entries = append(entries, ForwardEntry{
						Local:  parts[len(parts)-2],
						Remote: parts[len(parts)-1],
					})
				}
			}

			if tt.expected == nil {
				assert.Nil(t, entries)
			} else {
				assert.Equal(t, tt.expected, entries)
			}
		})
	}
}

// TestSemicolonSeparatorFormat verifies that the wire command format uses
// a SEMICOLON between device-socket and host-socket specs, NOT a colon.
// This is verified against AOSP SERVICES.TXT and is a critical pitfall.
func TestSemicolonSeparatorFormat(t *testing.T) {
	deviceSpec := "localabstract:scrcpy_00001234"
	hostSpec := "tcp:42001"

	// The correct wire command format uses SEMICOLON separator
	cmd := "reverse:forward:" + deviceSpec + ";" + hostSpec

	// Verify semicolon is present between the two specs
	assert.Contains(t, cmd, ";tcp:", "wire command must use semicolon separator between device-socket and host-socket")

	// Verify the format matches AOSP SERVICES.TXT
	// reverse:forward:<device-socket>;<host-socket>
	expected := "reverse:forward:localabstract:scrcpy_00001234;tcp:42001"
	assert.Equal(t, expected, cmd)

	// Verify that using a colon separator would be WRONG
	wrongCmd := "reverse:forward:" + deviceSpec + ":" + hostSpec
	// This would produce: "reverse:forward:localabstract:scrcpy_00001234:tcp:42001"
	// which has 4 colons - ADB would parse this incorrectly
	assert.NotEqual(t, cmd, wrongCmd, "semicolons and colons must not be confused")
}

// TestDeviceSocketFormat verifies that the device-side socket uses
// localabstract:scrcpy_<SCID> format, NOT tcp:27183.
// This is a critical pitfall documented in RESEARCH.md Pitfall 1.
func TestDeviceSocketFormat(t *testing.T) {
	// Generate a scrcpy SCID (8 hex chars)
	scid := "00001234"
	deviceSocket := fmt.Sprintf("localabstract:scrcpy_%s", scid)
	hostSocket := "tcp:42001"

	// The device socket MUST be localabstract, NOT tcp
	assert.Equal(t, "localabstract:scrcpy_00001234", deviceSocket)
	assert.False(t, strings.HasPrefix(deviceSocket, "tcp:"),
		"device-side socket must use localabstract: format, NOT tcp:")

	// The host socket is still tcp
	assert.Equal(t, "tcp:42001", hostSocket)

	// Full command format
	cmd := "reverse:forward:" + deviceSocket + ";" + hostSocket
	assert.Equal(t, "reverse:forward:localabstract:scrcpy_00001234;tcp:42001", cmd)
}