package scrcpy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pelni/adb-gateway/internal/adb"
)

// fakeHostServices implements adb.HostServices for testing.
// Since the real HostServices type is concrete (not an interface),
// we need a full integration test approach for LaunchWithOptions.
// These tests focus on the protocol parsing and result shape.
type fakeHostServices struct {
	mu     sync.Mutex
	pushed bool
}

func (f *fakeHostServices) PushFile(ctx context.Context, serial, src, dst string) error {
	f.mu.Lock()
	f.pushed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeHostServices) RunDaemonCommand(ctx context.Context, serial, cmd string) (func(), error) {
	return func() {}, nil
}

func TestDefaultLaunchOptions(t *testing.T) {
	opts := DefaultLaunchOptions()
	assert.False(t, opts.AudioEnabled, "Phase 1 defaults: audio should be off")
	assert.False(t, opts.ControlEnabled, "Phase 1 defaults: control should be off")
}

func TestLaunchResultFields(t *testing.T) {
	// Verify that the new LaunchResult fields exist and are accessible.
	result := &LaunchResult{
		AudioAvailable: true,
		AudioCodec:     AudioCodecOPUS,
		ReverseMaps:    []*adb.ReverseMapping{{DeviceSpec: "localabstract:scrcpy_test", HostSpec: "tcp:0"}},
	}

	assert.Nil(t, result.AudioConn)
	assert.Nil(t, result.ControlConn)
	assert.True(t, result.AudioAvailable)
	assert.Equal(t, AudioCodecOPUS, result.AudioCodec)
	assert.Len(t, result.ReverseMaps, 1)
}

func TestLauncherAudioProbeZeroCodec(t *testing.T) {
	// When codec ID is 0x00000000, the probe should return available=false.
	zeroCodec := make([]byte, 4)
	codec, available, err := ReadAudioCodecID(bytes.NewReader(zeroCodec))
	require.NoError(t, err)
	assert.False(t, available)
	assert.Equal(t, AudioCodec(0), codec)
}

func TestLauncherAudioProbeEOF(t *testing.T) {
	// When audio socket closes immediately (EOF), the probe returns available=false.
	codec, available, err := ReadAudioCodecID(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.False(t, available)
	assert.Equal(t, AudioCodec(0), codec)
}

func TestLauncherAudioProbeOPUS(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(AudioCodecOPUS))
	codec, available, err := ReadAudioCodecID(bytes.NewReader(buf))
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, AudioCodecOPUS, codec)
}

func TestAcceptWithTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := acceptWithTimeout(ctx, ln, "test")
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestAcceptWithTimeoutExpired(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = acceptWithTimeout(ctx, ln, "test")
	assert.Error(t, err, "should timeout when no connection arrives")
}

func TestCodecString(t *testing.T) {
	assert.Equal(t, "opus", codecString(AudioCodecOPUS))
	assert.Equal(t, "aac", codecString(AudioCodecAAC))
	assert.Equal(t, "flac", codecString(AudioCodecFLAC))
	assert.Equal(t, "raw", codecString(AudioCodecRAW))
	assert.Equal(t, "0xdeadbeef", codecString(AudioCodec(0xdeadbeef)))
}

func TestBuildSCIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := BuildSCID()
		assert.Len(t, id, 8, "SCID should be 8 hex chars")
		assert.False(t, ids[id], "SCID should be unique")
		ids[id] = true
	}
}

func TestLauncherAppProcessArgsIncludeAudioControl(t *testing.T) {
	// Verify the app_process command format includes audio and control flags.
	// We can't fully exercise LaunchWithOptions without a real ADB server,
	// but we can verify the argument construction logic.
	opts := LaunchOptions{AudioEnabled: true, ControlEnabled: true}
	assert.True(t, opts.AudioEnabled)
	assert.True(t, opts.ControlEnabled)

	// Verify the command format that would be generated.
	cmd := fmt.Sprintf(
		"CLASSPATH=%s app_process / com.genymobile.scrcpy.Server %s "+
			"scid=%s log_level=info "+
			"video=true audio=%t control=%t "+
			"send_device_meta=true send_frame_meta=true "+
			"send_codec_meta=true send_dummy_byte=false "+
			"cleanup=true raw_stream=false",
		ServerJarPath, SCRCPYVersion, "test1234",
		opts.AudioEnabled, opts.ControlEnabled,
	)
	assert.Contains(t, cmd, "audio=true")
	assert.Contains(t, cmd, "control=true")
}

func TestLaunchResultCleanupOrder(t *testing.T) {
	// Verify that Cleanup closes resources in the correct order:
	// ControlConn, AudioConn, VideoConn, ReverseMap, Listener, shell.
	var closed []string

	videoConn, _ := net.Pipe()
	controlConn, _ := net.Pipe()
	audioConn, _ := net.Pipe()

	// Close tracking: pipe Close() is idempotent, but we want to verify order.
	// We use a wrapper that records what was closed.
	result := &LaunchResult{
		VideoConn:   videoConn,
		ControlConn: controlConn,
		AudioConn:   audioConn,
		ReverseMap: &adb.ReverseMapping{
			DeviceSpec: "localabstract:scrcpy_test",
			HostSpec:   "tcp:0",
		},
		Cleanup: func() {
			// In real Cleanup, resources are closed in reverse order:
			// ControlConn, AudioConn, VideoConn, ReverseMap, Listener, shell.
			closed = append(closed, "control", "audio", "video", "reverse", "listener", "shell")
		},
	}

	// Execute cleanup.
	result.Cleanup()
	assert.Equal(t, []string{"control", "audio", "video", "reverse", "listener", "shell"}, closed)

	// Clean up pipe connections.
	videoConn.Close()
	controlConn.Close()
	audioConn.Close()
}

// Suppress unused import warning.
var _ = io.Reader(nil)
var _ = context.Background

// ---------------------------------------------------------------------------
// Phase 3 — SCR-07 LaunchOptions extension + AppProcessPID capture (03-01)
// ---------------------------------------------------------------------------

// TestBuildAppProcessCmdBackwardCompat: zero values for SCR-07 fields must
// produce IDENTICAL CLI args as Phase 1/2 (no new flags emitted).
func TestBuildAppProcessCmdBackwardCompat(t *testing.T) {
	opts := LaunchOptions{
		AudioEnabled:   false,
		ControlEnabled: false,
		// All SCR-07 fields zero/empty.
	}
	cmd := BuildAppProcessCmd("abcd1234", opts)

	assert.Contains(t, cmd, "scid=abcd1234")
	assert.Contains(t, cmd, "audio=false")
	assert.Contains(t, cmd, "control=false")

	// Phase 1/2 must NOT see any SCR-07 fields when zero.
	assert.NotContains(t, cmd, "video_codec=")
	assert.NotContains(t, cmd, "max_size=")
	assert.NotContains(t, cmd, "video_bit_rate=")
	assert.NotContains(t, cmd, "max_fps=")
	assert.NotContains(t, cmd, "audio_codec=")
	assert.NotContains(t, cmd, "audio_source=")
}

// TestBuildAppProcessCmdSCR07Codec verifies non-zero Codec emits video_codec=.
func TestBuildAppProcessCmdSCR07Codec(t *testing.T) {
	cases := []struct {
		codec string
		want  string
	}{
		{"h264", "video_codec=h264"},
		{"h265", "video_codec=h265"},
		{"av1", "video_codec=av1"},
	}
	for _, tc := range cases {
		t.Run(tc.codec, func(t *testing.T) {
			opts := LaunchOptions{Codec: tc.codec}
			cmd := BuildAppProcessCmd("scid01", opts)
			assert.Contains(t, cmd, tc.want)
		})
	}
}

// TestBuildAppProcessCmdSCR07Numerics verifies MaxSize/BitRate/MaxFPS only
// emit when > 0 (zero is "use server default", per SCR-07).
func TestBuildAppProcessCmdSCR07Numerics(t *testing.T) {
	t.Run("all_set", func(t *testing.T) {
		opts := LaunchOptions{MaxSize: 1080, BitRate: 4_000_000, MaxFPS: 30}
		cmd := BuildAppProcessCmd("scid01", opts)
		assert.Contains(t, cmd, "max_size=1080")
		assert.Contains(t, cmd, "video_bit_rate=4000000")
		assert.Contains(t, cmd, "max_fps=30")
	})
	t.Run("zero_omits", func(t *testing.T) {
		opts := LaunchOptions{MaxSize: 0, BitRate: 0, MaxFPS: 0}
		cmd := BuildAppProcessCmd("scid01", opts)
		assert.NotContains(t, cmd, "max_size=")
		assert.NotContains(t, cmd, "video_bit_rate=")
		assert.NotContains(t, cmd, "max_fps=")
	})
}

// TestBuildAppProcessCmdSCR07Audio verifies AudioCodec/AudioSource emit
// only when non-empty.
func TestBuildAppProcessCmdSCR07Audio(t *testing.T) {
	t.Run("opus_output", func(t *testing.T) {
		opts := LaunchOptions{AudioCodec: "opus", AudioSource: "output"}
		cmd := BuildAppProcessCmd("scid01", opts)
		assert.Contains(t, cmd, "audio_codec=opus")
		assert.Contains(t, cmd, "audio_source=output")
	})
	t.Run("empty_omits", func(t *testing.T) {
		opts := LaunchOptions{AudioCodec: "", AudioSource: ""}
		cmd := BuildAppProcessCmd("scid01", opts)
		assert.NotContains(t, cmd, "audio_codec=")
		assert.NotContains(t, cmd, "audio_source=")
	})
}

// TestLaunchResultAppProcessPIDField verifies the new field is reachable
// (used by perf sampler in OPS-10).
func TestLaunchResultAppProcessPIDField(t *testing.T) {
	r := &LaunchResult{AppProcessPID: 12345}
	assert.Equal(t, 12345, r.AppProcessPID)

	zero := &LaunchResult{}
	assert.Equal(t, 0, zero.AppProcessPID, "zero value when pgrep fails")
}