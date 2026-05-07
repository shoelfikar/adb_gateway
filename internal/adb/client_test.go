package adb

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pelni/adb-gateway/internal/adb/adbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	c := NewClient("localhost:5037")
	assert.Equal(t, "localhost:5037", c.Addr())
	assert.Equal(t, 10*time.Second, c.timeout)
}

func TestDialSuccess(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	c := NewClient(fake.Addr())
	conn, err := c.dial(context.Background())
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestDialTimeout(t *testing.T) {
	c := NewClient("localhost:1") // port 1 should refuse
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	conn, err := c.dial(ctx)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func TestSendMessageReadResponse(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	c := NewClient(fake.Addr())
	conn, err := c.dial(context.Background())
	require.NoError(t, err)
	defer conn.Close()

	// Send host:version and read OKAY
	err = sendMessage(conn, "host:version")
	require.NoError(t, err)

	status, err := readResponse(conn)
	require.NoError(t, err)
	assert.Equal(t, "OKAY", status)

	// Read the version string payload
	version, err := readStringResponse(conn)
	require.NoError(t, err)
	assert.Equal(t, "0024", version)
}

func TestSendMessageReadResponseFAIL(t *testing.T) {
	fake, cleanup := adbtest.Start(t)
	defer cleanup()

	// Override handler to return FAIL for an unknown command
	fake.SetHandler("unknown:cmd", func(conn net.Conn, msg string) {
		adbtest.WriteFAIL(conn, "command not recognized")
	})

	c := NewClient(fake.Addr())
	conn, err := c.dial(context.Background())
	require.NoError(t, err)
	defer conn.Close()

	err = sendMessage(conn, "unknown:cmd:test")
	require.NoError(t, err)

	status, err := readResponse(conn)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "FAIL")
	assert.Contains(t, err.Error(), "command not recognized")
	assert.NotEqual(t, "OKAY", status)
}

func TestReadStringResponse(t *testing.T) {
	// Test readStringResponse directly by writing to a pipe
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write a string response: 4-byte hex length + payload
	go func() {
		msg := "hello world"
		payload := fmt.Sprintf("%04x%s", len(msg), msg)
		server.Write([]byte(payload))
	}()

	result, err := readStringResponse(client)
	require.NoError(t, err)
	assert.Equal(t, "hello world", result)
}