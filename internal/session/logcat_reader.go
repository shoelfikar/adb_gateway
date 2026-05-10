// Package session — logcat_reader.go contains the per-device logcat reader
// loop for Plan 03-03. The reader runs `adb shell logcat -v threadtime`
// under the supervisor errgroup and appends every line to the per-device
// LogcatBuffer.
//
// Pitfall 1 (RESEARCH.md): logcat-process EOF must NOT propagate up to the
// errgroup, or it would cancel video/audio/control siblings. The reader
// uses cenkalti/backoff/v4 to retry indefinitely until ctx is cancelled,
// and only ever returns ctx.Err() (or nil — but errgroup treats nil as
// success). On a benign EOF we sleep a backoff interval and retry; on
// ctx.Done() we return ctx.Err().
package session

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// AttachLogcatReader wires a logcat reader onto this session. Must be
// called BEFORE Run if the caller wants the reader goroutine to run. The
// reader appends each `logcat -v threadtime` line to the LogcatBuffer
// previously attached via AttachLogcatBuffer.
//
// runner is typically *adb.HostServices (production) or a fake (tests).
// If logcatBuffer is nil at Run time, the reader returns immediately
// (still under errgroup; returns nil, no sibling cancellation).
func (s *DeviceSession) AttachLogcatReader(runner LogcatShellRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logcatRunner = runner
}

// logcatReaderLoop is the goroutine body. It survives logcat-process EOF
// and only returns on ctx cancellation.
func (s *DeviceSession) logcatReaderLoop(ctx context.Context) error {
	s.mu.Lock()
	runner := s.logcatRunner
	buf := s.logcatBuffer
	serial := s.Serial
	log := s.log
	s.mu.Unlock()

	if runner == nil || buf == nil {
		// Nothing to do — return nil so the errgroup is unaffected.
		return nil
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 1 * time.Second
	bo.MaxInterval = 30 * time.Second
	bo.MaxElapsedTime = 0 // retry indefinitely until ctx cancel

	op := func() error {
		if err := ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		stdout, stderr, _, err := runner.ShellV2Stream(ctx, serial, "logcat -v threadtime")
		if err != nil {
			return err // retryable
		}
		// Drain stderr in the background to avoid back-pressure.
		go func() {
			if stderr != nil {
				_, _ = bufio.NewReader(stderr).WriteTo(devNull{})
				_ = stderr.Close()
			}
		}()
		defer func() {
			if stdout != nil {
				_ = stdout.Close()
			}
		}()

		sc := bufio.NewScanner(stdout)
		// Allow long lines (logcat threadtime can have very long messages).
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return backoff.Permanent(err)
			}
			buf.Append(sc.Text())
		}
		// EOF: trigger backoff retry by returning a non-nil error.
		if err := sc.Err(); err != nil {
			return err
		}
		// Clean EOF — return a sentinel so backoff retries.
		return errLogcatEOF
	}

	notify := func(err error, next time.Duration) {
		if errors.Is(err, errLogcatEOF) {
			log.Debug("logcat: reader EOF, restarting", "next_in", next)
		} else {
			log.Warn("logcat: reader error, retrying", "error", err, "next_in", next)
		}
	}

	err := backoff.RetryNotify(op, backoff.WithContext(bo, ctx), notify)
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	// Suppress non-ctx errors so the errgroup is not killed (Pitfall 1).
	return nil
}

var errLogcatEOF = errors.New("logcat reader: clean EOF")

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// Suppress "unused import" if slog ever drops out — kept for future use.
var _ = slog.Default
