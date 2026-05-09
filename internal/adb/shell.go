package adb

// Phase 3 streaming ADB primitives — extends *HostServices in host_services.go.
//
// Verified prife/goadb v0.4.x API surface (assumption A1, A2 — RESEARCH.md):
//
//   *Device.NewSyncConn() (*wire.SyncConn, error)
//   *wire.SyncConn.Send(path, mode, mtime) (*SyncFileWriter, error)
//       SyncFileWriter has Write([]byte) (n int, err error) -> satisfies io.Writer.
//       Use io.Copy(syncFile, src). CopyDone() commits and reads ack.
//   *wire.SyncConn.Recv(path) (*SyncFileReader, error)
//       SyncFileReader has Read([]byte) (n int, err error) -> satisfies io.Reader.
//       Use io.Copy(dst, syncFile). EOF on DONE.
//   *Device.RunShellCommand(v2 bool, cmd, args...) (net.Conn, error)
//       Returns the merged stdout+stderr stream when v2=false. When v2=true
//       the connection still delivers the AOSP shell-v2 framed protocol —
//       prife/goadb does NOT split it for us, so we parse packet headers
//       ourselves: 1 byte id (0=stdin, 1=stdout, 2=stderr, 3=exit) +
//       4 byte little-endian length + payload.
//
// A1 RESOLVED: SyncFileWriter / SyncFileReader satisfy io.Writer / io.Reader,
// so SyncPushReader / SyncPullWriter use io.Copy directly. No hand-rolled
// SEND/DATA/DONE wire frames are needed.
//
// A2 RESOLVED: prife/goadb does not expose split-stream shell-v2; we own the
// demux locally (demuxShellV2). Same logic regardless of which goadb version
// ships, because the on-the-wire format is fixed by AOSP.
//
// Caller contract (T-03-01-04): SyncPushReader does NOT enforce a size cap.
// The caller (handlers_apk.go via cfg.APK.MaxBytes, handlers_files.go via
// http.MaxBytesReader) MUST wrap src to bound the upload, otherwise an
// adversary controlling the source can fill device storage.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"

	goadb "github.com/prife/goadb"
)

// shell-v2 packet IDs per AOSP packages/modules/adb SERVICES.TXT.
const (
	shellV2IDStdin  byte = 0
	shellV2IDStdout byte = 1
	shellV2IDStderr byte = 2
	shellV2IDExit   byte = 3
)

// ShellRunRaw executes a shell command on the device and returns the full
// raw stdout as bytes (no TrimSpace, no string conversion). Use this for
// binary outputs like `screencap -p` PNG.
//
// Bounded by ctx. Distinct from RunShellCommand which returns string.
func (h *HostServices) ShellRunRaw(ctx context.Context, serial, cmd string) ([]byte, error) {
	shellCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	conn, err := device.RunShellCommand(true, cmd)
	if err != nil {
		return nil, fmt.Errorf("shell raw: %w", err)
	}
	defer conn.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// shell:v2 wraps output in framed packets — demux to keep stdout pure.
		stdout := newGrowBuffer()
		stderr := io.Discard
		_, err := demuxShellV2RawIO(conn, stdout, stderr)
		ch <- result{out: stdout.Bytes(), err: err}
	}()

	select {
	case <-shellCtx.Done():
		// Closing the conn unblocks the goroutine via Read error.
		_ = conn.Close()
		return nil, fmt.Errorf("shell raw: %w", shellCtx.Err())
	case r := <-ch:
		if r.err != nil && r.err != io.EOF {
			return nil, fmt.Errorf("shell raw: %w", r.err)
		}
		return r.out, nil
	}
}

// SyncPushReader streams src to dest on the device using the ADB sync protocol.
// Never buffers the whole body. Honors ctx cancellation by closing the sync
// connection mid-stream.
//
// SECURITY: caller must size-cap src before passing it in (T-03-01-04).
func (h *HostServices) SyncPushReader(ctx context.Context, serial, dest string, src io.Reader, mode os.FileMode) error {
	pushCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	syncConn, err := device.NewSyncConn()
	if err != nil {
		return fmt.Errorf("sync push connect: %w", err)
	}
	defer syncConn.Close()

	syncFile, err := syncConn.Send(dest, mode, time.Now())
	if err != nil {
		return fmt.Errorf("sync push send init: %w", err)
	}

	// Mid-stream cancel: closing syncConn from a watcher unblocks io.Copy.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-pushCtx.Done():
			_ = syncConn.Close()
		case <-done:
		}
	}()

	if _, err := io.Copy(syncFile, src); err != nil {
		return fmt.Errorf("sync push write: %w", err)
	}
	if err := syncFile.CopyDone(); err != nil {
		return fmt.Errorf("sync push done: %w", err)
	}

	if pushCtx.Err() != nil {
		return fmt.Errorf("sync push: %w", pushCtx.Err())
	}
	return nil
}

// SyncPullWriter streams the remote file at src to dst using the ADB sync
// protocol. Honors ctx cancellation.
func (h *HostServices) SyncPullWriter(ctx context.Context, serial, src string, dst io.Writer) error {
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	syncConn, err := device.NewSyncConn()
	if err != nil {
		return fmt.Errorf("sync pull connect: %w", err)
	}
	defer syncConn.Close()

	syncFile, err := syncConn.Recv(src)
	if err != nil {
		return fmt.Errorf("sync pull recv init: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-pullCtx.Done():
			_ = syncConn.Close()
		case <-done:
		}
	}()

	if _, err := io.Copy(dst, syncFile); err != nil && err != io.EOF {
		return fmt.Errorf("sync pull read: %w", err)
	}

	if pullCtx.Err() != nil {
		return fmt.Errorf("sync pull: %w", pullCtx.Err())
	}
	return nil
}

// ShellV2Stream opens a shell-v2 connection and demultiplexes the stream into
// separate stdout / stderr readers and an exit-code channel.
//
// stdout and stderr are pipe readers that the caller MUST drain (or close) to
// avoid back-pressuring the device side (Pitfall in RunDaemonCommand).
// The exit channel delivers exactly one int when the command exits cleanly,
// or -1 if the connection closes before an exit packet is received.
//
// Cancelling ctx closes the underlying connection, which unblocks the
// background demuxer and propagates to stdout/stderr as Read errors.
func (h *HostServices) ShellV2Stream(ctx context.Context, serial, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error) {
	device := h.goadb.Device(goadb.DeviceWithSerial(serial))

	conn, err := device.RunShellCommand(true, cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("shell-v2 stream: %w", err)
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	exitCh := make(chan int, 1)

	// Cancel propagation: ctx done -> close conn -> demux Read errors -> close pipes.
	demuxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-demuxDone:
		}
	}()

	go func() {
		defer close(demuxDone)
		defer conn.Close()
		code, derr := demuxShellV2RawIO(conn, stdoutW, stderrW)
		// Closing the writer with an error makes Read on the reader return that error.
		if derr != nil && derr != io.EOF {
			stdoutW.CloseWithError(derr)
			stderrW.CloseWithError(derr)
		} else {
			stdoutW.Close()
			stderrW.Close()
		}
		exitCh <- code
		close(exitCh)
	}()

	return stdoutR, stderrR, exitCh, nil
}

// demuxShellV2 reads framed shell-v2 packets from r and demultiplexes them
// into stdout (id=1) and stderr (id=2) writers. Returns the exit code from
// the id=3 packet, or -1 on EOF without exit. Unknown IDs are skipped.
//
// Wire format (per AOSP SERVICES.TXT):
//   [1 byte id][4 byte LE length][length bytes payload]
//
// Closes the input reader on return.
func demuxShellV2(rc io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	defer rc.Close()
	return demuxShellV2RawIO(rc, stdout, stderr)
}

// demuxShellV2RawIO is the same as demuxShellV2 but does not close the input.
// Used internally where the caller owns the conn lifecycle.
func demuxShellV2RawIO(r io.Reader, stdout, stderr io.Writer) (int, error) {
	var hdr [5]byte
	for {
		_, err := io.ReadFull(r, hdr[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return -1, nil
		}
		if err != nil {
			return -1, err
		}

		id := hdr[0]
		ln := binary.LittleEndian.Uint32(hdr[1:5])

		if ln > 1<<24 {
			// Sanity cap: 16 MiB per packet. AOSP packets are bounded well below this.
			return -1, fmt.Errorf("shell-v2 packet too large: %d", ln)
		}

		if id == shellV2IDExit {
			// Exit payload is one byte: the exit code.
			payload := make([]byte, ln)
			if _, err := io.ReadFull(r, payload); err != nil {
				return -1, err
			}
			if len(payload) == 0 {
				return 0, nil
			}
			return int(payload[0]), nil
		}

		if ln == 0 {
			continue
		}

		// Stream payload into the matching writer (or discard for unknown).
		dst := io.Discard
		switch id {
		case shellV2IDStdout:
			dst = stdout
		case shellV2IDStderr:
			dst = stderr
		case shellV2IDStdin:
			// Server should never echo stdin; ignore defensively.
		}

		if _, err := io.CopyN(dst, r, int64(ln)); err != nil {
			return -1, err
		}
	}
}

// growBuffer is a minimal bytes.Buffer-shaped wrapper used internally so the
// callers in this file don't import bytes (and to make the signature explicit).
type growBuffer struct {
	buf []byte
}

func newGrowBuffer() *growBuffer            { return &growBuffer{} }
func (b *growBuffer) Write(p []byte) (int, error) { b.buf = append(b.buf, p...); return len(p), nil }
func (b *growBuffer) Bytes() []byte         { return b.buf }
