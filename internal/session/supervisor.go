// Package session manages device registry and session lifecycle state.
// supervisor.go defines DeviceSession, which orchestrates the scrcpy server
// lifecycle for a single device: push jar, set up reverse tunnels, launch
// app_process, and track session state through the FSM.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/obs"
	"github.com/pelni/adb-gateway/internal/scrcpy"
)

// Launcher is the interface for launching a scrcpy server on a device.
// *scrcpy.Launcher satisfies this interface. Defined here so tests can
// provide a mock without importing the concrete type.
type Launcher interface {
	LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error)
}

// SessionOpts configures DeviceSession creation. Phase 1 callers can use
// defaults; Phase 2 wiring passes values from config.
type SessionOpts struct {
	BufFrames      int // = cfg.Stream.ViewerBufferFrames
	MaxConsecDrops int // = cfg.Stream.MaxConsecutiveDrops
	AudioEnabled   bool
	// LogcatCapacity is the per-device logcat ring buffer size in lines
	// (Plan 03-03). Zero means "use the LogcatBuffer default" (10000).
	LogcatCapacity int

	// Scrcpy streaming tunables (SCR-07). Zero values mean "use the scrcpy
	// server default" — backward compatible with Phase 1/2.
	ScrcpyCodec       string // h264 | h265 | av1; empty = server default
	ScrcpyMaxSize     int    // px, 0 = device default
	ScrcpyBitRate     int    // bps, 0 = server default
	ScrcpyMaxFPS      int    // 0 = unlimited
	ScrcpyAudioCodec  string // opus | aac | raw | flac; empty = server default
	ScrcpyAudioSource string // output | mic | playback; empty = server default
}

// DefaultSessionOpts returns reasonable defaults for Phase 1 compatibility.
func DefaultSessionOpts() SessionOpts {
	return SessionOpts{BufFrames: 60, MaxConsecDrops: 120, AudioEnabled: false}
}

// DeviceSession manages the full lifecycle of a scrcpy session for one device.
// It tracks state transitions via the FSM (idle -> starting -> active -> stopping -> idle)
// and holds resources (video connection, reverse mapping, listener) acquired during startup.
//
// All public methods are thread-safe; the per-device mutex (ADB-07) prevents one hung
// device from blocking operations on others.
type DeviceSession struct {
	ID     string
	Serial string

	state SessionState
	mu    sync.Mutex
	log   *slog.Logger

	// Resources acquired during scrcpy launch; set in Start, cleaned up in Close.
	videoConn  net.Conn
	videoLn    net.Listener
	reverseMap *adb.ReverseMapping
	codecMeta  [12]byte
	deviceName string
	scid       string
	cleanup    func()

	// Phase 2: audio + control resources from LaunchWithOptions.
	audioConn      net.Conn
	controlConn    net.Conn
	audioAvailable bool
	audioCodec     scrcpy.AudioCodec

	// Phase 2: goroutine-managed components.
	videoHub      *Hub
	audioHub      *Hub
	controlWriter *scrcpy.ControlWriter
	deviceMsgs    chan scrcpy.DeviceMessage
	runCancel     context.CancelFunc // cancels the Run() errgroup context

	// readyCh is closed by Run() (or SetControlWriterForTest) once the
	// streaming components (videoHub, audioHub, controlWriter, deviceMsgs)
	// are instantiated. CreateSession blocks on Ready() before returning
	// HTTP 201 so a client connecting to /video|/audio|/control immediately
	// after the response cannot race the Run() goroutine and observe nil hubs.
	readyCh   chan struct{}
	readyOnce sync.Once

	// Phase 2: config snapshot.
	bufFrames      int
	maxConsecDrops int
	audioEnabled   bool

	// Phase 3 (SCR-07): scrcpy streaming tunables, captured at session creation.
	// Zero values mean "use server default" — passed through to LaunchOptions.
	scrcpyCodec       string
	scrcpyMaxSize     int
	scrcpyBitRate     int
	scrcpyMaxFPS      int
	scrcpyAudioCodec  string
	scrcpyAudioSource string

	// Phase 3 (Plan 03-02): captured LaunchOptions used at the most recent
	// successful launch. Recovery re-uses these to re-issue scrcpy on the
	// same DeviceSession. Read under s.mu via LaunchOptions().
	launchOpts scrcpy.LaunchOptions

	// Phase 3 (Plan 03-02): watchdog + recovery wiring. Both are optional —
	// when nil, Run skips the watchdog goroutine. Tests that don't need
	// stall recovery construct a session without setting these.
	watchdog *StallWatchdog
	recovery *Recovery

	// Phase 3 (Plan 03-03): per-device logcat ring buffer (OPS-05). Set by
	// the supervisor on session start. Survives a logcat-process restart so
	// late-joining /logcat WS subscribers always see the full ring.
	logcatBuffer   *LogcatBuffer
	logcatRunner   LogcatShellRunner
	logcatCapacity int

	// Dependencies injected at creation.
	adbClient *adb.Client
	launcher  Launcher
}

// NewDeviceSession creates a DeviceSession for the given device serial.
// It generates a UUID for the session ID and creates a per-device sublogger
// per OBS-03 (structured fields: device serial, session ID).
func NewDeviceSession(serial string, adbClient *adb.Client, launcher Launcher, opts SessionOpts) *DeviceSession {
	id := uuid.New().String()
	return &DeviceSession{
		ID:             id,
		Serial:         serial,
		state:          StateIdle,
		log:            slog.With("device", serial, "session", id),
		adbClient:      adbClient,
		launcher:       launcher,
		bufFrames:      opts.BufFrames,
		maxConsecDrops: opts.MaxConsecDrops,
		audioEnabled:   opts.AudioEnabled,
		logcatCapacity: opts.LogcatCapacity,
		scrcpyCodec:       opts.ScrcpyCodec,
		scrcpyMaxSize:     opts.ScrcpyMaxSize,
		scrcpyBitRate:     opts.ScrcpyBitRate,
		scrcpyMaxFPS:      opts.ScrcpyMaxFPS,
		scrcpyAudioCodec:  opts.ScrcpyAudioCodec,
		scrcpyAudioSource: opts.ScrcpyAudioSource,
		readyCh:        make(chan struct{}),
	}
}

// Ready returns a channel that is closed once Run() (or
// SetControlWriterForTest) has instantiated the streaming components.
// CreateSession waits on this before responding 201 to prevent a WS
// client from racing the Run goroutine and observing a nil ControlWriter.
//
// Lazy-init: readyCh may be nil on sessions created via struct literal
// (test helpers like NewActiveSessionForTest). Both Ready and markReady
// allocate it on demand.
func (s *DeviceSession) Ready() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readyCh == nil {
		s.readyCh = make(chan struct{})
	}
	return s.readyCh
}

// markReady closes readyCh exactly once. Safe to call from multiple paths
// (Run on first invocation, SetControlWriterForTest for unit tests).
func (s *DeviceSession) markReady() {
	s.mu.Lock()
	if s.readyCh == nil {
		s.readyCh = make(chan struct{})
	}
	ch := s.readyCh
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(ch) })
}

// Start acquires the device mutex, validates the idle->starting transition,
// calls launcher.LaunchWithOptions, stores acquired resources, and transitions
// to StateActive.
func (s *DeviceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newState, err := TransitionTo(s.state, StateStarting)
	if err != nil {
		return fmt.Errorf("cannot start session: %w", err)
	}
	s.state = newState
	s.log.Info("session starting")

	opts := scrcpy.LaunchOptions{
		AudioEnabled:   s.audioEnabled,
		ControlEnabled: true,
		Codec:       s.scrcpyCodec,
		MaxSize:     s.scrcpyMaxSize,
		BitRate:     s.scrcpyBitRate,
		MaxFPS:      s.scrcpyMaxFPS,
		AudioCodec:  s.scrcpyAudioCodec,
		AudioSource: s.scrcpyAudioSource,
	}
	s.launchOpts = opts // remember for Recovery re-launch
	result, err := s.launcher.LaunchWithOptions(ctx, s.Serial, opts)
	if err != nil {
		s.state = StateFailed
		s.log.Error("scrcpy launch failed", "error", err)
		return fmt.Errorf("launch: %w", err)
	}

	// Store resources from the successful launch.
	s.videoConn = result.VideoConn
	s.videoLn = result.VideoLn
	s.reverseMap = result.ReverseMap
	s.codecMeta = result.CodecMeta
	s.deviceName = result.DeviceName
	s.scid = result.SCID
	s.cleanup = result.Cleanup

	// Phase 2 resources.
	s.audioConn = result.AudioConn
	s.controlConn = result.ControlConn
	s.audioAvailable = result.AudioAvailable
	s.audioCodec = result.AudioCodec

	// Plan 03-03 OPS-05: allocate the per-device logcat buffer on session
	// start so that any /logcat WS request received in the same Active
	// window sees a valid buffer. The reader goroutine (if a runner is
	// attached) starts in Run and Appends into this buffer.
	if s.logcatBuffer == nil {
		s.logcatBuffer = NewLogcatBuffer(LogcatBufferOpts{
			Capacity: s.logcatCapacity,
			Log:      s.log,
		})
	}

	s.state = StateActive
	s.log.Info("session active",
		"codec", string(result.CodecMeta[:4]),
		"device_name", result.DeviceName,
		"scid", result.SCID,
		"audio_available", result.AudioAvailable,
		"audio_codec", result.AudioCodec,
		"control_enabled", result.ControlConn != nil,
	)
	return nil
}

// State returns the current session state. Thread-safe via mutex.
func (s *DeviceSession) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close transitions to StateStopping, cleans up all resources, and transitions to StateIdle.
func (s *DeviceSession) Close(ctx context.Context) error {
	s.mu.Lock()
	newState, err := TransitionTo(s.state, StateStopping)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("cannot close session in state %s: %w", s.state, err)
	}
	s.state = newState
	s.mu.Unlock()

	s.log.Info("session stopping")
	// Cancel the Run() errgroup goroutines (video/audio readers, hubs, etc.)
	if s.runCancel != nil {
		s.runCancel()
	}
	s.cleanupResources()

	s.mu.Lock()
	s.state = StateIdle
	s.mu.Unlock()

	s.log.Info("session closed")
	return nil
}

// ReleaseResources releases all resources held by the session without
// transitioning FSM states. Used when ADB disconnects.
func (s *DeviceSession) ReleaseResources() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
	if s.videoLn != nil {
		s.videoLn.Close()
		s.videoLn = nil
	}
	if s.videoConn != nil {
		s.videoConn.Close()
		s.videoConn = nil
	}
	if s.reverseMap != nil {
		s.reverseMap.Close()
		s.reverseMap = nil
	}
}

// cleanupResources closes all resources acquired during scrcpy launch.
func (s *DeviceSession) cleanupResources() {
	if s.cleanup != nil {
		s.cleanup()
		s.mu.Lock()
		s.cleanup = nil
		s.videoConn = nil
		s.videoLn = nil
		s.reverseMap = nil
		s.audioConn = nil
		s.controlConn = nil
		buf := s.logcatBuffer
		s.logcatBuffer = nil
		s.mu.Unlock()
		if buf != nil {
			buf.Shutdown()
		}
	}
}

// Run spawns 4-6 goroutines under errgroup: video reader → videoHub,
// audio reader → audioHub (if available), control writer, device message reader,
// and a closer goroutine for context cancellation cleanup.
// Returns the first error from the errgroup.
func (s *DeviceSession) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	s.deviceMsgs = make(chan scrcpy.DeviceMessage, 32)

	// Capture conn pointers into locals before starting goroutines to avoid
	// data race: the closer goroutine writes nil to s.controlConn etc via
	// cleanupResources, while the reader/writer goroutines read them.
	audioConn := s.audioConn
	controlConn := s.controlConn

	// Closer goroutine: close sockets when context cancels.
	g.Go(func() error {
		<-ctx.Done()
		s.cleanupResources()
		return ctx.Err()
	})

	// 1. Video Hub + Video Reader.
	s.videoHub = NewHub(HubOpts{
		Stream:              "video",
		BufFrames:           s.bufFrames,
		MaxConsecutiveDrops: s.maxConsecDrops,
		Log:                 s.log,
	})
	s.videoHub.SetCodecMeta(s.codecMeta)
	g.Go(func() error { return s.videoHub.Run(ctx) })
	g.Go(func() error { return s.videoReaderLoop(ctx) })

	// 2. Audio Hub + Audio Reader (only if audio is available).
	if s.audioAvailable && audioConn != nil {
		s.audioHub = NewHub(HubOpts{
			Stream:              "audio",
			BufFrames:           s.bufFrames,
			MaxConsecutiveDrops: s.maxConsecDrops,
			Log:                 s.log,
		})
		g.Go(func() error { return s.audioHub.Run(ctx) })
		g.Go(func() error { return s.audioReaderLoop(ctx, audioConn) })
	}

	// 3. Plan 03-02: stall watchdog (per-device) — runs only when one was
	// attached via SetStallWatchdog. The watchdog reads VideoHub.FrameCount
	// lock-free; onStall (if recovery is wired) spawns Recovery.Run on a
	// separate goroutine so the watchdog stays responsive to ctx cancel.
	if s.watchdog != nil {
		g.Go(func() error { return s.watchdog.Run(ctx) })
	}

	// Plan 03-03: per-device logcat reader. Runs only when a runner has
	// been attached via AttachLogcatReader. logcatReaderLoop suppresses
	// non-ctx errors (Pitfall 1) so a logcat EOF cannot cancel video/audio.
	if s.logcatRunner != nil && s.logcatBuffer != nil {
		g.Go(func() error { return s.logcatReaderLoop(ctx) })
	}

	// 4. Control Writer + Device Message Reader (on the same conn).
	if controlConn != nil {
		s.controlWriter = scrcpy.NewControlWriter(scrcpy.ControlWriterOpts{
			Conn:       controlConn,
			Log:        s.log,
			BufferSize: 64,
		})
		g.Go(func() error { return s.controlWriter.Run(ctx) })
		g.Go(func() error { return s.deviceMessageReaderLoop(ctx, controlConn) })
	}

	// All streaming components (videoHub, audioHub if available, controlWriter
	// if controlConn != nil, deviceMsgs) are now instantiated. Unblock any
	// caller waiting on Ready() — typically CreateSession before responding 201.
	s.markReady()

	return g.Wait()
}

// videoReaderLoop reads frames from the video connection and publishes to videoHub.
func (s *DeviceSession) videoReaderLoop(ctx context.Context) error {
	conn := s.VideoConn()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, payload, err := scrcpy.ReadVideoFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("video read: %w", err)
		}
		s.videoHub.Publish(&Frame{
			Header:       hdr.RawHeader(),
			Payload:      payload,
			KeyFrame:     hdr.KeyFrame,
			ConfigPacket: hdr.ConfigPacket,
		})
	}
}

// audioReaderLoop reads frames from the audio connection and publishes to audioHub.
// Codec ID was already consumed by the launcher's probe.
// The conn parameter is passed explicitly to avoid racing with cleanupResources nil-assignment.
func (s *DeviceSession) audioReaderLoop(ctx context.Context, conn net.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, payload, err := scrcpy.ReadAudioFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("audio read: %w", err)
		}
		s.audioHub.Publish(&Frame{
			Header:       hdr.RawHeader(),
			Payload:      payload,
			KeyFrame:     hdr.KeyFrame,
			ConfigPacket: hdr.ConfigPacket,
		})
	}
}

// deviceMessageReaderLoop reads DeviceMessages from the control connection
// and sends them on the deviceMsgs channel.
// The conn parameter is passed explicitly to avoid racing with cleanupResources nil-assignment.
func (s *DeviceSession) deviceMessageReaderLoop(ctx context.Context, conn net.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := scrcpy.ReadDeviceMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			if errors.Is(err, scrcpy.ErrUnknownDeviceMessage) {
				s.log.Warn("unknown device message; reader exiting", "error", err)
				return err
			}
			return fmt.Errorf("device message read: %w", err)
		}
		select {
		case s.deviceMsgs <- msg:
		case <-ctx.Done():
			return ctx.Err()
		default:
			s.log.Warn("device message dropped: consumer slow", "type", fmt.Sprintf("0x%02x", byte(msg.Type)))
		}
	}
}

// Accessor methods for Phase 2 WS handlers (plan 02-06).

// SetRunCancel stores the cancel function for the Run() errgroup context.
// Called by CreateSession after Start succeeds; used by Close to stop Run goroutines.
func (s *DeviceSession) SetRunCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCancel = cancel
}

func (s *DeviceSession) VideoHub() *Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoHub
}

func (s *DeviceSession) AudioHub() *Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioHub
}

func (s *DeviceSession) ControlWriter() *scrcpy.ControlWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlWriter
}

func (s *DeviceSession) DeviceMessages() <-chan scrcpy.DeviceMessage {
	return s.deviceMsgs
}

func (s *DeviceSession) AudioAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioAvailable
}

func (s *DeviceSession) AudioCodec() scrcpy.AudioCodec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioCodec
}

// NewActiveSessionForTest creates a DeviceSession in StateActive with the
// given Hub already set. Used for WS handler integration tests that don't
// need a full launcher + Run cycle.
func NewActiveSessionForTest(serial string, videoHub *Hub) *DeviceSession {
	return &DeviceSession{
		ID:        "test-" + serial,
		Serial:    serial,
		state:     StateActive,
		videoHub:  videoHub,
		log:       slog.Default(),
	}
}

// SetAudioHubForTest sets the audio hub on a test session. Only for use in tests.
func (s *DeviceSession) SetAudioHubForTest(hub *Hub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioHub = hub
	s.audioAvailable = hub != nil
}

// SetControlWriterForTest sets the control writer on a test session. Only for use in tests.
// Also marks the session ready so tests that exercise WS handlers via the real
// CreateSession path (which waits on Ready) do not block.
func (s *DeviceSession) SetControlWriterForTest(cw *scrcpy.ControlWriter) {
	s.mu.Lock()
	s.controlWriter = cw
	s.mu.Unlock()
	s.markReady()
}

// SetLogcatBufferForTest installs a LogcatBuffer on a test session. Only for
// use in tests (Plan 03-03 /logcat WS handler tests pre-populate a buffer).
func (s *DeviceSession) SetLogcatBufferForTest(b *LogcatBuffer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logcatBuffer = b
}

// SetStateForTest sets the session FSM state directly without going through
// TransitionTo. Only for use in tests that need to drive a session into a
// non-Active state for handler-side state-check coverage (e.g. Plan 03-03
// asserts the /logcat handler accepts StateReconnecting).
func (s *DeviceSession) SetStateForTest(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// LogcatBuffer returns the per-device logcat ring buffer, or nil when the
// session has none attached (e.g. test sessions without a supervisor).
func (s *DeviceSession) LogcatBuffer() *LogcatBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logcatBuffer
}

// AttachLogcatBuffer installs a LogcatBuffer onto the session. Production
// callers (the supervisor wiring) call this BEFORE Run so the WS handler
// always finds a valid buffer for active sessions. Plan 03-03.
func (s *DeviceSession) AttachLogcatBuffer(b *LogcatBuffer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logcatBuffer = b
}

// LogcatShellRunner is the minimal interface logcatReaderLoop needs. The
// production implementation is *adb.HostServices; tests inject a fake.
// Defined here (not in adb) so session does not import the concrete
// HostServices type.
type LogcatShellRunner interface {
	ShellV2Stream(ctx context.Context, serial, cmd string) (stdout, stderr io.ReadCloser, exit <-chan int, err error)
}

// AttachStallRecovery wires the Plan 03-02 watchdog + recovery pair onto
// this session. Must be called BEFORE Run. The watchdog observes
// VideoHub.FrameCount; on stall it spawns recovery.Run on a fresh
// background context so a normal Run-ctx cancel (DELETE /sessions) does
// NOT terminate an in-flight recovery — recovery itself observes the
// passed ctx and transitions to Stopping/Failed appropriately.
//
// recoveryCtx is the context the recovery loop uses (typically a
// context.Background-derived context owned by the registry, not the
// per-Run errgroup ctx). Pass nil to use context.Background().
func (s *DeviceSession) AttachStallRecovery(rec *Recovery, w *StallWatchdog, recoveryCtx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovery = rec
	if recoveryCtx == nil {
		recoveryCtx = context.Background()
	}
	// The watchdog's onStall trampoline must close over `recoveryCtx`, not
	// the supervisor's per-Run ctx, so recovery survives a benign DELETE.
	if w != nil && rec != nil {
		// We allocate a copy here that delegates onStall.
		s.watchdog = NewStallWatchdog(StallWatchdogOpts{
			Counter:   w.counter,
			Interval:  w.interval,
			Threshold: w.threshold,
			Log:       w.log,
			OnStall: func() {
				go func() {
					if err := rec.Run(recoveryCtx, s); err != nil {
						s.log.Warn("recovery: terminal", "error", err)
					}
				}()
			},
		})
	} else {
		s.watchdog = w
	}
}

// LaunchOptions returns the scrcpy LaunchOptions captured at the most recent
// successful Start. Plan 03-02 Recovery re-uses these to re-launch scrcpy on
// the same DeviceSession (preserving SCID, audio/control settings, codec).
func (s *DeviceSession) LaunchOptions() scrcpy.LaunchOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launchOpts
}

// transitionLocked atomically transitions the session FSM and emits the
// gateway_session_state Prometheus gauge. The caller MUST hold s.mu.
// Returns the new state on success.
func (s *DeviceSession) transitionLocked(target SessionState) (SessionState, error) {
	next, err := TransitionTo(s.state, target)
	if err != nil {
		return s.state, err
	}
	s.state = next
	obs.SetSessionState(s.Serial, next.String())
	return next, nil
}

// Existing Phase 1 accessors.

func (s *DeviceSession) CodecMeta() [12]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codecMeta
}

func (s *DeviceSession) VideoConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoConn
}

func (s *DeviceSession) DeviceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceName
}

func (s *DeviceSession) SCID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scid
}

func (s *DeviceSession) ReverseMap() *adb.ReverseMapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reverseMap
}

func (s *DeviceSession) SetReverseMap(rm *adb.ReverseMapping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reverseMap = rm
}

func (s *DeviceSession) VideoLn() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoLn
}

// mapLaunchError maps launcher errors to domain error categories.
func mapLaunchError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "push"):
		return "PUSH_FAILED"
	case strings.Contains(msg, "reverse forward"):
		return "REVERSE_FORWARD_FAILED"
	case strings.Contains(msg, "dial adb"):
		return "ADB_UNAVAILABLE"
	default:
		return "SCRCPY_LAUNCH_FAILED"
	}
}

// IsLaunchError checks if an error from DeviceSession.Start maps to a specific category.
func IsLaunchError(err error, category string) bool {
	return mapLaunchError(err) == category
}

// IsSessionActive checks if a session is in the active state.
// The caller must hold entry.Lock() before calling this method.
func IsSessionActive(entry *DeviceEntry) bool {
	return entry.State == StateActive && entry.Session != nil
}

type launchError struct {
	err      error
	category string
}

func (e *launchError) Error() string { return e.err.Error() }
func (e *launchError) Unwrap() error { return e.err }

func (e *launchError) Category() string { return e.category }

func WrapLaunchError(err error) error {
	if err == nil {
		return nil
	}
	return &launchError{err: err, category: mapLaunchError(err)}
}

func GetLaunchErrorCategory(err error) string {
	var le *launchError
	if errors.As(err, &le) {
		return le.Category()
	}
	return mapLaunchError(err)
}