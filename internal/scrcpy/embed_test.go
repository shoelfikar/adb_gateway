package scrcpy

import "testing"

func TestServerJarEmbedded(t *testing.T) {
	if ServerJar == nil {
		t.Fatal("ServerJar is nil -- embed directive may not be working")
	}
	if len(ServerJar) == 0 {
		t.Fatal("ServerJar is empty -- embed directive may not be working")
	}
}

func TestServerJarSize(t *testing.T) {
	// The server.jar is several MB; sanity check it is at least 10KB
	if len(ServerJar) < 10000 {
		t.Fatalf("ServerJar is only %d bytes, expected at least 10000 -- wrong file or corrupt", len(ServerJar))
	}
}

func TestBuildSCID(t *testing.T) {
	// SCID should be an 8-character hex string
	scid := BuildSCID()
	if len(scid) != 8 {
		t.Fatalf("BuildSCID() returned %q, expected 8 characters", scid)
	}
	// Verify all characters are hex digits
	for _, c := range scid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("BuildSCID() returned non-hex character %q in %q", c, scid)
		}
	}
	// Two calls should produce different values (extremely unlikely collision with crypto/rand)
	scid2 := BuildSCID()
	if scid == scid2 {
		t.Logf("Warning: two consecutive BuildSCID calls produced the same value %q (unlikely but possible)", scid)
	}
}