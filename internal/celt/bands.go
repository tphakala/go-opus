// Verbatim transliteration of the DECODE side of celt/bands.c (libopus v1.6.1,
// commit 3da9f7a6) for the frozen FIXED_POINT + DISABLE_FLOAT_API,
// non-CUSTOM_MODES, non-ENABLE_QEXT build: the CELT residual (PVQ) band
// quantizer. This is a named verbatim zone (docs/hard-parts.md section 2, the
// quant_all_bands machinery): every temporary keeps its C name and declaration
// order and every fixed-point expression keeps C's exact form; correctness is
// proved by the differential sweep in internal/reftest/oracle (bands_test.go),
// not by review.
//
// On DECODE, resynth = !encode is always 1, so the synthesis-side band code
// (folding, the LCG noise fill, stereo_merge, renormalise) always runs; this
// file ports it. The range-decode order (ec_dec_bit_logp / ec_dec_uint /
// ec_decode+ec_dec_update / decode_pulses) matches docs/celt-bitstream.md.
//
// C's single ec_ctx serves both directions; here the band context carries a
// *rangecoding.Decoder and an `encode` flag kept for shape. The ENCODER paths
// (op_pvq_search / alg_quant, stereo_itheta, stereo_split, the MIN_STEREO_ENERGY
// copy-down, and the complexity>=8 theta-RDO trial loop with
// compute_channel_weights) require vq.c encode helpers that are not ported yet;
// every such branch is guarded with a // TODO(phase 4) panic and never runs on
// the decode path (encode==0 forces theta_rdo==0). intensity_stereo and
// stereo_merge are ported (stereo_merge runs on decode resynth); stereo_split is
// encoder-only and is not ported.

package celt

import (
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// TODO(phase 4): port the ENCODE side of bands.c (alg_quant via quant_partition,
// stereo_itheta / stereo_split / intensity_stereo mixing in compute_theta, the
// MIN_STEREO_ENERGY copy-down and the theta-RDO trial loop) when the CELT
// encoder lands. This file is the pure decode/resynth path only.

const bandsEncodeTODO = "celt: bands.c encode path is phase 4"

// celtLcgRand is celt_lcg_rand (bands.c:61): the linear congruential generator
// that drives the folding noise and anti-collapse fill. uint32 arithmetic wraps,
// matching opus_uint32.
func celtLcgRand(seed uint32) uint32 {
	return 1664525*seed + 1013904223
}

// bitexactCos is bitexact_cos (bands.c:68): a cos() approximation that is
// bit-exact on any platform (its result feeds the bit allocation, so exactness
// matters).
func bitexactCos(x int16) int16 {
	tmp := (4096 + int32(x)*int32(x)) >> 13
	// celt_sig_assert(tmp<=32767)
	x2 := int16(tmp)
	x2 = int16((32767 - int32(x2)) + fracMul16(int32(x2), -7651+fracMul16(int32(x2), 8277+fracMul16(-626, int32(x2)))))
	// celt_sig_assert(x2<=32766)
	return 1 + x2
}

// bitexactLog2tan is bitexact_log2tan (bands.c:80).
func bitexactLog2tan(isin, icos int) int {
	lc := fixedmath.EC_ILOG(uint32(icos))
	ls := fixedmath.EC_ILOG(uint32(isin))
	icos <<= 15 - lc
	isin <<= 15 - ls
	return (ls-lc)*(1<<11) +
		int(fracMul16(int32(isin), fracMul16(int32(isin), -2597)+7932)) -
		int(fracMul16(int32(icos), fracMul16(int32(icos), -2597)+7932))
}

// computeBandEnergies ports compute_band_energies (bands.c:95, FIXED_POINT): the
// encoder-direction pass that computes the amplitude (sqrt energy) of each band
// of the MDCT spectrum X into bandE (celt_ener, opus_val32). The FIXED_POINT
// branch scales each band by a per-band shift derived from its peak magnitude so
// the sum-of-squares stays in range, then celt_sqrt32s it back down. arch is
// unused in the pure-Go build (there is no SIMD override); it is kept for a
// faithful signature.
func computeBandEnergies(m *celtMode, X, bandE []int32, end, C, LM, arch int) {
	_ = arch
	eBands := m.eBands
	N := m.shortMdctSize << LM
	c := 0
	for {
		for i := 0; i < end; i++ {
			var maxval int32
			var sum int32
			maxval = celtMaxabs32(X, c*N+int(eBands[i])<<LM, int(eBands[i+1]-eBands[i])<<LM)
			if maxval > 0 {
				shift := fixedmath.IMAX(0, 30-fixedmath.Celt_ilog2(maxval+(maxval>>14)+1)-((((int(m.logN[i])+7)>>bitRes)+LM+1)>>1))
				j := int(eBands[i]) << LM
				for {
					x := fixedmath.SHL32(X[j+c*N], shift)
					sum = fixedmath.ADD32(sum, fixedmath.MULT32_32_Q31(x, x))
					j++
					if j >= int(eBands[i+1])<<LM {
						break
					}
				}
				bandE[i+c*m.nbEBands] = fixedmath.MAX32(maxval, fixedmath.PSHR32(fixedmath.Celt_sqrt32(fixedmath.SHR32(sum, 1)), shift))
			} else {
				bandE[i+c*m.nbEBands] = 1 // EPSILON
			}
		}
		c++
		if c >= C {
			break
		}
	}
}

// normaliseBands ports normalise_bands (bands.c:125, FIXED_POINT): the
// encoder-direction pass that normalises each band of freq to unit energy using
// the previously computed bandE, writing the celt_norm result into X. Each band
// gets a per-band shift from celt_zlog2(E) so the reciprocal (celt_rcp_norm32)
// keeps full precision. M is the short-block multiplier (1<<LM).
func normaliseBands(m *celtMode, freq, X, bandE []int32, end, C, M int) {
	eBands := m.eBands
	N := M * m.shortMdctSize
	c := 0
	for {
		i := 0
		for {
			E := bandE[i+c*m.nbEBands]
			// For very low energies, we need this to make sure not to prevent
			// energy rounding from blowing up the normalized signal.
			if E < 10 {
				E += 1 // EPSILON
			}
			shift := 30 - fixedmath.Celt_zlog2(E)
			E = fixedmath.SHL32(E, shift)
			g := fixedmath.Celt_rcp_norm32(E)
			j := M * int(eBands[i])
			for {
				X[j+c*N] = fixedmath.PSHR32(fixedmath.MULT32_32_Q31(g, fixedmath.SHL32(freq[j+c*N], shift)), 30-normShift)
				j++
				if j >= M*int(eBands[i+1]) {
					break
				}
			}
			i++
			if i >= end {
				break
			}
		}
		c++
		if c >= C {
			break
		}
	}
}

// denormaliseBands ports denormalise_bands (bands.c:188): apply the per-band log
// energies to the normalized coefficients to produce the synthesis spectrum.
func denormaliseBands(m *celtMode, X, freq, bandLogE []int32, start, end, M, downsample, silence int) {
	eBands := m.eBands
	N := M * m.shortMdctSize
	bound := M * int(eBands[end])
	if downsample != 1 {
		bound = fixedmath.IMIN(bound, N/downsample)
	}
	if silence != 0 {
		bound = 0
		start = 0
		end = 0
	}
	fi := 0                      // f = freq
	xi := M * int(eBands[start]) // x = X + M*eBands[start]
	if start != 0 {
		for i := 0; i < M*int(eBands[start]); i++ {
			freq[fi] = 0
			fi++
		}
	} else {
		fi += M * int(eBands[start])
	}
	for i := start; i < end; i++ {
		j := M * int(eBands[i])
		bandEnd := M * int(eBands[i+1])
		lg := fixedmath.ADD32(bandLogE[i], fixedmath.SHL32(int32(eMeans[i]), dbShift-4))
		// Handle the integer part of the log energy.
		var g int32
		shift := 17 - int(lg>>dbShift)
		if shift >= 31 {
			shift = 0
			g = 0
		} else {
			// Handle the fractional part.
			g = fixedmath.SHL32(celtExp2DbFrac(lg&((1<<dbShift)-1)), 2)
		}
		// Handle extreme gains with negative shift.
		if shift < 0 {
			g = 2147483647
			shift = 0
		}
		for {
			freq[fi] = fixedmath.PSHR32(fixedmath.MULT32_32_Q31(fixedmath.SHL32(X[xi], 30-normShift), g), shift)
			fi++
			xi++
			j++
			if j >= bandEnd {
				break
			}
		}
	}
	// celt_assert(start <= end); OPUS_CLEAR(&freq[bound], N-bound)
	for i := bound; i < N; i++ {
		freq[i] = 0
	}
}

// antiCollapse ports anti_collapse (bands.c:259): reinject energy into bands that
// collapsed for transients with multiple short MDCTs, using the LCG. seed is
// passed by value (its evolution here is local and not propagated back, matching
// the void C signature).
func antiCollapse(m *celtMode, X_ []int32, collapseMasks []byte, LM, C, size, start, end int,
	logE, prev1logE, prev2logE []int32, pulses []int, seed uint32, encode int) {
	for i := start; i < end; i++ {
		N0 := int(m.eBands[i+1]) - int(m.eBands[i])
		// depth in 1/8 bits
		// celt_sig_assert(pulses[i]>=0)
		depth := int(fixedmath.Celt_udiv(uint32(1+pulses[i]), uint32(int(m.eBands[i+1])-int(m.eBands[i])))) >> LM

		sh := fixedmath.SHL16(int16(depth), 10-bitRes)
		thresh32 := fixedmath.SHR32(fixedmath.Celt_exp2(-sh), 1)
		thresh := int16(fixedmath.MULT16_32_Q15(fixedmath.QCONST16(0.5, 15), fixedmath.MIN32(32767, thresh32)))
		var shift int
		var sqrt1 int16
		{
			t := int32(N0 << LM)
			shift = fixedmath.Celt_ilog2(t) >> 1
			t = fixedmath.SHL32(t, (7-shift)<<1)
			sqrt1 = fixedmath.Celt_rsqrt_norm(t)
		}

		c := 0
		for {
			renormalize := 0
			prev1 := prev1logE[c*m.nbEBands+i]
			prev2 := prev2logE[c*m.nbEBands+i]
			if encode == 0 && C == 1 {
				prev1 = fixedmath.MAX32(prev1, prev1logE[m.nbEBands+i])
				prev2 = fixedmath.MAX32(prev2, prev2logE[m.nbEBands+i])
			}
			Ediff := logE[c*m.nbEBands+i] - fixedmath.MIN32(prev1, prev2)
			Ediff = fixedmath.MAX32(0, Ediff)

			var r int32
			if Ediff < gconst16 {
				r32 := fixedmath.SHR32(celtExp2Db(-Ediff), 1)
				r = 2 * fixedmath.MIN16(16383, r32)
			} else {
				r = 0
			}
			if LM == 3 {
				r = fixedmath.MULT16_16_Q14(23170, int16(fixedmath.MIN32(23169, r)))
			}
			r = int32(fixedmath.SHR16(int16(fixedmath.MIN16(int32(thresh), r)), 1))
			r = fixedmath.VSHR32(fixedmath.MULT16_16_Q15(sqrt1, int16(r)), shift+14-normShift)

			X := X_[c*size+(int(m.eBands[i])<<LM):]
			for k := 0; k < 1<<LM; k++ {
				// Detect collapse.
				if collapseMasks[i*C+c]&(1<<k) == 0 {
					// Fill with noise.
					for j := 0; j < N0; j++ {
						seed = celtLcgRand(seed)
						if seed&0x8000 != 0 {
							X[(j<<LM)+k] = r
						} else {
							X[(j<<LM)+k] = -r
						}
					}
					renormalize = 1
				}
			}
			// We just added some energy, so we need to renormalise.
			if renormalize != 0 {
				RenormaliseVector(X, N0<<LM, q31one)
			}

			c++
			if c >= C {
				break
			}
		}
	}
}

// intensityStereo ports intensity_stereo (bands.c:379): the stereo intensity
// mix (encoder direction within compute_theta). It is exercised directly by the
// differential test.
func intensityStereo(m *celtMode, X, Y, bandE []int32, bandID, N int) {
	i := bandID
	shift := fixedmath.Celt_zlog2(fixedmath.MAX32(bandE[i], bandE[i+m.nbEBands])) - 13
	left := int16(fixedmath.VSHR32(bandE[i], shift))
	right := int16(fixedmath.VSHR32(bandE[i+m.nbEBands], shift))
	norm := int16(1 /* EPSILON */ + fixedmath.Celt_sqrt(1+fixedmath.MULT16_16(left, left)+fixedmath.MULT16_16(right, right)))
	left = int16(fixedmath.MIN32(int32(left), int32(norm)-1))
	right = int16(fixedmath.MIN32(int32(right), int32(norm)-1))
	a1 := div32_16(fixedmath.SHL32(fixedmath.EXTEND32(left), 15), norm)
	a2 := div32_16(fixedmath.SHL32(fixedmath.EXTEND32(right), 15), norm)
	for j := 0; j < N; j++ {
		X[j] = fixedmath.ADD32(fixedmath.MULT16_32_Q15(a1, X[j]), fixedmath.MULT16_32_Q15(a2, Y[j]))
		// Side is not encoded, no need to calculate.
	}
}

// stereoMerge ports stereo_merge (bands.c:418): the decoder-side stereo
// un-mixing that turns the decoded mid/side pair back into left/right.
func stereoMerge(X, Y []int32, mid int32, N int) {
	// Compute the norm of X+Y and X-Y as |X|^2 + |Y|^2 +/- sum(xy).
	xp := celtInnerProdNormShift(Y, X, N)
	side := celtInnerProdNormShift(Y, Y, N)
	// Compensating for the mid normalization.
	xp = fixedmath.MULT32_32_Q31(mid, xp)
	// mid and side are in Q15, not Q14 like X and Y.
	El := fixedmath.SHR32(fixedmath.MULT32_32_Q31(mid, mid), 3) + side - 2*xp
	Er := fixedmath.SHR32(fixedmath.MULT32_32_Q31(mid, mid), 3) + side + 2*xp
	if Er < fixedmath.QCONST32(6e-4, 28) || El < fixedmath.QCONST32(6e-4, 28) {
		copy(Y[:N], X[:N])
		return
	}

	kl := fixedmath.Celt_ilog2(El) >> 1
	kr := fixedmath.Celt_ilog2(Er) >> 1
	t := fixedmath.VSHR32(El, (kl<<1)-29)
	lgain := fixedmath.Celt_rsqrt_norm32(t)
	t = fixedmath.VSHR32(Er, (kr<<1)-29)
	rgain := fixedmath.Celt_rsqrt_norm32(t)

	if kl < 7 {
		kl = 7
	}
	if kr < 7 {
		kr = 7
	}

	for j := 0; j < N; j++ {
		// Apply mid scaling (side is already scaled).
		l := fixedmath.MULT32_32_Q31(mid, X[j])
		r := Y[j]
		X[j] = fixedmath.VSHR32(fixedmath.MULT32_32_Q31(lgain, fixedmath.SUB32(l, r)), kl-15)
		Y[j] = fixedmath.VSHR32(fixedmath.MULT32_32_Q31(rgain, fixedmath.ADD32(l, r)), kr-15)
	}
}

// orderyTable is the natural-to-ordery Hadamard index table for N=2,4,8,16
// (bands.c:567).
var orderyTable = [...]int{
	1, 0,
	3, 0, 2, 1,
	7, 0, 4, 3, 6, 1, 5, 2,
	15, 0, 8, 7, 12, 3, 11, 4, 14, 1, 9, 6, 13, 2, 10, 5,
}

// deinterleaveHadamard is deinterleave_hadamard (bands.c:574).
func deinterleaveHadamard(X []int32, N0, stride, hadamard int) {
	N := N0 * stride
	tmp := make([]int32, N)
	// celt_assert(stride>0)
	if hadamard != 0 {
		ordery := orderyTable[stride-2:]
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[ordery[i]*N0+j] = X[j*stride+i]
			}
		}
	} else {
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[i*N0+j] = X[j*stride+i]
			}
		}
	}
	copy(X[:N], tmp)
}

// interleaveHadamard is interleave_hadamard (bands.c:600).
func interleaveHadamard(X []int32, N0, stride, hadamard int) {
	N := N0 * stride
	tmp := make([]int32, N)
	if hadamard != 0 {
		ordery := orderyTable[stride-2:]
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[j*stride+i] = X[ordery[i]*N0+j]
			}
		}
	} else {
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[j*stride+i] = X[i*N0+j]
			}
		}
	}
	copy(X[:N], tmp)
}

// haar1 is haar1 (bands.c:623): the in-place length-2 Hadamard butterfly over
// stride-interleaved pairs.
func haar1(X []int32, N0, stride int) {
	N0 >>= 1
	for i := 0; i < stride; i++ {
		for j := 0; j < N0; j++ {
			tmp1 := fixedmath.MULT32_32_Q31(sqrt2Inv31, X[stride*2*j+i])
			tmp2 := fixedmath.MULT32_32_Q31(sqrt2Inv31, X[stride*(2*j+1)+i])
			X[stride*2*j+i] = fixedmath.ADD32(tmp1, tmp2)
			X[stride*(2*j+1)+i] = fixedmath.SUB32(tmp1, tmp2)
		}
	}
}

// computeQn is compute_qn (bands.c:638): pick the resolution qn for the split
// angle theta.
func computeQn(N, b, offset, pulseCap, stereo int) int {
	exp2Table8 := [8]int16{16384, 17866, 19483, 21247, 23170, 25267, 27554, 30048}
	var qn, qb int
	N2 := 2*N - 1
	if stereo != 0 && N == 2 {
		N2--
	}
	// The upper limit ensures that in a stereo split with itheta==16384, we'll
	// always have enough bits left over to code at least one pulse in the side.
	qb = int(fixedmath.Celt_sudiv(int32(b+N2*offset), int32(N2)))
	qb = fixedmath.IMIN(b-pulseCap-(4<<bitRes), qb)
	qb = fixedmath.IMIN(8<<bitRes, qb)
	if qb < (1 << bitRes >> 1) {
		qn = 1
	} else {
		qn = int(exp2Table8[qb&0x7]) >> (14 - (qb >> bitRes))
		qn = (qn + 1) >> 1 << 1
	}
	// celt_assert(qn <= 256)
	return qn
}

// bandCtx mirrors struct band_ctx (bands.c:664) for the frozen non-QEXT build.
// The encoder ec is not carried (encode is phase 4); dec is the range decoder.
type bandCtx struct {
	encode            int
	resynth           int
	m                 *celtMode
	i                 int
	intensity         int
	spread            int
	tf_change         int
	dec               *rangecoding.Decoder
	remaining_bits    int32
	bandE             []int32
	seed              uint32
	theta_round       int
	disable_inv       int
	avoid_split_noise int
}

// splitCtx mirrors struct split_ctx (bands.c:688) for the non-QEXT build.
type splitCtx struct {
	inv    int
	imid   int
	iside  int
	delta  int
	itheta int
	qalloc int
}

// computeTheta ports compute_theta (bands.c:700). It decodes the split angle
// theta at resolution qn and derives imid/iside/delta plus the qalloc bit cost.
func computeTheta(ctx *bandCtx, sctx *splitCtx, X, Y []int32, N int, b *int, B, B0, LM, stereo int, fill *int) {
	itheta := 0
	var delta int
	var imid, iside int
	var qalloc int
	inv := 0
	encode := ctx.encode
	m := ctx.m
	i := ctx.i
	intensity := ctx.intensity
	dec := ctx.dec

	// Decide on the resolution to give to the split parameter theta.
	pulseCap := int(m.logN[i]) + LM*(1<<bitRes)
	thetaOffset := qthetaOffset
	if stereo != 0 && N == 2 {
		thetaOffset = qthetaOffsetTwophase
	}
	offset := (pulseCap >> 1) - thetaOffset
	qn := computeQn(N, *b, offset, pulseCap, stereo)
	if stereo != 0 && i >= intensity {
		qn = 1
	}
	if encode != 0 {
		// TODO(phase 4): itheta_q30 = stereo_itheta(X, Y, stereo, N, arch)
		panic(bandsEncodeTODO)
	}
	tell := int32(dec.TellFrac())
	if qn != 1 {
		if encode != 0 {
			// TODO(phase 4): encoder theta quantization (theta_round / avoid_split_noise)
			panic(bandsEncodeTODO)
		}
		// Entropy coding of the angle. We use a uniform pdf for the time split,
		// a step for stereo, and a triangular one for the rest.
		if stereo != 0 && N > 2 {
			p0 := 3
			x0 := qn / 2
			ft := p0*(x0+1) + x0
			// Use a probability of p0 up to itheta=8192 and then use 1 after.
			fs := int(dec.Decode(uint32(ft)))
			var x int
			if fs < (x0+1)*p0 {
				x = fs / p0
			} else {
				x = x0 + 1 + (fs - (x0+1)*p0)
			}
			var fl, fh int
			if x <= x0 {
				fl = p0 * x
				fh = p0 * (x + 1)
			} else {
				fl = (x - 1 - x0) + (x0+1)*p0
				fh = (x - x0) + (x0+1)*p0
			}
			dec.DecUpdate(uint32(fl), uint32(fh), uint32(ft))
			itheta = x
		} else if B0 > 1 || stereo != 0 {
			// Uniform pdf.
			itheta = int(dec.DecUint(uint32(qn + 1)))
		} else {
			// Triangular pdf.
			ft := ((qn >> 1) + 1) * ((qn >> 1) + 1)
			fm := int(dec.Decode(uint32(ft)))
			var fl, fs int
			if fm < ((qn >> 1) * ((qn >> 1) + 1) >> 1) {
				itheta = (int(fixedmath.Isqrt32(uint32(8*fm+1))) - 1) >> 1
				fs = itheta + 1
				fl = itheta * (itheta + 1) >> 1
			} else {
				itheta = (2*(qn+1) - int(fixedmath.Isqrt32(uint32(8*(ft-fm-1)+1)))) >> 1
				fs = qn + 1 - itheta
				fl = ft - ((qn + 1 - itheta) * (qn + 2 - itheta) >> 1)
			}
			dec.DecUpdate(uint32(fl), uint32(fl+fs), uint32(ft))
		}
		// celt_assert(itheta>=0)
		itheta = int(fixedmath.Celt_udiv(uint32(itheta*16384), uint32(qn)))
		if encode != 0 && stereo != 0 {
			// TODO(phase 4): intensity_stereo / stereo_split on encode
			panic(bandsEncodeTODO)
		}
	} else if stereo != 0 {
		if encode != 0 {
			// TODO(phase 4): inv/intensity_stereo on encode
			panic(bandsEncodeTODO)
		}
		if *b > 2<<bitRes && ctx.remaining_bits > 2<<bitRes {
			inv = dec.DecBitLogp(2)
		} else {
			inv = 0
		}
		// inv flag override to avoid problems with downmixing.
		if ctx.disable_inv != 0 {
			inv = 0
		}
		itheta = 0
	}
	qalloc = int(int32(dec.TellFrac()) - tell)
	*b -= qalloc

	switch itheta {
	case 0:
		imid = 32767
		iside = 0
		*fill &= (1 << B) - 1
		delta = -16384
	case 16384:
		imid = 0
		iside = 32767
		*fill &= ((1 << B) - 1) << B
		delta = 16384
	default:
		imid = int(bitexactCos(int16(itheta)))
		iside = int(bitexactCos(int16(16384 - itheta)))
		// The mid vs side allocation that minimizes squared error in that band.
		delta = int(fracMul16(int32((N-1)<<7), int32(bitexactLog2tan(iside, imid))))
	}

	sctx.inv = inv
	sctx.imid = imid
	sctx.iside = iside
	sctx.delta = delta
	sctx.itheta = itheta
	sctx.qalloc = qalloc
}

// quantBandN1 ports quant_band_n1 (bands.c:934): the special one-sample case.
func quantBandN1(ctx *bandCtx, X, Y, lowbandOut []int32) uint32 {
	stereo := 0
	if Y != nil {
		stereo = 1
	}
	x := X
	c := 0
	for {
		sign := 0
		if ctx.remaining_bits >= 1<<bitRes {
			if ctx.encode != 0 {
				// TODO(phase 4): sign = x[0]<0; ec_enc_bits(ec, sign, 1)
				panic(bandsEncodeTODO)
			}
			sign = int(ctx.dec.DecBits(1))
			ctx.remaining_bits -= 1 << bitRes
		}
		if ctx.resynth != 0 {
			if sign != 0 {
				x[0] = -normScaling
			} else {
				x[0] = normScaling
			}
		}
		x = Y
		c++
		if c >= 1+stereo {
			break
		}
	}
	if lowbandOut != nil {
		lowbandOut[0] = fixedmath.SHR32(X[0], 4)
	}
	return 1
}

// quantPartition ports quant_partition (bands.c:973): encode/decode a mono
// partition, recursively splitting the band and transmitting the energy
// difference between the two half-bands.
func quantPartition(ctx *bandCtx, X []int32, N, b, B int, lowband []int32, LM int, gain int32, fill int) uint32 {
	var imid, iside int
	B0 := B
	var mid, side int32
	var cm uint32
	var Y []int32
	encode := ctx.encode
	m := ctx.m
	i := ctx.i
	spread := ctx.spread
	dec := ctx.dec

	// If we need 1.5 more bit than we can produce, split the band in two.
	cache := m.cache.bits[int(m.cache.index[(LM+1)*m.nbEBands+i]):]
	if LM != -1 && b > int(cache[cache[0]])+12 && N > 2 {
		var mbits, sbits, delta int
		var itheta int
		var qalloc int
		var sctx splitCtx
		var nextLowband2 []int32
		var rebalance int32

		N >>= 1
		Y = X[N:]
		LM -= 1
		if B == 1 {
			fill = (fill & 1) | (fill << 1)
		}
		B = (B + 1) >> 1

		computeTheta(ctx, &sctx, X, Y, N, &b, B, B0, LM, 0, &fill)
		imid = sctx.imid
		iside = sctx.iside
		delta = sctx.delta
		itheta = sctx.itheta
		qalloc = sctx.qalloc
		mid = fixedmath.SHL32(fixedmath.EXTEND32(int16(imid)), 16)
		side = fixedmath.SHL32(fixedmath.EXTEND32(int16(iside)), 16)

		// Give more bits to low-energy MDCTs than they would otherwise deserve.
		if B0 > 1 && (itheta&0x3fff) != 0 {
			if itheta > 8192 {
				// Rough approximation for pre-echo masking.
				delta -= delta >> (4 - LM)
			} else {
				// Corresponds to a forward-masking slope of 1.5 dB per 10 ms.
				delta = fixedmath.IMIN(0, delta+(N<<bitRes>>(5-LM)))
			}
		}
		mbits = fixedmath.IMAX(0, fixedmath.IMIN(b, (b-delta)/2))
		sbits = b - mbits
		ctx.remaining_bits -= int32(qalloc)

		if lowband != nil {
			nextLowband2 = lowband[N:] // >32-bit split case
		}

		rebalance = ctx.remaining_bits
		if mbits >= sbits {
			cm = quantPartition(ctx, X, N, mbits, B, lowband, LM, fixedmath.MULT32_32_Q31(gain, mid), fill)
			rebalance = int32(mbits) - (rebalance - ctx.remaining_bits)
			if rebalance > 3<<bitRes && itheta != 0 {
				sbits += int(rebalance) - (3 << bitRes)
			}
			cm |= quantPartition(ctx, Y, N, sbits, B, nextLowband2, LM, fixedmath.MULT32_32_Q31(gain, side), fill>>B) << (B0 >> 1)
		} else {
			cm = quantPartition(ctx, Y, N, sbits, B, nextLowband2, LM, fixedmath.MULT32_32_Q31(gain, side), fill>>B) << (B0 >> 1)
			rebalance = int32(sbits) - (rebalance - ctx.remaining_bits)
			if rebalance > 3<<bitRes && itheta != 16384 {
				mbits += int(rebalance) - (3 << bitRes)
			}
			cm |= quantPartition(ctx, X, N, mbits, B, lowband, LM, fixedmath.MULT32_32_Q31(gain, mid), fill)
		}
	} else {
		// This is the basic no-split case.
		q := bits2pulses(m, i, LM, b)
		currBits := pulses2bits(m, i, LM, q)
		ctx.remaining_bits -= int32(currBits)

		// Ensures we can never bust the budget.
		for ctx.remaining_bits < 0 && q > 0 {
			ctx.remaining_bits += int32(currBits)
			q--
			currBits = pulses2bits(m, i, LM, q)
			ctx.remaining_bits -= int32(currBits)
		}

		if q != 0 {
			K := getPulses(q)
			// Finally do the actual quantization.
			if encode != 0 {
				// TODO(phase 4): cm = alg_quant(X, N, K, spread, B, ec, gain, resynth, arch)
				panic(bandsEncodeTODO)
			}
			cm = AlgUnquant(X, N, K, spread, B, dec, gain)
		} else {
			// If there's no pulse, fill the band anyway.
			if ctx.resynth != 0 {
				cmMask := uint32(1<<B) - 1
				fill &= int(cmMask)
				if fill == 0 {
					for j := 0; j < N; j++ {
						X[j] = 0
					}
				} else {
					if lowband == nil {
						// Noise.
						for j := 0; j < N; j++ {
							ctx.seed = celtLcgRand(ctx.seed)
							X[j] = fixedmath.SHL32(int32(ctx.seed)>>20, normShift-14)
						}
						cm = cmMask
					} else {
						// Folded spectrum.
						for j := 0; j < N; j++ {
							ctx.seed = celtLcgRand(ctx.seed)
							// About 48 dB below the "normal" folding level.
							tmp := fixedmath.QCONST16(1.0/256, normShift-4)
							if ctx.seed&0x8000 == 0 {
								tmp = -tmp
							}
							X[j] = lowband[j] + int32(tmp)
						}
						cm = uint32(fill)
					}
					RenormaliseVector(X, N, gain)
				}
			}
		}
	}
	return cm
}

// quantBand ports quant_band (bands.c:1248): encode/decode a band for the mono
// case, handling the time-frequency recombine/subdivide and Hadamard reordering
// around quant_partition.
func quantBand(ctx *bandCtx, X []int32, N, b, B int, lowband []int32, LM int, lowbandOut []int32, gain int32, lowbandScratch []int32, fill int) uint32 {
	N0 := N
	N_B := N
	var N_B0 int
	B0 := B
	timeDivide := 0
	recombine := 0
	longBlocks := 0
	var cm uint32
	encode := ctx.encode
	tfChange := ctx.tf_change

	if B0 == 1 {
		longBlocks = 1
	}
	N_B = int(fixedmath.Celt_udiv(uint32(N_B), uint32(B)))

	// Special case for one sample.
	if N == 1 {
		return quantBandN1(ctx, X, nil, lowbandOut)
	}

	if tfChange > 0 {
		recombine = tfChange
	}
	// Band recombining to increase frequency resolution.
	if lowbandScratch != nil && lowband != nil && (recombine != 0 || ((N_B&1) == 0 && tfChange < 0) || B0 > 1) {
		copy(lowbandScratch[:N], lowband[:N])
		lowband = lowbandScratch
	}

	for k := 0; k < recombine; k++ {
		bitInterleaveTable := [16]byte{0, 1, 1, 1, 2, 3, 3, 3, 2, 3, 3, 3, 2, 3, 3, 3}
		if encode != 0 {
			// TODO(phase 4): haar1(X, N>>k, 1<<k)
			panic(bandsEncodeTODO)
		}
		if lowband != nil {
			haar1(lowband, N>>k, 1<<k)
		}
		fill = int(bitInterleaveTable[fill&0xF]) | int(bitInterleaveTable[fill>>4])<<2
	}
	B >>= recombine
	N_B <<= recombine

	// Increasing the time resolution.
	for (N_B&1) == 0 && tfChange < 0 {
		if encode != 0 {
			// TODO(phase 4): haar1(X, N_B, B)
			panic(bandsEncodeTODO)
		}
		if lowband != nil {
			haar1(lowband, N_B, B)
		}
		fill |= fill << B
		B <<= 1
		N_B >>= 1
		timeDivide++
		tfChange++
	}
	B0 = B
	N_B0 = N_B

	// Reorganize the samples in time order instead of frequency order.
	if B0 > 1 {
		if encode != 0 {
			// TODO(phase 4): deinterleave_hadamard(X, N_B>>recombine, B0<<recombine, longBlocks)
			panic(bandsEncodeTODO)
		}
		if lowband != nil {
			deinterleaveHadamard(lowband, N_B>>recombine, B0<<recombine, longBlocks)
		}
	}

	cm = quantPartition(ctx, X, N, b, B, lowband, LM, gain, fill)

	// This code is used by the decoder and by the resynthesis-enabled encoder.
	if ctx.resynth != 0 {
		// Undo the sample reorganization going from time order to frequency order.
		if B0 > 1 {
			interleaveHadamard(X, N_B>>recombine, B0<<recombine, longBlocks)
		}

		// Undo time-freq changes that we did earlier.
		N_B = N_B0
		B = B0
		for k := 0; k < timeDivide; k++ {
			B >>= 1
			N_B <<= 1
			cm |= cm >> B
			haar1(X, N_B, B)
		}

		for k := 0; k < recombine; k++ {
			bitDeinterleaveTable := [16]byte{
				0x00, 0x03, 0x0C, 0x0F, 0x30, 0x33, 0x3C, 0x3F,
				0xC0, 0xC3, 0xCC, 0xCF, 0xF0, 0xF3, 0xFC, 0xFF,
			}
			cm = uint32(bitDeinterleaveTable[cm])
			haar1(X, N0>>k, 1<<k)
		}
		B <<= recombine

		// Scale output for later folding.
		if lowbandOut != nil {
			n := int16(fixedmath.Celt_sqrt(fixedmath.SHL32(fixedmath.EXTEND32(int16(N0)), 22)))
			for j := 0; j < N0; j++ {
				lowbandOut[j] = fixedmath.MULT16_32_Q15(n, X[j])
			}
		}
		cm &= uint32((1 << B) - 1)
	}
	return cm
}

// quantBandStereo ports quant_band_stereo (bands.c:1387): encode/decode a band
// for the stereo case, coding the mid/side split and (on resynth) un-mixing.
func quantBandStereo(ctx *bandCtx, X, Y []int32, N, b, B int, lowband []int32, LM int, lowbandOut, lowbandScratch []int32, fill int) uint32 {
	var imid, iside int
	var inv int
	var mid, side int32
	var cm uint32
	var mbits, sbits, delta int
	var itheta int
	var qalloc int
	var sctx splitCtx
	encode := ctx.encode
	dec := ctx.dec

	// Special case for one sample.
	if N == 1 {
		return quantBandN1(ctx, X, Y, lowbandOut)
	}

	origFill := fill

	if encode != 0 {
		// TODO(phase 4): MIN_STEREO_ENERGY copy-down of a near-silent channel
		panic(bandsEncodeTODO)
	}
	computeTheta(ctx, &sctx, X, Y, N, &b, B, B, LM, 1, &fill)
	inv = sctx.inv
	imid = sctx.imid
	iside = sctx.iside
	delta = sctx.delta
	itheta = sctx.itheta
	qalloc = sctx.qalloc
	mid = fixedmath.SHL32(fixedmath.EXTEND32(int16(imid)), 16)
	side = fixedmath.SHL32(fixedmath.EXTEND32(int16(iside)), 16)

	// Special case for N=2 that only works for stereo and takes advantage of the
	// fact that mid and side are orthogonal to encode the side with just one bit.
	if N == 2 {
		sign := 0
		mbits = b
		sbits = 0
		// Only need one bit for the side.
		if itheta != 0 && itheta != 16384 {
			sbits = 1 << bitRes
		}
		mbits -= sbits
		c := b2i(itheta > 8192)
		ctx.remaining_bits -= int32(qalloc + sbits)

		var x2, y2 []int32
		if c != 0 {
			x2 = Y
			y2 = X
		} else {
			x2 = X
			y2 = Y
		}
		if sbits != 0 {
			if encode != 0 {
				// TODO(phase 4): sign = ...; ec_enc_bits(ec, sign, 1)
				panic(bandsEncodeTODO)
			}
			sign = int(dec.DecBits(1))
		}
		sign = 1 - 2*sign
		// We use origFill here because we want to fold the side, but if
		// itheta==16384, we'll have cleared the low bits of fill.
		cm = quantBand(ctx, x2, N, mbits, B, lowband, LM, lowbandOut, q31one, lowbandScratch, origFill)
		// We don't split N=2 bands, so cm is either 1 or 0 (for a fold-collapse).
		y2[0] = int32(-sign) * x2[1]
		y2[1] = int32(sign) * x2[0]
		if ctx.resynth != 0 {
			X[0] = fixedmath.MULT32_32_Q31(mid, X[0])
			X[1] = fixedmath.MULT32_32_Q31(mid, X[1])
			Y[0] = fixedmath.MULT32_32_Q31(side, Y[0])
			Y[1] = fixedmath.MULT32_32_Q31(side, Y[1])
			tmp := X[0]
			X[0] = fixedmath.SUB32(tmp, Y[0])
			Y[0] = fixedmath.ADD32(tmp, Y[0])
			tmp = X[1]
			X[1] = fixedmath.SUB32(tmp, Y[1])
			Y[1] = fixedmath.ADD32(tmp, Y[1])
		}
	} else {
		// "Normal" split code.
		var rebalance int32
		mbits = fixedmath.IMAX(0, fixedmath.IMIN(b, (b-delta)/2))
		sbits = b - mbits
		ctx.remaining_bits -= int32(qalloc)

		rebalance = ctx.remaining_bits
		if mbits >= sbits {
			// In stereo mode we do not apply a scaling to the mid because we need
			// the normalized mid for folding later.
			cm = quantBand(ctx, X, N, mbits, B, lowband, LM, lowbandOut, q31one, lowbandScratch, fill)
			rebalance = int32(mbits) - (rebalance - ctx.remaining_bits)
			if rebalance > 3<<bitRes && itheta != 0 {
				sbits += int(rebalance) - (3 << bitRes)
			}
			// For a stereo split, the high bits of fill are always zero, so no
			// folding will be done to the side.
			cm |= quantBand(ctx, Y, N, sbits, B, nil, LM, nil, side, nil, fill>>B)
		} else {
			// For a stereo split, the high bits of fill are always zero.
			cm = quantBand(ctx, Y, N, sbits, B, nil, LM, nil, side, nil, fill>>B)
			rebalance = int32(sbits) - (rebalance - ctx.remaining_bits)
			if rebalance > 3<<bitRes && itheta != 16384 {
				mbits += int(rebalance) - (3 << bitRes)
			}
			// We need the normalized mid for folding later.
			cm |= quantBand(ctx, X, N, mbits, B, lowband, LM, lowbandOut, q31one, lowbandScratch, fill)
		}
	}

	// This code is used by the decoder and by the resynthesis-enabled encoder.
	if ctx.resynth != 0 {
		if N != 2 {
			stereoMerge(X, Y, mid, N)
		}
		if inv != 0 {
			for j := 0; j < N; j++ {
				Y[j] = -Y[j]
			}
		}
	}
	return cm
}

// specialHybridFolding ports special_hybrid_folding (bands.c:1575): duplicate
// enough of the first band's folding data to fold the second band (no data is
// copied for CELT-only mode, where start==0).
func specialHybridFolding(m *celtMode, norm, norm2 []int32, start, M, dualStereo int) {
	eBands := m.eBands
	n1 := M * (int(eBands[start+1]) - int(eBands[start]))
	n2 := M * (int(eBands[start+2]) - int(eBands[start+1]))
	// OPUS_COPY(&norm[n1], &norm[2*n1 - n2], n2-n1); no-op when n2<=n1.
	if n2 > n1 {
		copy(norm[n1:n2], norm[2*n1-n2:n1])
		if dualStereo != 0 {
			copy(norm2[n1:n2], norm2[2*n1-n2:n1])
		}
	}
}

// quantAllBands ports quant_all_bands (bands.c:1589), decode direction. It runs
// the per-band loop, threading the LCG seed and the folding norm buffers through
// quant_band / quant_band_stereo. encode!=0 is phase 4 (theta_rdo is forced off
// on decode).
func quantAllBands(encode int, m *celtMode, start, end int, X_, Y_ []int32, collapseMasks []byte,
	bandE []int32, pulses []int, shortBlocks, spread, dualStereo, intensity int, tfRes []int,
	totalBits, balance int32, dec *rangecoding.Decoder, LM, codedBands int, seed *uint32, disableInv int) {
	eBands := m.eBands
	updateLowband := 1
	C := 1
	if Y_ != nil {
		C = 2
	}
	// theta_rdo = encode && Y_!=NULL && !dual_stereo && complexity>=8; always 0
	// on decode. resynth = !encode || theta_rdo.
	thetaRdo := 0
	resynth := 0
	if encode == 0 {
		resynth = 1
	}
	var ctx bandCtx

	M := 1 << LM
	B := 1
	if shortBlocks != 0 {
		B = M
	}
	normOffset := M * int(eBands[start])
	// No need to allocate norm for the last band because we don't need an output
	// in that band.
	normLen := M*int(eBands[m.nbEBands-1]) - normOffset
	_norm := make([]int32, C*normLen)
	norm := _norm[:normLen]
	norm2 := _norm[normLen:]

	// For decoding, we can use the last band as scratch space because we don't
	// need that scratch space for the last band and we don't care about the data
	// there until we're decoding the last band.
	var lowbandScratch []int32
	lowbandScratch = X_[M*int(eBands[m.effEBands-1]):]

	lowbandOffset := 0
	ctx.bandE = bandE
	ctx.dec = dec
	ctx.encode = encode
	ctx.intensity = intensity
	ctx.m = m
	ctx.seed = *seed
	ctx.spread = spread
	ctx.disable_inv = disableInv
	ctx.resynth = resynth
	ctx.theta_round = 0
	// Avoid injecting noise in the first band on transients.
	ctx.avoid_split_noise = b2i(B > 1)

	for i := start; i < end; i++ {
		ctx.i = i
		last := b2i(i == end-1)

		X := X_[M*int(eBands[i]):]
		var Y []int32
		if Y_ != nil {
			Y = Y_[M*int(eBands[i]):]
		}
		N := M*int(eBands[i+1]) - M*int(eBands[i])
		// celt_assert(N > 0)
		tell := int32(dec.TellFrac())

		// Compute how many bits we want to allocate to this band.
		if i != start {
			balance -= tell
		}
		remainingBits := totalBits - tell - 1
		ctx.remaining_bits = remainingBits
		var b int
		if i <= codedBands-1 {
			currBalance := fixedmath.Celt_sudiv(balance, int32(fixedmath.IMIN(3, codedBands-i)))
			b = fixedmath.IMAX(0, fixedmath.IMIN(16383, fixedmath.IMIN(int(remainingBits)+1, pulses[i]+int(currBalance))))
		} else {
			b = 0
		}

		if resynth != 0 && (M*int(eBands[i])-N >= M*int(eBands[start]) || i == start+1) && (updateLowband != 0 || lowbandOffset == 0) {
			lowbandOffset = i
		}
		if i == start+1 {
			specialHybridFolding(m, norm, norm2, start, M, dualStereo)
		}

		tfChange := tfRes[i]
		ctx.tf_change = tfChange
		if i >= m.effEBands {
			X = norm
			if Y_ != nil {
				Y = norm
			}
			lowbandScratch = nil
		}
		if last != 0 && thetaRdo == 0 {
			lowbandScratch = nil
		}

		var xCm, yCm uint32
		effectiveLowband := -1
		// Get a conservative estimate of the collapse_mask's for the bands we're
		// going to be folding from.
		if lowbandOffset != 0 && (spread != spreadAggressive || B > 1 || tfChange < 0) {
			// This ensures we never repeat spectral content within one band.
			effectiveLowband = fixedmath.IMAX(0, M*int(eBands[lowbandOffset])-normOffset-N)
			foldStart := lowbandOffset
			for {
				foldStart--
				if !(M*int(eBands[foldStart]) > effectiveLowband+normOffset) {
					break
				}
			}
			foldEnd := lowbandOffset - 1
			for {
				foldEnd++
				if !(foldEnd < i && M*int(eBands[foldEnd]) < effectiveLowband+normOffset+N) {
					break
				}
			}
			foldI := foldStart
			for {
				xCm |= uint32(collapseMasks[foldI*C+0])
				yCm |= uint32(collapseMasks[foldI*C+C-1])
				foldI++
				if !(foldI < foldEnd) {
					break
				}
			}
		} else {
			// Otherwise, we'll be using the LCG to fold, so all blocks will
			// (almost always) be non-zero.
			xCm = uint32((1 << B) - 1)
			yCm = xCm
		}

		if dualStereo != 0 && i == intensity {
			// Switch off dual stereo to do intensity.
			dualStereo = 0
			if resynth != 0 {
				for j := 0; j < M*int(eBands[i])-normOffset; j++ {
					norm[j] = fixedmath.HALF32(norm[j] + norm2[j])
				}
			}
		}
		if dualStereo != 0 {
			var lb1, lb2, lo1, lo2 []int32
			if effectiveLowband != -1 {
				lb1 = norm[effectiveLowband:]
				lb2 = norm2[effectiveLowband:]
			}
			if last == 0 {
				lo1 = norm[M*int(eBands[i])-normOffset:]
				lo2 = norm2[M*int(eBands[i])-normOffset:]
			}
			xCm = quantBand(&ctx, X, N, b/2, B, lb1, LM, lo1, q31one, lowbandScratch, int(xCm))
			yCm = quantBand(&ctx, Y, N, b/2, B, lb2, LM, lo2, q31one, lowbandScratch, int(yCm))
		} else {
			if Y != nil {
				if thetaRdo != 0 && i < intensity {
					// TODO(phase 4): theta-RDO trial loop (encode only)
					panic(bandsEncodeTODO)
				}
				ctx.theta_round = 0
				var lb, lo []int32
				if effectiveLowband != -1 {
					lb = norm[effectiveLowband:]
				}
				if last == 0 {
					lo = norm[M*int(eBands[i])-normOffset:]
				}
				xCm = quantBandStereo(&ctx, X, Y, N, b, B, lb, LM, lo, lowbandScratch, int(xCm|yCm))
			} else {
				var lb, lo []int32
				if effectiveLowband != -1 {
					lb = norm[effectiveLowband:]
				}
				if last == 0 {
					lo = norm[M*int(eBands[i])-normOffset:]
				}
				xCm = quantBand(&ctx, X, N, b, B, lb, LM, lo, q31one, lowbandScratch, int(xCm|yCm))
			}
			yCm = xCm
		}
		collapseMasks[i*C+0] = byte(xCm)
		collapseMasks[i*C+C-1] = byte(yCm)
		balance += int32(pulses[i]) + tell

		// Update the folding position only as long as we have 1 bit/sample depth.
		updateLowband = b2i(b > (N << bitRes))
		// We only need to avoid noise on a split for the first band. After that,
		// we have folding.
		ctx.avoid_split_noise = 0
	}
	*seed = ctx.seed
}

// QuantAllBands is the exported decode seam over quantAllBands, bound to the
// frozen mode48000_960 (mirroring the other internal/celt adapters). It drives
// the pure-Go CELT residual decode (encode==0) so the reftest oracle can pin it
// bit-exact against libopus. bandE is nil on decode (the C decoder passes NULL).
// seed carries the LCG state in and out (the decoder's st->rng).
func QuantAllBands(start, end int, X, Y []int32, collapseMasks []byte, bandE []int32,
	pulses []int, shortBlocks, spread, dualStereo, intensity int, tfRes []int,
	totalBits, balance int32, dec *rangecoding.Decoder, LM, codedBands int, seed *uint32, disableInv int) {
	quantAllBands(0, &mode48000_960, start, end, X, Y, collapseMasks, bandE, pulses,
		shortBlocks, spread, dualStereo, intensity, tfRes, totalBits, balance, dec,
		LM, codedBands, seed, disableInv)
}

// DenormaliseBands is the exported seam over denormaliseBands bound to
// mode48000_960 (celt/celt_decoder.c:453). freq must have length
// M*shortMdctSize; X and bandLogE cover the bands being synthesized.
func DenormaliseBands(X, freq, bandLogE []int32, start, end, M, downsample, silence int) {
	denormaliseBands(&mode48000_960, X, freq, bandLogE, start, end, M, downsample, silence)
}

// ComputeBandEnergies is the exported encode seam over computeBandEnergies bound
// to mode48000_960, letting the refc differential harness drive the pure-Go
// band-energy computation against libopus. X is the MDCT spectrum (length
// C*(shortMdctSize<<LM)); bandE receives C*nbEBands amplitudes.
func ComputeBandEnergies(X, bandE []int32, end, C, LM int) {
	computeBandEnergies(&mode48000_960, X, bandE, end, C, LM, 0)
}

// NormaliseBands is the exported encode seam over normaliseBands bound to
// mode48000_960. freq is the MDCT spectrum, bandE the amplitudes from
// ComputeBandEnergies, and X receives the C*(M*shortMdctSize) unit-energy
// coefficients. M is 1<<LM.
func NormaliseBands(freq, X, bandE []int32, end, C, M int) {
	normaliseBands(&mode48000_960, freq, X, bandE, end, C, M)
}

// AntiCollapse is the exported seam over antiCollapse bound to mode48000_960
// (celt/celt_decoder.c). encode is 0 on the decode path.
func AntiCollapse(X []int32, collapseMasks []byte, LM, C, size, start, end int,
	logE, prev1logE, prev2logE []int32, pulses []int, seed uint32, encode int) {
	antiCollapse(&mode48000_960, X, collapseMasks, LM, C, size, start, end, logE, prev1logE, prev2logE, pulses, seed, encode)
}

// IntensityStereo is the exported seam over intensityStereo bound to
// mode48000_960. intensity_stereo is called only on the encode side of
// compute_theta (bands.c), so it is not reached by the decode differential test;
// the seam keeps the ported helper live for the phase-4 encoder. stereo_merge is
// exercised directly through the stereo quant_all_bands decode path.
func IntensityStereo(X, Y, bandE []int32, bandID, N int) {
	intensityStereo(&mode48000_960, X, Y, bandE, bandID, N)
}
