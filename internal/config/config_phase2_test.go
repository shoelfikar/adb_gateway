package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase2DefaultValues(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("api_key_primary: test-key\n"), 0644))

	origArgs := os.Args
	os.Args = []string{"adb-gateway", "--config", cfgPath}
	defer func() { os.Args = origArgs }()

	cfg, err := Load()
	require.NoError(t, err)

	// Defaults raised in debug session ws-disconnect-remote-stream: 60->240 / 120->300.
	assert.Equal(t, 240, cfg.Stream.ViewerBufferFrames, "default viewer_buffer_frames")
	assert.Equal(t, 300, cfg.Stream.MaxConsecutiveDrops, "default max_consecutive_drops")
	assert.True(t, cfg.Stream.AudioEnabled, "default audio_enabled")
	assert.Equal(t, 60, cfg.Control.LeaseTTLSeconds, "default lease_ttl_seconds")
	assert.Equal(t, 25, cfg.WS.PingIntervalSeconds, "default ping_interval_seconds")
	assert.Equal(t, 90, cfg.WS.IdleTimeoutSeconds, "default idle_timeout_seconds")
	assert.Equal(t, int64(4194304), cfg.WS.ReadLimitBytes, "default read_limit_bytes")
	assert.Equal(t, 10, cfg.WS.WriteTimeoutSeconds, "default write_timeout_seconds")
}

func TestPhase2YAMLOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `api_key_primary: test-key
stream:
  viewer_buffer_frames: 30
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0644))

	origArgs := os.Args
	os.Args = []string{"adb-gateway", "--config", cfgPath}
	defer func() { os.Args = origArgs }()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 30, cfg.Stream.ViewerBufferFrames, "YAML override viewer_buffer_frames")
	// Others should still be defaults (raised in debug session ws-disconnect-remote-stream).
	assert.Equal(t, 300, cfg.Stream.MaxConsecutiveDrops)
}

func TestPhase2EnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("api_key_primary: test-key\n"), 0644))

	t.Setenv("ADB_GW_STREAM_VIEWER_BUFFER_FRAMES", "15")

	origArgs := os.Args
	os.Args = []string{"adb-gateway", "--config", cfgPath}
	defer func() { os.Args = origArgs }()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 15, cfg.Stream.ViewerBufferFrames, "env override viewer_buffer_frames")
}

func TestPhase2Validate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(cfg *Config)
		wantErr string
	}{
		{
			name: "viewer_buffer_frames_zero",
			modify: func(c *Config) { c.Stream.ViewerBufferFrames = 0 },
			wantErr: "viewer_buffer_frames",
		},
		{
			name: "viewer_buffer_frames_negative",
			modify: func(c *Config) { c.Stream.ViewerBufferFrames = -1 },
			wantErr: "viewer_buffer_frames",
		},
		{
			name: "max_consecutive_drops_zero",
			modify: func(c *Config) { c.Stream.MaxConsecutiveDrops = 0 },
			wantErr: "max_consecutive_drops",
		},
		{
			name: "lease_ttl_seconds_below_min",
			modify: func(c *Config) { c.Control.LeaseTTLSeconds = 4 },
			wantErr: "lease_ttl_seconds",
		},
		{
			name: "read_limit_bytes_below_min",
			modify: func(c *Config) { c.WS.ReadLimitBytes = 1024 },
			wantErr: "read_limit_bytes",
		},
		{
			name: "ping_interval_seconds_zero",
			modify: func(c *Config) { c.WS.PingIntervalSeconds = 0 },
			wantErr: "ping_interval_seconds",
		},
		{
			name: "idle_timeout_not_greater_than_ping",
			modify: func(c *Config) { c.WS.PingIntervalSeconds = 25; c.WS.IdleTimeoutSeconds = 25 },
			wantErr: "idle_timeout_seconds",
		},
		{
			name: "write_timeout_seconds_zero",
			modify: func(c *Config) { c.WS.WriteTimeoutSeconds = 0 },
			wantErr: "write_timeout_seconds",
		},
		{
			name: "write_timeout_seconds_negative",
			modify: func(c *Config) { c.WS.WriteTimeoutSeconds = -1 },
			wantErr: "write_timeout_seconds",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				APIKeyPrimary: "test-key",
				ListenAddr:    "127.0.0.1:8080",
				Stream: StreamConfig{
					ViewerBufferFrames:  60,
					MaxConsecutiveDrops: 120,
					AudioEnabled:        true,
				},
				Control: ControlConfig{
					LeaseTTLSeconds: 60,
				},
				WS: WSConfig{
					PingIntervalSeconds: 25,
					IdleTimeoutSeconds:  90,
					ReadLimitBytes:      4194304,
					WriteTimeoutSeconds: 10,
				},
			}
			tc.modify(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
