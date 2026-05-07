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

// Config holds all gateway configuration.
type Config struct {
	ListenAddr      string `koanf:"listen_addr"`
	ADBAddr         string `koanf:"adb_addr"`
	APIKeyPrimary   string `koanf:"api_key_primary"`
	APIKeySecondary string `koanf:"api_key_secondary"`
	LogLevel        string `koanf:"log_level"`
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

	// 2. Environment provider (prefix ADB_GW_, underscores map to dots)
	if err := k.Load(env.Provider("ADB_GW_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "ADB_GW_")), "_", ".")
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

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// Validate checks that required config fields are set.
func (c *Config) Validate() error {
	if c.APIKeyPrimary == "" {
		return fmt.Errorf("api_key_primary is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	return nil
}