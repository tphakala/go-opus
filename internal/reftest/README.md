# internal/reftest: libopus differential oracle

The cgo differential-test harness for go-opus. It builds the pinned libopus as a
**bit-exact fixed-point oracle** and calls its encoder and decoder from Go, so
every go-opus stage can be diffed against the reference (`docs/plan.md`, test
strategy 2-3). Everything cgo/libopus-touching is behind the `refc` build tag;
the normal `go build ./...` and `go vet ./...` stay pure-Go and never invoke a C
compiler.

## Layout

| Path | What |
|---|---|
| `libopus/` | libopus git submodule, pinned at tag **v1.6.1** |
| `oracle/` | cgo package that compiles libopus as the FIXED_POINT + DISABLE_FLOAT_API oracle and exposes a Go-typed encoder/decoder/final-range/state-hash surface (all `refc`-tagged) |
| `oracle/w_*.c` | 123 generated one-line wrappers, one per libopus source (see below) |
| `oracle/gen_wrappers.sh` | regenerates `w_*.c` from libopus's own `*_sources.mk` manifests |
| `oracle/shim.h` / `shim.c` | thin C surface (keeps the CTL varargs macros on the C side) + compile-time config asserts + state-hash tap |
| `oracle/oracle_cgo.go` | Go bindings |
| `oracle/oracle_test.go` | build-config assertion + round-trip smoke + state-hash tests |
| `doc.go`, `oracle/doc.go` | no-tag package docs so both packages build pure-Go |

## Pinned reference

- Tag: **v1.6.1**
- Resolved commit: **`22244de5a79bd1d6d623c32e72bf1954b56235be`**
- Origin: `https://github.com/xiph/opus.git` (GitHub mirror; `gitlab.xiph.org` is
  the canonical origin). Matches `docs/plan.md` ground rules.

Re-pinning is a deliberate, per-release act: check out the new tag inside
`libopus/`, re-run `oracle/gen_wrappers.sh` (picks up added/removed sources), and
re-run the tests on amd64 and arm64.

## Frozen oracle configuration (non-negotiable)

Built exactly per `CLAUDE.md` / `docs/hard-parts.md` section 4:

```
-DFIXED_POINT -DDISABLE_FLOAT_API -DOPUS_BUILD -DHAVE_STDINT_H -DVAR_ARRAYS -O2
```

with **OPUS_FAST_INT64 = 1** (automatic on 64-bit via `celt/arch.h`; the harness
only targets amd64/arm64). No `CUSTOM_MODES`, `ENABLE_QEXT`, `RESYNTH`, dnn, or
SIMD. `shim.c` turns each of these into a compile-time `#error`, so a mis-flagged
build fails to compile instead of silently producing a non-oracle:

```c
#if !defined(FIXED_POINT)  #error ...
#if !defined(DISABLE_FLOAT_API)  #error ...
#if !OPUS_FAST_INT64  #error ...     /* also fails on a 32-bit host */
#if defined(CUSTOM_MODES) / defined(ENABLE_QEXT)  #error ...
```

`GetBuildConfig()` also surfaces the same flags to Go, and `TestBuildConfig`
prints and asserts them. libopus's own version string self-reports `-fixed`
(see `celt/celt.c`), a second independent confirmation of the arithmetic path.
It reads `libopus unknown-fixed` because `PACKAGE_VERSION` is intentionally not
injected (it would be a hand-maintained literal that could drift from the pin);
the authoritative version is the submodule commit above.

## Build approach (a): compile libopus sources directly via cgo

Chosen over approach (b) (autotools/CMake static lib): it is the most
self-contained for CI. The only build dependency is a C compiler with
`CGO_ENABLED=1`; no autotools, no meson, no prebuilt library. The CI `reftest`
job checks out submodules recursively and runs
`go test -tags refc ./internal/reftest/...` directly.

### Why one wrapper per source (not an amalgamation)

libopus is **not** unity/amalgamation-safe. `include/opus_custom.h` gates the
`opus_custom_{encoder,decoder}_{get_size,init}` prototypes on per-file macros
(`CELT_ENCODER_C` / `CELT_DECODER_C`), and `celt/arch.h` gates `celt_fatal` on
`CELT_C`. Each `.c` self-defines its gating macro before including headers, but
combining sources into one translation unit only honours the first includer
(because of the `opus_custom.h` include guard), producing implicit-declaration
errors and, worse, the risk of silent macro-leak miscompiles that would corrupt
the oracle. cgo compiles every `.c` in the package directory, so each source
gets its own translation unit via a one-line wrapper:

```c
//go:build refc

#include "libopus/celt/bands.c"
```

This is exactly how libopus builds itself, and it eliminates the entire class of
unity-build hazards. The `//go:build refc` constraint keeps the wrappers out of
the pure-Go build (Go honours build constraints on `.c` files, so `go build ./...`
neither sees nor rejects them).

### Source set

Taken straight from libopus's `celt_sources.mk` (`CELT_SOURCES`),
`silk_sources.mk` (`SILK_SOURCES` + `SILK_SOURCES_FIXED`), and a `src/` subset
(`opus.c`, `opus_decoder.c`, `opus_encoder.c`, `extensions.c`, `repacketizer.c`).
The full SILK encoder/decoder is linked because `opus_encoder.c` /
`opus_decoder.c` reference SILK symbols unconditionally, even though the harness
forces CELT-only at runtime.

Deliberately **excluded**: all `*_X86_*` / `*_ARM_*` / `*_NEON_*` / `mips` SIMD
sources (no RTCD feature macros are defined, so the portable C fallbacks are
used), `dnn/` (DRED/OSCE/neural PLC), `SILK_SOURCES_FLOAT` and
`OPUS_SOURCES_FLOAT` (`analysis.c`, `mlp*.c` - compiled out by
`DISABLE_FLOAT_API`), and `opus_multistream*` / `opus_projection*` /
`mapping_matrix.c`.

### Include paths (cgo CFLAGS, via `${SRCDIR}`)

```
-I${SRCDIR}/..                       (so wrappers can #include "libopus/...")
-I${SRCDIR}/../libopus               (libopus root)
-I${SRCDIR}/../libopus/include       (public opus.h etc.)
-I${SRCDIR}/../libopus/celt
-I${SRCDIR}/../libopus/silk
-I${SRCDIR}/../libopus/silk/fixed
-I${SRCDIR}/../libopus/src           (opus_private.h: MODE_CELT_ONLY, OPUS_SET_FORCE_MODE)
```

`HAVE_CONFIG_H` is intentionally undefined, so libopus never includes a
`config.h`; all configuration is passed via `-D`.

## Go surface (`refc`)

```go
enc, _ := oracle.NewCELTEncoder(48000, 1, 64000 /*bitrate*/, 5 /*complexity*/)
pkt, _ := enc.Encode(pcm /*[]int16*/, 960 /*frameSize*/)
r := enc.FinalRange()            // OPUS_GET_FINAL_RANGE - primary differential check
h := enc.StateHash()             // per-frame persistent-state hash tap

dec, _ := oracle.NewDecoder(48000, 1)
out, _ := dec.Decode(pkt, 960, false /*decodeFEC*/)
dr := dec.FinalRange()           // must equal enc.FinalRange() for the packet
```

`NewCELTEncoder` forces `MODE_CELT_ONLY` (`OPUS_SET_FORCE_MODE`) with
`OPUS_APPLICATION_AUDIO`, matching the phase-4 encoder oracle exactly.

## Per-frame persistent-state hash tap (hard-parts.md 5 & 7)

`Encoder.StateHash()` / `oracle_encoder_state_hash` exists so multi-frame
sequence tests catch cross-frame divergence on the frame it first appears,
rather than frames later.

**Phase-0 status: working stub.** It is FNV-1a over the whole `OpusEncoder`
allocation (`opus_encoder_get_size(channels)` bytes), which embeds every
cross-frame CELT state field listed in hard-parts.md section 5 (vbr_reservoir/
drift/offset/count, oldBandE/oldLogE/oldLogE2, energyError, prefilter memory,
rng, consec_transient, delayedIntra, ...). It is a real within-run determinism
probe: the hash changes exactly when persistent state changes (`TestStateHashEvolves`
asserts this).

Two documented caveats drive the phase-4 refinement:

1. The allocation includes the embedded `const CELTMode *mode` pointer, so raw
   hash values are not stable across process runs (ASLR). Compare frame-to-frame
   within one run, not against a golden literal.
2. Cross-language comparison against the Go port needs identical **field-level**
   hashing (layouts differ), not a raw struct dump. The phase-4 version will
   `#include "celt/celt_encoder.c"` into `shim.c` (and drop `w_celt_celt_encoder.c`
   from the wrapper set to avoid a duplicate translation unit), then hash the
   named fields from hard-parts.md section 5 in a fixed order; the Go encoder
   taps the same fields the same way.

## Float variant (planned, not yet implemented)

A separate float-configured build is wanted for decoder cross-checks
(`docs/plan.md` test strategy 2). The FIXED_POINT + DISABLE_FLOAT_API build is
the priority and is what exists here. The float variant will be a parallel cgo
package (e.g. `internal/reftest/oracle_float/`) reusing the same submodule and
wrapper-generation approach, differing only in configuration: drop `FIXED_POINT`
and `DISABLE_FLOAT_API`, and additionally compile `SILK_SOURCES_FLOAT` and
`OPUS_SOURCES_FLOAT` (`analysis.c`, `mlp.c`, `mlp_data.c`). Its results are
compared with a quality threshold, never bit-exactly, because Go's permitted FMA
fusion makes float bit-matching unreliable (that is the whole reason the encoder
oracle is fixed-point).

## Running

```
CGO_ENABLED=1 go test -tags refc ./internal/reftest/...   # what CI runs (amd64 + arm64)
task reftest                                              # same, via Taskfile
```

The pure-Go module is unaffected:

```
go build ./...        # no cgo, no libopus
go vet ./...
```
