package api

import (
	"bufio"
	"path"
	"strconv"
	"strings"
	"time"
)

// Entry represents a single file/directory/symlink entry from `ls -lA`.
// Matches the D-FB-03 listing entry shape.
type Entry struct {
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	Type          string    `json:"type"` // "file" | "dir" | "symlink"
	Size          int64     `json:"size"`
	Mode          string    `json:"mode"` // "0644" (octal string)
	MTime         time.Time `json:"mtime"`
	SymlinkTarget *string   `json:"symlink_target"`
}

var (
	isoFullLayout  = "2006-01-02 15:04:05.000000000 -0700"
	isoShortLayout = "2006-01-02 15:04:05 -0700"
)

// ParseLSLine handles toybox `ls -lA --time-style=full-iso` output.
// Returns (entry, true) for a parseable file/dir/symlink line; (_, false)
// for "total N" lines and unparseable noise (which we LOG but skip).
func ParseLSLine(line, dir string) (Entry, bool) {
	if strings.HasPrefix(line, "total ") {
		return Entry{}, false
	}
	// Fields: perm, links, owner, group, size, date, time, tz, name [-> target]
	// Use Fields (whitespace-collapse) with a twist: filename may contain
	// spaces, so re-join everything from index 8 onward.
	f := strings.Fields(line)
	if len(f) < 9 {
		return Entry{}, false
	}
	perm := f[0]
	size, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil {
		return Entry{}, false
	}

	ts := f[5] + " " + f[6] + " " + f[7]
	mtime, err := time.Parse(isoFullLayout, ts)
	if err != nil {
		// Older toybox emits seconds without nanoseconds.
		mtime, err = time.Parse(isoShortLayout, ts)
		if err != nil {
			return Entry{}, false
		}
	}

	rest := strings.Join(f[8:], " ")
	var name string
	var target *string
	if perm[0] == 'l' {
		parts := strings.SplitN(rest, " -> ", 2)
		name = parts[0]
		if len(parts) == 2 {
			t := parts[1]
			target = &t
		}
	} else {
		name = rest
	}

	typ := "file"
	switch perm[0] {
	case 'd':
		typ = "dir"
	case 'l':
		typ = "symlink"
	}

	return Entry{
		Name:          name,
		Path:          path.Join(dir, name),
		Type:          typ,
		Size:          size,
		Mode:          permToOctal(perm),
		MTime:         mtime.UTC(),
		SymlinkTarget: target,
	}, true
}

// ParseLSOutput parses the full output of `ls -lA --time-style=full-iso`
// into a slice of Entry. Unparseable lines (headers, noise) are skipped.
func ParseLSOutput(out []byte, dir string) []Entry {
	entries := make([]Entry, 0)
	s := bufio.NewScanner(strings.NewReader(string(out)))
	for s.Scan() {
		entry, ok := ParseLSLine(s.Text(), dir)
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// permToOctal converts a 10-char permission string like "drwxrwx---" to
// an octal mode string like "0770". The leading character (d/l/-/etc.)
// is ignored for mode calculation.
func permToOctal(perm string) string {
	if len(perm) < 10 {
		return "0000"
	}
	// Parse owner/group/other rwx + setuid/setgid/sticky bits
	// For typical Android files the top 3 bits are 0.
	owner := permBits(perm[1:4])
	group := permBits(perm[4:7])
	other := permBits(perm[7:10])
	mode := owner*64 + group*8 + other
	return fmtOctal(mode)
}

// permBits converts a 3-char rwx string to its octal value (0-7).
func permBits(s string) int {
	v := 0
	if len(s) >= 1 && s[0] == 'r' {
		v += 4
	}
	if len(s) >= 2 && s[1] == 'w' {
		v += 2
	}
	if len(s) >= 3 && s[2] == 'x' {
		v += 1
	}
	return v
}

// fmtOctal formats mode as a 4-digit octal string (e.g. "0644").
func fmtOctal(mode int) string {
	return strings.Repeat("0", 4-len(strconv.FormatInt(int64(mode), 8))) + strconv.FormatInt(int64(mode), 8)
}