package opus

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
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

// semverAccept and semverReject are the version-string table both validators must
// agree on, case for case: opus.semverCore (RE2), exercised here by TestSemverCore,
// and the bash ERE in scripts/release.sh, exercised by
// TestReleaseShellEREMatchesSemverCore, which reads that ERE out of the script and
// runs the same table through bash. Add a case here and both dialects are checked
// against it.
var semverAccept = []string{
	"1.1.0", "0.0.0", "10.20.30",
	// A pre-release is a dot-separated list of identifiers, each either a
	// numeric identifier with no leading zero or one carrying a letter or
	// hyphen (semver 2.0.0 rule 9).
	"1.1.0-rc.1", "1.1.0-0", "1.1.0-0a", "1.1.0-alpha-1", "1.1.0-x.7.z.92",
}

var semverReject = []string{
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

// TestSemverCore exercises the shape regexp directly, since in normal use it only
// ever sees the one correct Version constant. The reject cases pin the rules the
// pattern enforces: three dot-separated numeric identifiers, no leading zero, no
// leading "v", no build metadata, and a nonempty pre-release after the hyphen.
func TestSemverCore(t *testing.T) {
	for _, s := range semverAccept {
		if !semverCore.MatchString(s) {
			t.Errorf("semverCore rejected %q, want accept", s)
		}
	}
	for _, s := range semverReject {
		if semverCore.MatchString(s) {
			t.Errorf("semverCore accepted %q, want reject", s)
		}
	}
}

// TestReleaseShellEREMatchesSemverCore closes the drift gap between the two version
// validators (issue #73). scripts/release.sh gates the version with a bash ERE and
// opus.semverCore gates it with an RE2 pattern; they must accept and reject the
// same strings, but nothing ran the shell side, so the agreement was kept by hand.
// This reads the ERE out of release.sh (rather than restating it, which would just
// add a third copy that can drift), runs the shared accept/reject table through the
// same `[[ =~ ]]` construct release.sh uses, and fails on any verdict that differs
// from semverCore. It skips where bash is unavailable, which on the CI test matrix
// (ubuntu only) it never is.
func TestReleaseShellEREMatchesSemverCore(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not in PATH; the shell-ERE cross-check only runs where bash is available")
	}
	ere := releaseScriptSemverERE(t)
	for _, group := range [][]string{semverAccept, semverReject} {
		for _, s := range group {
			shell := shellEREMatches(t, bash, ere, s)
			re2 := semverCore.MatchString(s)
			if shell != re2 {
				t.Errorf("version %q: scripts/release.sh ERE matched=%v, opus.semverCore matched=%v; the shell and Go version validators have drifted apart",
					s, shell, re2)
			}
		}
	}
}

// releaseScriptSemverERE extracts the semver ERE from scripts/release.sh by reading
// the pattern out of the `[[ "$version" =~ <ERE> ]]` guard line. The ERE carries no
// whitespace, so it is exactly the run of non-space characters between "=~ " and
// " ]]". Reading it (not restating it) is the point: the test and the script share
// one source of truth. The path is resolved from this test file's location so it
// holds regardless of the go test working directory.
func releaseScriptSemverERE(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate scripts/release.sh")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "scripts", "release.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	m := regexp.MustCompile(`"\$version" =~ (\S+) \]\]`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("could not find the `[[ \"$version\" =~ <ERE> ]]` guard in %s; if the guard was reworded, update this extractor", scriptPath)
	}
	return string(m[1])
}

// shellEREMatches runs one version string against the extracted ERE using the exact
// `[[ =~ ]]` construct release.sh uses: the ERE is inserted literally (unquoted on
// the right of =~, so bash treats it as a regex, and its trailing "$" anchor is
// followed by a space so bash does not read it as an expansion), and the candidate
// is passed as a positional argument rather than interpolated into the script. Exit
// 0 is a match, exit 1 a non-match; any other exit is a bash error (a broken
// pattern) and fails the test.
func shellEREMatches(t *testing.T, bash, ere, version string) bool {
	t.Helper()
	// A pure `[[ =~ ]]` match returns immediately, but bound it anyway so a
	// misbehaving bash can never hang the suite.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	script := `[[ "$1" =~ ` + ere + ` ]]`
	err := exec.CommandContext(ctx, bash, "-c", script, "bash", version).Run()
	if err == nil {
		return true
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("bash rejected the extracted ERE for %q (not a match/no-match result): %v", version, err)
	return false
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
