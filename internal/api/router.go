package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/session"
)

// NewRouter creates and returns a chi.Router with the full middleware stack
// and route registrations for the ADB Gateway API.
func NewRouter(cfg *config.Config, registry *session.Registry, adbClient *adb.Client, hostServices *adb.HostServices) http.Handler {
	r := chi.NewRouter()

	// Global middleware stack (order matters)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Public endpoints (no auth required)
	r.Get("/healthz", Healthz(version, sha))
	r.Handle("/metrics", promhttp.Handler())

	// Protected endpoints (auth required)
	r.Group(func(r chi.Router) {
		r.Use(APIKeyAuth(cfg.APIKeyPrimary, cfg.APIKeySecondary))

		// Device and session routes
		r.Route("/devices", func(r chi.Router) {
			r.Get("/", ListDevices(registry))
			r.Route("/{serial}", func(r chi.Router) {
				r.Post("/sessions", CreateSession(registry, adbClient, hostServices))
				r.Delete("/sessions/{sessionID}", DeleteSession(registry))
				r.Get("/video", StreamVideo(registry))
			})
		})
	})

	return r
}

// SetBuildInfo sets the version and build SHA for the healthz endpoint.
// These are typically set via -ldflags at build time.
func SetBuildInfo(v, s string) {
	version = v
	sha = s
}

var (
	version = "dev"
	sha     = "unknown"
)