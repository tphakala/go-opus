#!/usr/bin/env bash
# Cut a go-opus release: bump opus.Version, run the release-tag guard, commit and
# tag in one step so the constant and the tag cannot disagree (the v1.1.0 tag
# shipped with the constant still at 1.0.0; see GitHub issue #68).
#
# Usage: scripts/release.sh X.Y.Z[-pre]   (or: task release VERSION=X.Y.Z)
# Pushes nothing; it prints the push command at the end.
set -euo pipefail

version="${1:-}"
if [[ -z "$version" ]]; then
  echo "usage: $0 X.Y.Z[-pre]" >&2
  exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "error: '$version' is not MAJOR.MINOR.PATCH[-prerelease] (no leading v)" >&2
  exit 2
fi
tag="v$version"

cd "$(git rev-parse --show-toplevel)"
branch="$(git branch --show-current)"
if [[ "$branch" != "main" ]]; then
  echo "error: releases are cut from main (currently on '$branch')" >&2
  exit 1
fi
if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: working tree is not clean" >&2
  exit 1
fi
git fetch --quiet --tags origin
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  echo "error: tag $tag already exists; if it exists only locally, remove it with 'git tag -d $tag' and re-run" >&2
  exit 1
fi
# Refuse to release from a main that is behind its remote. git updates refs
# independently, so pushing a tag off a stale main would publish a tag whose
# commit is not on origin/main. Only checked when origin/main is known locally.
if git rev-parse -q --verify refs/remotes/origin/main >/dev/null; then
  if ! git merge-base --is-ancestor refs/remotes/origin/main HEAD; then
    echo "error: local main is behind origin/main; pull first" >&2
    exit 1
  fi
fi

file="opus/opus.go"
if ! grep -q '^const Version = "' "$file"; then
  echo "error: no 'const Version = \"...\"' line in $file" >&2
  exit 1
fi
# Portable in-place edit (BSD sed has no GNU -i form): rewrite via a temp file.
# $log (used by the guard below) is created here too so a single EXIT trap covers
# both temp files. Both live outside the repo, so they never dirty the tree.
tmp="$(mktemp)"
log="$(mktemp)"
trap 'rm -f "$tmp" "$log"' EXIT
sed "s/^const Version = \".*\"\$/const Version = \"$version\"/" "$file" > "$tmp"
# cat, not mv: mktemp creates the temp at 0600, and mv would carry that mode onto
# opus.go (observed 664 -> 600). Writing through cat keeps $file's existing mode.
cat "$tmp" > "$file"
# From here until `git add`, restore opus.go to HEAD on any failure (the gofmt
# check, the guard test), so a partial run never leaves the bump uncommitted for
# the next run's clean-tree guard to trip over. The clean-tree check above means
# HEAD is exactly the pre-bump state.
trap 'git checkout -q -- "$file"' ERR
if [[ -n "$(gofmt -l "$file")" ]]; then
  echo "error: $file is not gofmt clean after the bump" >&2
  exit 1
fi

# Verify the constant against the tag and require the guard's PASS line, not just
# a green `go test`: renaming TestVersionMatchesReleaseTag (so '^TestVersion' no
# longer selects it) or unsetting GOOPUS_RELEASE_TAG (so it SKIPs) would leave the
# run green without the guard ever running, and a SKIP is not a PASS. The oggopus
# vendor-string tests compare symbolically against opus.Version, so they cannot
# fail on its value; they are folded in here at no cost.
echo "verifying opus.Version against $tag"
GOOPUS_RELEASE_TAG="$tag" go test -count=1 -v \
  -run '^TestVersion|^TestVendorString|^TestConfigVendorDefault' ./opus/ ./oggopus/ | tee "$log"
grep -qF -- '--- PASS: TestVersionMatchesReleaseTag (' "$log"

trap - ERR
git add "$file"
if git diff --cached --quiet; then
  # The constant already read $version, so there is nothing to commit. The point
  # of this run is the tag, so tag HEAD anyway instead of dying on git commit's
  # "nothing to commit, working tree clean".
  echo "opus.Version already reads $version; tagging HEAD without a new commit"
else
  git commit --quiet -m "chore: bump version to $version"
fi
git tag -a "$tag" -m "$tag"
# --atomic so a rejected main (someone pushed first) also rejects the tag, instead
# of leaving a published tag whose commit never reached origin/main.
echo "tagged $tag; publish with:"
echo "  git push --atomic origin main $tag"
