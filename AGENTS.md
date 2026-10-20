# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (status, benchmarks, approach); `THIRD_PARTY.md` records provenance. This file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-opus` is a pure-Go Opus codec (RFC 6716), a transliteration of libopus v1.6.1 built as `FIXED_POINT + DISABLE_FLOAT_API`. It is the Opus member of a family with go-wav, go-flac, go-aac and go-m4a, and it presents the same API shape as them (`pcm` facade, `NewX` plus `Reset`, flat `Config`, one-shot helpers). It aims to:

1. **Build anywhere Go builds.** No cgo in the published module. The runtime dependency is `github.com/tphakala/simd` (assembly plus pure-Go fallback, also cgo-free) with its indirect `golang.org/x/sys`. `go-ruleguard/dsl` is lint-only behind a build tag.
2. **Be bit-exact with libopus.** Every ported layer, and the encoder's whole packets, are asserted byte-identical to the pinned C reference. The decoder passes all 12 RFC 6716 / RFC 8251 test vectors. Bit-exactness is the correctness contract of the internal codec packages.
3. **Survive hostile input.** The packet parser, the Ogg readers and the decoder parse untrusted bytes. They must never panic, and allocations driven by a stream's own claims must stay bounded. Consumers are long-running services (BirdNET-Go is the main one).
4. **Stay fast and allocation-light.** Steady-state encode and decode of the `opus` codec allocate nothing per frame (benchmarked in `opus/bench_test.go`, asserted by test for the multistream encoder). Encoders and decoders are poolable through `Reset`.

Packages:

| Package | Role |
|---|---|
| `opus` | Raw packet codec: `Encoder`, `Decoder`, multistream variants, packet inspection, `Version`, sentinel errors. Decoder is the full RFC 6716 decoder; encoder is CELT-only |
| `oggopus` | RFC 7845 Ogg Opus container: io-based `Encoder` and `Decoder`, unexported container reader and writer layers (`reader.go`, `writer.go`), pages, OpusHead and OpusTags, seek and `Length`, one-shot helpers |
| `pcm` | Uniform `<module>/pcm` facade over `oggopus`; its types are aliases, so the two interoperate without conversion |
| `internal/celt` | CELT codec port (decoder and CELT-only encoder), scalar references and SIMD dispatch |
| `internal/silk` | SILK decoder port |
| `internal/opusdec`, `internal/opusenc` | Top-level `opus_decoder.c` and `opus_encoder.c` ports (mode transitions, redundancy, PLC, FEC, multistream) |
| `internal/rangecoding`, `internal/fixedmath`, `internal/silkmath` | Range coder and the two fixed-point macro dialects |
| `internal/packet` | TOC, frame codes 0-3, self-delimited framing, padding, repacketizer |
| `internal/opuscompare` | Go port of `opus_compare.c`, the RFC conformance comparator |
| `internal/reftest` | cgo differential harness against pinned libopus (build tag `refc`, test-only) |
| `internal/wav` | Minimal 16-bit PCM WAV reader for the CLIs |
| `cmd/goopusdec`, `cmd/wav2opus`, `cmd/gentables` | Decoder CLI, encoder CLI and benchmark side, table generator |
| `rules`, `tools` | gocritic ruleguard matchers (build tag `ruleguard`, lint-only); pinned tool dependencies |

Non-goals: SILK-only and hybrid encoding (the encoder always emits CELT-only packets), the float API, and the libopus options compiled out of the frozen config (QEXT, RES24, DRED, deep PLC, OSCE, custom modes).

### API stability

The module is past v1. The exported API of `opus`, `oggopus` and `pcm` is the contract: do not change a signature, remove a symbol, change an error sentinel or change emitted bytes without calling it out as breaking and a major-version discussion first. A change that breaks alignment with the sibling libraries needs the siblings considered. `opus.Version` (in `opus/opus.go`) must equal the release tag; the release workflow checks it. Bump it only through `task release VERSION=x.y.z` (`scripts/release.sh`), never by hand.

## Design rules you must not break

A change that violates one of these is wrong even if tests pass.

1. **No cgo in the module, ever.** `CGO_ENABLED=0` must build everything outside `internal/reftest`. cgo lives only in `internal/reftest/oracle` behind the `refc` tag and is never imported by library code. Do not add a runtime dependency.
2. **Fixed-point only, bit-exact with libopus v1.6.1.** The frozen config is `FIXED_POINT + DISABLE_FLOAT_API`, so `opus_res` is `int16` throughout. Do not introduce floating point into the codec path, and do not "simplify" a macro helper in `fixedmath` or `silkmath`: the exact integer expression is the contract (wrapping, truncation and shift order included).
3. **Keep the mapping back to the C.** The transliteration packages keep C names, control flow and per-file provenance comments (e.g. `// celt/rate.c:249`). A refactor that makes them idiomatic at the cost of diffability against libopus is unwanted. Public packages are idiomatic Go.
4. **The libopus pin is deliberate.** v1.6.1 is a submodule at `internal/reftest/libopus`. Re-pinning is a per-release act (regenerate the oracle wrappers, rerun on amd64 and arm64), never a drive-by.
5. **SIMD is an optimisation, never a behaviour.** Each `tphakala/simd`-backed kernel (pitch, haar1, band-gain, comb filter, maxabs) keeps an in-tree scalar reference (`*_ref.go`); the FFT butterflies keep scalar variants in `kiss_fft.go` and a frozen reference in `fft_cint_oracle_test.go`. The SIMD result is asserted bit-exact against each, and output must not depend on the CPU. A new or changed kernel needs a differential test and, where the input space is large, a fuzz target.
6. **The scratch zeroing contract.** C's stack `ALLOC` is uninitialised, Go's `make` zeroes, and a pooled buffer carries the previous frame's values. Every pooled buffer must be fully written before it is read. `internal/celt/scratch.go` documents the contract and the `poison` tag enforces it; read it before touching any scratch field.
7. **Decoding untrusted bytes is total and bounded.** Malformed packets and Ogg input return an error, never a panic or an index error. A length or count read from the stream is checked against what is present before it drives a `make`, a loop or an offset. The one-shot `oggopus.DecodeInterleaved` stops at `DefaultMaxDecodedBytes` (1 GiB) with a wrapped `ErrDecodeLimit`, because a tiny DTX stream decodes to far more than its size; `DecodeInterleavedLimit` takes the ceiling and `NewDecoder` streams in bounded memory. A packet reassembled across Ogg pages is capped: an audio packet over 122,880 bytes per Opus stream fails with a wrapped `ErrPacketTooLarge`, latched until a successful `SeekToSample` or `Reset`, and OpusHead and OpusTags are cut at 61,440 bytes instead of failing.
8. **Typed errors with `%w`.** Map conditions to sentinels so callers can use `errors.Is`:
   - `opus`: `ErrBadArg`, `ErrBufferTooSmall`, `ErrInvalidPacket`, `ErrInternal`, `ErrUnsupported`.
   - `oggopus`: `ErrInvalidConfig`, `ErrClosed`, `ErrNotOpus` (first packet is not an OpusHead; not returned for non-Ogg input, a corrupt OpusHead or OpusTags errors), `ErrSeekUnsupported`, `ErrInvalidSeek`, `ErrPacketTooLarge`.
   - `ErrDecodeLimit` and `ErrPacketTooLarge` are resource limits, not verdicts on the stream. Keep them distinct from the corruption errors.
   - A nil sink or source, and the zero-value encoder or decoder, return plain errors rather than panicking.
9. **Decode output is always 48 kHz interleaved little-endian `int16`.** `Info.InputSampleRate` is informational only; label decoded PCM with the output rate (`OutputSampleRate`). The encoder accepts 8, 12, 16, 24 and 48 kHz, mono or stereo; `Config.Complexity` zero means the default (10), so the useful range is 1..10; frame durations are multiples of 20 ms (20 to 120).
10. **Container timing is inline.** The Ogg writer carries duration in per-page granule positions, so no seeking is required on the sink. Pre-skip and end trim must decode back to the exact sample (impulse test), and output is validated against ffmpeg, not only the package's own reader.
11. **The version is single-sourced.** The OpusTags vendor string derives from `opus.Version`; do not restate it as a literal.
12. **Concurrency.** An `Encoder` or `Decoder` is not safe for concurrent use. There is no mutable package-level state that affects output: the only package-level mutables are the `oggopus` encoder pool and the two codec-constructor test seams, and the SIMD dispatch is decided once at init.
13. **Interop and the C reference are the oracle.** Encoder packets are byte-identical to libopus across the gate corpus; decoder output matches the RFC 8251 reference outputs. Do not weaken a differential assertion to make a change pass.

## Layout

```
opus/            encoder.go, decoder.go, multistream_*.go, opus.go (Version, errors), conformance_test.go
oggopus/         encoder.go, decoder.go, writer.go, reader.go, page.go, header.go, seek.go,
                 decode_oneshot.go, config.go, errors.go, crc.go
pcm/             facade (aliases of oggopus), one-shot helpers, example_test.go
internal/celt/   codec port; *_ref.go scalar references, *_simd.go dispatchers, scratch*.go pool,
                 spreading_hist_{amd64,arm64,noasm}.go plus .s, dispatch_count*.go
internal/silk/   decoder port; internal/opusdec, internal/opusenc: top-level ports
internal/reftest/ README.md, libopus/ (submodule), oracle/ (refc cgo harness, generated w_*.c wrappers)
scripts/         fetch-vectors.sh, release.sh, verify-version.sh, bench-encoders.sh
testdata/        README.md, vectors.sha256 (payloads are fetched, not committed)
```

## Testing approach

- **Regression tests.** New behaviour gets a test; a bug fix gets a regression test that fails before the fix. A test must be able to fail: break the line it pins and confirm.
- **Conformance.** `opus/conformance_test.go` decodes the RFC 6716 / RFC 8251 vectors and skips without them. `task vectors` (`scripts/fetch-vectors.sh`) fetches them and verifies sha256 against `testdata/vectors.sha256`; the payloads are never committed.
- **Differential gate.** `task reftest` (`go test -tags refc ./internal/reftest/...`) builds the pinned libopus oracle (needs the submodule and a C compiler) and asserts bit-exactness layer by layer and for whole encoder packets. CI runs it on amd64 and arm64, plus a nightly full-length tier (`gate-long`).
- **SIMD parity.** `*_simd_test.go` compare each dispatcher with its scalar reference over exhaustive lengths and adversarial patterns. The `forcego` CI job sets `SIMD_DISABLE=all` to exercise the pure-Go fallback; the `dispatchcount` tag proves the dispatchers actually reach the vector kernels (a green parity suite alone cannot tell).
- **Poison harness.** `task test:poison` (`-tags poison`) stamps reused scratch memory to catch a read-before-write.
- **Fuzzing.** CI smoke-runs `FuzzParsePacket` (`internal/packet`), `FuzzDecode` (`opus`), `FuzzReadPage`, `FuzzParseOpusHead`, `FuzzParseOpusTags`, `FuzzContainerReader` (`oggopus`) and the celt SIMD differentials `FuzzXcorrKernel`, `FuzzCombFilterConst`, `FuzzHaar1`, `FuzzBandGainRequant`. A `-fuzz` pattern that matches nothing passes without running, so run a new target in the package that defines it. Crashers are committed under `testdata/fuzz` as regression seeds.
- **Benchmarks.** `task bench`; allocation counts matter on the hot paths. The Go-versus-C comparison lives in `internal/reftest/oracle` (`-tags refc`).

## Commands

Go version is read from `go.mod` (currently 1.27). The golangci-lint version is pinned in `.github/workflows/ci.yml` (`GOLANGCI_LINT_VERSION`) and must match the local toolchain.

```
task build         # go build ./...
task test          # go test -v ./...
task test:short    # go test ./...
task test:race     # go test -race ./...
task test:poison   # codec tests with the scratch poison harness
task bench         # all benchmarks with allocation counts
task reftest       # libopus differential harness (tag refc; needs submodule and C toolchain)
task vectors       # fetch and verify the RFC conformance vectors
task vet           # go vet for amd64, arm64 and 386
task lint          # golangci-lint run ./...
task tidy          # go mod tidy and verify
task check         # build, vet, lint, race tests
task release VERSION=x.y.z   # bump opus.Version, verify, commit and tag (pushes nothing)
```

Run `task check` before pushing, and `task reftest` and `task test:poison` for anything under `internal/`. CI additionally runs the cross-build matrix (linux, darwin, windows on amd64 and arm64), the forced-Go-fallback job, a 386 test job (the only leg with a 32-bit int), a dispatch-count step in the test job, the fuzz smoke job, govulncheck and OpenSSF Scorecard.

## Conventions

- **Lint.** Config is `.golangci.yaml`. The transliteration packages (`celt`, `silk`, `rangecoding`, `fixedmath`, `silkmath`, `opuscompare`, `opusdec`, `opusenc`) have style and complexity linters relaxed on purpose, since fidelity to the C is the metric; correctness linters (staticcheck `SA*`, govet, ineffassign, nilerr) still apply. `opus/`, `oggopus/`, `pcm/` and `cmd/` are held at full strength. `internal/reftest` is held to the idiomatic standard minus a few cgo-hostile checks.
- **No `//nolint` without a linter name and a reason.** If a linter is wrong often enough to silence globally, change `.golangci.yaml` with a comment saying why.
- **Errors.** Error strings carry the package prefix (`opus:`, `oggopus:`). Wrap with `%w`; use `errors.New` for constant messages.
- **Comments.** Explain why: the C source location, the failure being avoided, the measurement behind a constant. Match the surrounding density.
- **Docs follow code.** A behaviour change updates the package doc, the README and the sentinel's comment in the same change. A sentence that is false for some mode or rate is a defect.
- **Commits** use an imperative subject with a type prefix (`feat:`, `fix:`, `docs:`, `build:`, `ci:`, `chore:`, `refactor:`, `perf:`, `deps:`, `test:`), optionally scoped, with `!` for a breaking change. There is no CHANGELOG file; release notes are written on the GitHub release.
- **Dependencies.** Dependabot keeps Go modules and GitHub Actions current. Workflow actions are pinned to a major version tag (not a SHA).

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a caller of the library would see.

Flag these, they are real defects here:

- Any break of the design rules above: cgo in the module, a new runtime dependency, floating point in the codec path, a SIMD kernel without a scalar reference and parity test.
- A divergence from libopus: any change to emitted packets, range state or decoded samples that the `refc` gate or the RFC vectors would not accept.
- A panic, index error or unbounded allocation reachable from packet or Ogg bytes.
- A pooled scratch buffer read before it is written, or state that survives `Reset` when it should not.
- A resource limit reported as a corruption error, or a corrupt stream reported as a limit.
- A hand-edited `opus.Version`, or a version literal duplicated elsewhere.
- A differential or conformance assertion weakened or skipped silently, a test that cannot fail on the behaviour it names, or a doc sentence that is false for some rate, mode or container.

Intentional, do not flag:

- C-shaped code in the transliteration packages: C names, long functions, `var x T; x = e`, no `range int`, original comments. The idiomatic rewrite is the defect there.
- Macro helpers written as long exact expressions, and constants without further explanation when a provenance comment points at the C line.
- The encoder emitting CELT-only packets, and the encoder taking only 8, 12, 16, 24 and 48 kHz.
- Frame durations restricted to multiples of 20 ms in `oggopus.Config`, and complexity zero meaning the default.
- Decode output fixed at 48 kHz regardless of the stream's original rate.
- No tonality analysis (`analysis.c`) in the encoder: it is compiled out by `DISABLE_FLOAT_API`.
- The `pcm` package name colliding with its siblings; callers alias the import.
- The cgo harness under `internal/reftest` and its generated `w_*.c` wrappers.

## Things that do not belong in the repo

- Design docs, specs, plans, research notes and handoff prompts. `docs/` is gitignored on purpose.
- Conformance vector payloads, the local audio corpus (`testdata/corpus/`), raw captures or scratch media. Only the fetch script and checksums are tracked.
- Build outputs (`*.test`, `*.out`, `*.exe`), coverage and profile files, `.serena/`, `.claude/` and `.codegraph/`.
