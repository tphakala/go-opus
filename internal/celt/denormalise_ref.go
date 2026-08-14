package celt

import "github.com/tphakala/go-opus/internal/fixedmath"

// denormaliseGainGeneric is the scalar reference and frozen source of truth for
// the SIMD-backed denormaliseGain (denormalise_simd.go). It is the inner requant
// of denormaliseBands (bands.c:denormalise_bands): each normalised sample src[k]
// is left-shifted by preShift, multiplied by the Q31 band gain g, then rounded
// back down by postShift, writing the frequency-domain sample dst[k].
//
// Like haar1_ref.go and pitch_ref.go this file carries no build tag on purpose:
// it compiles into every build so TestDenormaliseGainMatchesScalar,
// TestDenormaliseGainExtremes and FuzzDenormaliseGain always have the reference
// to diff denormaliseGain against, sample for sample, even in the CI jobs that
// do not build the cgo reftest oracle.
//
// Each output element is an independent expression with no accumulation across
// elements, so unlike the pitch kernels there is no reassociation argument to
// make: the SIMD path earns its bit-exactness by computing the same per-element
// expression, not by regrouping a reduction. n = min(len(dst), len(src)) matches
// i32.GainQ31's length contract exactly, so a short dst leaves the same tail
// untouched on both paths.
func denormaliseGainGeneric(dst, src []int32, g int32, preShift, postShift int) {
	n := min(len(dst), len(src))
	for k := 0; k < n; k++ {
		dst[k] = fixedmath.PSHR32(fixedmath.MULT32_32_Q31(fixedmath.SHL32(src[k], preShift), g), postShift)
	}
}
