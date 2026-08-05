package celt

import (
	"github.com/tphakala/simd/i32"

	"github.com/tphakala/go-opus/internal/fixedmath"
)

// haar1 is the CELT Hadamard/Haar step (bands.c:623), the SIMD-backed form of
// haar1Generic (haar1_ref.go). The per-element 1/sqrt(2) scale is the expensive
// part (a 32x32 -> Q31 multiply run twice per pair); it is hoisted out of the
// comb loop into a single vectorized i32.ScaleQ31 pass over the whole touched
// region, and the cheap wrapping add/sub butterfly stays scalar.
//
// Bit-exactness. i32.ScaleQ31 computes int32(int64(a[i]) * int64(k) >> 31), the
// integer MULT32_32_Q31: a truncating (arithmetic, no rounding) Q31 multiply
// that wraps in int32, identical to fixedmath.MULT32_32_Q31 for every input (the
// multiply is commutative, so scaling by sqrt2Inv31 on either operand is the
// same value). ScaleQ31 documents exact element-for-element aliasing, so the
// in-place dst==src scale is defined. Scaling every touched element up front and
// combining afterwards yields the same values as haar1Generic's interleaved
// order, because the scale is elementwise and carries no dependency into the
// add/sub. The differential proof over the (N0, stride) matrix, the int32
// wrapping edges, and fuzz is TestHaar1SIMDMatchesScalar / TestHaar1SIMDExtremes
// / FuzzHaar1 in haar1_simd_test.go.
func haar1(X []int32, N0, stride int) {
	m := N0 >> 1
	// stride<=0 or m<=0 makes the original double loop empty; touch nothing.
	if stride <= 0 || m <= 0 {
		return
	}
	// haar1 only ever reaches the first m*stride*2 elements of X (m combs, each
	// 2*stride wide); the exact-bound reslice is also what ScaleQ31 scales.
	rest := X[:m*stride*2]
	// One contiguous pass scales every even and odd sample by sqrt2Inv31 in
	// place. This is the whole SIMD win: it amortizes the one expensive op (the
	// Q31 multiply) across the buffer and reaches the vector kernel once. The
	// butterfly below stays scalar on purpose. The only vectorizable form is a
	// per-comb i32.Butterfly(evens, odds), but that kernel only takes its SIMD
	// path at len>=8, and the common strides here are 1, 2 and 4, so a per-comb
	// call would drop to a scalar fallback inside a non-inlined call, losing to
	// this inlined add/sub loop (and stride==1 means one call per pair). The
	// scale is the ALU-bound cost; the add/sub is L1-bound and best left inline.
	i32.ScaleQ31(rest, rest, sqrt2Inv31)
	for len(rest) >= 2*stride {
		evens := rest[:stride]
		odds := rest[stride:][:stride]
		for i := range evens {
			t1 := evens[i]
			t2 := odds[i]
			evens[i] = fixedmath.ADD32(t1, t2)
			odds[i] = fixedmath.SUB32(t1, t2)
		}
		rest = rest[2*stride:]
	}
}
