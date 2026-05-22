package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// StreamSession exposes a unified WS endpoint: video frames fan out to the
// client, and (when ?control=1) control messages flow back over the same
// connection. The lease is acquired implicitly on upgrade and released on
// close, eliminating the POST /reservation + separate /control WS dance
// that previously caused frequent re-acquire cycles for interactive users.
//
// Wire protocol:
//   - Server → Client: binary frames = video; text frames = JSON events.
//     First text frame after upgrade (control mode): {"type":"lease","id":..,"expires_at":..}.
//     Force-release: {"type":"lease_released","reason":..} followed by close.
//     Control payload errors: {"error":{"code":..,"message":..}}.
//   - Client → Server: text frames = control envelopes (only when control=1).
func StreamSession(registry *session.Registry, allowedOrigins []string, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" || !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceOffline)
			return
		}
		sess := entry.GetSession()
		if sess == nil || sess.State() != session.StateActive {
			writeError(w, ErrDeviceOffline)
			return
		}
		hub := sess.VideoHub()
		if hub == nil {
			writeError(w, ErrDeviceOffline)
			return
		}

		wantControl := r.URL.Query().Get("control") == "1"

		var (
			mgr   *session.LeaseManager
			lease session.Lease
			cw    *scrcpy.ControlWriter
		)
		if wantControl {
			mgr = entry.GetLeaseManager()
			if mgr == nil {
				writeError(w, ErrDeviceOffline)
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
			lease = l
			cw = sess.ControlWriter()
			if cw == nil {
				mgr.ForceRelease(session.ReasonDeviceGone)
				writeError(w, ErrDeviceOffline)
				return
			}
		}

		opts := buildAcceptOptions(allowedOrigins, r)
		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			if wantControl {
				mgr.ForceRelease(session.ReasonClientReleased)
			}
			slog.Error("ws session accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()
		applyWSDefaults(ws, cfg)

		// Release lease on disconnect — no grace, the WS lifetime IS the lease.
		if wantControl {
			defer mgr.ForceRelease(session.ReasonClientReleased)
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		viewerID := uuid.NewString()
		log := slog.With("device", serial, "viewer_id", viewerID, "control", wantControl)
		log.Info("session connected")

		if wantControl {
			payload, _ := json.Marshal(map[string]any{
				"type":       "lease",
				"id":         lease.ID,
				"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
			})
			if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
				return
			}
		}

		ch, unsub, err := hub.Subscribe(viewerID)
		if err != nil {
			log.Error("hub subscribe failed", "error", err)
			return
		}
		defer unsub()

		readErr := make(chan error, 1)
		go func() {
			for {
				msgType, raw, err := ws.Read(ctx)
				if err != nil {
					readErr <- err
					return
				}
				if !wantControl {
					continue
				}
				if msgType != websocket.MessageText {
					writeWSError(ctx, ws, cfg, "INVALID_MESSAGE_TYPE", "control messages must be text JSON")
					continue
				}
				cmsg, derr := decodeControlEnvelope(raw)
				if derr != nil {
					code := "INVALID_CONTROL_MESSAGE"
					if errors.Is(derr, scrcpy.ErrUnknownControlType) {
						code = "UNKNOWN_CONTROL_TYPE"
					} else if errors.Is(derr, scrcpy.ErrControlPayloadTooLarge) {
						code = "CONTROL_PAYLOAD_TOO_LARGE"
					}
					writeWSError(ctx, ws, cfg, code, derr.Error())
					continue
				}
				if !mgr.IsHeldBy(lease.ID) {
					writeWSError(ctx, ws, cfg, "NOT_CONTROLLER", "lease no longer held")
					ws.Close(4001, "lease_lost")
					readErr <- errors.New("lease lost")
					return
				}
				select {
				case cw.In() <- cmsg:
				case <-ctx.Done():
					readErr <- ctx.Err()
					return
				}
			}
		}()

		var releaseCh <-chan session.ReleaseReason
		if wantControl {
			releaseCh = mgr.ReleaseChanFor(lease.ID)
		}

		pingErr := make(chan error, 1)
		go func() { pingErr <- pingLoop(ctx, ws, cfg) }()

		for {
			select {
			case <-ctx.Done():
				return
			case err := <-readErr:
				log.Info("session reader ended", "error", err)
				return
			case err := <-pingErr:
				ws.Close(websocket.StatusGoingAway, "idle timeout")
				log.Info("session ping idle", "error", err)
				return
			case reason, ok := <-releaseCh:
				if !ok {
					return
				}
				payload, _ := json.Marshal(map[string]string{
					"type":   "lease_released",
					"reason": string(reason),
				})
				_ = ws.Write(ctx, websocket.MessageText, payload)
				ws.Close(websocket.StatusNormalClosure, "lease_released")
				return
			case msg, ok := <-ch:
				if !ok {
					ws.Close(websocket.StatusPolicyViolation, "slow_consumer")
					return
				}
				if err := ws.Write(ctx, websocket.MessageBinary, msg); err != nil {
					log.Info("video write failed", "error", err)
					return
				}
			}
		}
	}
}
