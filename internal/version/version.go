// Package version reports which build a binary is, so a binary handed to a
// customer can say so itself.
package version

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// Set at link time by the release build, because the image is built without
// .git and the toolchain cannot see the tag or commit. Elsewhere they are empty
// and the build info is used instead. revision may end in "-dirty".
var (
	release  string
	revision string
)

// revisionLength is how much of a commit hash is shown: git's short hash, so
// it can be pasted into git as it is.
const revisionLength = 7

// String reports the version and the commit the binary was built from, as
// "v1.2.0+d1c9ed2", "devel+d1c9ed2" or "devel+d1c9ed2-dirty". The same commit
// reads the same whether it was built by `make build`, `make image`,
// `make dist` or the flake.
func String() string {
	info, _ := debug.ReadBuildInfo()
	return format(release, revision, info)
}

func format(release, revision string, info *debug.BuildInfo) string {
	v := release
	rev, dirty := strings.CutSuffix(revision, "-dirty")
	if info != nil {
		var checkout bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				checkout = true
				if revision == "" {
					rev = s.Value
				}
			case "vcs.modified":
				if revision == "" {
					dirty = s.Value == "true"
				}
			}
		}
		if v == "" {
			v = toolchainVersion(info.Main.Version, checkout)
		}
	}
	if v == "" {
		v = "devel"
	}
	if rev == "" {
		return v
	}
	if len(rev) > revisionLength {
		rev = rev[:revisionLength]
	}
	if dirty {
		rev += "-dirty"
	}
	return v + "+" + rev
}

// pseudoVersion matches the end of a Go pseudo-version, such as
// v0.2.1-0.20261005043153-d1c9ed2fa3ad.
var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}$`)

// toolchainVersion is the version the Go toolchain stamped, kept only when it
// is a release. In a checkout the toolchain makes up a pseudo-version from the
// last tag, which a build without .git cannot, so it reads "devel" like those.
// Outside a checkout, as with `go install ...@commit`, the pseudo-version is
// the only record of the commit and is kept.
func toolchainVersion(v string, checkout bool) string {
	v = strings.TrimSuffix(v, "+dirty")
	switch {
	case v == "" || v == "(devel)":
		return "devel"
	case checkout && pseudoVersion.MatchString(v):
		return "devel"
	default:
		return v
	}
}
