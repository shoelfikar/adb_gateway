package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseLSLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		dir        string
		wantOK     bool
		wantName   string
		wantType   string // "file" | "dir" | "symlink"
		wantSize   int64
		wantMode   string // octal string e.g. "0660"
		wantTarget *string
	}{
		{
			name:     "regular_file",
			line:     "-rw-rw---- 1 u0_a123 sdcard_rw 12345 2026-05-17 10:23:45.000000000 +0000 photo.jpg",
			dir:      "/sdcard",
			wantOK:   true,
			wantName: "photo.jpg",
			wantType: "file",
			wantSize: 12345,
			wantMode: "0660",
		},
		{
			name:     "directory",
			line:     "drwxrwx--- 2 u0_a123 sdcard_rw 4096 2026-05-15 08:00:00.000000000 +0000 subdir",
			dir:      "/sdcard",
			wantOK:   true,
			wantName: "subdir",
			wantType: "dir",
			wantSize: 4096,
			wantMode: "0770",
		},
		{
			name:       "symlink",
			line:        "lrwxrwxrwx 1 root root 11 2026-05-10 12:00:00.000000000 +0000 link -> /sdcard/foo",
			dir:         "/sdcard",
			wantOK:      true,
			wantName:    "link",
			wantType:    "symlink",
			wantSize:    11,
			wantMode:    "0777",
			wantTarget:  strPtr("/sdcard/foo"),
		},
		{
			name:   "total_line",
			line:   "total 24",
			dir:    "/sdcard",
			wantOK: false,
		},
		{
			name:     "filename_with_space",
			line:     "-rw-rw---- 1 u0_a123 sdcard_rw 999 2026-05-17 10:23:45.000000000 +0000 My File.txt",
			dir:      "/sdcard",
			wantOK:   true,
			wantName: "My File.txt",
			wantType: "file",
			wantSize: 999,
			wantMode: "0660",
		},
		{
			name:     "short_iso_layout",
			line:     "-rw-r--r-- 1 root root 2048 2026-05-17 10:23:45 +0000 foo.bin",
			dir:      "/sdcard",
			wantOK:   true,
			wantName: "foo.bin",
			wantType: "file",
			wantSize: 2048,
			wantMode: "0644",
		},
		{
			name:   "garbage_line",
			line:   "not a valid ls line at all",
			dir:    "/sdcard",
			wantOK: false,
		},
		{
			name:   "empty_line",
			line:   "",
			dir:    "/sdcard",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := ParseLSLine(tc.line, tc.dir)
			assert.Equal(t, tc.wantOK, ok)
			if !ok {
				return
			}
			assert.Equal(t, tc.wantName, entry.Name)
			assert.Equal(t, tc.wantType, entry.Type)
			assert.Equal(t, tc.wantSize, entry.Size)
			assert.Equal(t, tc.wantMode, entry.Mode)
			if tc.wantTarget != nil {
				assert.NotNil(t, entry.SymlinkTarget)
				if entry.SymlinkTarget != nil {
					assert.Equal(t, *tc.wantTarget, *entry.SymlinkTarget)
				}
			} else {
				assert.Nil(t, entry.SymlinkTarget)
			}
		})
	}
}

func TestParseLSOutput(t *testing.T) {
	input := `total 24
-rw-rw---- 1 u0_a123 sdcard_rw 12345 2026-05-17 10:23:45.000000000 +0000 photo.jpg
drwxrwx--- 2 u0_a123 sdcard_rw 4096 2026-05-15 08:00:00.000000000 +0000 subdir
lrwxrwxrwx 1 root root 11 2026-05-10 12:00:00.000000000 +0000 link -> /sdcard/foo
`
	entries := ParseLSOutput([]byte(input), "/sdcard")
	assert.Len(t, entries, 3)
	assert.Equal(t, "photo.jpg", entries[0].Name)
	assert.Equal(t, "file", entries[0].Type)
	assert.Equal(t, "subdir", entries[1].Name)
	assert.Equal(t, "dir", entries[1].Type)
	assert.Equal(t, "link", entries[2].Name)
	assert.Equal(t, "symlink", entries[2].Type)
}

func TestPermToOctal(t *testing.T) {
	cases := []struct {
		perm  string
		want  string
	}{
		{"-rw-rw----", "0660"},
		{"drwxrwx---", "0770"},
		{"lrwxrwxrwx", "0777"},
		{"-rwxr-xr-x", "0755"},
		{"-rw-r--r--", "0644"},
		{"-r--------", "0400"},
	}
	for _, tc := range cases {
		t.Run(tc.perm, func(t *testing.T) {
			assert.Equal(t, tc.want, permToOctal(tc.perm))
		})
	}
}

func TestParseLSLinePathConstruction(t *testing.T) {
	line := "-rw-rw---- 1 u0_a123 sdcard_rw 12345 2026-05-17 10:23:45.000000000 +0000 photo.jpg"
	entry, ok := ParseLSLine(line, "/sdcard/DCIM")
	assert.True(t, ok)
	assert.Equal(t, "/sdcard/DCIM/photo.jpg", entry.Path)
}

func TestParseLSLineMTimeUTC(t *testing.T) {
	line := "-rw-rw---- 1 u0_a123 sdcard_rw 12345 2026-05-17 10:23:45.000000000 +0000 photo.jpg"
	entry, ok := ParseLSLine(line, "/sdcard")
	assert.True(t, ok)
	expected, err := time.Parse("2006-01-02 15:04:05.000000000 -0700", "2026-05-17 10:23:45.000000000 +0000")
	if err == nil {
		assert.Equal(t, expected.UTC(), entry.MTime)
	}
}

// strPtr is a test helper to get a *string from a string literal.
func strPtr(s string) *string { return &s }