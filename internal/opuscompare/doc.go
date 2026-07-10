// Package opuscompare is a Go port of libopus's opus_compare.c: the RFC 6716
// conformance comparator (a self-contained naive DFT, no kiss_fft dependency),
// so the conformance gate runs in CI with no C toolchain.
package opuscompare
