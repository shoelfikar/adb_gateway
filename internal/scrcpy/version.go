package scrcpy

import (
	"crypto/rand"
	"fmt"
)

// SCRCPYVersion is the pinned scrcpy server version.
// MUST match the embedded server.jar. Bump both in the same commit.
const SCRCPYVersion = "3.3.4"

// ServerJarPath is the destination path on the Android device.
// Uses a gateway-specific filename to avoid stomping system scrcpy
// and to serve as a reconciliation marker for orphan process cleanup (D-10).
const ServerJarPath = "/data/local/tmp/scrcpy-server-gateway.jar"

// SHA-256 of the embedded server.jar for integrity verification.
// sha256sum internal/scrcpy/assets/scrcpy-server-v3.3.4
const ServerJarSHA256 = "8588238c9a5a00aa542906b6ec7e6d5541d9ffb9b5d0f6e1bc0e365e2303079e"

// BuildSCID generates a random scrcpy session ID (31-bit value, 8 hex chars).
// The SCID is used in the device-side socket name: localabstract:scrcpy_<SCID>
func BuildSCID() string {
	var buf [4]byte
	// Use crypto/rand for strong randomness; mask top bit for 31-bit range
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback should never happen with crypto/rand, but handle gracefully
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	// Mask top bit to produce a 31-bit value
	scid := uint32(buf[0])&0x7f<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	return fmt.Sprintf("%08x", scid)
}