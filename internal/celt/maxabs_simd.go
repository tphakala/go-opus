// The three CELT max-abs reductions (celt_maxabs16, celt_maxabs_res and
// celt_maxabs32) are backed by github.com/tphakala/simd on every architecture:
// i16.MaxAbs for the two int16 forms and i32.MaxAbs for the int32 form. The
// library carries the per-arch assembly (NEON on arm64, SSE2/AVX2 on amd64) and a
// pure-Go fallback elsewhere, so there is no in-tree assembly and no cgo.
//
// Both library functions are a single signed min/max scan folded as
// max(maxVal, -minVal); signed min/max has no accumulation order, so the library's
// SIMD lane grouping is bit-identical to its own scalar fold on every backend. The
// two int16 forms need nothing more: i16.MaxAbs takes the negate after widening to
// int32, so |MinInt16| = 32768 is exact and its result equals celtMaxabs16Generic /
// celtMaxabsResGeneric for every input; celtMaxabs16 / celtMaxabsRes are a direct
// int32(i16.MaxAbs(...)).
//
// The int32 form needs one reconciling step, because i32.MaxAbs folds over the TRUE
// signed min/max whereas celt_maxabs32 (and celtMaxabs32Generic) seed maxval at 0
// and so floor the result at 0. They agree on every input except an all-negative
// window whose most-negative element is MinInt32: there -minVal wraps back to
// MinInt32, i32.MaxAbs returns the negative array max, and celt_maxabs32 returns 0.
// celtMaxabs32 clamps with max(0, i32.MaxAbs(...)); that is the only input for which
// i32.MaxAbs is negative, so the clamp is 0 exactly in that regime and a no-op
// everywhere else, making it bit-identical to celtMaxabs32Generic across the whole
// int32 range. See the celtMaxabs32 doc below for the full argument.
//
// The differential proof is TestCeltMaxabs16SIMDMatchesScalar /
// TestCeltMaxabs32SIMDMatchesScalar and the fuzz targets in maxabs_simd_test.go,
// which compare the library-backed reductions against the scalar reference on every
// length 0..600 plus adversarial and fuzz inputs.
//
// The explicit window guards below make a mis-sized caller panic here, before the
// library runs, preserving the original bounds-check-before-scan contract
// (i16.MaxAbs / i32.MaxAbs would instead clamp to the slice they are handed). An
// empty window returns 0 from both the library and the reference.

package celt

import (
	"github.com/tphakala/simd/i16"
	"github.com/tphakala/simd/i32"

	"github.com/tphakala/go-opus/internal/fixedmath"
)

// celtMaxabs16 is celt_maxabs16 (mathops.h:86): max |x[i]| as opus_val32.
//
// i16.MaxAbs returns the peak magnitude in [0, 32768] (|-32768| = 32768 widens out
// of int16, mirroring celt_maxabs16), so the int cast back to int32 is exact and
// non-negative.
func celtMaxabs16(x []int16, len_ int) int32 {
	if len_ < 0 || len_ > len(x) {
		panic("celt: celtMaxabs16: len_ out of range")
	}
	observeMaxabs16Dispatch() // dispatch-observation hook, compiled out unless -tags dispatchcount
	return int32(i16.MaxAbs(x[:len_]))
}

// celtMaxabsRes is celt_maxabs_res (mathops.h:118) with an explicit base offset:
// exactly celtMaxabs16 over x[xOff:xOff+len_] in the frozen (non-ENABLE_RES24)
// config. See celtMaxabsResGeneric for the mapping.
func celtMaxabsRes(x []int16, xOff, len_ int) int32 {
	if xOff < 0 || len_ < 0 || xOff > len(x) || len_ > len(x)-xOff {
		panic("celt: celtMaxabsRes: window exceeds operand length")
	}
	observeMaxabs16Dispatch() // dispatch-observation hook, compiled out unless -tags dispatchcount
	return int32(i16.MaxAbs(x[xOff : xOff+len_]))
}

// celtMaxabs32 is celt_maxabs32 (mathops.h:122): max |x[i]| for opus_val32 input.
//
// i32.MaxAbs folds the scan as max(maxVal, -minVal) over the TRUE signed min and
// max of the window, whereas celt_maxabs32 seeds maxval and minval at 0, i.e. it
// floors maxval at 0. The two differ on exactly one regime: an all-negative window
// whose most-negative element is MinInt32, where -minVal wraps back to MinInt32 and
// i32.MaxAbs returns that negative array max while celt_maxabs32 returns 0. That is
// the only input for which i32.MaxAbs is negative at all (celt_maxabs32 is always
// >= 0, being a max with a non-negative floored maxval), so clamping the result up
// to 0 reproduces the C floor and makes this bit-identical to celt_maxabs32 across
// the whole int32 range, not just the CELT-reachable subset. The clamp is one
// compare outside the vector scan. This regime is unreachable from real CELT data
// (see i32.MaxAbs's own doc), but the clamp removes the caveat entirely and is what
// TestCeltMaxabs32SIMDMatchesScalar pins over every full-range adversarial input.
func celtMaxabs32(x []int32, xOff, len_ int) int32 {
	if xOff < 0 || len_ < 0 || xOff > len(x) || len_ > len(x)-xOff {
		panic("celt: celtMaxabs32: window exceeds operand length")
	}
	observeMaxabs32Dispatch() // dispatch-observation hook, compiled out unless -tags dispatchcount
	return fixedmath.MAX32(0, i32.MaxAbs(x[xOff:xOff+len_]))
}
