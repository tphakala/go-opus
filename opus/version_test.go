package opus

import (
	"os"
	"regexp"
	"testing"
)

// semverCore is the shape Version must have: MAJOR.MINOR.PATCH, each a numeric
// identifier with no leading zero (so Go's module resolver accepts it), with an
// optional pre-release suffix and no leading "v" (the tag adds that). Build
// metadata ("+...") is excluded because Go module tags cannot carry it. Keep this
// in exact agreement, case for case, with the bash ERE in scripts/release.sh
// (each written in its own dialect, kept in sync by hand); TestSemverCore exercises
// this RE2 pattern.
var semverCore = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(-(0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

// TestSemverCore exercises the shape regexp directly, since in normal use it only
// ever sees the one correct Version constant. The reject cases pin the rules the
// pattern enforces: three dot-separated numeric identifiers, no leading zero, no
// leading "v", no build metadata, and a nonempty pre-release after the hyphen. The
// bash ERE in scripts/release.sh must agree case for case; that agreement is kept by
// hand and is not asserted by any automated test today.
func TestSemverCore(t *testing.T) {
	accept := []string{
		"1.1.0", "0.0.0", "10.20.30",
		// A pre-release is a dot-separated list of identifiers, each either a
		// numeric identifier with no leading zero or one carrying a letter or
		// hyphen (semver 2.0.0 rule 9).
		"1.1.0-rc.1", "1.1.0-0", "1.1.0-0a", "1.1.0-alpha-1", "1.1.0-x.7.z.92",
	}
	reject := []string{
		"v1.1.0", "1.1", "", "1.1.0+meta", "1.1.0.0", "1.1.0 ",
		// Leading zeros in a numeric identifier, in each of the four positions
		// the pattern repeats the rule, so a copy-paste slip in any one of them
		// is detectable rather than only the MAJOR position.
		"01.2.3", "1.02.3", "1.2.03", "1.1.0-01",
		// Empty pre-release identifiers: trailing, doubled separator, and a lone
		// dot. Go rejects all three as module versions, and `task release` would
		// otherwise bump the constant and cut a tag for them.
		"1.1.0-", "1.1.0-rc..1", "1.1.0-.", "1.1.0-rc.",
	}
	for _, s := range accept {
		if !semverCore.MatchString(s) {
			t.Errorf("semverCore rejected %q, want accept", s)
		}
	}
	for _, s := range reject {
		if semverCore.MatchString(s) {
			t.Errorf("semverCore accepted %q, want reject", s)
		}
	}
}

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
