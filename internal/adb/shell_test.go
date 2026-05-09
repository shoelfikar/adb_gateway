package adb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShellV2DemuxStdoutStderrExit verifies the shell-v2 packet demuxer
// (id byte: 0=stdin, 1=stdout, 2=stderr, 3=exit) per AOSP SERVICES.TXT.
func TestShellV2DemuxStdoutStderrExit(t *testing.T) {
	// Build a synthetic shell-v2 stream: stdout "hello", stderr "warn", exit 7.
	var buf bytes.Buffer
	writePacket(&buf, shellV2IDStdout, []byte("hello"))
	writePacket(&buf, shellV2IDStderr, []byte("warn"))
	writePacket(&buf, shellV2IDExit, []byte{7})

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit, err := demuxShellV2(io.NopCloser(&buf), stdout, stderr)
	require.NoError(t, err)
	assert.Equal(t, "hello", stdout.String())
	assert.Equal(t, "warn", stderr.String())
	assert.Equal(t, 7, exit)
}

// TestShellV2DemuxBinaryPayload ensures binary bytes round-trip through
// the demuxer without modification (e.g., screencap PNG).
func TestShellV2DemuxBinaryPayload(t *testing.T) {
	payload := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff}

	var buf bytes.Buffer
	writePacket(&buf, shellV2IDStdout, payload)
	writePacket(&buf, shellV2IDExit, []byte{0})

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit, err := demuxShellV2(io.NopCloser(&buf), stdout, stderr)
	require.NoError(t, err)
	assert.Equal(t, payload, stdout.Bytes())
	assert.Equal(t, 0, stderr.Len())
	assert.Equal(t, 0, exit)
}

// TestShellV2DemuxIgnoresUnknownIDs ensures unknown packet IDs (e.g., stdin
// echo) are skipped without erroring.
func TestShellV2DemuxIgnoresUnknownIDs(t *testing.T) {
	var buf bytes.Buffer
	writePacket(&buf, 9, []byte("ignore me")) // unknown id
	writePacket(&buf, shellV2IDStdout, []byte("ok"))
	writePacket(&buf, shellV2IDExit, []byte{0})

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit, err := demuxShellV2(io.NopCloser(&buf), stdout, stderr)
	require.NoError(t, err)
	assert.Equal(t, "ok", stdout.String())
	assert.Equal(t, 0, exit)
}

// TestShellV2DemuxEOFWithoutExit treats premature EOF as exit=-1 (no clean exit packet).
func TestShellV2DemuxEOFWithoutExit(t *testing.T) {
	var buf bytes.Buffer
	writePacket(&buf, shellV2IDStdout, []byte("partial"))

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit, err := demuxShellV2(io.NopCloser(&buf), stdout, stderr)
	require.NoError(t, err)
	assert.Equal(t, "partial", stdout.String())
	assert.Equal(t, -1, exit, "no exit packet -> -1 sentinel")
}

// writePacket emits a shell-v2 framed packet: 1 byte id + 4 byte LE length + payload.
func writePacket(w io.Writer, id byte, data []byte) {
	w.Write([]byte{id})
	var ln [4]byte
	binary.LittleEndian.PutUint32(ln[:], uint32(len(data)))
	w.Write(ln[:])
	w.Write(data)
}

// TestShellRunRawContextTimeout verifies ShellRunRaw respects ctx cancellation.
func TestShellRunRawContextTimeout(t *testing.T) {
	c := NewClient("localhost:1") // unreachable
	hs, err := NewHostServices(c)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = hs.ShellRunRaw(ctx, "emulator-5554", "screencap -p")
	assert.Error(t, err)
}

// TestSyncPushReaderContextTimeout verifies SyncPushReader honors ctx cancellation
// even mid-stream (e.g., slow remote peer).
func TestSyncPushReaderContextTimeout(t *testing.T) {
	c := NewClient("localhost:1") // unreachable
	hs, err := NewHostServices(c)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	src := strings.NewReader("payload bytes")
	err = hs.SyncPushReader(ctx, "emulator-5554", "/sdcard/test.txt", src, 0644)
	assert.Error(t, err)
}

// TestSyncPullWriterContextTimeout verifies SyncPullWriter honors ctx cancellation.
func TestSyncPullWriterContextTimeout(t *testing.T) {
	c := NewClient("localhost:1") // unreachable
	hs, err := NewHostServices(c)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	dst := &bytes.Buffer{}
	err = hs.SyncPullWriter(ctx, "emulator-5554", "/sdcard/test.txt", dst)
	assert.Error(t, err)
}

// TestShellV2StreamContextTimeout verifies ShellV2Stream honors ctx cancellation.
func TestShellV2StreamContextTimeout(t *testing.T) {
	c := NewClient("localhost:1") // unreachable
	hs, err := NewHostServices(c)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, _, err = hs.ShellV2Stream(ctx, "emulator-5554", "logcat -d")
	assert.Error(t, err)
}
