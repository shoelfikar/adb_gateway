package scrcpy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
)

// Launcher orchestrates the scrcpy server startup sequence.
// It pushes the embedded server.jar, sets up reverse tunnels, launches the
// scrcpy server process, and reads the initial metadata from the video stream.
type Launcher struct {
	adbClient    *adb.Client
	hostServices *adb.HostServices
}

// NewLauncher creates a Launcher that uses the given ADB client and host services.
func NewLauncher(adbClient *adb.Client, hostServices *adb.HostServices) *Launcher {
	return &Launcher{
		adbClient:    adbClient,
		hostServices: hostServices,
	}
}

// LaunchResult holds the resources acquired during a successful scrcpy server launch.
// The caller is responsible for calling Cleanup() when done with these resources.
type LaunchResult struct {
	// VideoConn is the accepted TCP connection for the video stream.
	VideoConn net.Conn
	// VideoLn is the TCP listener for the video stream. In v3.x scrcpy the
	// same listener accepts audio and control connections after video.
	VideoLn net.Listener
	// AudioConn is the accepted TCP connection for the audio stream.
	// Nil when audio is unavailable for this device.
	AudioConn net.Conn
	// AudioLn is kept for API symmetry but shares the same listener as VideoLn
	// in v3.x scrcpy (one listener, three sequential Accepts). Nil when audio is off.
	AudioLn net.Listener
	// ControlConn is the accepted TCP connection for the control stream
	// (bidirectional: writes go to device, reads return DeviceMessages).
	// Nil when control is disabled.
	ControlConn net.Conn
	// ControlLn is kept for API symmetry; shares the listener in v3.x.
	// Nil when control is off.
	ControlLn net.Listener
	// AudioAvailable is true when the audio probe succeeded (codec ID is a known
	// value). False when the device sent 0x00000000 or immediate EOF.
	AudioAvailable bool
	// AudioCodec holds the parsed codec ID when AudioAvailable is true.
	AudioCodec AudioCodec
	// DeviceName is the device model name read from the scrcpy device metadata.
	DeviceName string
	// CodecMeta contains the raw 12-byte codec metadata (codec ID + width + height).
	CodecMeta [12]byte
	// ReverseMap is the reverse forward mapping for video. Kept for Phase 1
	// back-compat; ReverseMaps is the canonical list.
	ReverseMap *adb.ReverseMapping
	// ReverseMaps holds all reverse forward mappings (1 entry in v3.x — one
	// listener shared by video/audio/control). Closing it removes the reverse tunnel.
	ReverseMaps []*adb.ReverseMapping
	// SCID is the scrcpy session ID used for the device-side socket name.
	SCID string
	// Cleanup releases all resources acquired during launch in reverse order.
	Cleanup func()
}

// LaunchOptions configures which scrcpy streams to enable.
type LaunchOptions struct {
	// AudioEnabled enables the audio stream. When true, the launcher accepts
	// an audio connection after video and reads the codec ID. Per D-11, audio
	// is always-on in Phase 2; this flag allows ops to disable fleet-wide.
	AudioEnabled bool
	// ControlEnabled enables the control stream. When true, the launcher
	// accepts a control connection after video (and audio, if enabled).
	// Phase 2 sets this to true.
	ControlEnabled bool
}

// DefaultLaunchOptions returns options that preserve Phase 1 behavior (video-only).
func DefaultLaunchOptions() LaunchOptions {
	return LaunchOptions{AudioEnabled: false, ControlEnabled: false}
}

// Launch preserves Phase 1's signature (video-only). Phase 2 supervisor
// calls LaunchWithOptions directly.
func (l *Launcher) Launch(ctx context.Context, serial string) (*LaunchResult, error) {
	return l.LaunchWithOptions(ctx, serial, DefaultLaunchOptions())
}

// LaunchWithOptions executes the scrcpy server startup with configurable streams.
// The v3.x protocol uses ONE listener and ONE reverse forward — the server
// connects sequentially to the same localabstract socket: video, then audio,
// then control. The launcher Accepts in that order.
//
// On failure at step N, all resources acquired in steps 1..N-1 are cleaned up per D-05.
func (l *Launcher) LaunchWithOptions(ctx context.Context, serial string, opts LaunchOptions) (*LaunchResult, error) {
	result := &LaunchResult{}
	var cleanupSteps []func()

	cleanupOnFailure := func() {
		for i := len(cleanupSteps) - 1; i >= 0; i-- {
			cleanupSteps[i]()
		}
	}

	// Step 1: Push server.jar
	pushCtx, pushCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pushCancel()
	slog.Info("pushing server.jar to device", "device", serial, "path", ServerJarPath)
	if err := l.hostServices.PushFile(pushCtx, serial, ServerJar, ServerJarPath); err != nil {
		return nil, fmt.Errorf("push server.jar: %w", err)
	}

	// Step 2: Generate SCID
	result.SCID = BuildSCID()
	deviceSocket := fmt.Sprintf("localabstract:scrcpy_%s", result.SCID)

	// Step 3: Allocate ONE host-side TCP listener (ephemeral port).
	// In v3.x scrcpy, video/audio/control all connect to the same socket name.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	result.VideoLn = ln
	hostSpec := fmt.Sprintf("tcp:%d", ln.Addr().(*net.TCPAddr).Port)
	cleanupSteps = append(cleanupSteps, func() { ln.Close() })

	// Step 4: Install ONE reverse forward (v3.x: shared socket for all streams).
	slog.Info("installing reverse tunnel", "device", serial, "deviceSocket", deviceSocket, "hostSpec", hostSpec)
	reverseCtx, reverseCancel := context.WithTimeout(ctx, 10*time.Second)
	reverseMap, err := l.adbClient.ReverseForward(reverseCtx, serial, deviceSocket, hostSpec)
	reverseCancel()
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("reverse forward: %w", err)
	}
	result.ReverseMap = reverseMap
	result.ReverseMaps = []*adb.ReverseMapping{reverseMap}
	cleanupSteps = append(cleanupSteps, func() { reverseMap.Close() })

	// Step 5: Launch app_process via shell:v2 with audio/control flags.
	appProcessCmd := fmt.Sprintf(
		"CLASSPATH=%s app_process / com.genymobile.scrcpy.Server %s "+
			"scid=%s log_level=info "+
			"video=true audio=%t control=%t "+
			"send_device_meta=true send_frame_meta=true "+
			"send_codec_meta=true send_dummy_byte=false "+
			"cleanup=true raw_stream=false",
		ServerJarPath, SCRCPYVersion, result.SCID,
		opts.AudioEnabled, opts.ControlEnabled,
	)
	slog.Info("launching scrcpy server", "device", serial, "scid", result.SCID,
		"audio", opts.AudioEnabled, "control", opts.ControlEnabled)
	shellCleanup, err := l.hostServices.RunDaemonCommand(ctx, serial, appProcessCmd)
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("launch app_process: %w", err)
	}
	cleanupSteps = append(cleanupSteps, shellCleanup)

	// Step 6: Accept video connection (first connect from scrcpy server).
	videoConn, err := acceptWithTimeout(ctx, ln, "video")
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("accept video connection: %w", err)
	}
	result.VideoConn = videoConn
	cleanupSteps = append(cleanupSteps, func() { videoConn.Close() })

	// Step 6': Accept audio connection (second connect) when audio is enabled.
	if opts.AudioEnabled {
		audioConn, err := acceptWithTimeout(ctx, ln, "audio")
		if err != nil {
			cleanupOnFailure()
			return nil, fmt.Errorf("accept audio connection: %w", err)
		}
		result.AudioConn = audioConn
		result.AudioLn = ln // shared listener
		cleanupSteps = append(cleanupSteps, func() { audioConn.Close() })
	}

	// Step 6'': Accept control connection (third connect) when control is enabled.
	if opts.ControlEnabled {
		ctlConn, err := acceptWithTimeout(ctx, ln, "control")
		if err != nil {
			cleanupOnFailure()
			return nil, fmt.Errorf("accept control connection: %w", err)
		}
		result.ControlConn = ctlConn
		result.ControlLn = ln // shared listener
		cleanupSteps = append(cleanupSteps, func() { ctlConn.Close() })
	}

	// Set a read deadline for the metadata reads on the video connection.
	videoConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer videoConn.SetReadDeadline(time.Time{}) // clear deadline after reads

	// Step 7: Read device metadata (64 bytes when send_device_meta=true)
	var deviceNameBuf [64]byte
	if _, err := io.ReadFull(videoConn, deviceNameBuf[:]); err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("read device meta: %w", err)
	}
	result.DeviceName = strings.TrimRight(string(deviceNameBuf[:]), "\x00")

	// Step 8: Read codec metadata (12 bytes) from video stream.
	var codecMetaBuf [12]byte
	if _, err := io.ReadFull(videoConn, codecMetaBuf[:]); err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("read codec meta: %w", err)
	}
	result.CodecMeta = codecMetaBuf

	codecID := string(codecMetaBuf[0:4])
	width := binary.BigEndian.Uint32(codecMetaBuf[4:8])
	height := binary.BigEndian.Uint32(codecMetaBuf[8:12])

	// Step 8': Audio capability probe (when audio is enabled and connection was accepted).
	if opts.AudioEnabled && result.AudioConn != nil {
		result.AudioConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		codec, available, perr := ReadAudioCodecID(result.AudioConn)
		result.AudioConn.SetReadDeadline(time.Time{})
		if perr != nil {
			// Genuine I/O error (not EOF) — fail the launch.
			cleanupOnFailure()
			return nil, fmt.Errorf("audio codec probe: %w", perr)
		}
		if !available {
			// Defensive parse: 0 codec or EOF. Close audio conn cleanly; keep launch.
			result.AudioConn.Close()
			result.AudioConn = nil
			result.AudioLn = nil
			result.AudioAvailable = false
			slog.Info("audio unavailable for device", "device", serial,
				"codec_raw", fmt.Sprintf("0x%08x", uint32(codec)))
		} else {
			result.AudioAvailable = true
			result.AudioCodec = codec
			slog.Info("audio available for device", "device", serial,
				"codec", codecString(codec))
		}
	}

	slog.Info("scrcpy session active",
		"device", serial, "scid", result.SCID,
		"device_name", result.DeviceName,
		"codec", codecID,
		"width", width,
		"height", height,
		"audio_available", result.AudioAvailable,
		"control_enabled", opts.ControlEnabled,
	)

	// On success, set Cleanup function that closes resources in reverse order.
	result.Cleanup = func() {
		if result.ControlConn != nil {
			result.ControlConn.Close()
		}
		if result.AudioConn != nil {
			result.AudioConn.Close()
		}
		videoConn.Close()
		reverseMap.Close()
		ln.Close()
		shellCleanup()
	}

	return result, nil
}

// acceptWithTimeout accepts a connection on the listener with a 10s timeout.
func acceptWithTimeout(ctx context.Context, ln net.Listener, name string) (net.Conn, error) {
	acceptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	acceptCh := make(chan net.Conn)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		acceptCh <- conn
	}()

	select {
	case conn := <-acceptCh:
		return conn, nil
	case err := <-acceptErr:
		return nil, fmt.Errorf("accept %s: %w", name, err)
	case <-acceptCtx.Done():
		return nil, fmt.Errorf("accept %s: timeout waiting for device to connect", name)
	}
}

// codecString returns a human-readable representation of an AudioCodec.
func codecString(c AudioCodec) string {
	switch c {
	case AudioCodecOPUS:
		return "opus"
	case AudioCodecAAC:
		return "aac"
	case AudioCodecFLAC:
		return "flac"
	case AudioCodecRAW:
		return "raw"
	default:
		return fmt.Sprintf("0x%08x", uint32(c))
	}
}