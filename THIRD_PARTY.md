# Third-party provenance

go-opus is a native Go port of the Opus audio codec. Unlike a clean-room
reimplementation, the codec logic here is a deliberate transliteration of
libopus: the decoder is specified by RFC 6716, but the encoder is not specified
by any RFC, so libopus is effectively the specification and the only sane route
is a faithful port of it. go-opus is therefore a derivative work of libopus and
is licensed under the same 3-clause BSD license (see LICENSE).

## libopus (BSD 3-clause, transliteration reference)

- Upstream: https://gitlab.xiph.org/xiph/opus (GitHub mirror: xiph/opus).
- Transliteration reference pinned at tag **v1.6.1**. Doc line numbers in docs/
  cite commit 3da9f7a6 (v1.6.1+50); expect a few lines of drift against the tag.
- Every transliterated Go file keeps the upstream libopus BSD 3-clause copyright
  header. The frozen build configuration ported is FIXED_POINT +
  DISABLE_FLOAT_API, with no QEXT/RES24/dnn/CUSTOM_MODES.
- Opus is an IETF codec with royalty-free patent grants (see LICENSE for the
  IPR links). A reimplementation/port is clean with respect to those grants.
- The pinned libopus sources live under internal/reftest as a git submodule and
  are built only by the cgo differential-test harness (build tag `refc`); they
  are never compiled into the published pure-Go module.

## SIMD

- github.com/tphakala/simd (MIT, same author) is a runtime dependency: it backs
  the two hot pitch kernels (celt_inner_prod and xcorr_kernel) through
  i16.DotProduct and i16.XCorr, mirroring the go-flac scalar-reference-plus-SIMD
  structure. The library is pure Go plus its own per-arch assembly (SMLAL/SMLAL2
  on arm64, PMADDWD/AVX2 on amd64) and has no cgo. Each kernel keeps a scalar
  reference (internal/celt/pitch_ref.go) that the library results are asserted
  bit-exact against (internal/celt/pitch_simd_test.go). The remaining
  Opus-specific fused kernels are still scalar and are candidates for later SIMD.

## Test corpus and conformance vectors

- RFC 6716 / RFC 8251 official Opus test vectors: fetched and sha256-verified by
  scripts/fetch-vectors.sh, used for testing only, never committed (see
  .gitignore and testdata/vectors.sha256). RFC 8251 is the archive that matters;
  it only revised the .dec reference outputs.
- Local corpus (bird recordings, speech, music, sine sweeps, silence, noise) is
  used by differential tests and is not committed.

## Ruleguard DSL

- github.com/quasilyte/go-ruleguard/dsl (BSD 3-clause): build-time-only
  dependency backing the custom gocritic ruleguard matchers in rules/. Anchored
  by tools/tools.go; never compiled into the module.
