// Package version reports which build a binary is, so a binary handed to a
// customer can say so itself.
package version

import "runtime/debug"

// Set at link time by the release build, because the image is built without
// .git and the toolchain cannot see the tag or commit. Elsewhere they are empty
// and the build info is used instead.
var (
	release  string
	revision string
)

// String reports the module version stamped in by the Go toolchain, with the
// revision it was built from when the build was a checkout.
func String() string {
	if release != "" {
		return release + suffix(revision)
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		v = "devel"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return v + suffix(s.Value)
		}
	}
	return v
}

// suffix renders a revision to append to a version, or nothing when there is
// none.
func suffix(rev string) string {
	switch {
	case rev == "":
		return ""
	case len(rev) > 12:
		return "+" + rev[:12]
	default:
		return "+" + rev
	}
}
