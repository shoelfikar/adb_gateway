package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigScrcpyDefaults verifies SCR-07 koanf defaults.
func TestConfigScrcpyDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("api_key_primary: test-key\n"), 0644))

	origArgs := os.Args
	os.Args = []string{"adb-gateway", "--config", cfgPath}
	defer func() { os.Args = origArgs }()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "h264", cfg.Scrcpy.Codec, "default scrcpy.codec")
	assert.Equal(t, "opus", cfg.Scrcpy.AudioCodec, "default scrcpy.audio_codec")
	assert.Equal(t, "output", cfg.Scrcpy.AudioSource, "default scrcpy.audio_source")
	assert.Equal(t, 0, cfg.Scrcpy.MaxSize, "default scrcpy.max_size = 0 (server default)")
	assert.Equal(t, 0, cfg.Scrcpy.BitRate, "default scrcpy.bit_rate = 0 (server default)")
	assert.Equal(t, 0, cfg.Scrcpy.MaxFPS, "default scrcpy.max_fps = 0 (unlimited)")
}

// TestConfigScrcpyYAMLOverride verifies the koanf key path scrcpy.* round-trips.
func TestConfigScrcpyYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `api_key_primary: test-key
scrcpy:
  codec: h265
  max_size: 1920
  bit_rate: 8000000
  max_fps: 60
  audio_codec: aac
  audio_source: mic
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0644))

	origArgs := os.Args
	os.Args = []string{"adb-gateway", "--config", cfgPath}
	defer func() { os.Args = origArgs }()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "h265", cfg.Scrcpy.Codec)
	assert.Equal(t, 1920, cfg.Scrcpy.MaxSize)
	assert.Equal(t, 8_000_000, cfg.Scrcpy.BitRate)
	assert.Equal(t, 60, cfg.Scrcpy.MaxFPS)
	assert.Equal(t, "aac", cfg.Scrcpy.AudioCodec)
	assert.Equal(t, "mic", cfg.Scrcpy.AudioSource)
}
