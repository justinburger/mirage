package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	rev := []debug.BuildSetting{{Key: "vcs.revision", Value: "4a93e99c0ffee1234567890abcdef1234567890"}}
	tests := []struct {
		name             string
		version, commit  string
		bi               *debug.BuildInfo
		wantVer, wantCmt string
	}{
		{
			name:    "go install from the module proxy: module version, no VCS",
			version: "dev",
			bi:      &debug.BuildInfo{Main: debug.Module{Path: "github.com/justinburger/mirage", Version: "v0.1.1"}},
			wantVer: "v0.1.1",
		},
		{
			name:    "build in a checkout without a usable version",
			version: "dev",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: rev},
			wantVer: "dev", wantCmt: "4a93e99",
		},
		{
			name:    "build in a checkout with a pseudo-version",
			version: "dev",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1-0.20261026051000-4a93e99c0ffe"}, Settings: rev},
			wantVer: "v0.1.1-0.20261026051000-4a93e99c0ffe", wantCmt: "4a93e99",
		},
		{
			name:    "ldflags win over build info",
			version: "v0.1.1", commit: "abcdef1",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: rev},
			wantVer: "v0.1.1", wantCmt: "abcdef1",
		},
		{
			name:    "no build info",
			version: "dev",
			wantVer: "dev",
		},
		{
			name:    "short revision is ignored",
			version: "dev",
			bi:      &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}},
			wantVer: "dev",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, c := resolve(tc.version, tc.commit, tc.bi)
			if v != tc.wantVer || c != tc.wantCmt {
				t.Errorf("resolve = (%q, %q), want (%q, %q)", v, c, tc.wantVer, tc.wantCmt)
			}
		})
	}
}

func TestString(t *testing.T) {
	defer func(v, c string) { Version, Commit = v, c }(Version, Commit)
	for _, tc := range []struct{ version, commit, want string }{
		{"v0.1.1", "", "v0.1.1"},
		{"v0.1.1", "4a93e99", "v0.1.1 (4a93e99)"},
		{"dev", "4a93e99", "dev (4a93e99)"},
		// A pseudo-version already names the commit.
		{"v0.1.1-0.20261026051000-4a93e99c0ffe", "4a93e99", "v0.1.1-0.20261026051000-4a93e99c0ffe"},
	} {
		Version, Commit = tc.version, tc.commit
		if got := String(); got != tc.want {
			t.Errorf("String() with (%q, %q) = %q, want %q", tc.version, tc.commit, got, tc.want)
		}
	}
}
