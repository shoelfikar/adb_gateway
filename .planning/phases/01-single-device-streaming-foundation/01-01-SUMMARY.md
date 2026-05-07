---
phase: "01-single-device-streaming-foundation"
plan: "01"
subsystem: "foundation"
tags: ["config", "auth", "logging", "healthz", "errors", "router"]
dependency_graph:
  requires: []
  provides: ["config-loading", "structured-logging", "api-key-auth", "domain-errors", "healthz-endpoint", "chi-router"]
  affects: ["all-future-plans"]
tech_stack:
  added:
    - "go-chi/chi/v5@v5.2.5"
    - "knadh/koanf/v2@v2.3.4"
    - "knadh/koanf/providers/file@v1.2.1"
    - "knadh/koanf/providers/env@v1.1.0"
    - "knadh/koanf/providers/posflag@v1.0.1"
    - "knadh/koanf/parsers/yaml@v1.1.0"
    - "spf13/pflag@v1.0.6"
    - "prometheus/client_golang@v1.23.2"
    - "stretchr/testify@v1.11.1"
  patterns:
    - "koanf config loading (file + env + flags)"
    - "slog JSON handler with key redaction"
    - "chi router with middleware stack"
    - "API key auth with SHA-256 + ConstantTimeCompare"
    - "Domain error codes with JSON envelope"
key_files:
  created:
    - "cmd/gateway/main.go"
    - "internal/config/config.go"
    - "internal/obs/logging.go"
    - "internal/obs/logging_test.go"
    - "internal/api/auth.go"
    - "internal/api/auth_test.go"
    - "internal/api/errors.go"
    - "internal/api/handlers_healthz.go"
    - "internal/api/handlers_healthz_test.go"
    - "internal/api/router.go"
    - "deploy/config.example.yaml"
    - ".golangci.yml"
  modified:
    - "go.mod"
    - "go.sum"
    - ".gitignore"
decisions:
  - "Used spf13/pflag instead of stdlib flag for koanf posflag provider compatibility"
  - "Healthz and metrics endpoints do NOT require auth (monitoring endpoints)"
  - "Protected routes use chi.Group() with APIKeyAuth middleware"
  - "Config env vars use ADB_GW_ prefix with lowercase underscore-preserved keys"
  - "Build version/SHA passed to healthz via api.SetBuildInfo() from main.go"
metrics:
  duration: "28m"
  completed: "2026-05-07"
  tasks_completed: 3
  files_created: 12
  files_modified: 3
  tests_passing: 27
---

# Phase 1 Plan 01: Bootstrap Project Scaffold Summary

Go module initialized with config loading (koanf), structured JSON logging with key redaction (slog), healthz endpoint, API-key auth middleware with constant-time compare, and domain error codes. Every subsequent plan imports config, uses the router with auth middleware, and relies on domain errors.

## Must-Have Truths Verified

- [x] Service starts with a valid config file and env vars (koanf file+env+flags loading)
- [x] Healthz returns 200 with version, scrcpy version, and build SHA
- [x] All REST endpoints require a valid API key (except healthz and metrics)
- [x] Invalid or missing API key returns 401 with domain error code UNAUTHORIZED
- [x] API keys never appear in structured JSON logs (redactingHandler strips api_key, password, secret, token)
- [x] Startup log records version, scrcpy version, build SHA, and effective config with secrets redacted

## Key Artifacts

| File | Purpose |
|------|---------|
| `cmd/gateway/main.go` | Entry point with config loading, logger init, router, and signal handling |
| `internal/config/config.go` | Config struct with koanf loading (file + env ADB_GW_ prefix + flags) |
| `internal/obs/logging.go` | slog JSON handler with key redaction for api_key/password/secret/token |
| `internal/api/auth.go` | APIKeyAuth middleware with SHA-256 + ConstantTimeCompare |
| `internal/api/errors.go` | 9 domain error codes per D-08 with JSON envelope per D-07/D-09 |
| `internal/api/handlers_healthz.go` | Healthz endpoint returning status, version, build_sha, scrcpy_version |
| `internal/api/router.go` | chi.Router with middleware stack (RequestID, RealIP, Logger, Recoverer, APIKeyAuth) |
| `deploy/config.example.yaml` | Example config file with defaults and env var comments |
| `.golangci.yml` | Linter config with gosec, errcheck, bodyclose, revive, govet, staticcheck |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed koanf env provider key transformation**

- **Found during:** Task 1 integration test (start gateway with ADB_GW_API_KEY_PRIMARY env var)
- **Issue:** The env provider transformation `strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "ADB_GW_")), "_", ".")` converted `ADB_GW_API_KEY_PRIMARY` to `api.key.primary`, but koanf with `.` delimiter treats this as a nested key that doesn't match the flat struct tag `koanf:"api_key_primary"`.
- **Fix:** Changed the transformation to `strings.ToLower(strings.TrimPrefix(s, "ADB_GW_"))` which preserves underscores, so `ADB_GW_API_KEY_PRIMARY` maps to `api_key_primary` matching the struct tag.
- **Files modified:** `internal/config/config.go`
- **Commit:** 1d4767e

**2. [Rule 3 - Blocking] Fixed posflag incompatibility with stdlib flag**

- **Found during:** Task 1 initial build
- **Issue:** `koanf/providers/posflag` expects `*pflag.FlagSet` (from spf13/pflag), not `*flag.FlagSet` (stdlib). Build failed with type mismatch.
- **Fix:** Changed import from stdlib `flag` to `github.com/spf13/pflag` aliased as `flag`, and updated the Load function to use pflag's FlagSet.
- **Files modified:** `internal/config/config.go`
- **Commit:** Included in Task 1 commit (6dcfaaf)

**3. [Rule 1 - Bug] Fixed middleware ordering test**

- **Found during:** Task 3 test run
- **Issue:** `TestMiddlewareOrdering` checked for `X-Request-Id` response header, but chi's `middleware.RequestID` sets the ID in the request context, not the response header.
- **Fix:** Refactored test to capture the request ID from context using `middleware.GetReqID()` in a handler.
- **Files modified:** `internal/api/auth_test.go`
- **Commit:** Included in Task 3 commit (192fa4e)

## Tests

| Package | Tests | Status |
|---------|-------|--------|
| internal/obs | 6 top-level + 20 subtests | PASS |
| internal/api | 15 top-level + 9 subtests | PASS |
| **Total** | **27 passing** | |

### Test Coverage

- **Auth**: Valid primary key, valid secondary key, missing key (401), invalid key (401), query parameter auth, timing safety, identical error responses
- **Domain Errors**: All 9 error codes (ADB_UNAVAILABLE, DEVICE_OFFLINE, DEVICE_NOT_FOUND, PUSH_FAILED, REVERSE_FORWARD_FAILED, SCRCPY_LAUNCH_FAILED, SESSION_CONFLICT, SESSION_NOT_FOUND, UNAUTHORIZED) with correct HTTP status and JSON body
- **Router**: Healthz no auth, metrics no auth, devices requires auth, devices with auth passes through, middleware stack
- **Logging**: InitLogger sets default handler, redacts api_key/password/secret/token, preserves normal keys, case-insensitive redaction
- **Healthz**: Returns 200 with status/version/build_sha/scrcpy_version, correct content type, all required keys
- **Internal error handling**: writeError returns 500 with INTERNAL_ERROR for non-domain errors, writeJSON helper

## Verification Results

```bash
$ go build ./cmd/gateway/  # exit 0
$ go test ./internal/obs/... ./internal/api/... -count=1  # all PASS
$ go vet ./...  # exit 0

# Manual verification:
$ ADB_GW_API_KEY_PRIMARY=testkey go run ./cmd/gateway/
# /healthz -> 200 {"status":"ok","version":"dev","build_sha":"unknown","scrcpy_version":"3.3.4"}
# /devices (no auth) -> 401 {"error":{"code":"UNAUTHORIZED","message":"Invalid or missing API key"}}
# /devices (with X-API-Key: testkey) -> 404 (auth passes, no handlers yet)
```

## Commits

| Commit | Message |
|--------|---------|
| 6dcfaaf | feat(01-01): Go module init + project structure + config loading |
| 71fe927 | feat(01-01): structured logging with key redaction + healthz endpoint |
| 192fa4e | feat(01-01): API key auth middleware + domain error codes + router wiring |
| 1d4767e | fix(01-01): env provider key transformation preserved underscores |
| b77490d | chore(01-01): update go.mod with direct deps and add binary to gitignore |

## Known Stubs

No blocking stubs. The `/devices` route group is a placeholder (no handlers yet) -- this is intentional per the plan: "actual handlers added in Plan 05".

## Threat Flags

| Flag | File | Description |
|------|------|-------------|
| threat_flag: information_disclosure | internal/api/auth.go | Query parameter `api_key` is less secure than `X-API-Key` header (appears in proxy logs); logged as warning per T-01-05 (accepted risk) |

## Self-Check

- [x] All created files exist at expected paths
- [x] All commits exist in git log
- [x] go build, go test, go vet all pass
- [x] All must-have truths verified