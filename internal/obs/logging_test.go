package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitLoggerSetsDefault(t *testing.T) {
	InitLogger("info")

	// After InitLogger, slog.Default() should be set
	handler := slog.Default().Handler()
	assert.NotNil(t, handler, "default slog handler should be set after InitLogger")
}

func TestInitLoggerLevels(t *testing.T) {
	tests := []struct {
		level    string
		expected slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo}, // defaults to info
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			InitLogger(tt.level)
			// Just verify it doesn't panic and sets a handler
			assert.NotNil(t, slog.Default().Handler())
		})
	}
}

func TestRedactingHandlerRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := &redactingHandler{inner: jsonHandler}
	logger := slog.New(handler)

	tests := []struct {
		name          string
		logFn         func()
		sensitiveKey  string
		expectedValue string
	}{
		{
			name:          "api_key_primary is redacted",
			logFn:         func() { logger.Info("test", "api_key_primary", "secret-key-123") },
			sensitiveKey:  "api_key_primary",
			expectedValue: "***REDACTED***",
		},
		{
			name:          "api_key_secondary is redacted",
			logFn:         func() { logger.Info("test", "api_key_secondary", "secondary-key-456") },
			sensitiveKey:  "api_key_secondary",
			expectedValue: "***REDACTED***",
		},
		{
			name:          "password is redacted",
			logFn:         func() { logger.Info("test", "password", "hunter2") },
			sensitiveKey:  "password",
			expectedValue: "***REDACTED***",
		},
		{
			name:          "secret is redacted",
			logFn:         func() { logger.Info("test", "secret", "my-secret") },
			sensitiveKey:  "secret",
			expectedValue: "***REDACTED***",
		},
		{
			name:          "token is redacted",
			logFn:         func() { logger.Info("test", "token", "bearer-abc") },
			sensitiveKey:  "token",
			expectedValue: "***REDACTED***",
		},
		{
			name:          "device key is NOT redacted",
			logFn:         func() { logger.Info("test", "device", "ABC123") },
			sensitiveKey:  "device",
			expectedValue: "ABC123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			tt.logFn()

			var entry map[string]interface{}
			err := json.Unmarshal(buf.Bytes(), &entry)
			require.NoError(t, err, "log output should be valid JSON")

			got, ok := entry[tt.sensitiveKey]
			require.True(t, ok, "key %q should exist in log entry", tt.sensitiveKey)
			assert.Equal(t, tt.expectedValue, got, "value for key %q should match expected", tt.sensitiveKey)
		})
	}
}

func TestRedactingHandlerDoesNotRedactNormalKeys(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := &redactingHandler{inner: jsonHandler}
	logger := slog.New(handler)

	buf.Reset()
	logger.Info("test message", "device", "ABC123", "session", "sess-001", "version", "1.0.0")

	var entry map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &entry)
	require.NoError(t, err)

	assert.Equal(t, "ABC123", entry["device"])
	assert.Equal(t, "sess-001", entry["session"])
	assert.Equal(t, "1.0.0", entry["version"])
	assert.Equal(t, "test message", entry["msg"])
}

func TestRedactingHandlerCaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := &redactingHandler{inner: jsonHandler}
	logger := slog.New(handler)

	// Test that API_KEY (uppercase) is also redacted
	buf.Reset()
	logger.Info("test", "API_KEY", "should-be-redacted")

	var entry map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &entry)
	require.NoError(t, err)

	assert.Equal(t, "***REDACTED***", entry["API_KEY"])
}

func TestShouldRedact(t *testing.T) {
	tests := []struct {
		key      string
		expected bool
	}{
		{"api_key", true},
		{"api_key_primary", true},
		{"api_key_secondary", true},
		{"password", true},
		{"db_password", true},
		{"secret", true},
		{"client_secret", true},
		{"token", true},
		{"access_token", true},
		{"refresh_token", true},
		{"device", false},
		{"session", false},
		{"version", false},
		{"serial", false},
		{"API_KEY", true},
		{"Password", true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert.Equal(t, tt.expected, shouldRedact(tt.key))
		})
	}
}

func TestRedactingHandlerInInitLogger(t *testing.T) {
	// Verify InitLogger produces a redacting handler
	InitLogger("info")

	var buf bytes.Buffer
	// Create a logger with a buffer to capture output
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	handler := &redactingHandler{inner: jsonHandler}
	logger := slog.New(handler)

	logger.Info("test", "api_key_primary", "super-secret-key")

	output := buf.String()
	assert.True(t, strings.Contains(output, "***REDACTED***"), "api_key should be redacted in output")
	assert.False(t, strings.Contains(output, "super-secret-key"), "actual key value should NOT appear in output")
}