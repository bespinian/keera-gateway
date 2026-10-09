package version

import (
	"runtime/debug"
	"testing"
)

const commit = "d1c9ed2fa3ad12be406c6996be72e8f66db0d74f"

// checkout is a build from a checkout of commit.
func checkout(version string, modified bool) *debug.BuildInfo {
	m := "false"
	if modified {
		m = "true"
	}
	return &debug.BuildInfo{
		Main: debug.Module{Version: version},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: commit},
			{Key: "vcs.modified", Value: m},
		},
	}
}

func noVCS(version string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Version: version}}
}

func TestOneCommitReadsTheSameEverywhere(t *testing.T) {
	// Each build of one commit, clean and dirty: `make build` from a
	// checkout, `make image` and `make dist` stamping the full hash, and the
	// flake stamping release=devel and a short one.
	for _, tc := range []struct {
		name              string
		release, revision string
		info              *debug.BuildInfo
		want              string
	}{
		{"make build", "", "", checkout("v0.2.1-0.20261005043153-d1c9ed2fa3ad", false), "devel+d1c9ed2"},
		{"make image", "", commit, noVCS("(devel)"), "devel+d1c9ed2"},
		{"make dist", "", commit, checkout("v0.2.1-0.20261005043153-d1c9ed2fa3ad", false), "devel+d1c9ed2"},
		{"flake", "devel", "d1c9ed2", noVCS("devel"), "devel+d1c9ed2"},

		{"make build, dirty", "", "", checkout("v0.2.1-0.20261005043153-d1c9ed2fa3ad+dirty", true), "devel+d1c9ed2-dirty"},
		{"make image, dirty", "", commit + "-dirty", noVCS("(devel)"), "devel+d1c9ed2-dirty"},
		{"flake, dirty", "devel", "d1c9ed2-dirty", noVCS("devel"), "devel+d1c9ed2-dirty"},

		{"make build, tag", "", "", checkout("v0.2.0", false), "v0.2.0+d1c9ed2"},
		{"make build, tag, dirty", "", "", checkout("v0.2.0+dirty", true), "v0.2.0+d1c9ed2-dirty"},
		{"release", "v0.2.0", commit, noVCS("(devel)"), "v0.2.0+d1c9ed2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := format(tc.release, tc.revision, tc.info); got != tc.want {
				t.Errorf("format = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithoutACommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"go install @version", noVCS("v0.2.0"), "v0.2.0"},
		// Without a checkout the pseudo-version is all that names the commit.
		{"go install @commit", noVCS("v0.2.1-0.20261005043153-d1c9ed2fa3ad"), "v0.2.1-0.20261005043153-d1c9ed2fa3ad"},
		{"go run", noVCS("(devel)"), "devel"},
		{"no build info", nil, "devel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := format("", "", tc.info); got != tc.want {
				t.Errorf("format = %q, want %q", got, tc.want)
			}
		})
	}
}
