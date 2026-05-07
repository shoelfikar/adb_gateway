// Package adbtest provides a fake ADB server for testing the ADB client
// without requiring a real ADB daemon or Android device.
package adbtest

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// HandlerFunc is called when the fake ADB server receives a matching command.
// The handler receives the connection (for reading/writing responses) and the
// full ADB message (e.g. "host:version", "reverse:forward:localabstract:scrcpy_00001234;tcp:42001").
type HandlerFunc func(conn net.Conn, msg string)

// FakeADB is a test double for the ADB server that listens on a random port
// and responds to ADB wire protocol messages based on configurable handlers.
type FakeADB struct {
	ln      net.Listener
	addr    string
	handlers map[string]HandlerFunc
	mu      sync.RWMutex
	t       *testing.T

	// Default responses for common commands.
	DeviceList string // Response for host:devices (default: empty)
	Version   string // Response for host:version (default: "0024")
}

// Start creates and starts a FakeADB server on a random port.
// Returns the fake, its address, and a cleanup function.
// The fake runs for the duration of the test and is cleaned up automatically.
func Start(t *testing.T) (*FakeADB, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	f := &FakeADB{
		ln:        ln,
		addr:      ln.Addr().String(),
		handlers:  make(map[string]HandlerFunc),
		t:         t,
		DeviceList: "",
		Version:   "0024",
	}

	// Set default handlers
	f.SetHandler("host:version", f.HandleVersion)
	f.SetHandler("host:devices", f.HandleDevices)
	f.SetHandler("host:track-devices", f.HandleTrackDevices)

	go f.serve()

	cleanup := func() {
		ln.Close()
	}

	return f, cleanup
}

// Addr returns the "host:port" address the fake ADB server is listening on.
func (f *FakeADB) Addr() string {
	return f.addr
}

// SetHandler registers a custom handler for commands that start with the given prefix.
// The prefix is matched against the beginning of the received ADB message.
// For example, SetHandler("reverse:forward", ...) will match "reverse:forward:localabstract:scrcpy_00001234;tcp:42001".
func (f *FakeADB) SetHandler(cmdPrefix string, handler HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[cmdPrefix] = handler
}

// serve accepts connections and dispatches them to handlers.
func (f *FakeADB) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go f.handleConn(conn)
	}
}

// handleConn reads ADB messages from a connection and dispatches them to handlers.
func (f *FakeADB) handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		// Read ADB message: 4-byte hex length + payload
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return // connection closed
		}
		msgLen, err := strconv.ParseInt(string(lenBuf), 16, 32)
		if err != nil {
			return
		}

		payload := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		msg := string(payload)

		// Find matching handler
		handler := f.findHandler(msg)
		if handler != nil {
			handler(conn, msg)
		} else {
			// No handler found: respond with FAIL
			WriteFAIL(conn, fmt.Sprintf("unknown command: %s", msg))
		}
	}
}

// findHandler returns the handler for the given message, matching by prefix.
func (f *FakeADB) findHandler(msg string) HandlerFunc {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Try exact match first, then prefix match (longest prefix first)
	best := ""
	var handler HandlerFunc
	for prefix, h := range f.handlers {
		if msg == prefix || (len(msg) > len(prefix) && msg[:len(prefix)+1] == prefix+":") || (len(msg) > len(prefix) && msg[:len(prefix)] == prefix && (len(msg) == len(prefix) || msg[len(prefix)] == ':')) {
			if len(prefix) > len(best) {
				best = prefix
				handler = h
			}
		}
	}

	// Also try simpler prefix matching: msg starts with prefix
	for prefix, h := range f.handlers {
		if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
			if len(prefix) > len(best) {
				best = prefix
				handler = h
			}
		}
	}

	_ = best
	return handler
}

// HandleVersion responds to host:version with the configured version string.
func (f *FakeADB) HandleVersion(conn net.Conn, msg string) {
	WriteOKAY(conn)
	WriteString(conn, f.Version)
}

// HandleDevices responds to host:devices with the configured device list.
func (f *FakeADB) HandleDevices(conn net.Conn, msg string) {
	WriteOKAY(conn)
	WriteString(conn, f.DeviceList)
}

// HandleTrackDevices responds to host:track-devices by sending the device list
// and keeping the connection open (simulating long-poll).
func (f *FakeADB) HandleTrackDevices(conn net.Conn, msg string) {
	WriteOKAY(conn)
	WriteString(conn, f.DeviceList)
	// Keep connection open (do not close; the real ADB server keeps it open)
	select {} // block forever
}

// WriteOKAY writes the OKAY status to the connection.
func WriteOKAY(conn net.Conn) {
	conn.Write([]byte("OKAY"))
}

// WriteFAIL writes the FAIL status + error message to the connection.
func WriteFAIL(conn net.Conn, errMsg string) {
	conn.Write([]byte("FAIL"))
	payload := fmt.Sprintf("%04x%s", len(errMsg), errMsg)
	conn.Write([]byte(payload))
}

// WriteString writes a string response (4-byte hex length + payload) to the connection.
func WriteString(conn net.Conn, s string) {
	payload := fmt.Sprintf("%04x%s", len(s), s)
	conn.Write([]byte(payload))
}