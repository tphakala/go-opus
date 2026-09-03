// Scalar reference implementations of the three CELT max-abs reductions
// (celt_maxabs16, celt_maxabs_res and celt_maxabs32). These are the
// transliteration of the C generic path and they are the source of truth: the
// library-backed celtMaxabs16 / celtMaxabsRes / celtMaxabs32 in maxabs_simd.go
// (github.com/tphakala/simd i16.MaxAbs and i32.MaxAbs) are verified against them,
// sample for sample, by the differential and fuzz suites in maxabs_simd_test.go.
//
// This file carries no build tag on purpose. It compiles into every build, so the
// differential tests always have the reference to compare the library-backed
// reductions against.
//
// Why reordering the scan is legal here, and why that is the whole basis for
// vectorizing these reductions at all: each is a signed min and max over the
// window, folded at the end as MAX32(maxval, -minval). Signed min and max are
// associative and commutative, so any lane grouping and any horizontal-reduction
// order yields the bit-identical (minval, maxval) pair, and therefore the same
// result, for every input. The 0-seed is what makes the corner safe: each scan
// starts maxval and minval at 0, so maxval never drops below 0 and the fold
// MAX32(maxval, -minval) is always >= 0. For an all-negative window whose
// most-negative element is MinInt32, -minval wraps back to MinInt32, but the 0-floor
// on maxval means the reference returns 0 there, not the wrapped negative. The
// library i32.MaxAbs omits that floor and would return the negative array max, which
// is why celtMaxabs32 in maxabs_simd.go clamps with max(0, ...) to match this
// reference. The int16 forms widen to int32 before negating, so |MinInt16| = 32768
// is represented exactly and there is no wrap in their range.

package celt

import "github.com/tphakala/go-opus/internal/fixedmath"

// celtMaxabs16Generic is celt_maxabs16 (mathops.h:86): max |x[i]| as opus_val32.
func celtMaxabs16Generic(x []int16, len_ int) int32 {
	var maxval, minval int16
	// Bound the window against len(x), the same bound the original per-sample x[i]
	// enforced, then range over it so the reduction is check-free. The guard keeps
	// a mis-sized caller panicking deterministically rather than letting the
	// two-index reslice widen silently into spare capacity (x[:len_] bounds against
	// cap, not len). One check per call replaces one per element; the arithmetic is
	// untouched, so the result stays bit-identical.
	if len_ < 0 || len_ > len(x) {
		panic("celt: celtMaxabs16: len_ out of range")
	}
	for _, v := range x[:len_] {
		if v > maxval {
			maxval = v
		}
		if v < minval {
			minval = v
		}
	}
	return fixedmath.MAX32(fixedmath.EXTEND32(maxval), -fixedmath.EXTEND32(minval))
}

// celtMaxabsResGeneric is celt_maxabs_res (mathops.h:118) with an explicit base
// offset, so a caller can express C's celt_maxabs_res(pcm + off, len).
// ENABLE_RES24 is not defined in the frozen config, so celt_maxabs_res is a
// #define for celt_maxabs16 and opus_res is opus_int16: this is exactly
// celtMaxabs16 over x[xOff:xOff+len_]. celt_encode_with_ec:1972-1973 uses both the
// offset form (st->overlap_max) and the plain form (sample_max); note the lengths
// there are scaled by C == stream_channels, not CC == channels.
func celtMaxabsResGeneric(x []int16, xOff, len_ int) int32 {
	var maxval, minval int16
	// Bound the [xOff, xOff+len_) window against len(x), the same bound the original
	// per-sample x[xOff+i] enforced, then range over it check-free. The guard keeps
	// a mis-sized caller panicking deterministically rather than letting the
	// two-index reslice widen silently into spare capacity (it bounds against cap,
	// not len). One check per call replaces one per element; the arithmetic is
	// untouched, so the result stays bit-identical.
	if xOff < 0 || len_ < 0 || xOff > len(x) || len_ > len(x)-xOff {
		panic("celt: celtMaxabsRes: window exceeds operand length")
	}
	for _, v := range x[xOff : xOff+len_] {
		if v > maxval {
			maxval = v
		}
		if v < minval {
			minval = v
		}
	}
	return fixedmath.MAX32(fixedmath.EXTEND32(maxval), -fixedmath.EXTEND32(minval))
}

// celtMaxabs32Generic is celt_maxabs32 (mathops.h:122): max |x[i]| for opus_val32
// input.
func celtMaxabs32Generic(x []int32, xOff, len_ int) int32 {
	var maxval, minval int32
	// Bound the [xOff, xOff+len_) window against len(x), the same bound the original
	// per-sample x[xOff+i] enforced, then range over it so the reduction is
	// check-free (xOff+i is otherwise not provably in range). The guard keeps a
	// mis-sized caller panicking deterministically rather than letting the two-index
	// reslice widen silently into spare capacity (it bounds against cap, not len).
	// len_==0 yields an empty window and the original zero result. The arithmetic is
	// unchanged, so the result stays bit-identical.
	if xOff < 0 || len_ < 0 || xOff > len(x) || len_ > len(x)-xOff {
		panic("celt: celtMaxabs32: window exceeds operand length")
	}
	for _, v := range x[xOff : xOff+len_] {
		if v > maxval {
			maxval = v
		}
		if v < minval {
			minval = v
		}
	}
	return fixedmath.MAX32(maxval, -minval)
}
