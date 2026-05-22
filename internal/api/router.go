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

			// TCP/IP device connect — opt-in to the project's
			// "Local ADB only" constraint (PROJECT.md). The static
			// /connect path is registered before the /{serial} group so
			// chi resolves it as a literal route, not a wildcard match.
			r.Post("/connect", ConnectDevice(hostServices))
			r.Delete("/connect/{serial}", DisconnectDevice(hostServices))

			r.Route("/{serial}", func(r chi.Router) {
				r.Post("/sessions", CreateSession(registry, adbClient, hostServices, cfg))
				r.Delete("/sessions/{sessionID}", DeleteSession(registry))
				r.Get("/video", StreamVideo(registry, origins, cfg))
				r.Get("/audio", StreamAudio(registry, origins, cfg))
				r.Get("/control", StreamControl(registry, origins, cfg))
				r.Get("/session", StreamSession(registry, origins, cfg))
				r.Post("/reservation", CreateReservation(registry))
				r.Patch("/reservation", ExtendReservation(registry))
				r.Delete("/reservation", ReleaseReservation(registry))

				// Phase 3 Plan 03-03 endpoints.
				r.Get("/logcat", StreamLogcat(registry, origins, cfg))             // OPS-05
				r.Post("/screenshot", CaptureScreenshot(registry, hostServices, cfg)) // OPS-06

				// Phase 3 + 03.1 — file browser + file ops.
				// Per CONTEXT.md Claude's Discretion, read ops (list/stat/details/download/apk-export)
				// share NO bucket (cheap shell calls); write ops (mkdir/upload/upload-folder/
				// rename/recursive-delete/backup/uninstall) share the fileappWriteLimiter at
				// 30/min/key. Tuning hook: cfg.Files.WritesPerMinutePerKey if added later.
				fileappWriteLimiter := newKeyLimiter(30.0) // 30 writes/min/key
				filesDispatcher := NewFilesDispatcher(
					ListFiles(registry, hostServices, cfg),      // op=list (REQ-FB-LIST)
					StatFile(registry, hostServices, cfg),       // op=stat (REQ-FB-STAT)
					DownloadFile(registry, hostServices, cfg),   // no op (Phase 3 compat)
					MkdirFile(registry, hostServices, cfg),      // op=mkdir (REQ-FB-MKDIR)
					UploadFile(registry, hostServices, cfg),     // no op (Phase 3 compat)
					UploadFolder(registry, hostServices, cfg),  // op=upload-folder (REQ-FB-UPLOAD-FOLDER)
					DownloadFolder(registry, hostServices, cfg), // op=download-folder (REQ-FB-DOWNLOAD-FOLDER)
					RenameFile(registry, hostServices, cfg),    // op=rename (REQ-FB-RENAME)
					DeleteFile(registry, hostServices, cfg),     // no op / ?recursive=1 (REQ-FB-DELETE-REC)
				)
				r.Route("/files", func(r chi.Router) { // OPS-08 + Phase 03.1
					r.Get("/", filesDispatcher.Get)
					r.With(requireWriteRateLimit(fileappWriteLimiter)).Post("/", filesDispatcher.Post)
					r.With(requireWriteRateLimit(fileappWriteLimiter)).Patch("/", filesDispatcher.Patch)
					r.With(requireWriteRateLimit(fileappWriteLimiter)).Delete("/", filesDispatcher.Delete)
				})

				// Phase 3 Plan 03-04 endpoints.
				r.Post("/apks", InstallAPK(registry, hostServices, cfg)) // OPS-07
				r.Post("/recordings", StartRecording(registry, cfg))     // OPS-09
				r.Get("/recordings", ListRecordings(registry))           // OPS-09
				r.Delete("/recordings/{id}", StopRecording(registry))    // OPS-09

				// Phase 03.1 — app manager (plans 04-06).
				r.Route("/apps", func(r chi.Router) {
					// Read: list — no rate limit (REQ-AM-LIST)
					r.Get("/", ListApps(registry, hostServices, cfg))
					r.Route("/{pkg}", func(r chi.Router) {
						// Read: details + apk export — no rate limit (REQ-AM-DETAILS, REQ-AM-APK-EXPORT)
						r.Get("/", GetAppDetails(registry, hostServices, cfg))
						r.Get("/apk", ExportAPK(registry, hostServices, cfg))
						// Write: launch + backup + uninstall — rate-limited (REQ-AM-09, REQ-AM-BACKUP, REQ-AM-UNINSTALL)
						r.With(requireWriteRateLimit(fileappWriteLimiter)).Post("/launch", LaunchApp(registry, hostServices, cfg))
						r.With(requireWriteRateLimit(fileappWriteLimiter)).Post("/backup", BackupApp(registry, hostServices, cfg))
						r.With(requireWriteRateLimit(fileappWriteLimiter)).Delete("/", UninstallApp(registry, hostServices, cfg))
					})
				})

				// Phase 3 Plan 03-02 handoff: manual restart of a sticky-Failed
				// device. The launcher factory is constructed per call so a
				// fresh launcher binds to the current adbClient/hostServices.
				factory := LauncherFactory(func() session.Launcher {
					return scrcpy.NewLauncher(adbClient, hostServices)
				})
				r.Post("/restart", RestartSession(registry, cfg, factory))

				// Device power management — reboot and shutdown.
				r.With(requireWriteRateLimit(fileappWriteLimiter)).Post("/reboot", RebootDevice(registry, hostServices, cfg))
				r.With(requireWriteRateLimit(fileappWriteLimiter)).Post("/shutdown", ShutdownDevice(registry, hostServices, cfg))
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