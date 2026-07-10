package celt

// Transliteration of libopus celt/mdct.c (v1.6.1, commit 3da9f7a6) for the
// frozen FIXED_POINT + DISABLE_FLOAT_API + non-QEXT configuration. The CELT
// decoder uses the inverse (backward) MDCT: pre-rotation of the frequency
// coefficients, an N/4-point complex FFT, post-rotation, and the weighted
// overlap-add mirroring for time-domain aliasing cancellation.
//
// The pre/post trig rotation reads the mode's int16 .trig table and the
// rounding-brittle S_MUL/PSHR32_ovflw macros; they are transliterated exactly
// (docs/peer-review.md section 7). The forward MDCT is a phase-4 encoder concern
// and is left as a stub (see below).

import "github.com/tphakala/go-opus/internal/fixedmath"

// clt_mdct_backward computes a backward MDCT (no scaling) and performs the
// weighted overlap-add (implicit 1/2 scale). l is the mode's mdct_lookup, in the
// frequency-domain input (read with the given stride), out the time-domain
// overlap-add buffer, window the overlap window, overlap/shift/stride the usual
// CELT parameters (shift = maxLM-LM for a non-transient block). (celt/mdct.c:268)
func cltMDCTBackward(l *mdctLookup, in, out []int32, window []int16, overlap, shift, stride int) {
	var N, N2, N4 int
	trigOff := 0

	N = l.n
	for i := 0; i < shift; i++ {
		N >>= 1
		trigOff += N
	}
	N2 = N >> 1
	N4 = N >> 2
	trig := l.trig

	// Fixed-point headroom: derive pre/post/fft shifts from the input range.
	var preShift, postShift, fftShift int
	{
		sumval := int32(N2)
		maxval := int32(0)
		for i := 0; i < N2; i++ {
			maxval = fixedmath.MAX32(maxval, fixedmath.ABS32(in[i*stride]))
			sumval = fixedmath.ADD32_ovflw(sumval, fixedmath.ABS32(fixedmath.SHR32(in[i*stride], 11)))
		}
		preShift = fixedmath.IMAX(0, 29-fixedmath.Celt_zlog2(1+maxval))
		// Worst-case where all the energy goes to a single sample.
		postShift = fixedmath.IMAX(0, 19-fixedmath.Celt_ilog2(fixedmath.ABS32(sumval)))
		postShift = fixedmath.IMIN(postShift, preShift)
		fftShift = preShift - postShift
	}

	// The FFT works on a private N4-length complex buffer. In C the pre-rotation
	// is stored directly into out+(overlap/2) in bitrev order and the FFT runs
	// in place there; keeping a separate f2 slice reproduces the exact values
	// (the post-rotation only reads what was already written) without a []int32
	// -> []kissFFTCpx reinterpretation.
	f2 := make([]kissFFTCpx, N4)

	// Pre-rotate.
	{
		xp1 := 0
		xp2 := stride * (N2 - 1)
		bitrev := l.kfft[shift].bitrev
		for i := 0; i < N4; i++ {
			rev := int(bitrev[i])
			x1 := fixedmath.SHL32_ovflw(in[xp1], preShift)
			x2 := fixedmath.SHL32_ovflw(in[xp2], preShift)
			yr := fixedmath.ADD32_ovflw(sMul(x2, trig[trigOff+i]), sMul(x1, trig[trigOff+N4+i]))
			yi := fixedmath.SUB32_ovflw(sMul(x1, trig[trigOff+i]), sMul(x2, trig[trigOff+N4+i]))
			// We swap real and imag because we use an FFT instead of an IFFT;
			// storing the pre-rotation directly in the bitrev order.
			f2[rev].i = yr
			f2[rev].r = yi
			xp1 += 2 * stride
			xp2 -= 2 * stride
		}
	}

	opusFFTImpl(l.kfft[shift], f2, fftShift)

	// Post-rotate and de-shuffle from both ends of the FFT output into out,
	// starting at out+(overlap/2). o0 walks the low half up, o1 the high half
	// down; k1 is the descending complex index (matching yp1 in C).
	{
		o := overlap >> 1
		for i := 0; i < (N4+1)>>1; i++ {
			k0 := i
			k1 := N4 - i - 1

			// We swap real and imag because we're using an FFT instead of an IFFT.
			re := f2[k0].i
			im := f2[k0].r
			t0 := trig[trigOff+i]
			t1 := trig[trigOff+N4+i]
			// We'd scale up by 2 here, but it's done when mixing the windows.
			yr0 := fixedmath.PSHR32_ovflw(fixedmath.ADD32_ovflw(sMul(re, t0), sMul(im, t1)), postShift)
			yi0 := fixedmath.PSHR32_ovflw(fixedmath.SUB32_ovflw(sMul(re, t1), sMul(im, t0)), postShift)

			re = f2[k1].i
			im = f2[k1].r
			// out[o+2*k0] and out[o+N2-1-2*k0] correspond to yp0[0] and yp1[1].
			out[o+2*k0] = yr0
			out[o+N2-1-2*k0] = yi0

			t0 = trig[trigOff+(N4-i-1)]
			t1 = trig[trigOff+(N2-i-1)]
			yr1 := fixedmath.PSHR32_ovflw(fixedmath.ADD32_ovflw(sMul(re, t0), sMul(im, t1)), postShift)
			yi1 := fixedmath.PSHR32_ovflw(fixedmath.SUB32_ovflw(sMul(re, t1), sMul(im, t0)), postShift)
			// out[o+N2-2-2*k0] and out[o+2*k0+1] correspond to yp1[0] and yp0[1].
			out[o+N2-2-2*k0] = yr1
			out[o+2*k0+1] = yi1
		}
	}

	// Mirror on both sides for TDAC.
	{
		xp1 := overlap - 1
		yp1 := 0
		wp1 := 0
		wp2 := overlap - 1
		for i := 0; i < overlap/2; i++ {
			x1 := out[xp1]
			x2 := out[yp1]
			out[yp1] = fixedmath.SUB32_ovflw(sMul(x2, window[wp2]), sMul(x1, window[wp1]))
			out[xp1] = fixedmath.ADD32_ovflw(sMul(x2, window[wp1]), sMul(x1, window[wp2]))
			yp1++
			xp1--
			wp1++
			wp2--
		}
	}
}

// cltMDCTForward is the forward MDCT (encoder side).
//
// TODO(phase 4): port clt_mdct_forward_c from celt/mdct.c. The decoder does not
// use it; it is an encoder-only concern and deliberately unimplemented here.
func cltMDCTForward(l *mdctLookup, in, out []int32, window []int16, overlap, shift, stride int) {
	panic("celt: clt_mdct_forward not implemented (phase 4)")
}

// InverseMDCT drives clt_mdct_backward on mode48000_960 for LM in 0..3 (mono,
// non-transient, stride 1): shift = maxLM-LM, so the FFT length is 60/120/240/
// 480 and the input holds shortMdctSize<<LM = 120<<LM frequency coefficients.
// The returned slice is the written region of the overlap-add output,
// overlap/2 + (120<<LM) samples long. Exported only so the refc cgo differential
// harness (internal/reftest/oracle) can drive the pure-Go inverse MDCT against
// libopus; it is not part of the decoder API.
func InverseMDCT(lm int, in []int32) []int32 {
	m := &mode48000_960
	shift := m.maxLM - lm
	overlap := m.overlap
	N := m.mdct.n >> shift
	N2 := N >> 1
	// Backward MDCT reads in[0..N2-1] (stride 1); copy defensively so the caller
	// input is never mutated (the C entry point treats it as read-only here).
	inCopy := make([]int32, N2)
	copy(inCopy, in)
	out := make([]int32, overlap/2+N2)
	cltMDCTBackward(&m.mdct, inCopy, out, m.window, overlap, shift, 1)
	return out
}
