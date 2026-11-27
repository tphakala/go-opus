// Package silk is a Go port of the libopus SILK codec (silk/): the decoder
// (phase 3) and, optionally, the encoder (phase 5). It carries per-file
// provenance references to the libopus v1.6.1 source and is held bit-exact
// against it by the refc differential gate in internal/reftest/oracle;
// bit-exactness of the output is the sole correctness contract.
//
// C's per-frame VARDECL/ALLOC buffers are fixed-size Go arrays sized by the C
// maxima and sliced to the C length with a full slice expression (arr[:n:n]).
// They stay on the goroutine stack (go build -gcflags=-m shows none moved to the
// heap) and are zeroed on every call as make zeroed them. Nothing is pooled, so
// the CELT scratch zeroing contract and the poison harness do not apply here. A
// new buffer follows the same pattern, with its bound commented at the site.
package silk
