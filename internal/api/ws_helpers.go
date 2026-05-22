package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// applyWSDefaults sets the read limit (STR-09) on a freshly-accepted WS.
func applyWSDefaults(ws *websocket.Conn, cfg *config.Config) {
	ws.SetReadLimit(cfg.WS.ReadLimitBytes)
}

// pingLoop runs server-initiated pings at PingInterval; cancels its parent ctx
// (via the cancel function) when more than IdleTimeout has elapsed without a
// successful pong (STR-08). Returns when ctx is cancelled.
//
// Returns an error describing why it exited; the caller closes the WS with
// an appropriate code (StatusGoingAway for normal idle close).
func pingLoop(ctx context.Context, ws *websocket.Conn, cfg *config.Config) error {
	interval := time.Duration(cfg.WS.PingIntervalSeconds) * time.Second
	idle := time.Duration(cfg.WS.IdleTimeoutSeconds) * time.Second
	// Number of consecutive missed pings that constitute idle.
	// Conservative: idle / interval (e.g. 90/25 ~ 3.6 -> threshold 3 misses).
	threshold := int(idle / interval)
	if threshold < 1 {
		threshold = 1
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	misses := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, interval)
			err := ws.Ping(pingCtx)
			pingCancel()
			if err != nil {
				misses++
				if misses >= threshold {
					return fmt.Errorf("ping idle timeout: %d consecutive misses", misses)
				}
			} else {
				misses = 0
			}
		}
	}
}

// wsWriteWithTimeout wraps ws.Write with a per-call deadline derived from
// cfg.WS.WriteTimeoutSeconds. A bounded write deadline ensures that a stalled
// browser TCP path fails fast with a defined error rather than blocking
// indefinitely on the long-lived session context — which would in turn stall
// drain of the viewer's send channel and cause the Hub to evict the viewer
// with `slow_consumer` (1008). See debug session ws-disconnect-remote-stream.
func wsWriteWithTimeout(ctx context.Context, ws *websocket.Conn, cfg *config.Config, typ websocket.MessageType, msg []byte) error {
	timeout := time.Duration(cfg.WS.WriteTimeoutSeconds) * time.Second
	if timeout <= 0 {
		// Defensive: if misconfigured, fall back to a sane default rather than
		// reverting to the unbounded behavior that caused the original bug.
		timeout = 10 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ws.Write(wctx, typ, msg)
}

// subscribeAndRelay is the shared body for /video and /audio handlers:
//  1. Subscribe to hub.
//  2. Spawn ping goroutine.
//  3. Drain viewer channel; write each message as binary WS frame.
//  4. On hub eviction (channel close) close WS with 1008 + "slow_consumer".
//
// viewerID is gateway-generated UUID; never logged at INFO with API key context.
func subscribeAndRelay(ctx context.Context, ws *websocket.Conn, hub *session.Hub, streamName string, viewerID string, cfg *config.Config) error {
	ch, unsub, err := hub.Subscribe(viewerID)
	if err != nil {
		return fmt.Errorf("hub subscribe: %w", err)
	}
	defer unsub()

	// Read-side goroutine: processes ping/pong and close frames.
	// Required by coder/websocket — without this, pongs are never dispatched
	// and Ping() blocks forever, causing idle-timeout disconnections (code 1006).
	ws.CloseRead(ctx)

	// Run ping loop in parallel; cancel parent ctx if it returns an idle error.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pingErr := make(chan error, 1)
	go func() {
		pingErr <- pingLoop(ctx, ws, cfg)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-pingErr:
			// Ping idle close: send GoingAway then return.
			ws.Close(websocket.StatusGoingAway, "idle timeout")
			return err
		case msg, ok := <-ch:
			if !ok {
				// Hub evicted us (slow_consumer per D-05).
				ws.Close(websocket.StatusPolicyViolation, "slow_consumer")
				return fmt.Errorf("hub evicted viewer %s on stream %s", viewerID, streamName)
			}
			if err := wsWriteWithTimeout(ctx, ws, cfg, websocket.MessageBinary, msg); err != nil {
				return fmt.Errorf("ws write: %w", err)
			}
		}
	}
}

// buildAcceptOptions extracts the WS Accept options (origin patterns, subprotocol
// echo, compression disabled) from the allowed origins list and the request headers.
// Shared by /video, /audio, and /control handlers.
func buildAcceptOptions(allowedOrigins []string, r *http.Request) *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	}
	if len(allowedOrigins) == 0 {
		opts.OriginPatterns = []string{"*"}
	} else {
		opts.OriginPatterns = allowedOrigins
	}
	if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		for _, p := range strings.Split(proto, ",") {
			p = strings.TrimSpace(p)
			// Accept "api.<key>" and "lease.<id>" prefixed subprotocols used by
			// browser WebSocket clients that cannot set custom headers.
			if strings.HasPrefix(p, "api.") || strings.HasPrefix(p, "lease.") {
				opts.Subprotocols = append(opts.Subprotocols, p)
			} else if len(p) == 64 {
				// Legacy: raw 64-char subprotocol (API key or lease ID without prefix).
				opts.Subprotocols = append(opts.Subprotocols, p)
			}
		}
	}
	return opts
}

// extractAPIKeyFromSubprotocol reads "api.<key>" from Sec-WebSocket-Protocol.
// Used by the auth middleware when the WS client passes the API key via subprotocol
// (browser clients that cannot set custom headers during upgrade).
func extractAPIKeyFromSubprotocol(r *http.Request) string {
	proto := r.Header.Get("Sec-WebSocket-Protocol")
	if proto == "" {
		return ""
	}
	for _, p := range strings.Split(proto, ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "api.") {
			return strings.TrimPrefix(p, "api.")
		}
	}
	return ""
}
