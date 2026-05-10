package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pelni/adb-gateway/internal/adb"
	"github.com/pelni/adb-gateway/internal/config"
	"github.com/pelni/adb-gateway/internal/scrcpy"
	"github.com/pelni/adb-gateway/internal/session"
)

// NewRouter creates and returns a chi.Router with the full middleware stack
// and route registrations for the ADB Gateway API.
func NewRouter(cfg *config.Config, registry *session.Registry, adbClient *adb.Client, hostServices *adb.HostServices) http.Handler {
	r := chi.NewRouter()

	// Parse allowed origins from config.
	origins := cfg.ParseAllowedOrigins()

	// Global middleware stack (order matters)
	r.Use(CORS(origins))
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
				r.Post("/sessions", CreateSession(registry, adbClient, hostServices, cfg))
				r.Delete("/sessions/{sessionID}", DeleteSession(registry))
				r.Get("/video", StreamVideo(registry, origins, cfg))
				r.Get("/audio", StreamAudio(registry, origins, cfg))
				r.Get("/control", StreamControl(registry, origins, cfg))
				r.Post("/reservation", CreateReservation(registry))
				r.Patch("/reservation", ExtendReservation(registry))
				r.Delete("/reservation", ReleaseReservation(registry))

				// Phase 3 Plan 03-03 endpoints.
				r.Get("/logcat", StreamLogcat(registry, origins, cfg))             // OPS-05
				r.Post("/screenshot", CaptureScreenshot(registry, hostServices, cfg)) // OPS-06
				r.Route("/files", func(r chi.Router) {                              // OPS-08
					r.Post("/", UploadFile(registry, hostServices, cfg))
					r.Get("/", DownloadFile(registry, hostServices, cfg))
					r.Delete("/", DeleteFile(registry, hostServices, cfg))
				})

				// Phase 3 Plan 03-04 endpoints.
				r.Post("/apks", InstallAPK(registry, hostServices, cfg)) // OPS-07
				r.Post("/recordings", StartRecording(registry, cfg))     // OPS-09
				r.Get("/recordings", ListRecordings(registry))           // OPS-09
				r.Delete("/recordings/{id}", StopRecording(registry))    // OPS-09

				// Phase 3 Plan 03-02 handoff: manual restart of a sticky-Failed
				// device. The launcher factory is constructed per call so a
				// fresh launcher binds to the current adbClient/hostServices.
				factory := LauncherFactory(func() session.Launcher {
					return scrcpy.NewLauncher(adbClient, hostServices)
				})
				r.Post("/restart", RestartSession(registry, cfg, factory))
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