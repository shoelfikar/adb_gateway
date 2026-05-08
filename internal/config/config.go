package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/parsers/yaml"
	flag "github.com/spf13/pflag"
)

// SCRCPYVersion is the pinned scrcpy server version.
// MUST match the embedded server.jar. Bump both in the same commit.
const SCRCPYVersion = "3.3.4"

// StreamConfig holds streaming fan-out controls (per Hub).
type StreamConfig struct {
	AudioEnabled        bool `koanf:"audio_enabled"`
	ViewerBufferFrames  int  `koanf:"viewer_buffer_frames"`
	MaxConsecutiveDrops int  `koanf:"max_consecutive_drops"`
}

// ControlConfig holds reservation lease controls.
type ControlConfig struct {
	LeaseTTLSeconds int `koanf:"lease_ttl_seconds"`
}

// WSConfig holds WebSocket lifecycle controls (apply to /video, /audio, /control).
type WSConfig struct {
	PingIntervalSeconds int   `koanf:"ping_interval_seconds"`
	IdleTimeoutSeconds  int   `koanf:"idle_timeout_seconds"`
	ReadLimitBytes      int64 `koanf:"read_limit_bytes"`
}

// Config holds all gateway configuration.
type Config struct {
	ListenAddr      string `koanf:"listen_addr"`
	ADBAddr         string `koanf:"adb_addr"`
	APIKeyPrimary   string `koanf:"api_key_primary"`
	APIKeySecondary string `koanf:"api_key_secondary"`
	LogLevel        string `koanf:"log_level"`
	AllowedOrigins  string `koanf:"allowed_origins"`
	Stream          StreamConfig  `koanf:"stream"`
	Control         ControlConfig `koanf:"control"`
	WS              WSConfig      `koanf:"ws"`
}

// ParseAllowedOrigins splits the comma-separated allowed_origins config value
// into a slice. Returns an empty slice (allow all) if not set.
func (c *Config) ParseAllowedOrigins() []string {
	if c.AllowedOrigins == "" {
		return nil
	}
	parts := strings.Split(c.AllowedOrigins, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// Load reads configuration from file, environment, and CLI flags (in priority order).
func Load() (*Config, error) {
	k := koanf.New(".")

	// 0. Define CLI flags (parsed last but highest priority)
	f := flag.NewFlagSet("adb-gateway", flag.ContinueOnError)
	f.String("config", "config.yaml", "path to config file")
	f.String("listen-addr", "", "listen address (default 127.0.0.1:8080)")
	f.String("adb-addr", "", "ADB server address (default localhost:5037)")
	f.String("log-level", "", "log level: debug, info, warn, error (default info)")
	if err := f.Parse(os.Args[1:]); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}

	// 1. File provider (lowest priority)
	configPath, _ := f.GetString("config")
	if configPath == "" {
		configPath = "config.yaml"
	}

	if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
		slog.Warn("no config file found, using defaults and env", "path", configPath, "error", err)
	}

	// 2. Environment provider (prefix ADB_GW_)
	// Transform: ADB_GW_LISTEN_ADDR -> listen_addr (lowercase, strip prefix, keep underscores)
	// For nested Phase 2 keys, the first underscore after a known parent prefix becomes a dot:
	// ADB_GW_STREAM_VIEWER_BUFFER_FRAMES -> stream.viewer_buffer_frames
	var nestedPrefixes = []string{"stream_", "control_", "ws_"}
	if err := k.Load(env.Provider("ADB_GW_", ".", func(s string) string {
		key := strings.ToLower(strings.TrimPrefix(s, "ADB_GW_"))
		for _, p := range nestedPrefixes {
			if strings.HasPrefix(key, p) {
				return p[:len(p)-1] + "." + key[len(p):]
			}
		}
		return key
	}), nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}

	// 3. Flag provider (highest priority)
	if err := k.Load(posflag.Provider(f, ".", k), nil); err != nil {
		return nil, fmt.Errorf("load flags: %w", err)
	}

	// Set defaults for missing values
	if k.String("listen_addr") == "" {
		_ = k.Set("listen_addr", "127.0.0.1:8080")
	}
	if k.String("adb_addr") == "" {
		_ = k.Set("adb_addr", "localhost:5037")
	}
	if k.String("log_level") == "" {
		_ = k.Set("log_level", "info")
	}

	// Phase 2 defaults — use k.Exists to distinguish "unset" from "set to zero"
	if !k.Exists("stream.viewer_buffer_frames") {
		_ = k.Set("stream.viewer_buffer_frames", 60)
	}
	if !k.Exists("stream.max_consecutive_drops") {
		_ = k.Set("stream.max_consecutive_drops", 120)
	}
	if !k.Exists("stream.audio_enabled") {
		_ = k.Set("stream.audio_enabled", true)
	}
	if !k.Exists("control.lease_ttl_seconds") {
		_ = k.Set("control.lease_ttl_seconds", 60)
	}
	if !k.Exists("ws.ping_interval_seconds") {
		_ = k.Set("ws.ping_interval_seconds", 25)
	}
	if !k.Exists("ws.idle_timeout_seconds") {
		_ = k.Set("ws.idle_timeout_seconds", 90)
	}
	if !k.Exists("ws.read_limit_bytes") {
		_ = k.Set("ws.read_limit_bytes", 4194304)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// Validate checks that required config fields are set and values are in range.
func (c *Config) Validate() error {
	if c.APIKeyPrimary == "" {
		return fmt.Errorf("api_key_primary is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if c.Stream.ViewerBufferFrames <= 0 {
		return fmt.Errorf("stream.viewer_buffer_frames must be > 0")
	}
	if c.Stream.MaxConsecutiveDrops <= 0 {
		return fmt.Errorf("stream.max_consecutive_drops must be > 0")
	}
	if c.Control.LeaseTTLSeconds < 5 {
		return fmt.Errorf("control.lease_ttl_seconds must be >= 5")
	}
	if c.WS.PingIntervalSeconds < 1 {
		return fmt.Errorf("ws.ping_interval_seconds must be > 0")
	}
	if c.WS.IdleTimeoutSeconds <= c.WS.PingIntervalSeconds {
		return fmt.Errorf("ws.idle_timeout_seconds must be > ws.ping_interval_seconds")
	}
	if c.WS.ReadLimitBytes < 65536 {
		return fmt.Errorf("ws.read_limit_bytes must be >= 65536")
	}
	return nil
}