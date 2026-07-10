# go-opus

A native Go implementation of the [Opus](https://opus-codec.org/) audio codec
(RFC 6716), built as a pure-Go port of [libopus](https://gitlab.xiph.org/xiph/opus).
No cgo and no external libraries in the published module.

## Status

Work in progress. Every codec layer is validated bit-for-bit against the C
reference before it lands (see Approach), so the pieces marked done are done in
the strong sense: byte-identical to libopus.

- **Decoder: complete.** The full Opus decoder passes RFC 6716 conformance,
  decoding all 12 official test vectors with the per-packet range state matching
  libopus exactly. This covers CELT, SILK, and hybrid modes, mode switching and
  redundancy (including the SILK/CELT crossovers), packet-loss concealment, and
  inband FEC/LBRR.
- **Encoder: in development.** A CELT-only fixed-point encoder is being built to
  the same bit-exact standard.

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

## Packages

- `opus` - the raw packet codec: a `Decoder` (and, later, an `Encoder`)
  operating on Opus packets and interleaved 16-bit PCM.
- `oggopus` - the RFC 7845 Ogg Opus container, an io-based streaming reader and
  writer whose page granules are written inline, so the output duration is
  correct even on a non-seekable sink.

The public API follows the conventions of its sibling project
[go-flac](https://github.com/tphakala/go-flac).

## License

BSD 3-clause. go-opus is a derivative work of libopus and is distributed under
the same license, carrying the upstream Opus copyright and the Opus
royalty-free patent grants. See [LICENSE](LICENSE) and
[THIRD_PARTY.md](THIRD_PARTY.md).
