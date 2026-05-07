package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// redactPatterns lists key substrings that trigger value redaction.
var redactPatterns = []string{"api_key", "password", "secret", "token"}

// redactedValue is the replacement for sensitive field values.
const redactedValue = "***REDACTED***"

// redactingHandler wraps a slog.Handler and redacts sensitive field values.
type redactingHandler struct {
	inner slog.Handler
}

// Enabled delegates to the inner handler.
func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle redacts sensitive values in log record attributes, then delegates to inner handler.
func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, redactAttr(a))
		return true
	})

	// Build a new record with the same fields but redacted attributes
	newR := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range attrs {
		newR.AddAttrs(a)
	}

	return h.inner.Handle(ctx, newR)
}

// WithAttrs returns a new handler whose attributes are appended to the existing ones.
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(redacted)}
}

// WithGroup returns a new handler with the given group prepended.
func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr redacts a single attribute if its key matches a sensitive pattern.
// For group values, it recursively redacts nested attributes.
func redactAttr(a slog.Attr) slog.Attr {
	if a.Key != "" && shouldRedact(a.Key) {
		a.Value = slog.StringValue(redactedValue)
		return a
	}

	// Handle group values recursively
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		redacted := make([]slog.Attr, len(group))
		for i, ga := range group {
			redacted[i] = redactAttr(ga)
		}
		a.Value = slog.GroupValue(redacted...)
	}

	return a
}

// shouldRedact checks if a key contains any sensitive pattern.
func shouldRedact(key string) bool {
	lower := strings.ToLower(key)
	for _, p := range redactPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// InitLogger sets up the default slog handler with structured JSON output and key redaction.
func InitLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})

	handler := &redactingHandler{inner: jsonHandler}
	slog.SetDefault(slog.New(handler))
}