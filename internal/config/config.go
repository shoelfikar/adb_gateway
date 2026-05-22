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
	// WriteTimeoutSeconds bounds every ws.Write call. Without a per-write
	// deadline, a stalled browser-side TCP path blocks the relay drain loop,
	// allowing the Hub's non-blocking fan-out to accumulate drops until it
	// evicts the viewer with `slow_consumer` (1008) — surfacing as silent
	// mid-session disconnects. See debug session ws-disconnect-remote-stream.
	WriteTimeoutSeconds int `koanf:"write_timeout_seconds"`
}

// ScrcpyConfig holds Phase 3 SCR-07 launcher tunables. Zero values mean
// "use the scrcpy server default" — backward compatible with Phase 1/2.
type ScrcpyConfig struct {
	Codec       string `koanf:"codec"`        // h264 | h265 | av1
	MaxSize     int    `koanf:"max_size"`     // px, 0 = device default
	BitRate     int    `koanf:"bit_rate"`     // bps, 0 = server default
	MaxFPS      int    `koanf:"max_fps"`      // 0 = unlimited
	AudioCodec  string `koanf:"audio_codec"`  // opus | aac | raw | flac
	AudioSource string `koanf:"audio_source"` // output | mic | playback
}

// LogcatConfig holds Phase 3 OPS-05 logcat ring buffer tunables.
type LogcatConfig struct {
	// RingBufferLines is the per-device retroactive logcat ring size.
	// Default 10000 (per CONTEXT.md D-02).
	RingBufferLines int `koanf:"ring_buffer_lines"`
}

// ScreenshotConfig holds Phase 3 OPS-06 tunables for the screenshot endpoint.
type ScreenshotConfig struct {
	// DefaultQuality is the WebP encoder quality used when ?q= is omitted.
	// Range 1..100. Default 80.
	DefaultQuality int `koanf:"default_quality"`
	// RatePerSecPerKey caps screenshots per second per API key (Pitfall 4).
	// Default 5.0.
	RatePerSecPerKey float64 `koanf:"rate_per_sec_per_key"`
}

// APKConfig holds Phase 3 OPS-07 tunables for the APK install endpoint.
type APKConfig struct {
	// MaxBytes caps the request body size for APK uploads.
	// Default 524288000 (500 MiB).
	MaxBytes int64 `koanf:"max_bytes"`
	// InstallTimeoutSeconds bounds the entire install operation
	// (push + pm install). Default 300 (5 min) per CONTEXT.md D-09.
	InstallTimeoutSeconds int `koanf:"install_timeout_seconds"`
	// InstallsPerMinutePerKey caps APK installs per minute per API key
	// (Pitfall 4 + T-03-04-03). Default 5.0.
	InstallsPerMinutePerKey float64 `koanf:"installs_per_minute_per_key"`
}

// RecordingConfig holds Phase 3 OPS-09 tunables for screen recording.
type RecordingConfig struct {
	// Dir is the on-disk directory under which recordings are written
	// (one subdirectory per device serial). Default "./recordings".
	Dir string `koanf:"dir"`
	// MaxFileBytes triggers rotation to a new file. Default 2147483648
	// (2 GiB). On rotate, current file is closed cleanly and a new
	// monotonic-suffix file is opened with re-emitted SPS/PPS.
	MaxFileBytes int64 `koanf:"max_file_bytes"`
	// Container selects the on-disk container format. Currently only
	// "mkv" is implemented (mkvcore + AVC). Default "mkv".
	Container string `koanf:"container"`
}

// FilesConfig holds Phase 3 OPS-08 tunables for file push/pull/delete.
type FilesConfig struct {
	// AllowedBasePaths is the allowlist of on-device base directories
	// (D-11/D-12). Default ["/sdcard/", "/data/local/tmp/"].
	AllowedBasePaths []string `koanf:"allowed_base_paths"`
	// DefaultBrowsePath is the on-device path used when the file browser
	// is opened without specifying a path. Defaults to the first
	// AllowedBasePaths entry ("/sdcard/").
	DefaultBrowsePath string `koanf:"default_browse_path"`
	// MaxUploadBytes caps the request body size for uploads (D-14).
	// Default 524288000 (500 MiB).
	MaxUploadBytes int64 `koanf:"max_upload_bytes"`
}

// Config holds all gateway configuration.
type Config struct {
	ListenAddr      string `koanf:"listen_addr"`
	ADBAddr         string `koanf:"adb_addr"`
	APIKeyPrimary   string `koanf:"api_key_primary"`
	APIKeySecondary string `koanf:"api_key_secondary"`
	LogLevel        string `koanf:"log_level"`
	AllowedOrigins  string `koanf:"allowed_origins"`
	Stream          StreamConfig     `koanf:"stream"`
	Control         ControlConfig    `koanf:"control"`
	WS              WSConfig         `koanf:"ws"`
	Scrcpy          ScrcpyConfig     `koanf:"scrcpy"`
	Logcat          LogcatConfig     `koanf:"logcat"`
	Screenshot      ScreenshotConfig `koanf:"screenshot"`
	Files           FilesConfig      `koanf:"files"`
	APK             APKConfig        `koanf:"apk"`
	Recording       RecordingConfig  `koanf:"recording"`
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
	var nestedPrefixes = []string{"stream_", "control_", "ws_", "scrcpy_", "logcat_", "screenshot_", "files_", "apk_", "recording_"}
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
	// viewer_buffer_frames: raised from 60 -> 240 (~4s headroom @ 60fps) to
	// absorb transient browser-side TCP/Wi-Fi stalls without evicting viewers.
	// See debug session ws-disconnect-remote-stream.
	if !k.Exists("stream.viewer_buffer_frames") {
		_ = k.Set("stream.viewer_buffer_frames", 240)
	}
	// max_consecutive_drops: raised from 120 -> 300 (~5s @ 60fps) for the
	// same reason — be more forgiving of brief backpressure bursts.
	if !k.Exists("stream.max_consecutive_drops") {
		_ = k.Set("stream.max_consecutive_drops", 300)
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
	// Per-write deadline for every ws.Write — prevents stalled browser
	// sockets from blocking the relay drain loop indefinitely.
	if !k.Exists("ws.write_timeout_seconds") {
		_ = k.Set("ws.write_timeout_seconds", 10)
	}

	// Phase 3 SCR-07 defaults — strings default to scrcpy server defaults,
	// numerics stay at 0 to mean "let the server decide" (backward compat).
	if !k.Exists("scrcpy.codec") {
		_ = k.Set("scrcpy.codec", "h264")
	}
	if !k.Exists("scrcpy.audio_codec") {
		_ = k.Set("scrcpy.audio_codec", "opus")
	}
	if !k.Exists("scrcpy.audio_source") {
		_ = k.Set("scrcpy.audio_source", "output")
	}

	// Phase 3 Plan 03-03 defaults.
	if !k.Exists("logcat.ring_buffer_lines") {
		_ = k.Set("logcat.ring_buffer_lines", 10000)
	}
	if !k.Exists("screenshot.default_quality") {
		_ = k.Set("screenshot.default_quality", 80)
	}
	if !k.Exists("screenshot.rate_per_sec_per_key") {
		_ = k.Set("screenshot.rate_per_sec_per_key", 5.0)
	}
	if !k.Exists("files.allowed_base_paths") {
		_ = k.Set("files.allowed_base_paths", []string{"/storage/emulated/0/", "/sdcard/", "/data/local/tmp/"})
	}
	if !k.Exists("files.default_browse_path") {
		_ = k.Set("files.default_browse_path", "/storage/emulated/0/")
	}
	if !k.Exists("files.max_upload_bytes") {
		_ = k.Set("files.max_upload_bytes", int64(524288000))
	}

	// Phase 3 Plan 03-04 defaults.
	if !k.Exists("apk.max_bytes") {
		_ = k.Set("apk.max_bytes", int64(524288000))
	}
	if !k.Exists("apk.install_timeout_seconds") {
		_ = k.Set("apk.install_timeout_seconds", 300)
	}
	if !k.Exists("apk.installs_per_minute_per_key") {
		_ = k.Set("apk.installs_per_minute_per_key", 5.0)
	}
	if !k.Exists("recording.dir") {
		_ = k.Set("recording.dir", "./recordings")
	}
	if !k.Exists("recording.max_file_bytes") {
		_ = k.Set("recording.max_file_bytes", int64(2147483648))
	}
	if !k.Exists("recording.container") {
		_ = k.Set("recording.container", "mkv")
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
	if c.WS.WriteTimeoutSeconds <= 0 {
		return fmt.Errorf("ws.write_timeout_seconds must be > 0")
	}
	return nil
}
