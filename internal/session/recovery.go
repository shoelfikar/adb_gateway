// Package session — recovery.go implements the auto-recovery orchestrator
// for Plan 03-02. When the StallWatchdog signals onStall, the supervisor
// invokes Recovery.Run on a separate goroutine. Recovery transitions the
// session to StateReconnecting, retries scrcpy launch up to maxAttempts
// times via cenkalti/backoff/v4, and finally transitions to either
// StateActive (success) or StateFailed (sticky exhaustion).
//
// Lock discipline (Pitfall 9, PATTERNS.md "Per-Device Mutex Discipline"):
//   - Acquire s.mu, transition Active -> Reconnecting, release.
//   - Run the backoff loop WITHOUT holding s.mu — the launcher I/O can
//     take many seconds and must never block other ops on the same device.
//   - Re-acquire s.mu briefly to commit the terminal state.
//
// The orchestrator emits a structured log on every retry (RetryNotify) and
// updates the gateway_session_state gauge after every transition.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/pelni/adb-gateway/internal/scrcpy"
)

// RecoveryOpts configures a Recovery orchestrator. Launcher is required.
type RecoveryOpts struct {
	// Launcher is invoked on each retry attempt.
	Launcher Launcher
	// MaxAttempts caps the total number of launcher invocations. 0 means
	// the package default (3).
	MaxAttempts uint64
	// Backoff supplies the inter-attempt delay schedule. nil means a
	// reasonable default: 1s initial, 30s max, indefinite (capped by
	// MaxAttempts via WithMaxRetries).
	Backoff backoff.BackOff
	// Log is used for retry warnings and terminal state info.
	Log *slog.Logger
}

// Recovery is the auto-recovery orchestrator. It is stateless across calls
// to Run; the Recovery instance can be reused for many devices serially or
// shared (each call gets its own backoff state via reset).
type Recovery struct {
	launcher    Launcher
	maxAttempts uint64
	bo          backoff.BackOff
	log         *slog.Logger
}

// NewRecovery constructs a Recovery orchestrator.
func NewRecovery(opts RecoveryOpts) *Recovery {
	if opts.Launcher == nil {
		panic("session: NewRecovery requires a Launcher")
	}
	max := opts.MaxAttempts
	if max == 0 {
		max = 3
	}
	bo := opts.Backoff
	if bo == nil {
		exp := backoff.NewExponentialBackOff()
		exp.InitialInterval = 1 * time.Second
		exp.MaxInterval = 30 * time.Second
		exp.MaxElapsedTime = 0
		bo = exp
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Recovery{
		launcher:    opts.Launcher,
		maxAttempts: max,
		bo:          bo,
		log:         log,
	}
}

// Run drives the recovery loop for a single DeviceSession. The session must
// currently be in StateActive (the watchdog only fires on active devices).
//
// Outcomes:
//   - All attempts succeed before maxAttempts: state -> Active, returns nil.
//   - All attempts fail (or ctx cancelled): state -> Failed, returns error.
//
// The error returned is the last launcher error (or ctx.Err() on cancel).
// The caller (typically the supervisor's onStall trampoline goroutine) is
// expected to log it and not retry — sticky Failed only reverses via the
// manual POST /restart endpoint.
func (r *Recovery) Run(ctx context.Context, sess *DeviceSession) error {
	// Step 1: Active -> Reconnecting under per-device lock.
	sess.mu.Lock()
	if _, err := sess.transitionLocked(StateReconnecting); err != nil {
		sess.mu.Unlock()
		return fmt.Errorf("recovery: cannot enter reconnecting from %s: %w", sess.state, err)
	}
	opts := sess.launchOpts
	serial := sess.Serial
	sess.mu.Unlock()

	r.log.Info("recovery: starting",
		"device", serial,
		"max_attempts", r.maxAttempts,
	)

	// Step 2: backoff loop WITHOUT holding s.mu — launcher I/O can take
	// many seconds and must not block other ops on the same device.
	r.bo.Reset()
	limited := backoff.WithMaxRetries(r.bo, r.maxAttempts-1) // N retries == N+1 attempts
	limited = backoff.WithContext(limited, ctx)

	var lastResult *scrcpy.LaunchResult
	op := func() error {
		// Honour ctx cancellation BEFORE attempting (Permanent so the
		// backoff loop exits immediately rather than scheduling another).
		if err := ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		res, err := r.launcher.LaunchWithOptions(ctx, serial, opts)
		if err != nil {
			return err
		}
		lastResult = res
		return nil
	}
	notify := func(err error, next time.Duration) {
		r.log.Warn("recovery: attempt failed",
			"device", serial,
			"error", err,
			"next_in", next,
		)
	}

	runErr := backoff.RetryNotify(op, limited, notify)

	// Step 3: re-acquire lock and commit the terminal state.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// If a normal stop intervened (DELETE /sessions transitioned us to
	// Stopping), respect that: don't override.
	if sess.state == StateStopping {
		r.log.Info("recovery: aborted by concurrent stop", "device", serial)
		return runErr
	}

	if runErr == nil {
		// Success: install the new resources and go Active.
		if lastResult != nil {
			sess.applyLaunchResultLocked(lastResult)
		}
		if _, err := sess.transitionLocked(StateActive); err != nil {
			r.log.Error("recovery: cannot mark active after successful relaunch",
				"device", serial, "error", err,
			)
			return err
		}
		r.log.Info("recovery: succeeded", "device", serial)
		return nil
	}

	// Failure: sticky Failed.
	if _, err := sess.transitionLocked(StateFailed); err != nil {
		// We are already in Reconnecting; transition Reconnecting->Failed
		// is valid by the FSM, so this branch should be unreachable. Log
		// and surface the original launch error regardless.
		r.log.Error("recovery: cannot mark failed",
			"device", serial, "error", err,
		)
	}
	r.log.Warn("recovery: exhausted",
		"device", serial,
		"attempts", r.maxAttempts,
		"error", runErr,
	)
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return runErr
	}
	return fmt.Errorf("recovery exhausted after %d attempts: %w", r.maxAttempts, runErr)
}

// applyLaunchResultLocked replaces the session's connection set with the
// freshly launched one. Caller MUST hold s.mu. Existing resources are NOT
// closed here — the supervisor's video/audio/control reader goroutines are
// expected to have unwound from EOF on the previous (stalled) connections.
// Recovery's job is to install the new pointers; reader-loop re-attachment
// is owned by the supervisor in 03-03/03-04.
func (s *DeviceSession) applyLaunchResultLocked(result *scrcpy.LaunchResult) {
	s.videoConn = result.VideoConn
	s.videoLn = result.VideoLn
	s.reverseMap = result.ReverseMap
	s.codecMeta = result.CodecMeta
	s.deviceName = result.DeviceName
	s.scid = result.SCID
	s.cleanup = result.Cleanup
	s.audioConn = result.AudioConn
	s.controlConn = result.ControlConn
	s.audioAvailable = result.AudioAvailable
	s.audioCodec = result.AudioCodec
}
