package api

import (
	"encoding/json"
	"net/http"
)

// Healthz returns an http.HandlerFunc that reports service health, version, and scrcpy version.
// buildVersion and buildSHA are set via ldflags at build time.
func Healthz(buildVersion, buildSHA string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(healthResponse{
			Status:        "ok",
			Version:       buildVersion,
			BuildSHA:      buildSHA,
			SCRCPYVersion: "3.3.4",
		})
	}
}

type healthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	BuildSHA      string `json:"build_sha"`
	SCRCPYVersion string `json:"scrcpy_version"`
}