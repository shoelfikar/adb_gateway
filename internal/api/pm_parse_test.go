package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePMList(t *testing.T) {
	fixture := `package:com.foo.bar versionCode:42 installer=com.android.vending uid:10123
package:com.baz.qux versionCode:7 installer=null uid:10456
`

	t.Run("user_only_default", func(t *testing.T) {
		entries := ParsePMList([]byte(fixture), false, false)
		require.Len(t, entries, 2)
		assert.Equal(t, "com.foo.bar", entries[0].Package)
		assert.Equal(t, int64(42), entries[0].VersionCode)
		assert.Equal(t, "com.android.vending", entries[0].Installer)
		assert.Equal(t, 10123, entries[0].UID)
		assert.False(t, entries[0].System, "default user-only: System should be false")
		assert.True(t, entries[0].Enabled, "default not-disabled: Enabled should be true")

		assert.Equal(t, "com.baz.qux", entries[1].Package)
		assert.Equal(t, int64(7), entries[1].VersionCode)
		assert.Equal(t, "", entries[1].Installer, "installer=null should become empty string")
		assert.Equal(t, 10456, entries[1].UID)
	})

	t.Run("include_system", func(t *testing.T) {
		entries := ParsePMList([]byte(fixture), true, false)
		require.Len(t, entries, 2)
		assert.True(t, entries[0].System, "includeSystem=true: System should be true")
		assert.True(t, entries[1].System)
	})

	t.Run("include_disabled", func(t *testing.T) {
		entries := ParsePMList([]byte(fixture), false, true)
		require.Len(t, entries, 2)
		assert.False(t, entries[0].Enabled, "includeDisabled=true: Enabled should be false")
		assert.False(t, entries[1].Enabled)
	})

	t.Run("include_all", func(t *testing.T) {
		entries := ParsePMList([]byte(fixture), true, true)
		require.Len(t, entries, 2)
		assert.True(t, entries[0].System, "all: System should be true")
		assert.False(t, entries[0].Enabled, "all: Enabled should be false")
	})
}

func TestParsePMListMissingVersionCode(t *testing.T) {
	// Pitfall 5: Android < 8 may not include versionCode
	fixture := "package:com.old.app uid:10000\n"
	entries := ParsePMList([]byte(fixture), false, false)
	require.Len(t, entries, 1)
	assert.Equal(t, "com.old.app", entries[0].Package)
	assert.Equal(t, int64(0), entries[0].VersionCode, "missing versionCode should be 0")
}

func TestParsePMListEmpty(t *testing.T) {
	entries := ParsePMList([]byte(""), false, false)
	assert.Empty(t, entries)
}

func TestParsePMPath(t *testing.T) {
	t.Run("single_apk", func(t *testing.T) {
		fixture := "package:/data/app/~~AbC==/com.foo.bar-DeF==/base.apk\n"
		paths := ParsePMPath([]byte(fixture))
		assert.Len(t, paths, 1)
		assert.Equal(t, "/data/app/~~AbC==/com.foo.bar-DeF==/base.apk", paths[0])
	})

	t.Run("split_apk", func(t *testing.T) {
		fixture := `package:/data/app/~~AbC==/com.foo.bar-DeF==/base.apk
package:/data/app/~~AbC==/com.foo.bar-DeF==/split_config.arm64_v8a.apk
package:/data/app/~~AbC==/com.foo.bar-DeF==/split_config.en.apk
`
		paths := ParsePMPath([]byte(fixture))
		assert.Len(t, paths, 3)
		assert.Contains(t, paths[0], "base.apk")
		assert.Contains(t, paths[1], "split_config.arm64_v8a.apk")
		assert.Contains(t, paths[2], "split_config.en.apk")
	})
}

func TestParseDumpsysPackage(t *testing.T) {
	fixture := `Packages:
  Package [com.foo.bar] (12abc34):
    versionCode=42 minSdk=24 targetSdk=34
    versionName=1.2.3
    firstInstallTime=2025-12-01 10:23:45
    lastUpdateTime=2026-04-15 18:00:01
    Signing cert SHA-256: AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99
    apk signing version: 3
    requested permissions:
      android.permission.INTERNET
      android.permission.CAMERA
    install permissions:
      android.permission.INTERNET: granted=true
    runtime permissions:
      android.permission.CAMERA: granted=true, flags=[ USER_SET ]
`

	detail, err := ParseDumpsysPackage([]byte(fixture), "com.foo.bar")
	require.NoError(t, err)
	assert.Equal(t, "com.foo.bar", detail.Package)
	assert.Equal(t, "1.2.3", detail.VersionName)
	assert.Equal(t, int64(42), detail.VersionCode)
	assert.Equal(t, "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99", detail.SigningCertSHA256)
	assert.Equal(t, 3, detail.APKSigningVersion)
	assert.Contains(t, detail.RequestedPermissions, "android.permission.INTERNET")
	assert.Contains(t, detail.RequestedPermissions, "android.permission.CAMERA")
	assert.Contains(t, detail.GrantedPermissions, "android.permission.INTERNET")
	assert.Contains(t, detail.GrantedPermissions, "android.permission.CAMERA")

	// Verify time fields parse correctly
	expectedFirst, _ := time.Parse("2006-01-02 15:04:05", "2025-12-01 10:23:45")
	assert.Equal(t, expectedFirst, detail.FirstInstallTime)
	expectedLast, _ := time.Parse("2006-01-02 15:04:05", "2026-04-15 18:00:01")
	assert.Equal(t, expectedLast, detail.LastUpdateTime)
}

func TestParseDumpsysPackageEpochMs(t *testing.T) {
	// A5: Some Android versions emit epoch milliseconds for firstInstallTime
	fixture := `Packages:
  Package [com.foo.bar] (12abc34):
    versionCode=42 minSdk=24 targetSdk=34
    versionName=1.2.3
    firstInstallTime=1733049825000
    lastUpdateTime=1744742401000
    Signing cert SHA-256: AA:BB:CC:DD
    apk signing version: 2
    requested permissions:
      android.permission.INTERNET
    install permissions:
      android.permission.INTERNET: granted=true
`
	detail, err := ParseDumpsysPackage([]byte(fixture), "com.foo.bar")
	require.NoError(t, err)
	assert.Equal(t, "com.foo.bar", detail.Package)
	// Epoch ms: 1733049825000 = 2024-12-01 10:23:45 UTC
	assert.False(t, detail.FirstInstallTime.IsZero(), "epoch ms firstInstallTime should parse")
	assert.False(t, detail.LastUpdateTime.IsZero(), "epoch ms lastUpdateTime should parse")
}

func TestParseDumpsysPackageLargeInput(t *testing.T) {
	// Pitfall 8: dumpsys can be large. Ensure the scanner buffer handles it.
	var b strings.Builder
	b.WriteString("Packages:\n  Package [com.big.app] (abc):\n")
	b.WriteString("    versionCode=1 versionName=1.0\n")
	b.WriteString("    firstInstallTime=2025-01-01 00:00:00\n")
	b.WriteString("    lastUpdateTime=2025-01-01 00:00:00\n")
	// Generate a large requested permissions section (~200 KB)
	b.WriteString("    requested permissions:\n")
	for i := 0; i < 5000; i++ {
		b.WriteString("      android.permission.PERM_")
		b.WriteString(strings.Repeat("X", 30))
		b.WriteString("\n")
	}
	b.WriteString("    install permissions:\n")
	for i := 0; i < 5000; i++ {
		b.WriteString("      android.permission.PERM_")
		b.WriteString(strings.Repeat("X", 30))
		b.WriteString(": granted=true\n")
	}

	detail, err := ParseDumpsysPackage([]byte(b.String()), "com.big.app")
	require.NoError(t, err, "large dumpsys input should not fail (Pitfall 8)")
	assert.Equal(t, "com.big.app", detail.Package)
	assert.Greater(t, len(detail.RequestedPermissions), 0)
	assert.Greater(t, len(detail.GrantedPermissions), 0)
}