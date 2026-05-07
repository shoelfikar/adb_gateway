// Package adb provides an ADB client that communicates with the ADB server
// over the smart-sockets wire protocol, plus an in-house reverse:forward helper
// (no Go ADB library implements it).
//
// Well-supported operations (host:devices, track-devices, shell, push) are
// delegated to prife/goadb. The raw protocol codec (sendMessage/readResponse)
// is shared by both the host services wrappers and the reverse helper.
package adb

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Client communicates with a local ADB server at the given address (e.g. "localhost:5037").
type Client struct {
	addr    string
	timeout time.Duration
}

// NewClient creates an ADB client that connects to the given address.
func NewClient(addr string) *Client {
	return &Client{
		addr:    addr,
		timeout: 10 * time.Second,
	}
}

// Addr returns the ADB server address this client connects to.
func (c *Client) Addr() string {
	return c.addr
}

// dial opens a TCP connection to the ADB server, bounded by ctx.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("dial adb server %s: %w", c.addr, err)
	}
	return conn, nil
}

// sendMessage writes an ADB smart-sockets message: 4-byte hex length prefix + payload.
// This is the shared codec used by all raw ADB protocol operations.
func sendMessage(conn net.Conn, msg string) error {
	payload := fmt.Sprintf("%04x%s", len(msg), msg)
	_, err := conn.Write([]byte(payload))
	if err != nil {
		return fmt.Errorf("send message %q: %w", msg, err)
	}
	return nil
}

// readResponse reads a 4-byte status (OKAY or FAIL) from the ADB server.
// On OKAY, returns "OKAY" and nil error.
// On FAIL, reads the error message and returns it as an error.
func readResponse(conn net.Conn) (string, error) {
	status := make([]byte, 4)
	if _, err := io.ReadFull(conn, status); err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}
	if string(status) == "OKAY" {
		return "OKAY", nil
	}
	if string(status) == "FAIL" {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", fmt.Errorf("read fail length: %w", err)
		}
		msgLen, err := strconv.ParseInt(string(lenBuf), 16, 32)
		if err != nil {
			return "", fmt.Errorf("parse fail length %q: %w", string(lenBuf), err)
		}
		errMsg := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, errMsg); err != nil {
			return "", fmt.Errorf("read fail message: %w", err)
		}
		return "", fmt.Errorf("ADB FAIL: %s", string(errMsg))
	}
	return "", fmt.Errorf("unexpected ADB response: %q", string(status))
}

// readStringResponse reads a 4-byte hex length prefix followed by the string payload.
// Used by commands like host:version and reverse:list-forward that return data after OKAY.
func readStringResponse(conn net.Conn) (string, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return "", fmt.Errorf("read string length: %w", err)
	}
	msgLen, err := strconv.ParseInt(string(lenBuf), 16, 32)
	if err != nil {
		return "", fmt.Errorf("parse string length %q: %w", string(lenBuf), err)
	}
	data := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return "", fmt.Errorf("read string payload: %w", err)
	}
	return string(data), nil
}