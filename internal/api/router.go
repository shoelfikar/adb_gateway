package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/pelni/adb-gateway/internal/config"
)

// NewRouter creates and returns a chi.Router with the full middleware stack
// and route registrations for the ADB Gateway API.
func NewRouter(cfg *config.Config) http.Handler {
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

		// Device routes (handlers added in later plans)
		r.Route("/devices", func(r chi.Router) {
			// Placeholder -- actual handlers added in Plan 05
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