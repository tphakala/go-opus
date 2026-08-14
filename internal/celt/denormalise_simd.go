package celt

import "github.com/tphakala/simd/i32"

// denormaliseGain is the inner requant of denormaliseBands (bands.c:
// denormalise_bands), the SIMD-backed form of denormaliseGainGeneric
// (denormalise_ref.go). It applies one Q31 band gain g to a run of normalised
// samples, with a per-band input pre-shift and a rounding output post-shift, all
// fused into a single vectorized pass by i32.GainQ31.
//
// Bit-exactness. i32.GainQ31 writes, for k in [0, min(len(dst), len(src))):
//
//	dst[k] = PSHR32(MULT32_32_Q31(SHL32(src[k], preShift), g), postShift)
//
// which is denormaliseGainGeneric's body, term for term. Every stage wraps in
// int32: SHL32 is the wrapping left shift, MULT32_32_Q31 forms the product in
// int64 and truncates the Q31 result toward -inf with no rounding, and PSHR32
// adds the round-half-up bias and arithmetic-shifts. GainQ31 documents itself
// bit-identical to that fixedmath composition across AVX2, NEON and the pure-Go
// fallback, with no relaxed tier. There is no accumulation, so lane grouping
// cannot change any result. The differential proof over the (len, g, preShift,
// postShift, input) matrix, the int32 wrapping edges and fuzz is in
// denormalise_simd_test.go.
//
// Domain. GainQ31 panics unless preShift and postShift are both in [0,31].
// denormaliseBands only ever calls this with preShift = 30-normShift = 6 and
// postShift = shift, which its own clamps keep in [0,30] (shift>=31 becomes 0
// with g=0; shift<0 becomes 0), so both are always in range.
//
// Aliasing. GainQ31 allows dst to alias src exactly element for element (each
// lane reads src[k] before its own dst[k] store) but not a shifted overlap.
// denormaliseBands always passes the identical [fi:bandEnd] window for both, and
// its callers pass either distinct freq/X buffers or the same one, so dst and
// src are disjoint or exactly aliased, never shifted.
func denormaliseGain(dst, src []int32, g int32, preShift, postShift int) {
	i32.GainQ31(dst, src, g, preShift, postShift)
}
