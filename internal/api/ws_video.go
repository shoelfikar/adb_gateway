package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// StreamVideo returns an HTTP handler that upgrades the connection to a WebSocket
// and relays video frames from the scrcpy server to the WebSocket client.
//
// Phase 1: single-viewer only. No fan-out, no Hub, no frame dropping.
// The first WebSocket client connected to a device's video endpoint receives
// the codec metadata as the first binary message, then raw H.264 frames
// (12-byte header + payload) as subsequent binary messages.
//
// Per STR-01 and T-05-01: Auth middleware validates API key on the HTTP upgrade
// request before WebSocket.Accept is called.
func StreamVideo(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial := chi.URLParam(r, "serial")
		if serial == "" {
			writeError(w, ErrDeviceNotFound)
			return
		}

		// Validate serial per T-05-02.
		if !serialPattern.MatchString(serial) {
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

		// Upgrade to WebSocket. Compression disabled because raw H.264
		// does not compress well and adds CPU overhead.
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			slog.Error("ws accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Relay video from scrcpy to WebSocket client.
		if err := relayVideo(ctx, sess, ws); err != nil {
			// Context cancellation is expected on session end; log others.
			if ctx.Err() == nil {
				slog.Info("video relay ended", "device", serial, "error", err)
			}
		}

		ws.Close(websocket.StatusNormalClosure, "session ended")
	}
}

// relayVideo reads codec metadata and then streams video frames from the
// scrcpy connection to the WebSocket client.
//
// Per the scrcpy protocol:
// 1. First binary message: 12-byte codec metadata (codec ID + width + height)
// 2. Subsequent messages: 12-byte frame header + payload (frame-boundary-preserved)
//
// Frame boundaries are preserved by concatenating the raw 12-byte header with
// the payload into a single WebSocket binary message. This ensures the browser's
// WebCodecs decoder receives complete frames without having to reassemble them.
func relayVideo(ctx context.Context, sess *session.DeviceSession, ws *websocket.Conn) error {
	// Send codec metadata as first WS message (12 bytes, binary).
	codecMeta := sess.CodecMeta()
	if err := ws.Write(ctx, websocket.MessageBinary, codecMeta[:]); err != nil {
		return fmt.Errorf("write codec meta: %w", err)
	}

	// Get the video connection for reading frames.
	videoConn := sess.VideoConn()
	if videoConn == nil {
		return fmt.Errorf("video connection is nil")
	}

	// Read and relay frames until context cancellation or read error.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		hdr, payload, err := scrcpy.ReadVideoFrame(videoConn)
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}

		// Build WS message: 12-byte raw header + payload as single binary message.
		rawHeader := hdr.RawHeader()
		msg := make([]byte, 12+len(payload))
		copy(msg[:12], rawHeader[:])
		copy(msg[12:], payload)

		if err := ws.Write(ctx, websocket.MessageBinary, msg); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
	}
}