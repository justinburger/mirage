// Package version holds build information. Release builds set it with
// -ldflags:
//
//	go build -ldflags "-X github.com/justinburger/mirage/internal/version.Version=v0.1.0"
//
// Without ldflags it falls back to the build info the Go toolchain embeds:
// "go install github.com/justinburger/mirage/cmd/mirage@v0.1.1" records the
// module version v0.1.1, and a build inside a git checkout records the VCS
// revision (and, since Go 1.24, a pseudo-version).
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release version ("dev" when nothing better is known).
var Version = "dev"

// Commit is the VCS revision, read from build info when not set by ldflags.
var Commit = ""

func init() {
	bi, _ := debug.ReadBuildInfo()
	Version, Commit = resolve(Version, Commit, bi)
}

// resolve fills in what ldflags left unset from the toolchain's build info
// (which may be nil). An ldflags Version or Commit always wins.
func resolve(version, commit string, bi *debug.BuildInfo) (string, string) {
	if bi == nil {
		return version, commit
	}
	if version == "dev" {
		// "(devel)" means a build of the main module from a directory with no
		// version information; "" when the build info is incomplete.
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}
	if commit == "" {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				commit = s.Value[:7]
			}
		}
	}
	return version, commit
}

// String returns "v0.1.0 (abc1234)", or just the version when the commit is
// unknown or already part of it (pseudo-versions end in the commit hash).
func String() string {
	if Commit == "" || strings.Contains(Version, Commit) {
		return Version
	}
	return Version + " (" + Commit + ")"
}
