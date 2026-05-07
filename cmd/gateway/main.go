package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/api"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/obs"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// Build-time variables set via -ldflags.
var (
	buildVersion = "dev"
	buildSHA     = "unknown"
)

func main() {
	// Parse CLI flags before config loading.
	showVersion := flag.Bool("version", false, "print version information and exit")
	showLicenses := flag.Bool("licenses", false, "print third-party notices and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("adb-gateway %s\n", buildVersion)
		fmt.Printf("  scrcpy version: %s\n", scrcpy.SCRCPYVersion)
		fmt.Printf("  build SHA:     %s\n", buildSHA)
		os.Exit(0)
	}

	if *showLicenses {
		content, err := readThirdPartyNotices()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading THIRD_PARTY_NOTICES: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(content)
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	obs.InitLogger(cfg.LogLevel)

	// Set build info for healthz and other endpoints
	api.SetBuildInfo(buildVersion, buildSHA)

	slog.Info("starting adb-gateway",
		"version", buildVersion,
		"scrcpy_version", config.SCRCPYVersion,
		"build_sha", buildSHA,
		"listen_addr", cfg.ListenAddr,
		"adb_addr", cfg.ADBAddr,
		"log_level", cfg.LogLevel,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize ADB client and host services
	adbClient := adb.NewClient(cfg.ADBAddr)
	hostServices, err := adb.NewHostServices(adbClient)
	if err != nil {
		slog.Error("failed to initialize ADB host services", "error", err)
		os.Exit(1)
	}

	// Wait for ADB server to be available with exponential backoff.
	// This handles the case where the gateway starts before adbd.
	reconnector := adb.NewReconnector(adbClient)
	if err := reconnector.AwaitADBReady(ctx); err != nil {
		slog.Error("ADB server never became available", "error", err)
		os.Exit(1)
	}

	// Startup reconciliation per D-10/D-11: clean up orphan processes
	// and stale reverse forwards from previous gateway runs.
	// Best-effort: log errors but continue starting.
	reconciler := session.NewReconciler(hostServices, adbClient)
	if err := reconciler.Reconcile(ctx); err != nil {
		slog.Warn("startup reconciliation failed", "error", err)
	}

	// Initialize device registry
	registry := session.NewRegistry()

	// Start device watcher to track connected/disconnected devices.
	deviceEvents, err := hostServices.NewDeviceWatcher(ctx)
	if err != nil {
		slog.Error("failed to start device watcher", "error", err)
		os.Exit(1)
	}
	go registry.WatchDevices(ctx, deviceEvents)

	router := api.NewRouter(cfg, registry, adbClient, hostServices)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Full SIGTERM handler with 30-second drain per FND-01.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, starting graceful shutdown", "signal", sig)
		cancel() // cancel root context -> tears down all sessions
	}()

	go func() {
		slog.Info("http server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
			cancel()
		}
	}()

	// Block until context is cancelled (SIGTERM/SIGINT received or server error).
	<-ctx.Done()

	// Drain all active sessions within 30 seconds per FND-01.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer drainCancel()

	registry.CloseAllSessions(drainCtx)

	if err := srv.Shutdown(drainCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}

// readThirdPartyNotices locates and reads the THIRD_PARTY_NOTICES file.
// It searches in the same directory as the running binary, then in the
// current working directory, then in the project root (one directory up from
// cmd/gateway). This allows the notices to be found regardless of whether
// the gateway is running from the build directory or an installed location.
func readThirdPartyNotices() (string, error) {
	// Try the directory of the running executable.
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "THIRD_PARTY_NOTICES")
		if data, err := os.ReadFile(candidate); err == nil {
			return string(data), nil
		}
	}

	// Try the current working directory.
	if data, err := os.ReadFile("THIRD_PARTY_NOTICES"); err == nil {
		return string(data), nil
	}

	return "", fmt.Errorf("THIRD_PARTY_NOTICES file not found (searched executable directory and working directory)")
}