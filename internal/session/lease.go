// Package session manages device registry and session lifecycle state.
// lease.go implements the per-device reservation lease state machine
// (CTL-01..05). Lifecycle:
//
//	[no lease] --Acquire--> [held]
//	[held]     --Extend--> [held] (TTL refreshed to full TTL per D-08)
//	[held]     --Release--> [no lease]
//	[held]     --TTL expired--> [no lease] + signal "expired"
//	[held]     --BeginGrace (controller WS disconnect)--> [held, in grace]
//	[held, in grace] --CancelGrace (PATCH/DELETE during grace)--> [held]
//	[held, in grace] --grace timer fires--> [no lease] + signal "expired"
//	[held]     --ForceRelease(reason)--> [no lease] + signal reason
//
// The LeaseManager lives as a field on DeviceEntry (registered in plan 02-05);
// its mutex is independent of DeviceEntry.mu to avoid lock-order issues
// when the entry's session is being torn down.
package session

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ReleaseReason classifies why a lease was released. Sent on Lease.ReleaseChan.
type ReleaseReason string

const (
	ReasonExpired        ReleaseReason = "expired"
	ReasonAdminRevoked   ReleaseReason = "admin_revoked"
	ReasonDeviceGone     ReleaseReason = "device_gone"
	ReasonClientReleased ReleaseReason = "client_released" // explicit DELETE
)

// Errors returned by LeaseManager methods.
var (
	ErrLeaseHeldByOther = errors.New("lease: held by another client")
	ErrLeaseNotFound    = errors.New("lease: no lease held")
	ErrLeaseMismatch    = errors.New("lease: provided ID does not match")
)

// Lease is a snapshot of an active reservation. Returned by Acquire/Extend
// for the API layer to serialize. Mutable internal state (timers) lives in
// LeaseManager — Lease itself is immutable post-construction.
type Lease struct {
	ID        string
	OwnerKey  string    // API-key fingerprint of the holder; binds lease to acquirer
	ExpiresAt time.Time // RFC3339 in API responses
}

// LeaseManager owns the lease state for a single device. Methods are
// thread-safe; the mutex is internal (not exposed) to prevent deadlock
// with DeviceEntry.mu — caller MUST NOT hold DeviceEntry.mu when calling
// LeaseManager methods (verified by Phase 1's existing pattern of
// releasing entry.mu before long ops).
type LeaseManager struct {
	mu sync.Mutex

	// current lease (nil = no lease held)
	cur *internalLease

	ttl       time.Duration // from cfg.Control.LeaseTTLSeconds
	graceDur  time.Duration // 5s per D-10

	log *slog.Logger
}

// internalLease holds mutable state (timer pointer, release channel).
// Kept private so the Lease value handed to API consumers is immutable.
type internalLease struct {
	snapshot   Lease
	timer      *time.Timer        // expiry timer (or grace timer when graceUntil != zero)
	graceUntil time.Time          // non-zero when in grace period (D-10)
	releaseCh  chan ReleaseReason // buffered(1) per RESEARCH.md Pattern 5
}

// NewLeaseManager constructs a manager with the given TTL (CTL-02 default 60s).
// Grace period is fixed at 5s per D-10.
func NewLeaseManager(ttl time.Duration, log *slog.Logger) *LeaseManager {
	if log == nil {
		log = slog.Default()
	}
	return &LeaseManager{
		ttl:      ttl,
		graceDur: 5 * time.Second,
		log:      log,
	}
}

// newLeaseManagerForTest is a test affordance allowing customization of
// the grace duration. Not exported.
func newLeaseManagerForTest(ttl, grace time.Duration, log *slog.Logger) *LeaseManager {
	m := NewLeaseManager(ttl, log)
	m.graceDur = grace
	return m
}

// Acquire attempts to take the lease for ownerKey. Succeeds only if
// (a) no lease is held, OR (b) the held lease has already expired but
// the timer hasn't fired yet (treat as released). Otherwise returns
// ErrLeaseHeldByOther.
//
// Returns the new Lease snapshot; ReleaseChan can be obtained via
// ReleaseChanFor(leaseID) if the caller (control WS handler) needs
// to listen for force-release events.
func (m *LeaseManager) Acquire(ownerKey string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if m.cur != nil {
		// If currently in grace OR not expired, deny.
		if !m.cur.graceUntil.IsZero() && now.Before(m.cur.graceUntil) {
			return Lease{}, ErrLeaseHeldByOther
		}
		if m.cur.graceUntil.IsZero() && now.Before(m.cur.snapshot.ExpiresAt) {
			return Lease{}, ErrLeaseHeldByOther
		}
		// Otherwise the existing lease is effectively expired; reap it.
		m.reapLockedLocked(ReasonExpired)
	}

	l := &internalLease{
		snapshot: Lease{
			ID:        uuid.New().String(),
			OwnerKey:  ownerKey,
			ExpiresAt: now.Add(m.ttl),
		},
		releaseCh: make(chan ReleaseReason, 1),
	}
	l.timer = time.AfterFunc(m.ttl, func() { m.expireFromTimer(l.snapshot.ID) })
	m.cur = l
	m.log.Info("lease acquired", "lease_id", l.snapshot.ID, "owner_key_fp", fingerprint(ownerKey), "expires_at", l.snapshot.ExpiresAt.Format(time.RFC3339))
	return l.snapshot, nil
}

// Extend resets the TTL to a full m.ttl IF leaseID matches and the
// lease is held (or in grace — PATCH during grace re-anchors per D-10).
func (m *LeaseManager) Extend(leaseID string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cur == nil {
		return Lease{}, ErrLeaseNotFound
	}
	if !ctEqual(m.cur.snapshot.ID, leaseID) {
		return Lease{}, ErrLeaseMismatch
	}

	// PATCH during grace re-anchors per D-10: clear graceUntil,
	// restart timer at full TTL.
	m.cur.graceUntil = time.Time{}
	if m.cur.timer != nil {
		m.cur.timer.Stop()
	}
	m.cur.snapshot.ExpiresAt = time.Now().Add(m.ttl)
	leaseIDCopy := m.cur.snapshot.ID
	m.cur.timer = time.AfterFunc(m.ttl, func() { m.expireFromTimer(leaseIDCopy) })
	m.log.Info("lease extended", "lease_id", m.cur.snapshot.ID, "expires_at", m.cur.snapshot.ExpiresAt.Format(time.RFC3339))
	return m.cur.snapshot, nil
}

// Release explicitly releases the lease. Caller must supply the matching
// leaseID. Sends ReasonClientReleased on the release channel.
func (m *LeaseManager) Release(leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return ErrLeaseNotFound
	}
	if !ctEqual(m.cur.snapshot.ID, leaseID) {
		return ErrLeaseMismatch
	}
	m.reapLockedLocked(ReasonClientReleased)
	return nil
}

// ForceRelease releases the lease with the given reason regardless of
// ID match. Used for admin revoke (D-09) and device-gone (D-09).
func (m *LeaseManager) ForceRelease(reason ReleaseReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return
	}
	m.reapLockedLocked(reason)
}

// BeginGrace starts the 5s grace timer (D-10). Called when the controller
// WS disconnects unexpectedly. Idempotent: calling twice is a no-op.
func (m *LeaseManager) BeginGrace(leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return ErrLeaseNotFound
	}
	if !ctEqual(m.cur.snapshot.ID, leaseID) {
		return ErrLeaseMismatch
	}
	if !m.cur.graceUntil.IsZero() {
		return nil // already in grace
	}
	if m.cur.timer != nil {
		m.cur.timer.Stop()
	}
	m.cur.graceUntil = time.Now().Add(m.graceDur)
	leaseIDCopy := m.cur.snapshot.ID
	m.cur.timer = time.AfterFunc(m.graceDur, func() { m.expireFromTimer(leaseIDCopy) })
	m.log.Info("lease entered grace period", "lease_id", m.cur.snapshot.ID, "grace_until", m.cur.graceUntil.Format(time.RFC3339))
	return nil
}

// CancelGrace re-anchors the lease (returns to held, full TTL).
// Used when the controller reconnects within the grace window.
func (m *LeaseManager) CancelGrace(leaseID string) error {
	// Identical effect to Extend — easier to call this from the WS handler
	// on reconnect without exposing TTL math.
	_, err := m.Extend(leaseID)
	return err
}

// IsHeldBy reports whether leaseID matches the current held lease.
// Constant-time compare per Phase 1 auth pattern.
func (m *LeaseManager) IsHeldBy(leaseID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return false
	}
	// Reject if grace timer is about to fire OR already expired.
	now := time.Now()
	if !m.cur.graceUntil.IsZero() && now.After(m.cur.graceUntil) {
		return false
	}
	if m.cur.graceUntil.IsZero() && now.After(m.cur.snapshot.ExpiresAt) {
		return false
	}
	return ctEqual(m.cur.snapshot.ID, leaseID)
}

// ReleaseChanFor returns the receive-only channel that will get the
// ReleaseReason when the lease ends. Returns nil if leaseID doesn't
// match (the channel is per-lease, never reused).
func (m *LeaseManager) ReleaseChanFor(leaseID string) <-chan ReleaseReason {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || !ctEqual(m.cur.snapshot.ID, leaseID) {
		return nil
	}
	return m.cur.releaseCh
}

// Snapshot returns the current Lease snapshot (or zero-Lease + false).
func (m *LeaseManager) Snapshot() (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return Lease{}, false
	}
	return m.cur.snapshot, true
}

// expireFromTimer is the AfterFunc callback. It only releases if the
// current lease ID still matches (covers the race where the lease was
// explicitly Released and then a new one Acquired before the old timer
// fired).
func (m *LeaseManager) expireFromTimer(expectedID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || !ctEqual(m.cur.snapshot.ID, expectedID) {
		return
	}
	m.reapLockedLocked(ReasonExpired)
}

// reapLockedLocked tears down m.cur, signals the reason on the release
// channel (non-blocking — buffered(1)), and stops the timer (Pitfall 9).
// Caller MUST hold m.mu.
func (m *LeaseManager) reapLockedLocked(reason ReleaseReason) {
	if m.cur == nil {
		return
	}
	if m.cur.timer != nil {
		m.cur.timer.Stop()
		m.cur.timer = nil
	}
	// Non-blocking send: channel is buffered(1) and only one reason is
	// ever sent per lease. If the consumer is gone, we drop silently.
	select {
	case m.cur.releaseCh <- reason:
	default:
	}
	close(m.cur.releaseCh)
	m.log.Info("lease released",
		"lease_id", m.cur.snapshot.ID,
		"reason", string(reason),
	)
	m.cur = nil
}

// ctEqual returns true iff a and b are equal under constant-time compare.
// Lengths are NOT pre-hashed — UUID strings are fixed length, so the
// length-leak inherent in ConstantTimeCompare's len-check is acceptable.
func ctEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// fingerprint returns a short non-reversible identifier for an API key
// suitable for logging. Today this is just a length+prefix sketch — the
// OwnerKey field is populated by the WS layer (plan 02-06) with whatever
// identifier it wants (typically the SHA-256 hex of the API key).
func fingerprint(s string) string {
	if len(s) == 0 {
		return "(empty)"
	}
	if len(s) <= 8 {
		return "(short)"
	}
	return s[:8] + "..."
}