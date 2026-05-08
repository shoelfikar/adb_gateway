package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/session"
)

// leaseResponse is the JSON shape returned by all three reservation handlers.
type leaseResponse struct {
	LeaseID   string `json:"lease_id"`
	ExpiresAt string `json:"expires_at"` // RFC3339
}

type leaseRequestBody struct {
	LeaseID string `json:"lease_id"`
}

// ownerKeyFromRequest derives a non-reversible fingerprint of the API key
// for binding a lease to its acquirer. Used as Lease.OwnerKey.
func ownerKeyFromRequest(r *http.Request) string {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		// Subprotocol path (browser WS clients with no header support).
		key = extractAPIKeyFromSubprotocol(r)
	}
	if key == "" {
		return ""
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// CreateReservation handles POST /devices/{serial}/reservation (CTL-02).
func CreateReservation(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		mgr := entry.GetLeaseManager()
		if mgr == nil {
			writeError(w, ErrDeviceNotFound)
			return
		}
		ownerKey := ownerKeyFromRequest(r)
		if ownerKey == "" {
			writeError(w, ErrUnauthorized)
			return
		}
		l, err := mgr.Acquire(ownerKey)
		if err != nil {
			if errors.Is(err, session.ErrLeaseHeldByOther) {
				writeError(w, ErrLeaseHeldByOther)
				return
			}
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, leaseResponse{
			LeaseID:   l.ID,
			ExpiresAt: l.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// ExtendReservation handles PATCH /devices/{serial}/reservation.
func ExtendReservation(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		mgr := entry.GetLeaseManager()
		if mgr == nil {
			writeError(w, ErrDeviceNotFound)
			return
		}
		var body leaseRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LeaseID == "" {
			writeError(w, ErrLeaseInvalid)
			return
		}
		l, err := mgr.Extend(body.LeaseID)
		if err != nil {
			if errors.Is(err, session.ErrLeaseNotFound) || errors.Is(err, session.ErrLeaseMismatch) {
				writeError(w, ErrLeaseInvalid)
				return
			}
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, leaseResponse{
			LeaseID:   l.ID,
			ExpiresAt: l.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// ReleaseReservation handles DELETE /devices/{serial}/reservation.
func ReleaseReservation(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		mgr := entry.GetLeaseManager()
		if mgr == nil {
			writeError(w, ErrDeviceNotFound)
			return
		}
		var body leaseRequestBody
		// DELETE may carry a body or X-Lease-ID header; accept both.
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		if body.LeaseID == "" {
			body.LeaseID = r.Header.Get("X-Lease-ID")
		}
		if body.LeaseID == "" {
			writeError(w, ErrLeaseInvalid)
			return
		}
		if err := mgr.Release(body.LeaseID); err != nil {
			if errors.Is(err, session.ErrLeaseNotFound) || errors.Is(err, session.ErrLeaseMismatch) {
				writeError(w, ErrLeaseInvalid)
				return
			}
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}