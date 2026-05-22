package session

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockLauncher implements Launcher for testing (uses LaunchWithOptions).
type mockLauncher struct {
	result *scrcpy.LaunchResult
	err    error
	mu     sync.Mutex
	calls  int
}

func (m *mockLauncher) LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func (m *mockLauncher) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// createMockLaunchResult creates a LaunchResult with fake resources that can be
// closed. Used for testing session lifecycle.
func createMockLaunchResult() *scrcpy.LaunchResult {
	// Create a real TCP listener on an ephemeral port for the video listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	// Create a fake reverse mapping with a closed connection.
	rm := &adb.ReverseMapping{
		DeviceSpec: "localabstract:scrcpy_test",
		HostSpec:   "tcp:0",
	}

	return &scrcpy.LaunchResult{
		VideoConn:  nil, // Set separately if needed
		VideoLn:    ln,
		DeviceName: "test-device",
		CodecMeta:  [12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20},
		ReverseMap: rm,
		SCID:       "deadbeef",
		Cleanup: func() {
			ln.Close()
		},
	}
}

func TestNewDeviceSession(t *testing.T) {
	sess := NewDeviceSession("ABC123", nil, nil, DefaultSessionOpts())

	// ID should be a valid UUID (not empty, proper format)
	assert.NotEmpty(t, sess.ID)
	assert.Len(t, sess.ID, 36, "UUID should be 36 characters with hyphens")

	// Serial should be set
	assert.Equal(t, "ABC123", sess.Serial)

	// State should be idle
	assert.Equal(t, StateIdle, sess.State())
}

func TestSessionStartSuccess(t *testing.T) {
	// Create a mock launcher that returns a successful result.
	result := createMockLaunchResult()

	// Create a TCP connection pair to simulate videoConn.
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncher{result: result}
	sess := NewDeviceSession("ABC123", nil, launcher, DefaultSessionOpts())

	ctx := context.Background()
	err := sess.Start(ctx)
	require.NoError(t, err)

	// State should have transitioned from idle -> starting -> active
	assert.Equal(t, StateActive, sess.State())

	// Verify resources were stored
	assert.Equal(t, [12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20}, sess.CodecMeta())

	// Clean up
	sess.Close(ctx)
	client.Close()
}

func TestSessionStartFailure(t *testing.T) {
	// Create a mock launcher that returns an error.
	launcher := &mockLauncher{err: errors.New("push server.jar: connection refused")}
	sess := NewDeviceSession("ABC123", nil, launcher, DefaultSessionOpts())

	ctx := context.Background()
	err := sess.Start(ctx)
	require.Error(t, err)

	// State should have transitioned to failed
	assert.Equal(t, StateFailed, sess.State())
}

func TestSessionClose(t *testing.T) {
	// Create a session, start it, then close it.
	result := createMockLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncher{result: result}
	sess := NewDeviceSession("ABC123", nil, launcher, DefaultSessionOpts())

	ctx := context.Background()
	err := sess.Start(ctx)
	require.NoError(t, err)
	assert.Equal(t, StateActive, sess.State())

	// Close the session
	err = sess.Close(ctx)
	require.NoError(t, err)
	assert.Equal(t, StateIdle, sess.State())

	client.Close()
}

func TestSessionCloseFromIdle(t *testing.T) {
	// Closing an idle session should fail (invalid transition).
	sess := NewDeviceSession("ABC123", nil, nil, DefaultSessionOpts())
	ctx := context.Background()

	err := sess.Close(ctx)
	// Cannot transition from idle to stopping
	require.Error(t, err)
}

func TestIdempotentSessionCreate(t *testing.T) {
	// Simulate the idempotent check: if a session is already active,
	// return the same session instead of creating a new one.
	result := createMockLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncher{result: result}

	registry := NewRegistry()
	entry := registry.GetOrCreate("ABC123")

	// Create and start a session
	sess := NewDeviceSession("ABC123", nil, launcher, DefaultSessionOpts())
	ctx := context.Background()
	err := sess.Start(ctx)
	require.NoError(t, err)

	// Set it as active on the entry
	entry.Lock()
	entry.Session = sess
	entry.State = StateActive
	entry.Unlock()

	// Verify IsSessionActive returns true
	assert.True(t, IsSessionActive(entry))

	// Second start attempt on the same session should fail
	// (already in StateActive, can't transition to StateStarting)
	err = sess.Start(ctx)
	require.Error(t, err, "starting an already-active session should fail")

	sess.Close(ctx)
	client.Close()
}

func TestGetLaunchErrorCategory(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
	}{
		{"push error", errors.New("push server.jar: connection refused"), "PUSH_FAILED"},
		{"reverse forward error", errors.New("reverse forward: dial failed"), "REVERSE_FORWARD_FAILED"},
		{"adb dial error", errors.New("dial adb server localhost:5037: connection refused"), "ADB_UNAVAILABLE"},
		{"accept error", errors.New("accept video connection: timeout"), "SCRCPY_LAUNCH_FAILED"},
		{"device meta error", errors.New("read device meta: EOF"), "SCRCPY_LAUNCH_FAILED"},
		{"codec meta error", errors.New("read codec meta: unexpected EOF"), "SCRCPY_LAUNCH_FAILED"},
		{"nil error", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			category := GetLaunchErrorCategory(tt.err)
			assert.Equal(t, tt.category, category)
		})
	}
}

func TestMapLaunchError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
	}{
		{"push", errors.New("push server.jar: no space left"), "PUSH_FAILED"},
		{"reverse", errors.New("reverse forward: timeout"), "REVERSE_FORWARD_FAILED"},
		{"dial", errors.New("dial adb server: refused"), "ADB_UNAVAILABLE"},
		{"other", errors.New("accept video: timeout"), "SCRCPY_LAUNCH_FAILED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.category, mapLaunchError(tt.err))
		})
	}
}

func TestSessionStateTransitions(t *testing.T) {
	tests := []struct {
		name   string
		from   SessionState
		to     SessionState
		valid  bool
	}{
		{"idle to starting", StateIdle, StateStarting, true},
		{"idle to active", StateIdle, StateActive, false},
		{"idle to stopping", StateIdle, StateStopping, false},
		{"starting to active", StateStarting, StateActive, true},
		{"starting to failed", StateStarting, StateFailed, true},
		{"active to stopping", StateActive, StateStopping, true},
		{"active to failed", StateActive, StateFailed, true},
		{"stopping to idle", StateStopping, StateIdle, true},
		{"failed to idle", StateFailed, StateIdle, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := TransitionTo(tt.from, tt.to)
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, tt.to, result)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.from, result)
			}
		})
	}
}

func TestDeviceSessionCodecMeta(t *testing.T) {
	sess := NewDeviceSession("ABC123", nil, nil, DefaultSessionOpts())
	// Before starting, codec meta should be zero value
	assert.Equal(t, [12]byte{}, sess.CodecMeta())

	// After starting with a mock, codec meta should be set
	result := createMockLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &mockLauncher{result: result}
	sess2 := NewDeviceSession("DEF456", nil, launcher, DefaultSessionOpts())
	ctx := context.Background()
	err := sess2.Start(ctx)
	require.NoError(t, err)

	expected := [12]byte{'h', '2', '6', '4', 0, 0, 4, 0x80, 0, 0, 3, 0x20}
	assert.Equal(t, expected, sess2.CodecMeta())

	sess2.Close(ctx)
	client.Close()
}

func TestConcurrentSessionState(t *testing.T) {
	// Verify that State() is thread-safe
	sess := NewDeviceSession("ABC123", nil, nil, DefaultSessionOpts())
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sess.State()
		}()
	}
	wg.Wait()
	// If we get here without a race detector panic, the test passes
	assert.Equal(t, StateIdle, sess.State())
}

func TestSessionRunContextCancellation(t *testing.T) {
	// Test that Run exits when context is cancelled.
	// The closer goroutine in Run calls cleanupResources which closes
	// connections, unblocking reads. We close the server side to simulate
	// this: when ctx is cancelled, cleanup closes the conn, ReadFull returns
	// an error, and Run exits.
	result := createMockLaunchResult()
	srv, client := net.Pipe()
	result.VideoConn = client

	launcher := &mockLauncher{result: result}
	sess := NewDeviceSession("ABC123", nil, launcher, DefaultSessionOpts())

	ctx, cancel := context.WithCancel(context.Background())
	err := sess.Start(ctx)
	require.NoError(t, err)

	// Cancel context and close the server end of the pipe to unblock reads.
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Close()  // Unblock ReadFull on the client side
		cancel()
	}()

	// Run should exit when context is cancelled and connections are closed.
	_ = sess.Run(ctx)
	client.Close()
}
// TestSessionStartPassesScrcpyConfig verifies that scrcpy tunable fields
// from SessionOpts are forwarded to LaunchOptions in Start().
func TestSessionStartPassesScrcpyConfig(t *testing.T) {
	result := createMockLaunchResult()
	srv, client := net.Pipe()
	defer srv.Close()
	result.VideoConn = client

	launcher := &capturingLauncher{result: result}
	opts := SessionOpts{
		BufFrames:      60,
		MaxConsecDrops: 120,
		AudioEnabled:   true,
		ScrcpyCodec:       "h265",
		ScrcpyMaxSize:     1920,
		ScrcpyBitRate:     8000000,
		ScrcpyMaxFPS:      30,
		ScrcpyAudioCodec:  "aac",
		ScrcpyAudioSource: "mic",
	}
	sess := NewDeviceSession("TESTDEV", nil, launcher, opts)

	ctx := context.Background()
	err := sess.Start(ctx)
	require.NoError(t, err)
	defer sess.Close(ctx)
	defer srv.Close()

	// Verify LaunchOptions received the scrcpy tunables.
	assert.Equal(t, "h265", launcher.captured.Codec)
	assert.Equal(t, 1920, launcher.captured.MaxSize)
	assert.Equal(t, 8000000, launcher.captured.BitRate)
	assert.Equal(t, 30, launcher.captured.MaxFPS)
	assert.Equal(t, "aac", launcher.captured.AudioCodec)
	assert.Equal(t, "mic", launcher.captured.AudioSource)
	assert.True(t, launcher.captured.AudioEnabled)
	assert.True(t, launcher.captured.ControlEnabled)
}

// capturingLauncher captures the LaunchOptions for assertion.
type capturingLauncher struct {
	result   *scrcpy.LaunchResult
	err      error
	captured scrcpy.LaunchOptions
	mu       sync.Mutex
}

func (l *capturingLauncher) LaunchWithOptions(ctx context.Context, serial string, opts scrcpy.LaunchOptions) (*scrcpy.LaunchResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.captured = opts
	if l.err != nil {
		return nil, l.err
	}
	return l.result, nil
}

// TestDefaultSessionOptsScrcpyZeroValues verifies that DefaultSessionOpts
// produces zero-value scrcpy fields (which mean "use server defaults").
func TestDefaultSessionOptsScrcpyZeroValues(t *testing.T) {
	opts := DefaultSessionOpts()
	assert.Equal(t, "", opts.ScrcpyCodec)
	assert.Equal(t, 0, opts.ScrcpyMaxSize)
	assert.Equal(t, 0, opts.ScrcpyBitRate)
	assert.Equal(t, 0, opts.ScrcpyMaxFPS)
	assert.Equal(t, "", opts.ScrcpyAudioCodec)
	assert.Equal(t, "", opts.ScrcpyAudioSource)
}
