package scrcpy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
)

// Launcher orchestrates the 8-step scrcpy server startup sequence.
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
	// VideoLn is the TCP listener for the video stream. Keep alive for the
	// duration of the session only if re-accepting is needed; otherwise close
	// after the initial accept.
	VideoLn net.Listener
	// DeviceName is the device model name read from the scrcpy device metadata.
	DeviceName string
	// CodecMeta contains the raw 12-byte codec metadata (codec ID + width + height).
	CodecMeta [12]byte
	// ReverseMap is the reverse forward mapping. Closing it removes the reverse
	// tunnel from the device.
	ReverseMap *adb.ReverseMapping
	// SCID is the scrcpy session ID used for the device-side socket name.
	SCID string
	// Cleanup releases all resources acquired during launch in reverse order.
	// It closes VideoConn, ReverseMap, and VideoLn.
	Cleanup func()
}

// Launch executes the strictly sequential 8-step scrcpy server startup per D-04:
//  1. Push server.jar to device
//  2. Generate SCID (random session ID)
//  3. Allocate host-side TCP listener (ephemeral port)
//  4. Install reverse tunnel (localabstract:scrcpy_<SCID> -> tcp:<port>)
//  5. Launch app_process via shell:v2
//  6. Accept video connection from device
//  7. Read device metadata (64 bytes)
//  8. Read codec metadata (12 bytes)
//
// On failure at step N, all resources acquired in steps 1..N-1 are cleaned up per D-05.
func (l *Launcher) Launch(ctx context.Context, serial string) (*LaunchResult, error) {
	result := &LaunchResult{}
	var cleanupSteps []func()

	// Helper to run all cleanup steps on failure (in reverse order)
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

	// Step 3: Allocate host-side TCP listener for video (ephemeral port)
	videoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for video: %w", err)
	}
	result.VideoLn = videoLn
	videoPort := videoLn.Addr().(*net.TCPAddr).Port
	hostSpec := fmt.Sprintf("tcp:%d", videoPort)
	cleanupSteps = append(cleanupSteps, func() { videoLn.Close() })

	// Step 4: Install reverse tunnel
	// CRITICAL: device-side socket uses localabstract:scrcpy_<SCID>, NOT tcp:27183
	// CRITICAL: separator between device-socket and host-socket is SEMICOLON, not colon
	slog.Info("installing reverse tunnel", "device", serial, "deviceSocket", deviceSocket, "hostSpec", hostSpec)
	reverseCtx, reverseCancel := context.WithTimeout(ctx, 10*time.Second)
	reverseMap, err := l.adbClient.ReverseForward(reverseCtx, serial, deviceSocket, hostSpec)
	reverseCancel()
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("reverse forward: %w", err)
	}
	result.ReverseMap = reverseMap
	cleanupSteps = append(cleanupSteps, func() { reverseMap.Close() })

	// Step 5: Launch app_process via shell:v2
	// Per D-02: CLASSPATH uses gateway-specific filename scrcpy-server-gateway.jar
	// Per D-01: version arg is 3.3.4
	//
	// app_process runs indefinitely on the device. The ADB shell connection MUST
	// stay open for the process to survive — closing it sends SIGHUP, killing the
	// server. RunDaemonCommand keeps the connection open and returns a cleanup
	// function that closes it (terminating the scrcpy server on the device).
	appProcessCmd := fmt.Sprintf(
		"CLASSPATH=%s app_process / com.genymobile.scrcpy.Server %s "+
			"scid=%s log_level=info "+
			"video=true audio=false control=false "+
			"send_device_meta=true send_frame_meta=true "+
			"send_codec_meta=true send_dummy_byte=false "+
			"cleanup=true raw_stream=false",
		ServerJarPath, SCRCPYVersion, result.SCID,
	)
	slog.Info("launching scrcpy server", "device", serial, "scid", result.SCID)
	shellCleanup, err := l.hostServices.RunDaemonCommand(ctx, serial, appProcessCmd)
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("launch app_process: %w", err)
	}
	cleanupSteps = append(cleanupSteps, shellCleanup)

	// Step 6: Accept video connection with timeout
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 10*time.Second)
	defer acceptCancel()
	var videoConn net.Conn
	acceptCh := make(chan net.Conn)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := videoLn.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		acceptCh <- conn
	}()

	select {
	case conn := <-acceptCh:
		videoConn = conn
	case err := <-acceptErr:
		cleanupOnFailure()
		return nil, fmt.Errorf("accept video connection: %w", err)
	case <-acceptCtx.Done():
		cleanupOnFailure()
		return nil, fmt.Errorf("accept video connection: timeout waiting for device to connect")
	}
	result.VideoConn = videoConn
	cleanupSteps = append(cleanupSteps, func() { videoConn.Close() })

	// Set a read deadline for the metadata reads
	videoConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer videoConn.SetReadDeadline(time.Time{}) // clear deadline after reads

	// Step 7: Read device metadata (64 bytes when send_device_meta=true)
	var deviceNameBuf [64]byte
	if _, err := io.ReadFull(videoConn, deviceNameBuf[:]); err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("read device meta: %w", err)
	}
	result.DeviceName = strings.TrimRight(string(deviceNameBuf[:]), "\x00")

	// Step 8: Read codec metadata (12 bytes)
	// Read raw bytes first to preserve them in CodecMeta, then parse.
	var codecMetaBuf [12]byte
	if _, err := io.ReadFull(videoConn, codecMetaBuf[:]); err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("read codec meta: %w", err)
	}
	result.CodecMeta = codecMetaBuf

	// Parse codec metadata from raw bytes for logging
	codecID := string(codecMetaBuf[0:4])
	width := binaryBigEndianUint32(codecMetaBuf[4:8])
	height := binaryBigEndianUint32(codecMetaBuf[8:12])

	slog.Info("scrcpy session active",
		"device", serial, "scid", result.SCID,
		"device_name", result.DeviceName,
		"codec", codecID,
		"width", width,
		"height", height,
	)

	// On success, set Cleanup function that closes resources in reverse order.
	// Closing the shell connection sends SIGHUP to app_process, terminating
	// the scrcpy server on the device (cleanup=true is set in server args).
	result.Cleanup = func() {
		videoConn.Close()
		reverseMap.Close()
		videoLn.Close()
		shellCleanup()
	}

	return result, nil
}

// binaryBigEndianUint32 reads a uint32 from 4 bytes in big-endian order.
func binaryBigEndianUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}