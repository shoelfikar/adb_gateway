package api

import (
	"net/url"
	"path"
	"strings"
)

// ValidateDevicePath canonicalizes a client-supplied on-device path and
// returns it iff it falls within one of the allowed base directories.
//
// The pipeline (per Phase 3 D-11) is:
//
//  1. Reject empty input.
//  2. URL-decode exactly once. Browsers single-decode; looping enables
//     double-encoding bypass (%252e%252e).
//  3. path.Clean — POSIX semantics match Android's filesystem.
//  4. Require absolute path (cleaned starts with "/").
//  5. Accept iff cleaned starts with base + "/" for some base in the
//     allowlist, OR cleaned equals the allowlisted base itself (case-sensitive
//     — Android filesystems are case-sensitive, and the device is the source
//     of truth). The base directory must be listable for file browsing to
//     function; symlink-safety for write ops is handled at the handler level.
//
// Returns ErrPathNotAllowed for any rejection. Never returns a generic error
// so callers can pass the result straight to writeError.
func ValidateDevicePath(input string, allowed []string) (string, error) {
	if input == "" {
		return "", ErrPathNotAllowed
	}

	// Step 1: single URL-decode. Bad encoding (%zz, dangling %) -> reject.
	decoded, err := url.QueryUnescape(input)
	if err != nil {
		return "", ErrPathNotAllowed
	}

	// Step 2: path.Clean collapses ../, //, trailing /, and a/./b sequences.
	cleaned := path.Clean(decoded)

	// Must be absolute. path.Clean preserves the leading "/" only if input
	// had one; relative inputs become "sdcard/foo" -> reject.
	if !strings.HasPrefix(cleaned, "/") {
		return "", ErrPathNotAllowed
	}

	// path.Clean("/") == "/" — root is never a valid target.
	if cleaned == "/" {
		return "", ErrPathNotAllowed
	}

	// Allowlist match.
	for _, base := range allowed {
		// Normalize base: strip trailing slash for comparison so both
		// "/sdcard/" and "/sdcard" entries behave identically.
		baseTrim := strings.TrimSuffix(base, "/")
		if baseTrim == "" {
			continue
		}
		// Accept the base directory itself (e.g. /sdcard) for listing/stat
		// operations. Write handlers that need to reject the base directory
		// (e.g. recursive delete of /sdcard) must add their own check.
		if cleaned == baseTrim || strings.HasPrefix(cleaned, baseTrim+"/") {
			return cleaned, nil
		}
	}

	return "", ErrPathNotAllowed
}

// IsBaseDirPath checks whether a cleaned path is exactly one of the allowed
// base directories. Handlers for destructive operations (recursive delete)
// should use this to prevent accidental deletion of an entire base directory.
func IsBaseDirPath(cleaned string, allowed []string) bool {
	for _, base := range allowed {
		baseTrim := strings.TrimSuffix(base, "/")
		if baseTrim != "" && cleaned == baseTrim {
			return true
		}
	}
	return false
}