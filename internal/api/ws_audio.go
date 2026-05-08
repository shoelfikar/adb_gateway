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

// StreamAudio mirrors StreamVideo for the audio stream (STR-02).
// Returns 404 AUDIO_UNAVAILABLE per D-12 when DeviceEntry.AudioAvailable=false.
func StreamAudio(registry *session.Registry, allowedOrigins []string, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" {
			writeError(w, ErrDeviceNotFound)
			return
		}
		if !serialPattern.MatchString(serial) {
			writeError(w, ErrDeviceNotFound)
			return
		}

		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceOffline)
			return
		}
		if !entry.GetAudioAvailable() {
			writeError(w, ErrAudioUnavailable) // 404 per D-12 / D-19
			return
		}

		sess := entry.GetSession()
		if sess == nil || sess.State() != session.StateActive {
			writeError(w, ErrDeviceOffline)
			return
		}
		hub := sess.AudioHub()
		if hub == nil {
			// Defense in depth: AudioAvailable was true but Hub is nil
			// (could happen during shutdown). Return 404 cleanly.
			writeError(w, ErrAudioUnavailable)
			return
		}

		opts := buildAcceptOptions(allowedOrigins, r)

		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			slog.Error("ws audio accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()

		applyWSDefaults(ws, cfg)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		viewerID := uuid.NewString()
		slog.Info("audio viewer connected", "device", serial, "viewer_id", viewerID)

		if err := subscribeAndRelay(ctx, ws, hub, "audio", viewerID, cfg); err != nil {
			if ctx.Err() == nil {
				slog.Info("audio relay ended", "device", serial, "viewer_id", viewerID, "error", err)
			}
		}
	}
}