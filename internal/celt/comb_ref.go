package celt

import "github.com/tphakala/go-opus/internal/fixedmath"

// combFilterConstGeneric is the frozen scalar reference for comb_filter_const_c
// (celt.c:166), the constant-filter portion of the CELT post-filter (decode) and
// prefilter (encode). It is the differential oracle that comb_simd_test.go pins
// the SIMD dispatcher (combFilterConst, comb_simd.go) against on every build, and
// it is also the small-block fallback the dispatcher calls directly. Do not
// optimize this: its whole value is being a byte-identical, self-evidently
// correct transliteration of the C.
//
// y[yb..] and x[xb..] may alias the same slice with the same base (the decoder
// always applies the filter in place); negative x indices reach back into the
// decode-memory history. FIXED_POINT bias of -1 and SIG_SAT saturation match the
// C exactly. Each symmetric tap pair (x1+x3, x0+x4) is summed with wrapping
// int32 ADD32 BEFORE its single MULT16_32_Q15 truncation, so the per-sample body
// carries exactly three Q15 truncations (center g10, plus one per pair), not
// five.
func combFilterConstGeneric(y []int32, yb int, x []int32, xb int, T, N int, g10, g11, g12 int16) {
	x4 := x[xb-T-2]
	x3 := x[xb-T-1]
	x2 := x[xb-T]
	x1 := x[xb-T+1]
	for i := 0; i < N; i++ {
		x0 := x[xb+i-T+2]
		v := x[xb+i] +
			fixedmath.MULT16_32_Q15(g10, x2) +
			fixedmath.MULT16_32_Q15(g11, x1+x3) +
			fixedmath.MULT16_32_Q15(g12, x0+x4)
		v-- // FIXED_POINT bias (celt.c:184)
		y[yb+i] = fixedmath.SATURATE(v, sigSat)
		x4 = x3
		x3 = x2
		x2 = x1
		x1 = x0
	}
}
