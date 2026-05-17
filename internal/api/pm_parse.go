package api

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PkgListEntry represents a single package from `pm list packages` output.
// Matches D-AM-04 cheap-list entry shape.
type PkgListEntry struct {
	Package     string `json:"package"`
	VersionCode int64  `json:"version_code"`
	Installer   string `json:"installer"` // "" if "null"
	UID         int    `json:"uid"`
	System      bool   `json:"system"`
	Enabled     bool   `json:"enabled"`
}

// PkgDetail represents a rich package detail from `dumpsys package <pkg>`.
// Matches D-AM-04 detail entry shape.
type PkgDetail struct {
	Package              string    `json:"package"`
	VersionName          string    `json:"version_name"`
	VersionCode          int64     `json:"version_code"`
	FirstInstallTime     time.Time `json:"first_install_time"`
	LastUpdateTime       time.Time `json:"last_update_time"`
	SigningCertSHA256    string    `json:"signing_cert_sha256"`
	APKSigningVersion    int       `json:"apk_signing_version"`
	RequestedPermissions []string  `json:"requested_permissions"`
	GrantedPermissions   []string  `json:"granted_permissions"`
	TotalSizeBytes       int64     `json:"total_size_bytes,omitempty"` // populated by ?include_size=1 via du
}

var (
	pkgPrefix = "package:"
	vcRE      = regexp.MustCompile(`versionCode[:=](\d+)`)
	instRE    = regexp.MustCompile(`installer[:=](\S+)`)
	uidRE     = regexp.MustCompile(`uid[:=](\d+)`)
)

// ParsePMList parses output from `pm list packages -3 -U -i --show-versioncode`.
// The includeSystem and includeDisabled flags derive the System/Enabled fields
// based on which flags were sent to the `pm list` command (W4 — System/Enabled
// derivation lives in the parser, not post-hoc by handlers).
//
// includeSystem=true  -> output includes system pkgs -> entry.System = true
// includeDisabled=true -> output is disabled-only -> entry.Enabled = false
//
// When include=all (both no-`-3` and `-d`), call ParsePMList(out, true, true).
func ParsePMList(out []byte, includeSystem bool, includeDisabled bool) []PkgListEntry {
	var entries []PkgListEntry
	s := bufio.NewScanner(bytes.NewReader(out))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, pkgPrefix) {
			continue
		}
		// package name is the token after "package:" up to first space
		rest := line[len(pkgPrefix):]
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			entries = append(entries, PkgListEntry{
				Package: rest,
				System:  includeSystem,
				Enabled: !includeDisabled,
			})
			continue
		}
		e := PkgListEntry{
			Package: rest[:sp],
			System:  includeSystem,
			Enabled: !includeDisabled,
		}
		if m := vcRE.FindStringSubmatch(line); m != nil {
			e.VersionCode, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := instRE.FindStringSubmatch(line); m != nil && m[1] != "null" {
			e.Installer = m[1]
		}
		if m := uidRE.FindStringSubmatch(line); m != nil {
			v, _ := strconv.Atoi(m[1])
			e.UID = v
		}
		entries = append(entries, e)
	}
	return entries
}

// ParseDumpsysPackage extracts fields from `dumpsys package <pkg>` output.
// Uses a line-scanner with key-prefix matching (not a regex monolith).
// Tries ISO time format first, then falls back to epoch milliseconds
// (ASSUMED A5 — some Android versions emit epoch ms).
// Uses 1 MiB scanner buffer (Pitfall 8 — dumpsys can be large).
func ParseDumpsysPackage(out []byte, pkg string) (PkgDetail, error) {
	d := PkgDetail{Package: pkg}
	s := bufio.NewScanner(bytes.NewReader(out))
	s.Buffer(make([]byte, 64*1024), 1024*1024) // Pitfall 8

	var section string // "" | "requested" | "install" | "runtime"
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		switch {
		case strings.HasPrefix(line, "versionName="):
			d.VersionName = strings.TrimPrefix(line, "versionName=")
		case strings.HasPrefix(line, "versionCode="):
			// line is "versionCode=42 minSdk=... targetSdk=..."
			tok := strings.Fields(line)[0] // "versionCode=42"
			d.VersionCode, _ = strconv.ParseInt(tok[len("versionCode="):], 10, 64)
		case strings.HasPrefix(line, "firstInstallTime="):
			ts := strings.TrimPrefix(line, "firstInstallTime=")
			d.FirstInstallTime = parseTime(ts)
		case strings.HasPrefix(line, "lastUpdateTime="):
			ts := strings.TrimPrefix(line, "lastUpdateTime=")
			d.LastUpdateTime = parseTime(ts)
		case strings.HasPrefix(line, "Signing cert SHA-256:"):
			d.SigningCertSHA256 = strings.TrimSpace(strings.TrimPrefix(line, "Signing cert SHA-256:"))
		case strings.HasPrefix(line, "apk signing version:"):
			v, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "apk signing version:")))
			d.APKSigningVersion = v

		// Section transitions
		case line == "requested permissions:":
			section = "requested"
		case line == "install permissions:":
			section = "install"
		case line == "runtime permissions:":
			section = "runtime"
		case strings.HasSuffix(line, ":") && !strings.Contains(line, "permission."):
			section = ""

		// Permission lines (inside sections)
		case section == "requested" && strings.HasPrefix(line, "android.permission."):
			d.RequestedPermissions = append(d.RequestedPermissions, strings.SplitN(line, ":", 2)[0])
		case (section == "install" || section == "runtime") && strings.Contains(line, "granted=true"):
			name := strings.SplitN(line, ":", 2)[0]
			d.GrantedPermissions = append(d.GrantedPermissions, name)
		}
	}
	return d, nil
}

// parseTime tries ISO format first, then falls back to epoch milliseconds.
func parseTime(s string) time.Time {
	// Try ISO format: "2006-01-02 15:04:05"
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	// Try epoch milliseconds (A5)
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// ParsePMPath parses output from `pm path <pkg>` into a list of APK paths.
func ParsePMPath(out []byte) []string {
	var paths []string
	s := bufio.NewScanner(bytes.NewReader(out))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "package:") {
			paths = append(paths, strings.TrimPrefix(line, "package:"))
		}
	}
	return paths
}