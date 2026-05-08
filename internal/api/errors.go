package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// DomainError represents an application-specific error with an HTTP status code and machine-readable code.
type DomainError struct {
	Code       string // e.g. "DEVICE_OFFLINE"
	Message    string // human-readable
	HTTPStatus int    // e.g. 404
}

// Error implements the error interface.
func (e *DomainError) Error() string {
	return e.Message
}

// Sentinel domain errors per D-08.
var (
	ErrADBUnavailable       = &DomainError{Code: "ADB_UNAVAILABLE", HTTPStatus: http.StatusServiceUnavailable, Message: "ADB server is not available"}
	ErrDeviceOffline        = &DomainError{Code: "DEVICE_OFFLINE", HTTPStatus: http.StatusNotFound, Message: "Device is offline"}
	ErrDeviceNotFound       = &DomainError{Code: "DEVICE_NOT_FOUND", HTTPStatus: http.StatusNotFound, Message: "Device not found"}
	ErrPushFailed           = &DomainError{Code: "PUSH_FAILED", HTTPStatus: http.StatusBadGateway, Message: "Failed to push file to device"}
	ErrReverseForwardFailed = &DomainError{Code: "REVERSE_FORWARD_FAILED", HTTPStatus: http.StatusBadGateway, Message: "Failed to set up reverse tunnel"}
	ErrScrcpyLaunchFailed   = &DomainError{Code: "SCRCPY_LAUNCH_FAILED", HTTPStatus: http.StatusBadGateway, Message: "Failed to launch scrcpy server"}
	ErrSessionConflict      = &DomainError{Code: "SESSION_CONFLICT", HTTPStatus: http.StatusConflict, Message: "Session already exists for this device"}
	ErrSessionNotFound      = &DomainError{Code: "SESSION_NOT_FOUND", HTTPStatus: http.StatusNotFound, Message: "Session not found"}
	ErrUnauthorized         = &DomainError{Code: "UNAUTHORIZED", HTTPStatus: http.StatusUnauthorized, Message: "Invalid or missing API key"}

	// Phase 2 sentinels (D-19)
	ErrLeaseRequired    = &DomainError{Code: "LEASE_REQUIRED", HTTPStatus: http.StatusForbidden, Message: "Reservation lease required for this operation"}
	ErrLeaseInvalid     = &DomainError{Code: "LEASE_INVALID", HTTPStatus: http.StatusForbidden, Message: "Reservation lease is invalid or expired"}
	ErrLeaseHeldByOther = &DomainError{Code: "LEASE_HELD_BY_OTHER", HTTPStatus: http.StatusConflict, Message: "Reservation is held by another client"}
	ErrNotController    = &DomainError{Code: "NOT_CONTROLLER", HTTPStatus: http.StatusForbidden, Message: "Only the lease holder can send control messages"}
	ErrAudioUnavailable = &DomainError{Code: "AUDIO_UNAVAILABLE", HTTPStatus: http.StatusNotFound, Message: "Audio stream not available for this device"}
)

// errorResponse is the JSON envelope for error responses per D-07.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes a domain error or generic 500 to the response writer.
// Per D-09, no internal error details leak to the API consumer.
func writeError(w http.ResponseWriter, err error) {
	if domain, ok := err.(*DomainError); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(domain.HTTPStatus)
		json.NewEncoder(w).Encode(errorResponse{
			Error: errorBody{Code: domain.Code, Message: domain.Message},
		})
		return
	}
	// Internal errors get 500, no internal details leak per D-09
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(errorResponse{
		Error: errorBody{Code: "INTERNAL_ERROR", Message: "An internal error occurred"},
	})
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// mapError converts an internal error into a DomainError for API responses.
// This is a convenience function for mapping common error patterns.
func mapError(err error) *DomainError {
	if domain, ok := err.(*DomainError); ok {
		return domain
	}
	return &DomainError{
		Code:       "INTERNAL_ERROR",
		HTTPStatus: http.StatusInternalServerError,
		Message:    fmt.Sprintf("An internal error occurred: %v", err),
	}
}