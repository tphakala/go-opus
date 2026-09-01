package celt

import (
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/simd/i32"
)

// minCombBlock gates the SIMD comb-filter path. A call is routed to the vector
// kernel only when both the block width (T-2, see combFilterConst) and the
// output length N reach it; below that the per-block kernel entry cannot beat
// the register-carrying scalar loop. T is clamped to [combfilterMinperiod,
// combfilterMaxperiod-2] = [15, 1022] by the callers, so T-2 is in [13, 1020].
// Tuned by benchstat on amd64 (AVX2) and arm64 (Cortex-A76 NEON); see
// BenchmarkCombFilterConst in comb_simd_test.go.
const minCombBlock = 32

// combTile caps the per-block scratch so acc stays on the stack (2 KB) and
// L1-resident. Capping the block below T-2 is always bit-exact: a narrower block
// only reads further-back, already-finalized history, never a pending sample.
const combTile = 512

// combPairs is the K=2 mirror-pair layout FIRSymValidQ15 expects: pairs[0] is
// the +-1 tap (g11), pairs[1] the +-2 tap (g12).
const combPairs = 2

// combFilterConst is comb_filter_const_c (celt.c:166), the constant portion of
// the CELT post-filter (decode) and prefilter (encode): a symmetric 5-tap comb at
// pitch lag T with gains g10 (center), g11 (+-1), g12 (+-2), plus the input
// passthrough, a -1 FIXED_POINT bias and a SATURATE to sigSat. It is
// bit-identical to combFilterConstGeneric (comb_ref.go), which is both the
// differential oracle (comb_simd_test.go) and the fallback below the threshold.
//
// # Bit-exactness
//
// Per output i the C computes, all in wrapping int32 (M = MULT16_32_Q15):
//
//	v = x[i] + M(g10, x[i-T]) + M(g11, x[i-T+1]+x[i-T-1]) + M(g12, x[i-T+2]+x[i-T-2])
//	y[i] = SATURATE(v-1, sigSat)
//
// i32.FIRSymValidQ15 with center g10 and pairs {g11, g12} computes exactly the
// three M terms: each mirror pair is summed with a wrapping int32 add BEFORE its
// single Q15 truncation (int64 product, arithmetic shift toward -inf, no
// rounding), which is MULT16_32_Q15(g, ADD32(a, b)) term for term, and the three
// terms are accumulated in wrapping int32. The passthrough, bias and saturate are
// a scalar tail v = acc + x[i] - 1, y[i] = SATURATE(v, sigSat). Regrouping the C
// sum ((x[i] + M10) + M11) + M12 into ((M10 + M11) + M12) + x[i] is exact because
// two's-complement addition is associative and commutative, so the int32 fed to
// SATURATE is the same bit pattern. The simd kernel documents itself
// bit-identical across AVX2, NEON and its pure-Go fallback with no relaxed tier.
//
// # Recurrence and blocking
//
// The decoder runs the filter in place (y and x are the same slice at the same
// base), so y[i] overwrites x[i] and later outputs read earlier outputs: it is a
// recursive comb. Processing output blocks of width W <= T-2 makes every tap read
// within a block land at index <= blockStart-1 (the most-forward read is i-T+2 at
// i = blockStart+W-1), so a block depends only on samples finalized before it
// began, and running blocks in order reproduces C's recurrence exactly. For the
// separate-buffer callers (encoder prefilter) x is never written, so any width is
// trivially correct; blocking unconditionally means no aliasing test is needed.
//
// Within a block the kernel reads the history window x[s-T-2, s-T+2+W) (W+4
// samples, all below s) and writes the private stack scratch acc, which never
// overlaps x, satisfying FIRSymValidQ15's no-overlap contract. The scalar tail
// reads x[s+j] before writing y[yb+base+j] at the same index, which is what makes
// the in-place case safe. Nothing is allocated (TestCombFilterConstZeroAlloc).
func combFilterConst(y []int32, yb int, x []int32, xb int, T, N int, g10, g11, g12 int16) {
	blockW := T - 2
	if blockW < minCombBlock || N < minCombBlock {
		combFilterConstGeneric(y, yb, x, xb, T, N, g10, g11, g12)
		return
	}
	pairs := [combPairs]int16{g11, g12}
	var accArr [combTile]int32
	for base := 0; base < N; {
		w := min(N-base, blockW, combTile)
		s := xb + base
		acc := accArr[:w:w]
		// The window is the W+4 history samples x[s-T-2 .. s-T+2+W); the kernel
		// emits min(len(acc), len(win)-4) = W outputs, one per block sample.
		win := x[s-T-2 : s-T+2+w : s-T+2+w]
		i32.FIRSymValidQ15(acc, win, g10, pairs[:])
		for j := range w {
			v := acc[j] + x[s+j] - 1
			y[yb+base+j] = fixedmath.SATURATE(v, sigSat)
		}
		base += w
	}
}
