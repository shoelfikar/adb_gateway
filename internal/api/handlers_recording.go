// Package api — handlers_recording.go implements OPS-09:
//
//	POST   /devices/{serial}/recordings        start a recording
//	DELETE /devices/{serial}/recordings/{id}   stop a recording
//	GET    /devices/{serial}/recordings        list active recordings
//
// The recording subscriber lives in internal/session/recording.go and
// consumes the per-device video Hub via Hub.Subscribe — slow disk evicts
// the recorder via the existing drop-on-slow policy without back-pressuring
// live viewers (D-18, T-03-04-05).
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// ErrRecordingNotFound is the Phase 3 Plan 03-04 sentinel for an unknown
// recording_id on DELETE/GET.
var ErrRecordingNotFound = &DomainError{
	Code:       "RECORDING_NOT_FOUND",
	HTTPStatus: http.StatusNotFound,
	Message:    "Recording not found for this device",
}

// StartRecording handles POST /devices/{serial}/recordings.
func StartRecording(registry *session.Registry, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		// Phase 3 contract: recording requires StateActive. A device in
		// Reconnecting cannot start a fresh recording — the watchdog/
		// recovery cycle is in progress. Document trade-off: recording
		// stops cleanly when watchdog fires; restart manually.
		state := entry.GetState()
		if state != session.StateActive {
			writeError(w, &DomainError{
				Code:       "DEVICE_NOT_ACTIVE",
				HTTPStatus: http.StatusConflict,
				Message:    "Device must be in active state to start recording",
			})
			return
		}
		sess := entry.GetSession()
		if sess == nil {
			writeError(w, ErrDeviceNotFound)
			return
		}
		hub := sess.VideoHub()
		if hub == nil {
			writeError(w, &DomainError{
				Code:       ErrRecordingFailed.Code,
				HTTPStatus: ErrRecordingFailed.HTTPStatus,
				Message:    "Video hub not available",
			})
			return
		}

		id := uuid.New()
		rec, err := session.NewRecording(hub, id, serial, cfg.Recording.Dir, session.RecordingOpts{
			MaxFileBytes: cfg.Recording.MaxFileBytes,
			Log:          slog.With("device", serial),
		})
		if err != nil {
			slog.Warn("recording: NewRecording failed", "device", serial, "error", err)
			writeError(w, ErrRecordingFailed)
			return
		}

		// Long-lived ctx — survives the request lifecycle (D-08 parity
		// with APK install). Cancelled by StopRecording or device shutdown.
		recCtx, cancel := context.WithCancel(context.Background())

		// Register on entry — atomic with a busy-rejection guard.
		if err := entry.AddRecording(rec, cancel); err != nil {
			cancel()
			rec.Stop()
			if errors.Is(err, session.ErrRecordingBusy) {
				writeError(w, ErrDeviceBusy)
				return
			}
			writeError(w, ErrRecordingFailed)
			return
		}

		// Spawn the run goroutine. It cleans up on its own and removes
		// the handle from the entry on exit (so eviction by the Hub also
		// drops the registry entry).
		go func() {
			err := rec.Run(recCtx)
			entry.RemoveRecording(id)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("recording: run exited with error",
					"device", serial,
					"recording_id", id.String(),
					"error", err,
				)
			}
		}()

		writeJSON(w, http.StatusCreated, map[string]any{
			"recording_id": id.String(),
			"path":         rec.Path(),
			"started_at":   rec.StartedAt().Format(time.RFC3339),
		})
	}
}

// StopRecording handles DELETE /devices/{serial}/recordings/{id}.
func StopRecording(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		idStr := chi.URLParam(r, "id")
		id, err := uuid.Parse(idStr)
		if err != nil {
			writeError(w, ErrRecordingNotFound)
			return
		}
		h := entry.GetRecording(id)
		if h == nil {
			writeError(w, ErrRecordingNotFound)
			return
		}

		// Cancel and wait briefly for the goroutine to finish flushing.
		h.Cancel()

		// Poll for goroutine exit (it removes itself from the registry).
		// 10s timeout (per the plan).
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if entry.GetRecording(id) == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Defensive: ensure muxer is closed even if Run goroutine hasn't
		// observed the cancel yet.
		h.Rec.Stop()
		entry.RemoveRecording(id)

		writeJSON(w, http.StatusOK, map[string]any{
			"recording_id": id.String(),
			"path":         h.Rec.Path(),
			"bytes":        h.Rec.BytesWritten(),
			"frames":       h.Rec.FramesWritten(),
			"dropped":      h.Rec.DroppedFrames(),
		})
	}
}

// ListRecordings handles GET /devices/{serial}/recordings.
func ListRecordings(registry *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serial, ok := validateSerial(w, r)
		if !ok {
			return
		}
		entry, ok := registry.Get(serial)
		if !ok {
			writeError(w, ErrDeviceNotFound)
			return
		}
		recs := entry.ListRecordings()
		out := make([]map[string]any, 0, len(recs))
		for _, rec := range recs {
			out = append(out, map[string]any{
				"recording_id": rec.ID().String(),
				"path":         rec.Path(),
				"started_at":   rec.StartedAt().Format(time.RFC3339),
				"bytes":        rec.BytesWritten(),
				"frames":       rec.FramesWritten(),
				"dropped":      rec.DroppedFrames(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}
