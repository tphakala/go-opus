// Transliteration of the DECODE side of celt/vq.c (libopus v1.6.1, commit
// 3da9f7a6) for the frozen FIXED_POINT + DISABLE_FLOAT_API build: PVQ codeword
// dequantization. AlgUnquant decodes a pulse vector (DecodePulses) and normalises
// it to unit norm, ExpRotation applies the spread rotation (shared by encode and
// decode; transliterated exactly per docs/peer-review.md 7), and
// RenormaliseVector rescales a band to unit norm.
//
// In this build celt_norm is opus_val32 (arch.h:145) and NORM_SHIFT is 24
// (arch.h:183), so the norm buffers are int32 and the rotation runs in a scaled
// Q14 domain via normScaledown/normScaleup by NORM_SHIFT-14. Because celt_norm is
// 32-bit here, MULT16_16(coef, x) in the C source degenerates to a plain 32-bit
// product (the norm sample already fits well inside int16 after scaledown), so
// the rotation arithmetic is written out with explicit int32 products rather than
// the int16-typed fixedmath.MULT16_16 helper.
//
// The encoder-only op_pvq_search / alg_quant and the ENABLE_QEXT refine paths are
// phase-4 concerns; see the TODO below. The C alg_quant is invoked from the
// differential-test shim to PRODUCE codewords for this decode side.

package celt

import (
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// TODO(phase 4): port the encoder side of vq.c (op_pvq_search_c / alg_quant,
// plus the ENABLE_QEXT op_pvq_search_N2 / _extra refine paths) when the CELT
// encoder lands. This file is the pure decode path only.

// normShift is celt/arch.h NORM_SHIFT (24): the fixed-point scale of a celt_norm
// (opus_val32) unit-norm band sample. normScaledown/normScaleup shift between the
// storage domain and the Q14 rotation domain by normShift-14. (arch.h:183)
const normShift = 24

// spreadNone is celt/bands.h SPREAD_NONE; exp_rotation is a no-op for it.
const spreadNone = 0

// spreadFactor is celt/vq.c exp_rotation's SPREAD_FACTOR[3] for spread values
// LIGHT/NORMAL/AGGRESSIVE (1/2/3, indexed spread-1). (vq.c:106)
var spreadFactor = [3]int{15, 10, 5}

// normScaleup shifts each band sample left by shift (a no-op for shift<=0). The
// FIXED_POINT norm_scaleup (vq.c:44).
func normScaleup(X []int32, n, shift int) {
	if shift <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		X[i] = fixedmath.SHL32(X[i], shift)
	}
}

// normScaledown shifts each band sample right (rounding) by shift (a no-op for
// shift<=0). The FIXED_POINT norm_scaledown (vq.c:51).
func normScaledown(X []int32, n, shift int) {
	if shift <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		X[i] = fixedmath.PSHR32(X[i], shift)
	}
}

// celtInnerProdNorm is the FIXED_POINT celt_inner_prod_norm: a plain 32-bit
// accumulation of x[i]*y[i] over the scaled norm samples. (vq.c:58)
func celtInnerProdNorm(x, y []int32, length int) int32 {
	var sum int32
	for i := 0; i < length; i++ {
		sum += x[i] * y[i]
	}
	return sum
}

// expRotation1 applies one Givens-style rotation pass with the given stride and
// (c,s) coefficients. Rounding-brittle: the PSHR32 rounding and the scaledown/
// scaleup around the Q14 domain are transliterated exactly (vq.c:75). Because
// celt_norm is int32 here, MULT16_16(c,x) is a 32-bit product and MAC16_16 a
// 32-bit multiply-add; the results still fit int32 for scaled norm inputs.
func expRotation1(X []int32, length, stride int, c, s int16) {
	ms := int16(fixedmath.NEG16(s))
	normScaledown(X, length, normShift-14)
	xp := 0
	for i := 0; i < length-stride; i++ {
		x1 := X[xp]
		x2 := X[xp+stride]
		X[xp+stride] = int32(fixedmath.EXTRACT16(fixedmath.PSHR32(int32(c)*x2+int32(s)*x1, 15)))
		X[xp] = int32(fixedmath.EXTRACT16(fixedmath.PSHR32(int32(c)*x1+int32(ms)*x2, 15)))
		xp++
	}
	xp = length - 2*stride - 1
	for i := length - 2*stride - 1; i >= 0; i-- {
		x1 := X[xp]
		x2 := X[xp+stride]
		X[xp+stride] = int32(fixedmath.EXTRACT16(fixedmath.PSHR32(int32(c)*x2+int32(s)*x1, 15)))
		X[xp] = int32(fixedmath.EXTRACT16(fixedmath.PSHR32(int32(c)*x1+int32(ms)*x2, 15)))
		xp--
	}
	normScaleup(X, length, normShift-14)
}

// expRotation applies (dir<0) or undoes (dir>0) the spread rotation over the band
// X of the given length, split into stride interleaved blocks with K pulses and
// the given spread. No-op when 2*K>=len or spread==SPREAD_NONE. (vq.c:104)
func expRotation(X []int32, length, dir, stride, k, spread int) {
	if 2*k >= length || spread == spreadNone {
		return
	}
	factor := spreadFactor[spread-1]

	// gain = celt_div(MULT16_16(Q15_ONE,len), len+factor*K); celt_div(a,b) is
	// MULT32_32_Q31(a, celt_rcp(b)) in FIXED_POINT (mathops.h:539).
	num := int32(32767) * int32(length) // (opus_val32)MULT16_16(Q15_ONE,len)
	den := int32(length + factor*k)     // (opus_val32)(len+factor*K)
	gain := fixedmath.EXTRACT16(fixedmath.MULT32_32_Q31(num, fixedmath.Celt_rcp(den)))
	// theta = HALF16(MULT16_16_Q15(gain,gain)); the >>1 acts on the opus_val32
	// product before the opus_val16 truncation.
	theta := fixedmath.EXTRACT16(fixedmath.SHR32(fixedmath.MULT16_16_Q15(gain, gain), 1))

	c := fixedmath.Celt_cos_norm(fixedmath.EXTEND32(theta))
	// s = celt_cos_norm(Q15ONE - theta) = sin(theta).
	s := fixedmath.Celt_cos_norm(fixedmath.SUB16(32767, theta))

	stride2 := 0
	if length >= 8*stride {
		stride2 = 1
		// Equivalent to sqrt(len/stride) with rounding: increment while
		// (stride2+0.5)^2 < len/stride.
		for (stride2*stride2+stride2)*stride+(stride>>2) < length {
			stride2++
		}
	}
	length = int(fixedmath.Celt_udiv(uint32(length), uint32(stride)))
	for i := 0; i < stride; i++ {
		block := X[i*length:]
		if dir < 0 {
			if stride2 != 0 {
				expRotation1(block, length, stride2, s, c)
			}
			expRotation1(block, length, 1, c, s)
		} else {
			expRotation1(block, length, 1, c, int16(fixedmath.NEG16(s)))
			if stride2 != 0 {
				expRotation1(block, length, stride2, s, int16(fixedmath.NEG16(c)))
			}
		}
	}
}

// ExpRotation exposes expRotation for the differential test (both directions).
func ExpRotation(X []int32, length, dir, stride, k, spread int) {
	expRotation(X, length, dir, stride, k, spread)
}

// normaliseResidual normalises the decoded integer pulse vector iy to unit norm,
// writing the result into X. Ryy is the sum of squares from DecodePulses, gain is
// the band gain (Q31). The ENABLE_QEXT shift path is omitted (shift is always 0
// here). (vq.c:150)
func normaliseResidual(iy []int32, X []int32, n int, ryy, gain int32) {
	k := fixedmath.Celt_ilog2(ryy) >> 1
	t := fixedmath.VSHR32(ryy, 2*(k-7)-15)
	g := fixedmath.MULT32_32_Q31(fixedmath.Celt_rsqrt_norm32(t), gain)
	for i := 0; i < n; i++ {
		X[i] = fixedmath.VSHR32(fixedmath.MULT16_32_Q15(int16(iy[i]), g), k+15-normShift)
	}
}

// extractCollapseMask returns the anti-collapse mask: bit i is set when the i'th
// of B interleaved sub-blocks received at least one pulse. (vq.c:183)
func extractCollapseMask(iy []int32, n, b int) uint32 {
	if b <= 1 {
		return 1
	}
	n0 := int(fixedmath.Celt_udiv(uint32(n), uint32(b)))
	var collapseMask uint32
	for i := 0; i < b; i++ {
		var tmp uint32
		for j := 0; j < n0; j++ {
			tmp |= uint32(iy[i*n0+j])
		}
		collapseMask |= uint32(b2i(tmp != 0)) << i
	}
	return collapseMask
}

// AlgUnquant decodes a PVQ codeword and combines it into the final normalised
// band signal X (length N), returning the anti-collapse mask. K pulses, spread
// rotation, B interleaved blocks, band gain (Q31). (vq.c:619). Requires K>0, N>1.
func AlgUnquant(X []int32, n, k, spread, b int, dec *rangecoding.Decoder, gain int32) uint32 {
	iy := make([]int32, n)
	ryy := DecodePulses(iy, n, k, dec)
	normaliseResidual(iy, X, n, ryy, gain)
	expRotation(X, n, -1, b, k, spread)
	return extractCollapseMask(iy, n, b)
}

// RenormaliseVector rescales the band X (length N) to unit norm times gain (Q31).
// The decode side uses this for bands with no pulses. (vq.c:693)
func RenormaliseVector(X []int32, n int, gain int32) {
	normScaledown(X, n, normShift-14)
	e := int32(1) /* EPSILON */ + celtInnerProdNorm(X, X, n)
	k := fixedmath.Celt_ilog2(e) >> 1
	t := fixedmath.VSHR32(e, 2*(k-7))
	g := fixedmath.EXTRACT16(fixedmath.MULT32_32_Q31(fixedmath.EXTEND32(fixedmath.Celt_rsqrt_norm(t)), gain))
	for i := 0; i < n; i++ {
		X[i] = int32(fixedmath.EXTRACT16(fixedmath.PSHR32(int32(g)*X[i], k+15-14)))
	}
	normScaleup(X, n, normShift-14)
}
