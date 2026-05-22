package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// StreamVideo returns an HTTP handler that upgrades the connection to a WebSocket
// and streams video frames from the device's Hub (fan-out) to the client.
//
// Phase 2: replaces Phase 1's direct-relay approach with Hub.Subscribe.
// Multiple viewers can connect simultaneously; each receives the same frames.
// Late joiners receive cached codec metadata + most recent keyframe before
// the live tail (STR-07).
//
// Per STR-08: ping loop with idle disconnect.
// Per STR-09: SetReadLimit applied on every connection.
func StreamVideo(registry *session.Registry, allowedOrigins []string, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		slog.Info("ws video handler entered", "serial", serial, "method", r.Method, "path", r.URL.Path, "upgrade", r.Header.Get("Upgrade"))
		if serial == "" {
			slog.Error("ws video: serial is empty", "path", r.URL.Path)
			writeError(w, ErrDeviceNotFound)
			return
		}

		if !serialPattern.MatchString(serial) {
			slog.Error("ws video: serial pattern mismatch", "serial", serial)
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry, ok := registry.Get(serial)
		if !ok {
			slog.Error("ws video: device not in registry", "device", serial)
			writeError(w, ErrDeviceOffline)
			return
		}

		sess := entry.GetSession()
		if sess == nil {
			slog.Error("ws video: session is nil", "device", serial)
			writeError(w, ErrDeviceOffline)
			return
		}
		if sess.State() != session.StateActive {
			slog.Error("ws video: session not active", "device", serial, "state", sess.State())
			writeError(w, ErrDeviceOffline)
			return
		}

		hub := sess.VideoHub()
		if hub == nil {
			slog.Error("ws video: video hub is nil", "device", serial)
			writeError(w, ErrDeviceOffline)
			return
		}

		opts := buildAcceptOptions(allowedOrigins, r)

		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			slog.Error("ws video accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()

		applyWSDefaults(ws, cfg)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		viewerID := uuid.NewString()
		slog.Info("video viewer connected", "device", serial, "viewer_id", viewerID)

		// Track the final relay error so the deferred close-code logger can
		// attribute the disconnect. See debug session ws-disconnect-remote-stream.
		var relayErr error
		defer func() {
			closeCode := websocket.CloseStatus(relayErr)
			slog.Info("video viewer disconnected",
				"device", serial,
				"viewer_id", viewerID,
				"close_code", int(closeCode),
				"error", relayErr,
			)
		}()

		relayErr = subscribeAndRelay(ctx, ws, hub, "video", viewerID, cfg)
		if relayErr != nil && ctx.Err() == nil {
			slog.Info("video relay ended", "device", serial, "viewer_id", viewerID, "error", relayErr)
		}
	}
}
