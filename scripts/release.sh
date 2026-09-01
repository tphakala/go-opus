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
  echo "error: tag $tag already exists" >&2
  exit 1
fi

file="opus/opus.go"
if ! grep -q '^const Version = "' "$file"; then
  echo "error: no 'const Version = \"...\"' line in $file" >&2
  exit 1
fi
# Portable in-place edit (BSD sed has no GNU -i form): rewrite via a temp file.
tmp="$(mktemp)"
sed "s/^const Version = \".*\"\$/const Version = \"$version\"/" "$file" > "$tmp"
mv "$tmp" "$file"
if [[ -n "$(gofmt -l "$file")" ]]; then
  echo "error: $file is not gofmt clean after the bump" >&2
  exit 1
fi

echo "verifying opus.Version against $tag"
GOOPUS_RELEASE_TAG="$tag" go test -count=1 -run '^TestVersion' ./opus/
go test -count=1 -run '^TestVendorString|^TestConfigVendorDefault' ./oggopus/

git add "$file"
git commit --quiet -m "chore: bump version to $version"
git tag -a "$tag" -m "$tag"
echo "committed and tagged $tag; publish with:"
echo "  git push origin main $tag"
