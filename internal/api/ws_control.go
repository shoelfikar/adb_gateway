package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// StreamControl handles WS /devices/{serial}/control. It is lease-gated:
//   - X-Lease-ID header (or "lease.<id>" subprotocol) is REQUIRED for upgrade.
//   - On every incoming JSON message, lease validity is re-checked at the
//     ControlWriter.in send (race-clean against TTL expiry mid-session).
//   - Force-release events arrive on lease.ReleaseChan and are delivered
//     as a JSON text frame followed by graceful close (D-09).
//   - Disconnect starts the 5s grace timer via lease.BeginGrace (D-10).
func StreamControl(registry *session.Registry, allowedOrigins []string, cfg *config.Config) http.HandlerFunc {
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

		// Lease ID extraction: header first, then subprotocol "lease.<id>".
		leaseID := r.Header.Get("X-Lease-ID")
		if leaseID == "" {
			leaseID = extractLeaseIDFromSubprotocol(r)
		}
		if leaseID == "" {
			writeError(w, ErrLeaseRequired)
			return
		}
		mgr := entry.GetLeaseManager()
		if mgr == nil || !mgr.IsHeldBy(leaseID) {
			writeError(w, ErrLeaseInvalid)
			return
		}

		sess := entry.GetSession()
		if sess == nil || sess.State() != session.StateActive {
			writeError(w, ErrDeviceOffline)
			return
		}
		cw := sess.ControlWriter()
		if cw == nil {
			writeError(w, ErrDeviceOffline)
			return
		}

		opts := buildAcceptOptions(allowedOrigins, r)

		ws, err := websocket.Accept(w, r, opts)
		if err != nil {
			slog.Error("ws control accept failed", "device", serial, "error", err)
			return
		}
		defer ws.CloseNow()
		applyWSDefaults(ws, cfg)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		viewerID := uuid.NewString()
		log := slog.With("device", serial, "viewer_id", viewerID, "lease_id", leaseID)
		log.Info("control connected")

		// Track the final disconnect cause for structured close-code logging.
		// See debug session ws-disconnect-remote-stream.
		var finalErr error
		defer func() {
			closeCode := websocket.CloseStatus(finalErr)
			log.Info("control disconnected",
				"close_code", int(closeCode),
				"error", finalErr,
			)
		}()

		// On disconnect, start grace timer (D-10).
		defer func() {
			if err := mgr.BeginGrace(leaseID); err == nil {
				log.Info("controller disconnected; lease entered grace")
			}
		}()

		// Force-release listener.
		releaseCh := mgr.ReleaseChanFor(leaseID)
		forceReleaseDone := make(chan struct{})
		go func() {
			defer close(forceReleaseDone)
			if releaseCh == nil {
				return
			}
			select {
			case reason, ok := <-releaseCh:
				if !ok {
					return
				}
				payload, _ := json.Marshal(map[string]string{
					"type":   "lease_released",
					"reason": string(reason),
				})
				_ = wsWriteWithTimeout(ctx, ws, cfg, websocket.MessageText, payload)
				ws.Close(websocket.StatusNormalClosure, "lease_released")
			case <-ctx.Done():
				return
			}
		}()

		// Ping loop in parallel.
		pingErr := make(chan error, 1)
		go func() { pingErr <- pingLoop(ctx, ws, cfg) }()

		for {
			msgType, raw, err := ws.Read(ctx)
			if err != nil {
				finalErr = err
				cancel()
				return
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
			// Re-check lease at write time (race vs TTL expiry).
			if !mgr.IsHeldBy(leaseID) {
				writeWSError(ctx, ws, cfg, "NOT_CONTROLLER", "lease no longer held")
				finalErr = fmt.Errorf("lease_lost")
				ws.Close(4001, "lease_lost")
				return
			}
			select {
			case cw.In() <- cmsg:
			case <-ctx.Done():
				return
			}
		}
	}
}

// extractLeaseIDFromSubprotocol reads "lease.<uuid>" from Sec-WebSocket-Protocol.
func extractLeaseIDFromSubprotocol(r *http.Request) string {
	proto := r.Header.Get("Sec-WebSocket-Protocol")
	if proto == "" {
		return ""
	}
	for _, p := range strings.Split(proto, ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "lease.") {
			return strings.TrimPrefix(p, "lease.")
		}
	}
	return ""
}

// writeWSError sends a structured error envelope as a text frame without closing.
// Uses a bounded write deadline so a stalled browser cannot block the read loop.
func writeWSError(ctx context.Context, ws *websocket.Conn, cfg *config.Config, code, message string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
	_ = wsWriteWithTimeout(ctx, ws, cfg, websocket.MessageText, body)
}

// decodeControlEnvelope parses a JSON control message envelope into a scrcpy.ControlMsg.
// Returns ErrUnknownControlType or ErrControlPayloadTooLarge for invalid payloads.
func decodeControlEnvelope(raw []byte) (scrcpy.ControlMsg, error) {
	var head struct{ Type string `json:"type"` }
	if err := json.Unmarshal(raw, &head); err != nil {
		return scrcpy.ControlMsg{}, fmt.Errorf("invalid envelope: %w", err)
	}

	switch head.Type {
	case "INJECT_KEYCODE":
		var b struct {
			Type      string `json:"type"`
			Action    byte   `json:"action"`
			Keycode   uint32 `json:"keycode"`
			Repeat    uint32 `json:"repeat"`
			MetaState uint32 `json:"meta_state"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("inject_keycode body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type: scrcpy.TypeInjectKeycode,
			InjectKeycode: &scrcpy.InjectKeycodeFields{
				Action: b.Action, Keycode: b.Keycode, Repeat: b.Repeat, MetaState: b.MetaState,
			},
		}, nil

	case "INJECT_TEXT":
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("inject_text body: %w", err)
		}
		msg := scrcpy.ControlMsg{Type: scrcpy.TypeInjectText, InjectText: &scrcpy.InjectTextFields{Text: b.Text}}
		// Pre-validate length so we can attribute the error correctly.
		if _, err := scrcpy.Marshal(msg); err != nil {
			return scrcpy.ControlMsg{}, err
		}
		return msg, nil

	case "INJECT_TOUCH_EVENT":
		var b struct {
			Type         string `json:"type"`
			Action       byte   `json:"action"`
			PointerID    uint64 `json:"pointer_id"`
			X            int32  `json:"x"`
			Y            int32  `json:"y"`
			Width        uint16 `json:"width"`
			Height       uint16 `json:"height"`
			Pressure     uint16 `json:"pressure"`
			ActionButton uint32 `json:"action_button"`
			Buttons      uint32 `json:"buttons"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("inject_touch_event body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type: scrcpy.TypeInjectTouchEvent,
			InjectTouchEvent: &scrcpy.InjectTouchEventFields{
				Action: b.Action, PointerID: b.PointerID, X: b.X, Y: b.Y,
				Width: b.Width, Height: b.Height, Pressure: b.Pressure,
				ActionButton: b.ActionButton, Buttons: b.Buttons,
			},
		}, nil

	case "INJECT_SCROLL_EVENT":
		var b struct {
			Type    string `json:"type"`
			X       int32  `json:"x"`
			Y       int32  `json:"y"`
			Width   uint16 `json:"width"`
			Height  uint16 `json:"height"`
			HScroll int16  `json:"h_scroll"`
			VScroll int16  `json:"v_scroll"`
			Buttons uint32 `json:"buttons"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("inject_scroll_event body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type: scrcpy.TypeInjectScrollEvent,
			InjectScrollEvent: &scrcpy.InjectScrollEventFields{
				X: b.X, Y: b.Y, Width: b.Width, Height: b.Height,
				HScroll: b.HScroll, VScroll: b.VScroll, Buttons: b.Buttons,
			},
		}, nil

	case "BACK_OR_SCREEN_ON":
		var b struct {
			Type   string `json:"type"`
			Action byte   `json:"action"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("back_or_screen_on body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:            scrcpy.TypeBackOrScreenOn,
			BackOrScreenOn: &scrcpy.BackOrScreenOnFields{Action: b.Action},
		}, nil

	case "EXPAND_NOTIFICATION_PANEL":
		return scrcpy.ControlMsg{Type: scrcpy.TypeExpandNotificationPanel}, nil

	case "EXPAND_SETTINGS_PANEL":
		return scrcpy.ControlMsg{Type: scrcpy.TypeExpandSettingsPanel}, nil

	case "COLLAPSE_PANELS":
		return scrcpy.ControlMsg{Type: scrcpy.TypeCollapsePanels}, nil

	case "GET_CLIPBOARD":
		var b struct {
			Type    string `json:"type"`
			CopyKey byte   `json:"copy_key"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("get_clipboard body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:         scrcpy.TypeGetClipboard,
			GetClipboard: &scrcpy.GetClipboardFields{CopyKey: b.CopyKey},
		}, nil

	case "SET_CLIPBOARD":
		var b struct {
			Type     string `json:"type"`
			Sequence uint64 `json:"sequence"`
			Paste    bool   `json:"paste"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("set_clipboard body: %w", err)
		}
		msg := scrcpy.ControlMsg{
			Type: scrcpy.TypeSetClipboard,
			SetClipboard: &scrcpy.SetClipboardFields{
				Sequence: b.Sequence, Paste: b.Paste, Text: b.Text,
			},
		}
		if _, err := scrcpy.Marshal(msg); err != nil {
			return scrcpy.ControlMsg{}, err
		}
		return msg, nil

	case "SET_DISPLAY_POWER":
		var b struct {
			Type string `json:"type"`
			Mode byte   `json:"mode"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("set_display_power body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:            scrcpy.TypeSetDisplayPower,
			SetDisplayPower: &scrcpy.SetDisplayPowerFields{Mode: b.Mode},
		}, nil

	case "ROTATE_DEVICE":
		return scrcpy.ControlMsg{Type: scrcpy.TypeRotateDevice}, nil

	case "UHID_CREATE":
		var b struct {
			Type             string `json:"type"`
			ID               uint16 `json:"id"`
			VendorID         uint16 `json:"vendor_id"`
			ProductID        uint16 `json:"product_id"`
			Name             string `json:"name"`
			ReportDescriptor string `json:"report_descriptor"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("uhid_create body: %w", err)
		}
		rd, err := base64.StdEncoding.DecodeString(b.ReportDescriptor)
		if err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("uhid_create report_descriptor b64: %w", err)
		}
		return scrcpy.ControlMsg{
			Type: scrcpy.TypeUhidCreate,
			UhidCreate: &scrcpy.UhidCreateFields{
				ID: b.ID, VendorID: b.VendorID, ProductID: b.ProductID,
				Name: b.Name, ReportDescriptor: rd,
			},
		}, nil

	case "UHID_INPUT":
		var b struct {
			Type string `json:"type"`
			ID   uint16 `json:"id"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("uhid_input body: %w", err)
		}
		data, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("uhid_input data b64: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:      scrcpy.TypeUhidInput,
			UhidInput: &scrcpy.UhidInputFields{ID: b.ID, Data: data},
		}, nil

	case "UHID_DESTROY":
		var b struct {
			Type string `json:"type"`
			ID   uint16 `json:"id"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("uhid_destroy body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:        scrcpy.TypeUhidDestroy,
			UhidDestroy: &scrcpy.UhidDestroyFields{ID: b.ID},
		}, nil

	case "OPEN_HARD_KEYBOARD_SETTINGS":
		return scrcpy.ControlMsg{Type: scrcpy.TypeOpenHardKeyboardSettings}, nil

	case "START_APP":
		var b struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return scrcpy.ControlMsg{}, fmt.Errorf("start_app body: %w", err)
		}
		return scrcpy.ControlMsg{
			Type:     scrcpy.TypeStartApp,
			StartApp: &scrcpy.StartAppFields{Name: b.Name},
		}, nil

	case "RESET_VIDEO":
		return scrcpy.ControlMsg{Type: scrcpy.TypeResetVideo}, nil

	case "":
		return scrcpy.ControlMsg{}, fmt.Errorf("envelope missing type field")

	default:
		return scrcpy.ControlMsg{}, fmt.Errorf("%w: %q", scrcpy.ErrUnknownControlType, head.Type)
	}
}
