# go-opus

[![CI](https://github.com/tphakala/go-opus/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-opus/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-opus.svg)](https://pkg.go.dev/github.com/tphakala/go-opus)
[![codecov](https://codecov.io/gh/tphakala/go-opus/branch/main/graph/badge.svg)](https://codecov.io/gh/tphakala/go-opus)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-opus)](go.mod)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tphakala/go-opus/badge)](https://scorecard.dev/viewer/?uri=github.com/tphakala/go-opus)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

A native Go implementation of the [Opus](https://opus-codec.org/) audio codec
(RFC 6716), built as a pure-Go port of [libopus](https://gitlab.xiph.org/xiph/opus).
No cgo. The one runtime dependency is github.com/tphakala/simd (pure Go plus its own assembly, also cgo-free), which backs the hot pitch kernels.

## Status

Every codec layer is validated bit-for-bit against the C reference before it
lands (see Approach), so the pieces marked done are done in the strong sense:
byte-identical to libopus.

- **Decoder: complete.** The full Opus decoder passes RFC 6716 conformance,
  decoding all 12 official test vectors with the per-packet range state matching
  libopus exactly. This covers CELT, SILK, and hybrid modes, mode switching and
  redundancy (including the SILK/CELT crossovers), packet-loss concealment, and
  inband FEC/LBRR.
- **Encoder: complete (CELT-only).** The fixed-point encoder produces whole Opus
  packets that are byte-identical to the C reference. Its gate ran 10.3 million
  frame pairs, every frame of a 52-clip corpus across 16 to 128 kbps, CBR/VBR and
  constrained VBR, complexity 0 to 10, and 2.5/5/10/20 ms frames: 417 MB of packet
  bytes, all byte-identical, with the range state matching on every frame. Encoded
  packets also decode identically through the Go and the C decoder. Sample rates 8,
  12, 16, 24 and 48 kHz are all bit-exact, mono and stereo. SILK and hybrid
  encoding are not implemented, so the encoder is CELT-only; the decoder handles
  all three modes.

## Approach

go-opus is a faithful transliteration of libopus **v1.6.1**, kept honest by
differential testing. A cgo test harness (build tag `refc`) compiles a
fixed-point build of the C reference (`FIXED_POINT + DISABLE_FLOAT_API`), and
every ported layer, range coder, fixed-point math, MDCT, PVQ, bit allocation,
NLSF, the synthesis filters, is asserted **bit-exact** against it, usually over
exhaustive or multi-second randomized sequences. The published module itself is
pure Go with zero cgo.

The internal codec packages (`internal/celt`, `internal/silk`, the range coder,
and the fixed-point math) are deliberately written in a C-shaped style so they
stay diffable against upstream libopus; the public API is idiomatic Go.

## Why fixed-point

go-opus tracks the fixed-point build of libopus (`FIXED_POINT +
DISABLE_FLOAT_API`), not the float build, for one overriding reason: integer
arithmetic is exactly reproducible across every CPU and compiler, whereas float
output drifts bit-for-bit (FMA contraction, SIMD reassociation, rounding modes).
That determinism is what makes bit-exact differential testing against the C
reference possible at all, and it means go-opus produces byte-identical,
reproducible output on every platform, which is what archival and analysis
pipelines need. Quality is that of the fixed-point reference (both libopus builds
are RFC-conformant and perceptually equivalent); go-opus reproduces it exactly.
Fixed-point is also SIMD-friendly: the integer multiply-accumulate kernels map
straight onto SSE and NEON, and because integer SIMD is exact, accelerated
kernels can stay bit-exact against the same test suites (planned for a later
phase, behind the scalar function signatures).

## Performance

go-opus encodes at **1.6x to 2.0x the time of the equivalent C**, decodes at
**1.4x to 1.5x**, and **allocates nothing per frame on either path**. That is
the honest headline. There is more on the table.

The C it is measured against is the pinned v1.6.1 oracle built `FIXED_POINT +
DISABLE_FLOAT_API`, with libopus's hand-written NEON and SSE kernels switched
off. Both benchmarks live in one file and one package, driven from one PCM
generator and one config, so they cannot drift apart; and because the port is
bit-exact, the harness *asserts* that the two sides encode to byte-identical
packets before it times either of them. If they were doing different work, the
bytes would say so.

darwin/arm64 (Apple M4 Pro), Go 1.26, 48 kHz, 64 kbps per channel, median of 5.
"Before" is the pre-optimization baseline; the C column is the same C in both
runs, so it doubles as a drift control.

**Encode**

| Frame | Go before | Go now | C | Go/C before | **Go/C now** | Allocs |
| ----- | --------: | -----: | ------: | ---------: | -----------: | -----: |
| mono 10 ms | 45.9 us | **31.0 us** | 15.3 us | 2.99x | **2.03x** | 94 → **0** |
| mono 20 ms | 89.0 us | **59.6 us** | 30.4 us | 2.90x | **1.96x** | 151 → **0** |
| stereo 10 ms | 108.5 us | **85.8 us** | 52.0 us | 2.02x | **1.65x** | 256 → **0** |
| stereo 20 ms | 212.5 us | **173.7 us** | 101.8 us | 2.04x | **1.71x** | 501 → **0** |

**Decode**

Pooling the decoder's per-frame scratch took decode from its old 1.5x-1.8x to:

| Frame | Go | C | **Go/C** | Allocs |
| ----- | --: | ------: | -------: | -----: |
| mono 10 ms | 10.9 us | 7.5 us | **1.45x** | 32 → **0** |
| mono 20 ms | 23.0 us | 16.9 us | **1.36x** | 46 → **0** |
| stereo 10 ms | 21.7 us | 14.3 us | **1.52x** | 43 → **0** |
| stereo 20 ms | 45.1 us | 31.8 us | **1.42x** | 70 → **0** |

The encoder runs at roughly **115x realtime** for stereo 20 ms frames and the
decoder at **440x**, so an hour of audio encodes in about half a minute.

### Where the gap comes from, and what closed it

Switching libopus's hand-written SIMD off does **not** produce scalar C, which
is the trap this benchmark first fell into. `clang -O2` auto-vectorizes the
fixed-point kernels by itself: disassembling the oracle with its exact build
flags finds NEON integer multiply-accumulate (`smlal.4s`, `addv.4s`) in 15 of
the 19 hot kernels. Go's compiler emits none, anywhere. So this was never a
language-overhead measurement; it is **scalar Go against auto-vectorized C**.

The original gap, attributed by measurement rather than assumption: **72%**
missing vectorization, 15% allocation, 11% bounds checks, 2% GC. The control
that settles it is `quant_partition`: the one hot kernel clang did *not*
vectorize, and the one kernel where **Go is faster than C**. Go's code
generation is not the problem. Its lack of vectorization is.

Four things closed most of it:

- **A transliteration bug.** `celt_pitch_xcorr` had been ported from the `#if 0`
  branch of the C (the one libopus explicitly disables) instead of the live,
  register-blocked one. Fixing that was worth 2.3x on the kernel, in pure Go.
- **Hand-written NEON and SSE2** for `celt_inner_prod` and `xcorr_kernel`, which
  the profile named as the two worst. 6x and 7.4x on those kernels. Integer SIMD
  can be bit-exact (wrapping two's-complement addition is associative, so lane
  grouping cannot change the result), which is a property float SIMD does not
  have, and a large part of why this codec is fixed-point.
- **Pooling the per-frame scratch on both paths**, taking 501 allocations per
  stereo encode frame and 70 per decode frame to zero. Worth less time than it
  looks on encode (allocation was 15% of the gap, and GC ~0%) but 5-10% on
  decode, and 145 KB of garbage per encode frame is real pressure on a *host*
  application's collector, which a benchmark loop cannot see. The contract that
  makes pooling bit-exact (every buffer written before it is read) is enforced
  on every CI run by a build-tagged allocator that poisons reused buffers.
- **Bounds-check elimination in the four hottest scalar kernels** (the PVQ
  search, `exp_rotation`, the norm inner product, `haar1`): overlapping window
  slices let the compiler prove the hot loops in range, removing 2-4 checks per
  element. Worth 4-5% on stereo encode. Whole-program bounds checks are bounded
  at 11% of the gap, so the remaining long tail is tracked but not urgent.

What remains is the rest of that 72%: the fused, Opus-specific kernels the
profile ranks next: the PVQ search, `exp_rotation`, the MDCT butterflies, the
comb filter.

Reproduce with:

```bash
go test -tags refc ./internal/reftest/oracle/ -run '^$' -bench 'Encode|Decode' -benchmem
```

(That needs the pinned libopus submodule and a C toolchain; the published module
itself has no cgo.)

### Against libopus as it ships

The comparison above is deliberately artificial: it holds the build constant to
isolate the language. This one is the opposite, and answers the question a user
actually has, which is what happens if they swap `opusenc` for go-opus. Encoding
a 5-minute 48 kHz stereo WAV to Ogg Opus at 96 kbps, single-threaded:

| Encoder | Wall | Peak RSS | Throughput | Achieved |
| ------- | ---: | -------: | ---------: | -------: |
| go-opus (`cmd/wav2opus`) | 1.85 s | 9.1 MB | 31.1 MB/s | 95.6 kbps |
| opusenc (opus-tools 0.2, libopus 1.6.1) | 1.29 s | 3.0 MB | 44.7 MB/s | 93.0 kbps |
| ffmpeg 8.1.2 (libopus) | 1.45 s | 18.0 MB | 39.7 MB/s | 93.0 kbps |

**go-opus is about 1.4x slower than opusenc** (it was 1.9x before the encoder
optimizations), and the comparison is genuinely mode-matched: every packet from
all three encoders was parsed back and all 15,001 of them carry TOC config 31
(CELT-only, 20 ms, fullband), so this is CELT against CELT rather than CELT
against whatever libopus felt like choosing.

Two things make this number *kinder* to go-opus than it looks, and both are
worth stating plainly:

- **libopus is doing work go-opus simply does not do.** The shipped libopus is a
  float build, and at complexity >= 7 it runs a tonality analysis pass
  (`analysis.c`) that our `DISABLE_FLOAT_API` port compiles out entirely. So the
  C is paying for a stage we skip. That, not superior Go codegen, is why 1.9x
  here looks better than the 2.25x measured against the identical fixed-point C
  above. **The 2.25x is the honest codec-core ratio; the 1.9x is the honest
  end-to-end one.** Neither supersedes the other.
- ffmpeg spends more CPU than wall time even at `-threads 1` (its demux/mux
  helpers), so it gets parallelism go-opus does not. `opusenc`, whose CPU time
  matches its wall time, is the fair reference.

Reproduce with `scripts/bench-encoders.sh`, which generates a deterministic
input, prints both caveats, and skips any comparator that is not installed.

## Packages

- `opus`: the raw packet codec, an `Encoder` and a `Decoder` operating on Opus
  packets and interleaved 16-bit PCM.
- `oggopus`: the RFC 7845 Ogg Opus container, an io-based streaming reader and
  writer whose page granules are written inline, so the output duration is
  correct even on a non-seekable sink. The writer's pre-skip and end trim are
  checked by decoding an impulse back to the exact sample it went in at, and the
  output is validated against ffmpeg rather than only against our own reader.
- `pcm`: a thin facade over `oggopus` that presents the codec under the uniform
  `<module>/pcm` import path, matching
  [go-flac](https://github.com/tphakala/go-flac)'s and
  [go-aac](https://github.com/tphakala/go-aac)'s `pcm` packages so a consumer
  that wraps all three behind one codec interface swaps only the import path. Its
  types are aliases of the `oggopus` types, so the two packages interoperate
  without conversion.

The public API follows the conventions of its sibling project
[go-flac](https://github.com/tphakala/go-flac).

## License

BSD 3-clause. go-opus is a derivative work of libopus and is distributed under
the same license, carrying the upstream Opus copyright and the Opus
royalty-free patent grants. See [LICENSE](LICENSE) and
[THIRD_PARTY.md](THIRD_PARTY.md).
