package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pelni/adb-gateway/internal/adb"
)

func TestIsGatewayOwned(t *testing.T) {
	tests := []struct {
		name     string
		entry    adb.ForwardEntry
		expected bool
	}{
		{
			name: "gateway-owned scrcpy forward",
			entry: adb.ForwardEntry{
				Local:  "localabstract:scrcpy_aabbccdd",
				Remote: "tcp:42001",
			},
			expected: true,
		},
		{
			name: "gateway-owned scrcpy forward with different SCID",
			entry: adb.ForwardEntry{
				Local:  "localabstract:scrcpy_12345678",
				Remote: "tcp:42002",
			},
			expected: true,
		},
		{
			name: "non-gateway forward tcp local",
			entry: adb.ForwardEntry{
				Local:  "tcp:8080",
				Remote: "tcp:9090",
			},
			expected: false,
		},
		{
			name: "non-gateway forward localabstract without scrcpy prefix",
			entry: adb.ForwardEntry{
				Local:  "localabstract:adbhub",
				Remote: "tcp:6000",
			},
			expected: false,
		},
		{
			name: "non-gateway forward jdwp",
			entry: adb.ForwardEntry{
				Local:  "jdwp",
				Remote: "tcp:5005",
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := isGatewayOwned(tc.entry)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestParseOrphanPIDs(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected []int
	}{
		{
			name:     "single orphan process",
			output:   "  1234 CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process / com.genymobile.scrcpy.Server",
			expected: []int{1234},
		},
		{
			name: "multiple orphan processes",
			output: `  1234 CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process / com.genymobile.scrcpy.Server
  5678 CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process / com.genymobile.scrcpy.Server`,
			expected: []int{1234, 5678},
		},
		{
			name:     "empty output",
			output:   "",
			expected: nil,
		},
		{
			name:     "grep process filtered out",
			output:   "  9999 grep scrcpy-server-gateway.jar",
			expected: nil,
		},
		{
			name: "orphan with grep line",
			output: `  1234 CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process / com.genymobile.scrcpy.Server
  9999 grep --color=auto scrcpy-server-gateway.jar`,
			expected: []int{1234},
		},
		{
			name:     "non-numeric PID line skipped",
			output:   "  abc CLASSPATH=/data/local/tmp/scrcpy-server-gateway.jar app_process",
			expected: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseOrphanPIDs(tc.output)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestReconcilePreservesNonGatewayForwards(t *testing.T) {
	// Verify that non-gateway forwards are not identified for removal.
	entries := []adb.ForwardEntry{
		{Local: "tcp:8080", Remote: "tcp:9090"},
		{Local: "localabstract:adbhub", Remote: "tcp:6000"},
		{Local: "jdwp", Remote: "tcp:5005"},
	}

	for _, entry := range entries {
		assert.False(t, isGatewayOwned(entry),
			"non-gateway forward should not be identified as gateway-owned: %s", entry.Local)
	}
}

func TestReconcileIdentifiesGatewayForwards(t *testing.T) {
	// Verify that gateway forwards are identified for removal.
	entries := []adb.ForwardEntry{
		{Local: "localabstract:scrcpy_aabbccdd", Remote: "tcp:42001"},
		{Local: "localabstract:scrcpy_11223344", Remote: "tcp:42002"},
	}

	for _, entry := range entries {
		assert.True(t, isGatewayOwned(entry),
			"gateway forward should be identified as gateway-owned: %s", entry.Local)
	}
}

func TestReconcileMixedForwards(t *testing.T) {
	// Verify that only gateway forwards are identified in a mixed list.
	entries := []adb.ForwardEntry{
		{Local: "localabstract:scrcpy_aabbccdd", Remote: "tcp:42001"},
		{Local: "tcp:8080", Remote: "tcp:9090"},
		{Local: "localabstract:scrcpy_11223344", Remote: "tcp:42002"},
		{Local: "localabstract:adbhub", Remote: "tcp:6000"},
	}

	gatewayCount := 0
	for _, entry := range entries {
		if isGatewayOwned(entry) {
			gatewayCount++
		}
	}
	assert.Equal(t, 2, gatewayCount, "should identify exactly 2 gateway-owned forwards")
}

func TestNewReconciler(t *testing.T) {
	// Verify that NewReconciler creates a reconciler with the given dependencies.
	reconciler := NewReconciler(nil, nil)
	assert.NotNil(t, reconciler)
}