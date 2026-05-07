package adb

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// ReverseMapping holds an open connection that keeps a reverse forward alive.
// Closing the connection removes the reverse mapping from adbd.
// CRITICAL: The connection MUST stay open for the mapping to persist.
// Do NOT defer conn.Close() after ReverseForward succeeds.
type ReverseMapping struct {
	conn       net.Conn // kept alive for the mapping duration
	DeviceSpec string   // e.g. "localabstract:scrcpy_00001234"
	HostSpec   string   // e.g. "tcp:42001"
}

// Close closes the persistent connection to the ADB server, which removes
// the reverse forward mapping. This is the only way to remove a reverse
// mapping created by ReverseForward.
func (rm *ReverseMapping) Close() error {
	if rm.conn != nil {
		return rm.conn.Close()
	}
	return nil
}

// ForwardEntry represents a single reverse forward mapping returned by
// reverse:list-forward.
type ForwardEntry struct {
	Local  string // device-side socket spec (e.g. "localabstract:scrcpy_00001234")
	Remote string // host-side socket spec (e.g. "tcp:42001")
}

// ReverseForward creates a reverse port forwarding from the device to the host.
// It dials the ADB server, binds to the device transport, and sends the
// reverse:forward command.
//
// The returned ReverseMapping holds an open connection to the ADB server.
// The reverse mapping stays active as long as this connection remains open.
// Call ReverseMapping.Close() to remove the mapping.
//
// CRITICAL: The deviceSpec uses localabstract Unix domain sockets for scrcpy,
// NOT tcp ports. The correct format is "localabstract:scrcpy_<SCID>".
// The separator between deviceSpec and hostSpec is a SEMICOLON, not a colon.
func (c *Client) ReverseForward(ctx context.Context, serial, deviceSpec, hostSpec string) (*ReverseMapping, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("reverse forward dial: %w", err)
	}

	// Set deadline for the handshake, then clear it after success
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Step 1: Bind to device transport
	if err := sendMessage(conn, "host:transport:"+serial); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reverse forward transport: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reverse forward transport response: %w", err)
	}

	// Step 2: Send reverse:forward command
	// CRITICAL: Semicolon separator between device-socket and host-socket specs.
	// NOT a colon. Verified against AOSP SERVICES.TXT.
	cmd := "reverse:forward:" + deviceSpec + ";" + hostSpec
	if err := sendMessage(conn, cmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reverse forward send: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reverse forward response: %w", err)
	}

	// Clear the deadline so the connection stays alive indefinitely
	conn.SetDeadline(time.Time{})

	return &ReverseMapping{
		conn:       conn,
		DeviceSpec: deviceSpec,
		HostSpec:   hostSpec,
	}, nil
}

// ReverseListForward returns the list of reverse forward mappings for a device.
// Each line in the response is "<serial> <local> <remote>\n" where local is the
// device-side socket spec and remote is the host-side socket spec.
func (c *Client) ReverseListForward(ctx context.Context, serial string) ([]ForwardEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("reverse list forward dial: %w", err)
	}
	defer conn.Close()

	// Bind to device transport
	if err := sendMessage(conn, "host:transport:"+serial); err != nil {
		return nil, fmt.Errorf("reverse list transport: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return nil, fmt.Errorf("reverse list transport response: %w", err)
	}

	// Send reverse:list-forward
	if err := sendMessage(conn, "reverse:list-forward"); err != nil {
		return nil, fmt.Errorf("reverse list send: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return nil, fmt.Errorf("reverse list response: %w", err)
	}

	// Read listing: 4-byte hex length + text payload
	data, err := readStringResponse(conn)
	if err != nil {
		return nil, fmt.Errorf("reverse list read: %w", err)
	}

	// Parse: each line is "<serial> <local> <remote>\n" (space-separated)
	var entries []ForwardEntry
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
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
	return entries, nil
}

// ReverseRemove removes a specific reverse forward mapping from the device.
// This is a one-shot command: it dials the ADB server, sends the killforward
// command, and closes the connection.
func (c *Client) ReverseRemove(ctx context.Context, serial, deviceSpec string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("reverse remove dial: %w", err)
	}
	defer conn.Close()

	// Bind to device transport
	if err := sendMessage(conn, "host:transport:"+serial); err != nil {
		return fmt.Errorf("reverse remove transport: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return fmt.Errorf("reverse remove transport response: %w", err)
	}

	// Send reverse:killforward:<deviceSpec>
	cmd := "reverse:killforward:" + deviceSpec
	if err := sendMessage(conn, cmd); err != nil {
		return fmt.Errorf("reverse remove send: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return fmt.Errorf("reverse remove response: %w", err)
	}

	return nil
}

// ReverseKillforwardAll removes all reverse forward mappings from the device.
// This is a one-shot command.
func (c *Client) ReverseKillforwardAll(ctx context.Context, serial string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("reverse killforward-all dial: %w", err)
	}
	defer conn.Close()

	// Bind to device transport
	if err := sendMessage(conn, "host:transport:"+serial); err != nil {
		return fmt.Errorf("reverse killforward-all transport: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return fmt.Errorf("reverse killforward-all transport response: %w", err)
	}

	// Send reverse:killforward-all
	if err := sendMessage(conn, "reverse:killforward-all"); err != nil {
		return fmt.Errorf("reverse killforward-all send: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return fmt.Errorf("reverse killforward-all response: %w", err)
	}

	return nil
}