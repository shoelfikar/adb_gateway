package adb

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	goadb "github.com/prife/goadb"
)

// DeviceInfo holds information about a connected Android device.
type DeviceInfo struct {
	Serial string
	Model  string
	State  string
}

// DeviceEvent represents a device state change from the device watcher.
type DeviceEvent struct {
	Serial string
	State  string
}

// HostServices wraps prife/goadb for well-supported ADB operations:
// ListDevices, NewDeviceWatcher, ServerVersion, PushFile, RunShellCommand.
// It also holds a reference to the raw Client for operations that goadb
// doesn't support (reverse:forward, etc.).
type HostServices struct {
	client *Client
	goadb  *goadb.Adb
}

// NewHostServices creates a HostServices that delegates to prife/goadb for
// host-level operations. The client parameter provides the ADB server address.
func NewHostServices(client *Client) (*HostServices, error) {
	// Parse host and port from client address (e.g. "localhost:5037")
	host, portStr, err := net.SplitHostPort(client.Addr())
	if err != nil {
		return nil, fmt.Errorf("parse adb address %q: %w", client.Addr(), err)
	}
	var port int
	if portStr != "" {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("parse adb port %q: %w", portStr, err)
		}
	}

	adb, err := goadb.NewWithConfig(goadb.ServerConfig{
		Host: host,
		Port: port,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize goadb: %w", err)
	}
	return &HostServices{
		client: client,
		goadb:  adb,
	}, nil
}

// ListDevices returns the list of currently connected devices.
// It wraps goadb.ListDevices and converts to our DeviceInfo type.
func (h *HostServices) ListDevices(ctx context.Context) ([]DeviceInfo, error) {
	// goadb's ListDevices doesn't accept a context, but our caller should
	// have already set a timeout on the context if needed.
	// For operations without context support in goadb, we run in a goroutine
	// and respect context cancellation.
	type result struct {
		devices []*goadb.DeviceInfo
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		devices, err := h.goadb.ListDevices()
		ch <- result{devices: devices, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("list devices: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("list devices: %w", r.err)
		}
		result := make([]DeviceInfo, 0, len(r.devices))
		for _, d := range r.devices {
			result = append(result, DeviceInfo{
				Serial: d.Serial,
				Model:  d.Model,
				State:  string(d.State),
			})
		}
		return result, nil
	}
}

// NewDeviceWatcher returns a channel that emits device state change events.
// The channel is closed when the context is cancelled.
// It wraps goadb.NewDeviceWatcher and bridges to our DeviceEvent type.
func (h *HostServices) NewDeviceWatcher(ctx context.Context) (<-chan DeviceEvent, error) {
	watcher := h.goadb.NewDeviceWatcher()
	ch := make(chan DeviceEvent, 16)

	go func() {
		defer close(ch)
		defer watcher.Shutdown()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.C():
				if !ok {
					return
				}
				select {
				case ch <- DeviceEvent{Serial: event.Serial, State: event.NewState.String()}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// ServerVersion queries the ADB server for its version number (e.g. "0024" -> "36").
// Uses the raw protocol via our Client, not goadb, because goadb's ServerVersion
// returns an int and we want the raw string.
func (h *HostServices) ServerVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := h.client.dial(ctx)
	if err != nil {
		return "", fmt.Errorf("server version: %w", err)
	}
	defer conn.Close()

	if err := sendMessage(conn, "host:version"); err != nil {
		return "", fmt.Errorf("send host:version: %w", err)
	}
	if _, err := readResponse(conn); err != nil {
		return "", fmt.Errorf("host:version response: %w", err)
	}

	version, err := readStringResponse(conn)
	if err != nil {
		return "", fmt.Errorf("read version: %w", err)
	}
	return version, nil
}

// PushFile pushes data to a file on the device at destPath.
// It uses goadb's sync protocol to push the file content from an in-memory byte slice.
func (h *HostServices) PushFile(ctx context.Context, serial string, data []byte, destPath string) error {
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	// Use goadb's lower-level sync API to push from in-memory data.
	syncConn, err := device.NewSyncConn()
	if err != nil {
		return fmt.Errorf("push file sync connect: %w", err)
	}
	defer syncConn.Close()

	// Send opens the file on the device for writing.
	syncFile, err := syncConn.Send(destPath, 0644, time.Now())
	if err != nil {
		return fmt.Errorf("push file send init: %w", err)
	}

	// Write the data. SyncFileWriter.Write handles chunking internally.
	if _, err := syncFile.Write(data); err != nil {
		return fmt.Errorf("push file write: %w", err)
	}

	// CopyDone sends the DONE marker and reads the status response.
	if err := syncFile.CopyDone(); err != nil {
		return fmt.Errorf("push file done: %w", err)
	}

	// Check for context cancellation.
	select {
	case <-pushCtx.Done():
		return fmt.Errorf("push file: %w", pushCtx.Err())
	default:
		return nil
	}
}

// RunShellCommand executes a shell command on the device and returns the output.
// It uses goadb's shell:v2 protocol for reliability on modern Android versions.
func (h *HostServices) RunShellCommand(ctx context.Context, serial string, cmd string) (string, error) {
	shellCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	// Use shell:v2 for better handling on Android 14+
	conn, err := device.RunShellCommand(true, cmd)
	if err != nil {
		return "", fmt.Errorf("shell command: %w", err)
	}
	defer conn.Close()

	// Read output with context cancellation.
	type result struct {
		output []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := io.ReadAll(conn)
		ch <- result{output: out, err: err}
	}()

	select {
	case <-shellCtx.Done():
		return "", fmt.Errorf("shell command: %w", shellCtx.Err())
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("shell command: %w", r.err)
		}
		return string(r.output), nil
	}
}