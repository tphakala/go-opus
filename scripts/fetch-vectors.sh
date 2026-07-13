#!/usr/bin/env bash
#
# Fetch and verify the official RFC 6716 / RFC 8251 Opus conformance test
# vectors.
#
# The vectors are large (~75 MB per archive) and are never committed (see
# .gitignore, which excludes /testdata/vectors/ and the archives). This script
# downloads them into testdata/vectors/ and sha256-verifies every archive
# against testdata/vectors.sha256 BEFORE extracting; it refuses to extract a
# mismatching archive.
#
# The RFC 8251 archive (opus_newvectors/) is the one conformance needs: it
# carries the bitstreams AND the corrected .dec reference decoder outputs. The
# original RFC 6716 archive (opus_testvectors/) has byte-identical bitstreams
# but the pre-errata .dec outputs; fetch it alongside with FETCH_RFC6716=1.
#
# The script is idempotent: an archive that is already present and verifies is
# not re-downloaded, and an already-extracted vector set is left in place. Set
# FORCE=1 to re-extract over an existing set.
#
# Usage:
#   scripts/fetch-vectors.sh                    # fetch + verify + extract RFC 8251
#   FETCH_RFC6716=1 scripts/fetch-vectors.sh    # also fetch the RFC 6716 set
#   FORCE=1 scripts/fetch-vectors.sh            # re-extract even if present
set -euo pipefail

# Resolve the repo root from this script's own location so it works regardless
# of the current working directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

VECTORS_DIR="${REPO_ROOT}/testdata/vectors"
CHECKSUM_FILE="${REPO_ROOT}/testdata/vectors.sha256"
BASE_URL="https://opus-codec.org/static/testvectors"

log()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# download URL DEST: fetch URL to DEST, preferring curl, falling back to wget.
# Writes to a .partial file first so an interrupted download never masquerades
# as a complete one.
download() {
  local url="$1" dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fSL --retry 3 --connect-timeout 30 -o "${dest}.partial" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "${dest}.partial" "$url"
  else
    die "need either curl or wget to download vectors"
  fi
  mv -f "${dest}.partial" "$dest"
}

sha256_of()   { sha256sum "$1" | awk '{print $1}'; }
expected_sha() { awk -v n="$1" '$2 == n {print $1}' "$CHECKSUM_FILE"; }

# verify PATH NAME: true iff PATH's sha256 matches the recorded hash for NAME.
verify() {
  local path="$1" name="$2" expected actual
  expected="$(expected_sha "$name")"
  [[ -n "$expected" ]] || die "no sha256 recorded for '${name}' in ${CHECKSUM_FILE}"
  actual="$(sha256_of "$path")"
  [[ "$expected" == "$actual" ]]
}

# count_bits DIR: number of *.bit bitstreams directly in DIR.
count_bits() { find "$1" -maxdepth 1 -name '*.bit' 2>/dev/null | wc -l | tr -d ' '; }

# fetch_archive NAME EXTRACT_DIR: ensure the archive NAME is downloaded,
# verified, and extracted so that VECTORS_DIR/EXTRACT_DIR holds the vectors.
fetch_archive() {
  local name="$1" extract_dir="$2"
  local url="${BASE_URL}/${name}"
  local archive="${VECTORS_DIR}/${name}"
  local target="${VECTORS_DIR}/${extract_dir}"

  # Already extracted and not forced: nothing to do (no download required).
  if [[ -z "${FORCE:-}" && -d "$target" && "$(count_bits "$target")" -gt 0 ]]; then
    log "  ${extract_dir}/ already present ($(count_bits "$target") bitstreams); skipping"
    return 0
  fi

  # Reuse a present, verified archive; otherwise (re)download and verify.
  if [[ -f "$archive" ]] && verify "$archive" "$name"; then
    log "  ${name} already downloaded and verified; skipping download"
  else
    if [[ -f "$archive" ]]; then
      warn "${name} is present but its checksum did not match; re-downloading"
      rm -f "$archive"
    fi
    log "  downloading ${name} ..."
    download "$url" "$archive"
    if ! verify "$archive" "$name"; then
      local exp act
      exp="$(expected_sha "$name")"
      act="$(sha256_of "$archive")"
      rm -f "$archive"
      die "sha256 mismatch for ${name}
  expected: ${exp}
  actual:   ${act}
Refusing to extract a corrupt or tampered archive; deleted the bad download."
    fi
    log "  verified ${name} (sha256 OK)"
  fi

  log "  extracting ${name} -> testdata/vectors/${extract_dir}/"
  tar -xzf "$archive" -C "$VECTORS_DIR"
  [[ -d "$target" ]] || die "expected ${extract_dir}/ after extracting ${name}, not found"
}

# summary: print the vector sets present under testdata/vectors/.
summary() {
  log ""
  log "Vectors available under testdata/vectors/:"
  local found=0 d rel n
  for d in "${VECTORS_DIR}/opus_newvectors" "${VECTORS_DIR}/opus_testvectors"; do
    [[ -d "$d" ]] || continue
    rel="${d#"${VECTORS_DIR}/"}"
    n="$(count_bits "$d")"
    log "  ${rel}/: ${n} bitstreams"
    if [[ "$n" -gt 0 ]]; then
      # List the vector numbers on one wrapped line for a quick eyeball. A shell
      # glob, not `find -printf`: -printf is a GNU extension, so the BSD find on a
      # macOS dev box made this pipeline fail, and under `set -eo pipefail` that took
      # the whole script down with it (exit 1) AFTER the vectors had been fetched and
      # extracted perfectly. CI now runs this script and gates on its exit code, so a
      # cosmetic listing must not be able to fail the build.
      for f in "$d"/*.bit; do printf '%s\n' "${f##*/}"; done \
        | sort | sed 's/^testvector//; s/\.bit$//' | paste -sd' ' - \
        | sed 's/^/    testvector: /'
    fi
    found=1
  done
  [[ "$found" -eq 1 ]] || die "no vectors were extracted; see messages above"
}

main() {
  command -v sha256sum >/dev/null 2>&1 || die "required tool 'sha256sum' not found in PATH"
  command -v tar >/dev/null 2>&1       || die "required tool 'tar' not found in PATH"

  mkdir -p "$VECTORS_DIR"
  log "Fetching Opus conformance test vectors into testdata/vectors/"

  # RFC 8251 reference set: always fetched (the set conformance runs against).
  fetch_archive "opus_testvectors-rfc8251.tar.gz" "opus_newvectors"

  # Original RFC 6716 set: optional, byte-identical bitstreams, stale .dec outputs.
  if [[ -n "${FETCH_RFC6716:-}" ]]; then
    fetch_archive "opus_testvectors.tar.gz" "opus_testvectors"
  fi

  summary
}

main "$@"
