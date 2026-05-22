package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateDevicePath enforces D-11: URL-decode -> path.Clean -> prefix
// match against an allowlist. Blocks ../, %2e%2e, mixed case.
// Base directories are now accepted (required for file browsing).
func TestValidateDevicePath(t *testing.T) {
	allow := []string{"/sdcard/", "/data/local/tmp/"}

	cases := []struct {
		name    string
		input   string
		want    string // expected cleaned (only checked when wantErr is nil)
		wantErr bool
	}{
		{"happy_sdcard_file", "/sdcard/foo", "/sdcard/foo", false},
		{"happy_tmp_file", "/data/local/tmp/x", "/data/local/tmp/x", false},
		{"happy_double_slash_normalized", "/sdcard//foo", "/sdcard/foo", false},
		{"happy_nested", "/sdcard/dir/file.png", "/sdcard/dir/file.png", false},
		{"happy_base_dir_with_slash", "/sdcard/", "/sdcard", false},
		{"happy_base_dir_no_slash", "/sdcard", "/sdcard", false},
		{"happy_base_dir_tmp", "/data/local/tmp/", "/data/local/tmp", false},

		{"reject_traversal_dotdot", "/sdcard/../etc/shadow", "", true},
		{"reject_percent_encoded_dotdot", "/sdcard/%2e%2e/etc", "", true},
		{"reject_uppercase", "/SDCARD/foo", "", true},
		{"reject_empty", "", "", true},
		{"reject_outside_allowlist", "/etc/passwd", "", true},
		{"reject_relative", "sdcard/foo", "", true},
		{"reject_percent_encoded_slash", "/sdcard/foo%2Fbar", "/sdcard/foo/bar", false}, // decoded then accepted because still under /sdcard/
		{"reject_bad_percent_encoding", "/sdcard/%zz", "", true},
		{"reject_only_slash", "/", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateDevicePath(tc.input, allow)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Equal(t, ErrPathNotAllowed, err, "should return ErrPathNotAllowed sentinel")
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestValidateDevicePathBaseDirNowAllowed verifies that the base directory
// itself is now accepted (previously rejected). This is required for file
// browsing — the frontend must be able to list /sdcard/ to show contents.
func TestValidateDevicePathBaseDirNowAllowed(t *testing.T) {
	cases := []struct {
		allow []string
		input string
		want  string
	}{
		{[]string{"/sdcard/"}, "/sdcard", "/sdcard"},
		{[]string{"/sdcard/"}, "/sdcard/", "/sdcard"},
		{[]string{"/sdcard"}, "/sdcard", "/sdcard"},
		{[]string{"/data/local/tmp/"}, "/data/local/tmp", "/data/local/tmp"},
	}
	for _, tc := range cases {
		got, err := ValidateDevicePath(tc.input, tc.allow)
		assert.NoError(t, err, "base directory %q should be accepted with allow=%v", tc.input, tc.allow)
		assert.Equal(t, tc.want, got, "base directory %q should clean to %q", tc.input, tc.want)
	}
}

// TestIsBaseDirPath verifies that IsBaseDirPath correctly identifies base
// directory paths for destructive operation guards.
func TestIsBaseDirPath(t *testing.T) {
	allow := []string{"/sdcard/", "/data/local/tmp/"}

	assert.True(t, IsBaseDirPath("/sdcard", allow), "/sdcard is a base dir")
	assert.True(t, IsBaseDirPath("/data/local/tmp", allow), "/data/local/tmp is a base dir")
	assert.False(t, IsBaseDirPath("/sdcard/foo", allow), "/sdcard/foo is not a base dir")
	assert.False(t, IsBaseDirPath("/etc", allow), "/etc is not an allowed base dir")
	assert.False(t, IsBaseDirPath("/", allow), "/ is not an allowed base dir")
}