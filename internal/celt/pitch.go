// Pitch analysis primitives ported from libopus celt/pitch.c, the decoder subset
// used by the CELT packet-loss concealment: pitch_downsample (48 kHz -> LP,
// decimate by `factor`, whiten with an order-4 LPC), pitch_search (coarse 4x then
// fine 2x cross-correlation search with pseudo-interpolation), celt_pitch_xcorr_c
// (the scalar reference cross-correlation), find_best_pitch, celt_fir5 and
// celt_inner_prod. remove_doubling is NOT ported: celt_decode_lost reaches pitch.c
// only through celt_plc_pitch_search, which calls pitch_downsample + pitch_search
// and never remove_doubling.
//
// These are pure functions (docs/hard-parts.md section 8); their bit-exactness
// against celt/pitch.c is proven by the function-level differential tests in
// internal/reftest/oracle (plc_test.go). Type mapping (celt/arch.h):
// opus_val16 = int16, opus_val32/celt_sig = int32.
//
// SIMD (celt_pitch_xcorr, xcorr_kernel, celt_inner_prod overrides) is out of scope
// for phase 2; the scalar reference below is what the oracle compiles too, since
// the oracle build excludes x86/arm/mips. The unrolled xcorr_kernel and the scalar
// celt_inner_prod produce the same integer sum (two's-complement addition is
// associative), so this scalar celt_pitch_xcorr is bit-identical to the unrolled C.

package celt

import "github.com/tphakala/go-opus/internal/fixedmath"

// PLC pitch-lag search bounds (celt_decoder.c:62-65). PLC_PITCH_LAG_MAX (720)
// corresponds to a 66.67 Hz pitch, PLC_PITCH_LAG_MIN (100) to 480 Hz.
const (
	plcPitchLagMax = 720
	plcPitchLagMin = 100
)

// pitchIabs is abs() for the small int lag arithmetic in pitch_search.
func pitchIabs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// celtInnerProd is celt_inner_prod_c (pitch.h:159): sum_i x[xOff+i]*y[yOff+i].
func celtInnerProd(x []int16, xOff int, y []int16, yOff, N int) int32 {
	xy := int32(0)
	for i := 0; i < N; i++ {
		xy = fixedmath.MAC16_16(xy, x[xOff+i], y[yOff+i])
	}
	return xy
}

// celtPitchXcorr is celt_pitch_xcorr_c (pitch.c:230) for FIXED_POINT: fill
// xcorr[i] = sum_j x[j]*y[i+j] for i in [0,maxPitch) and return the running max
// (>=1). x and y are indexed from offset 0.
func celtPitchXcorr(x, y []int16, xcorr []int32, len_, maxPitch int) int32 {
	maxcorr := int32(1)
	for i := 0; i < maxPitch; i++ {
		sum := celtInnerProd(x, 0, y, i, len_)
		xcorr[i] = sum
		maxcorr = fixedmath.MAX32(maxcorr, sum)
	}
	return maxcorr
}

// findBestPitch is find_best_pitch (pitch.c:45) for FIXED_POINT: pick the two
// lags maximising xcorr[i]^2 / Syy, where Syy is the sliding energy of y over a
// window of length len_. bestPitch receives the top-two lags. y is indexed from
// yOff.
func findBestPitch(xcorr []int32, y []int16, yOff, len_, maxPitch int, bestPitch []int, yshift int, maxcorr int32) {
	Syy := int32(1)
	var bestNum [2]int16
	var bestDen [2]int32
	xshift := fixedmath.Celt_ilog2(maxcorr) - 14

	bestNum[0] = -1
	bestNum[1] = -1
	bestDen[0] = 0
	bestDen[1] = 0
	bestPitch[0] = 0
	bestPitch[1] = 1
	for j := 0; j < len_; j++ {
		Syy = fixedmath.ADD32(Syy, fixedmath.SHR32(fixedmath.MULT16_16(y[yOff+j], y[yOff+j]), yshift))
	}
	for i := 0; i < maxPitch; i++ {
		if xcorr[i] > 0 {
			xcorr16 := fixedmath.EXTRACT16(fixedmath.VSHR32(xcorr[i], xshift))
			num := fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(xcorr16, xcorr16))
			if fixedmath.MULT16_32_Q15(num, bestDen[1]) > fixedmath.MULT16_32_Q15(bestNum[1], Syy) {
				if fixedmath.MULT16_32_Q15(num, bestDen[0]) > fixedmath.MULT16_32_Q15(bestNum[0], Syy) {
					bestNum[1] = bestNum[0]
					bestDen[1] = bestDen[0]
					bestPitch[1] = bestPitch[0]
					bestNum[0] = num
					bestDen[0] = Syy
					bestPitch[0] = i
				} else {
					bestNum[1] = num
					bestDen[1] = Syy
					bestPitch[1] = i
				}
			}
		}
		Syy += fixedmath.SHR32(fixedmath.MULT16_16(y[yOff+i+len_], y[yOff+i+len_]), yshift) -
			fixedmath.SHR32(fixedmath.MULT16_16(y[yOff+i], y[yOff+i]), yshift)
		Syy = fixedmath.MAX32(1, Syy)
	}
}

// celtFir5 is celt_fir5 (pitch.c:105): an in-place order-5 FIR whitening filter
// with a SROUND16/ROUND16 output. mem holds the last five input samples.
func celtFir5(x []int16, num []int16, N int) {
	num0 := num[0]
	num1 := num[1]
	num2 := num[2]
	num3 := num[3]
	num4 := num[4]
	var mem0, mem1, mem2, mem3, mem4 int16
	for i := 0; i < N; i++ {
		sum := fixedmath.SHL32(fixedmath.EXTEND32(x[i]), sigShift)
		sum = fixedmath.MAC16_16(sum, num0, mem0)
		sum = fixedmath.MAC16_16(sum, num1, mem1)
		sum = fixedmath.MAC16_16(sum, num2, mem2)
		sum = fixedmath.MAC16_16(sum, num3, mem3)
		sum = fixedmath.MAC16_16(sum, num4, mem4)
		mem4 = mem3
		mem3 = mem2
		mem2 = mem1
		mem1 = mem0
		mem0 = x[i]
		x[i] = fixedmath.ROUND16(sum, sigShift)
	}
}

// pitchDownsample is pitch_downsample (pitch.c:140) for FIXED_POINT: downmix the
// C channels of x (48 kHz celt_sig) into the half-rate int16 x_lp with a [.25 .5
// .25] kernel, then whiten with an order-4 LPC (lag-windowed autocorrelation ->
// _celt_lpc -> celt_fir5). x[c] are the per-channel signal buffers; len_ is the
// output length; factor is the decimation factor (2 for the PLC).
func pitchDownsample(x [][]int32, xLp []int16, len_, C, factor int) {
	var ac [5]int32
	tmp := int16(q15one)
	var lpc [4]int16
	var lpc2 [5]int16
	c1 := fixedmath.QCONST16(0.8, 15)
	offset := factor / 2

	maxabs := celtMaxabs32(x[0], 0, len_*factor)
	if C == 2 {
		maxabs1 := celtMaxabs32(x[1], 0, len_*factor)
		maxabs = fixedmath.MAX32(maxabs, maxabs1)
	}
	if maxabs < 1 {
		maxabs = 1
	}
	shift := fixedmath.Celt_ilog2(maxabs) - 10
	if shift < 0 {
		shift = 0
	}
	if C == 2 {
		shift++
	}
	for i := 1; i < len_; i++ {
		xLp[i] = fixedmath.EXTRACT16(fixedmath.SHR32(x[0][factor*i-offset], shift+2) +
			fixedmath.SHR32(x[0][factor*i+offset], shift+2) +
			fixedmath.SHR32(x[0][factor*i], shift+1))
	}
	xLp[0] = fixedmath.EXTRACT16(fixedmath.SHR32(x[0][offset], shift+2) + fixedmath.SHR32(x[0][0], shift+1))
	if C == 2 {
		for i := 1; i < len_; i++ {
			xLp[i] = fixedmath.EXTRACT16(fixedmath.EXTEND32(xLp[i]) +
				fixedmath.SHR32(x[1][factor*i-offset], shift+2) +
				fixedmath.SHR32(x[1][factor*i+offset], shift+2) +
				fixedmath.SHR32(x[1][factor*i], shift+1))
		}
		xLp[0] = fixedmath.EXTRACT16(fixedmath.EXTEND32(xLp[0]) +
			fixedmath.SHR32(x[1][offset], shift+2) + fixedmath.SHR32(x[1][0], shift+1))
	}

	celtAutocorr(xLp, ac[:], nil, 0, 4, len_)

	// Noise floor -40 dB.
	ac[0] += fixedmath.SHR32(ac[0], 13)
	// Lag windowing.
	for i := 1; i <= 4; i++ {
		ac[i] -= fixedmath.MULT16_32_Q15(int16(2*i*i), ac[i])
	}

	celtLPC(lpc[:], ac[:], 4)
	for i := 0; i < 4; i++ {
		tmp = fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(fixedmath.QCONST16(0.9, 15), tmp))
		lpc[i] = fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(lpc[i], tmp))
	}
	// Add a zero.
	lpc2[0] = lpc[0] + fixedmath.QCONST16(0.8, sigShift)
	lpc2[1] = lpc[1] + fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(c1, lpc[0]))
	lpc2[2] = lpc[2] + fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(c1, lpc[1]))
	lpc2[3] = lpc[3] + fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(c1, lpc[2]))
	lpc2[4] = fixedmath.EXTRACT16(fixedmath.MULT16_16_Q15(c1, lpc[3]))
	celtFir5(xLp, lpc2[:], len_)
}

// pitchSearch is pitch_search (pitch.c:307) for FIXED_POINT: a coarse cross-
// correlation search at 4x decimation, a finer search at 2x decimation restricted
// to the coarse candidates, then pseudo-interpolation, writing the estimated
// pitch lag to *pitch. xLp is the whitened analysis signal (length >= len_>>1);
// y is the history it is correlated against (length >= lag>>1). len_ and maxPitch
// are the full-rate search parameters.
func pitchSearch(xLp []int16, y []int16, len_, maxPitch int, pitch *int) {
	bestPitch := [2]int{0, 0}
	var offset int

	lag := len_ + maxPitch

	xLp4 := make([]int16, len_>>2)
	yLp4 := make([]int16, lag>>2)
	xcorr := make([]int32, maxPitch>>1)

	// Downsample by 2 again.
	for j := 0; j < len_>>2; j++ {
		xLp4[j] = xLp[2*j]
	}
	for j := 0; j < lag>>2; j++ {
		yLp4[j] = y[2*j]
	}

	xmax := celtMaxabs16(xLp4, len_>>2)
	ymax := celtMaxabs16(yLp4, lag>>2)
	shift := fixedmath.Celt_ilog2(fixedmath.MAX32(1, fixedmath.MAX32(xmax, ymax))) - 14 + fixedmath.Celt_ilog2(int32(len_))/2
	if shift > 0 {
		for j := 0; j < len_>>2; j++ {
			xLp4[j] = fixedmath.SHR16(xLp4[j], shift)
		}
		for j := 0; j < lag>>2; j++ {
			yLp4[j] = fixedmath.SHR16(yLp4[j], shift)
		}
		// Use double the shift for a MAC.
		shift *= 2
	} else {
		shift = 0
	}

	// Coarse search with 4x decimation.
	maxcorr := celtPitchXcorr(xLp4, yLp4, xcorr, len_>>2, maxPitch>>2)
	findBestPitch(xcorr, yLp4, 0, len_>>2, maxPitch>>2, bestPitch[:], 0, maxcorr)

	// Finer search with 2x decimation.
	maxcorr = 1
	for i := 0; i < maxPitch>>1; i++ {
		xcorr[i] = 0
		if pitchIabs(i-2*bestPitch[0]) > 2 && pitchIabs(i-2*bestPitch[1]) > 2 {
			continue
		}
		sum := int32(0)
		for j := 0; j < len_>>1; j++ {
			sum += fixedmath.SHR32(fixedmath.MULT16_16(xLp[j], y[i+j]), shift)
		}
		xcorr[i] = fixedmath.MAX32(-1, sum)
		maxcorr = fixedmath.MAX32(maxcorr, sum)
	}
	findBestPitch(xcorr, y, 0, len_>>1, maxPitch>>1, bestPitch[:], shift+1, maxcorr)

	// Refine by pseudo-interpolation.
	if bestPitch[0] > 0 && bestPitch[0] < (maxPitch>>1)-1 {
		a := xcorr[bestPitch[0]-1]
		b := xcorr[bestPitch[0]]
		c := xcorr[bestPitch[0]+1]
		if (c - a) > fixedmath.MULT16_32_Q15(fixedmath.QCONST16(0.7, 15), b-a) {
			offset = 1
		} else if (a - c) > fixedmath.MULT16_32_Q15(fixedmath.QCONST16(0.7, 15), b-c) {
			offset = -1
		} else {
			offset = 0
		}
	} else {
		offset = 0
	}
	*pitch = 2*bestPitch[0] - offset
}

// --- Differential-test entry points (used by internal/reftest/oracle) --------

// CeltPitchXcorr runs celtPitchXcorr and returns xcorr[0..maxPitch) plus maxcorr.
func CeltPitchXcorr(x, y []int16, len_, maxPitch int) ([]int32, int32) {
	xcorr := make([]int32, maxPitch)
	maxcorr := celtPitchXcorr(x, y, xcorr, len_, maxPitch)
	return xcorr, maxcorr
}

// PitchDownsample runs pitchDownsample over the C channel buffers and returns the
// len_ downsampled samples.
func PitchDownsample(x [][]int32, len_, C, factor int) []int16 {
	xLp := make([]int16, len_)
	pitchDownsample(x, xLp, len_, C, factor)
	return xLp
}

// PitchSearch runs pitchSearch and returns the estimated pitch lag.
func PitchSearch(xLp, y []int16, len_, maxPitch int) int {
	var pitch int
	pitchSearch(xLp, y, len_, maxPitch, &pitch)
	return pitch
}
