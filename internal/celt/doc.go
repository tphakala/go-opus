// Package celt is a transliteration of the libopus CELT codec (celt/): the
// decoder (phase 2) and the CELT-only encoder (phase 4). It is deliberately
// C-shaped and diffable against libopus v1.6.1; do not restructure the verbatim
// zones (rate.c, quant_all_bands machinery). See docs/decoder-architecture.md,
// docs/encoder-architecture.md, docs/celt-bitstream.md.
package celt
