package opus

import (
	"os"
	"regexp"
	"testing"
)

// semverCore is the shape Version must have: MAJOR.MINOR.PATCH with an optional
// pre-release suffix and no leading "v" (the tag adds that). Build metadata
// ("+...") is excluded because Go module tags cannot carry it.
var semverCore = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// TestVersionIsSemver pins the constant's shape so a stray "v", build metadata
// ("+..."), or an empty string cannot ship as the codec version. A pre-release
// suffix like "-dev" or "-rc.1" is deliberately allowed: the release tooling cuts
// pre-release tags, so semverCore accepts them.
func TestVersionIsSemver(t *testing.T) {
	if !semverCore.MatchString(Version) {
		t.Fatalf("opus.Version = %q: want MAJOR.MINOR.PATCH (optional -prerelease), no leading v", Version)
	}
}

// TestVersionMatchesReleaseTag is the release-time guard behind issue #68: the
// v1.1.0 tag was cut with Version still "1.0.0", so every exported clip carried
// a stale OpusTags vendor string and nothing noticed. The Release workflow and
// scripts/release.sh set GOOPUS_RELEASE_TAG to the tag being cut; the test then
// fails unless the constant agrees. It skips (never passes vacuously) when the
// variable is unset, so the ordinary suite is unaffected.
func TestVersionMatchesReleaseTag(t *testing.T) {
	tag, ok := os.LookupEnv("GOOPUS_RELEASE_TAG")
	if !ok {
		t.Skip("GOOPUS_RELEASE_TAG not set; the release-tag check only runs when cutting a release")
	}
	if want := "v" + Version; tag != want {
		t.Fatalf("release tag %q does not match opus.Version %q (expected tag %q): bump the constant in opus/opus.go before tagging",
			tag, Version, want)
	}
}
