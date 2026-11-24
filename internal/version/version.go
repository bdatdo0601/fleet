package version

import (
	"fmt"
	"runtime"
)

// Version is the semantic version of the fleet CLI and server.
const Version = "0.1.0"

// Commit and BuiltAt are optionally populated at link time via -ldflags -X.
var (
	Commit  = "dev"
	BuiltAt = ""
)

// ShortCommit returns a short commit identifier (up to 7 characters) or "dev".
func ShortCommit() string {
	if Commit == "" || Commit == "dev" {
		return "dev"
	}
	if len(Commit) > 7 {
		return Commit[:7]
	}
	return Commit
}

// ClientID returns the X-Fleet-Client header value reported to the lighthouse.
func ClientID() string {
	return "go/" + ShortCommit()
}

// String returns a human-readable version banner.
func String() string {
	s := fmt.Sprintf("fleet %s (%s, %s/%s)", Version, ShortCommit(), runtime.GOOS, runtime.GOARCH)
	if BuiltAt != "" {
		s += " built " + BuiltAt
	}
	return s
}
