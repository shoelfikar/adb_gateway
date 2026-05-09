package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateDevicePath enforces D-11: URL-decode -> path.Clean -> prefix
// match against an allowlist. Blocks ../, %2e%2e, mixed case, base-dir-itself.
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

		{"reject_traversal_dotdot", "/sdcard/../etc/shadow", "", true},
		{"reject_percent_encoded_dotdot", "/sdcard/%2e%2e/etc", "", true},
		{"reject_uppercase", "/SDCARD/foo", "", true},
		{"reject_base_dir_itself_with_slash", "/sdcard/", "", true},
		{"reject_base_dir_itself_no_slash", "/sdcard", "", true},
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

// TestValidateDevicePathBaseDirItselfRule verifies that the base directory
// itself is rejected even when both with and without trailing slash forms
// appear in the allowlist.
func TestValidateDevicePathBaseDirItselfRule(t *testing.T) {
	cases := []struct {
		allow []string
		input string
	}{
		{[]string{"/sdcard/"}, "/sdcard"},
		{[]string{"/sdcard/"}, "/sdcard/"},
		{[]string{"/sdcard"}, "/sdcard"},
	}
	for _, tc := range cases {
		_, err := ValidateDevicePath(tc.input, tc.allow)
		assert.Equal(t, ErrPathNotAllowed, err, "input=%q allow=%v", tc.input, tc.allow)
	}
}
