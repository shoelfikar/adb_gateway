package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/api"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/obs"
	"github.com/pelni/adb-gateway/internal/session"
)

// Build-time variables set via -ldflags.
var (
	buildVersion = "dev"
	buildSHA     = "unknown"
)

func main() {
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

	// Initialize device registry
	registry := session.NewRegistry()

	router := api.NewRouter(cfg, registry, adbClient, hostServices)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, starting graceful shutdown", "signal", sig)
		cancel()
	}()

	go func() {
		slog.Info("http server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Drain all active sessions
	registry.CloseAllSessions(shutdownCtx)

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}