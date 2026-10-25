// Package version holds build information, set with -ldflags:
//
//	go build -ldflags "-X github.com/justinburger/mirage/internal/version.Version=v0.1.0"
package version

import "runtime/debug"

// Version is the release version ("dev" for local builds).
var Version = "dev"

// Commit is the VCS revision, read from build info when not set by ldflags.
var Commit = ""

func init() {
	if Commit != "" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				Commit = s.Value[:7]
			}
		}
	}
}

// String returns "v0.1.0 (abc1234)" or just the version.
func String() string {
	if Commit == "" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
