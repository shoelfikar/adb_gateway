package scrcpy

import _ "embed"

// ServerJar holds the embedded scrcpy server binary.
// It is pushed to devices as /data/local/tmp/scrcpy-server-gateway.jar
// during session startup.
//
//go:embed assets/scrcpy-server-v3.3.4
var ServerJar []byte