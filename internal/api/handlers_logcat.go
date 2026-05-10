package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// StreamLogcat is the WebSocket handler for OPS-05: streams the per-device
// logcat ring snapshot followed by a live tail of newly-appended lines as
// text frames (D-01).
//
// Acceptance gate: device must exist in the registry AND its session must
// be in StateActive OR StateReconnecting. Per Pitfall 1, the logcat
// buffer survives recovery, so a /logcat WS connection is valid while
// recovery is in flight.
//
// Late-joiner contract: snapshot lines are written first (one text frame
// per line, oldest -> newest), then live lines arrive on the per-subscriber
// channel. The Subscribe call atomically takes the snapshot and registers
// the subscriber under a single write lock so no lines are missed or
// duplicated between snapshot and live tail.
//
// Slow-consumer policy: identical to Phase 2 D-04/D-05 — when the
// subscriber's send channel saturates for `EvictionThreshold` consecutive
// Append calls, the LogcatBuffer evicts the subscriber by closing its
// channel; this handler maps that close to a WS close with code
// StatusPolicyViolation / "slow_consumer".
func StreamLogcat(registry *session.Registry, allowedOrigins []string, cfg *config.Config) http.HandlerFunc {
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
		sess := entry.GetSession()
		if sess == nil {
			writeError(w, ErrDeviceOffline)
			return
		}
		st := sess.State()
		if st != session.StateActive && st != session.StateReconnecting {
			writeError(w, ErrDeviceOffline)
			return
		}
		buf := sess.LogcatBuffer()
		if buf == nil {
			// Active session with no logcat buffer attached (e.g. logcat
			// reader not wired). Treat as offline for the /logcat surface.
			writeError(w, ErrDeviceOffline)
			return
		}

		opts := buildAcceptOptions(allowedOrigins, r)
		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			slog.Error("ws logcat accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()
		applyWSDefaults(ws, cfg)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		viewerID := uuid.New()
		slog.Info("logcat viewer connected", "device", serial, "viewer_id", viewerID.String())

		snapshot, ch, unsub := buf.Subscribe(viewerID)
		defer unsub()

		// 1. Replay snapshot, one text frame per line.
		for _, line := range snapshot {
			if err := ws.Write(ctx, websocket.MessageText, []byte(line)); err != nil {
				return
			}
		}

		// 2. Live tail. Channel close = eviction (slow_consumer).
		// Run a ping loop in parallel so idle connections close cleanly.
		pingErr := make(chan error, 1)
		go func() {
			pingErr <- pingLoop(ctx, ws, cfg)
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case err := <-pingErr:
				ws.Close(websocket.StatusGoingAway, "idle timeout")
				_ = err
				return
			case line, ok := <-ch:
				if !ok {
					ws.Close(websocket.StatusPolicyViolation, "slow_consumer")
					slog.Info("logcat viewer evicted",
						"device", serial,
						"viewer_id", viewerID.String(),
						"reason", "slow_consumer",
					)
					return
				}
				if err := ws.Write(ctx, websocket.MessageText, []byte(line)); err != nil {
					return
				}
			}
		}
	}
}

// Compile-time assertion that this file uses fmt only when needed; helps
// keep imports tidy if the file is later edited.
var _ = fmt.Sprintf
