package celt

// Transliteration of libopus celt/kiss_fft.c and celt/_kiss_fft_guts.h (v1.6.1,
// commit 3da9f7a6) for the frozen FIXED_POINT + DISABLE_FLOAT_API + non-QEXT +
// non-CUSTOM_MODES configuration. The complex FFT core (opus_fft_impl and the
// kf_bfly2/3/4/5 butterflies) plus the inverse transform (opus_ifft) are the
// heart of the CELT inverse MDCT the decoder runs.
//
// Type mapping (celt/kiss_fft.h, celt/arch.h):
//   kiss_fft_scalar     = opus_int32  -> int32   (FFT working values)
//   kiss_twiddle_scalar = celt_coef = opus_int16 -> int16 (twiddles, non-QEXT)
//   COEF_SHIFT          = 16
//   kiss_fft_cpx        -> kissFFTCpx {r,i int32}
//   kiss_twiddle_cpx    -> kissTwiddleCpx {r,i int16}  (defined in modes.go)
//
// The per-stage fixed-point downshifts (fft_downshift) are NOT pre-baked into
// the mode data; they are applied here exactly as in _kiss_fft_guts.h, driven by
// the `downshift` budget threaded through opus_fft_impl (docs/hard-parts.md
// section 8). Every _ovflw add/sub is Go's defined signed wrap, matching the C
// uint32 round-trip; the S_MUL macros are the OPUS_FAST_INT64 int64 forms in
// internal/fixedmath (docs/hard-parts.md section 4).

import (
	"sync"
	"unsafe"

	"github.com/tphakala/simd/cint"

	"github.com/tphakala/go-opus/internal/fixedmath"
)

// coefShift mirrors COEF_SHIFT for the non-QEXT build (celt/kiss_fft.h:52).
const coefShift = 16

// maxFFTRun bounds the largest contiguous per-block span a radix butterfly
// vectorizes: the radix-5 stage of the nfft=480 transform has m=96 (N=1 block of
// 96 complex). Scratch arrays sized to this live on the stack, so the cint paths
// stay zero-allocation.
const maxFFTRun = 96

// minCintBfly3 / minCintBfly5 are the per-butterfly run-length thresholds below
// which the scalar inner loop wins: the cint call plus scratch setup does not
// amortize on short runs. The values are the empirical crossovers from
// BenchmarkBfly3_* / BenchmarkBfly5_* on the arm64 NEON kernels. radix-3
// vectorizes both C_MULs plus the add/sub/scale and crosses over near m=16
// (-25% at m=16, +2% at m=8). radix-5 vectorizes only its four C_MULs (the large
// ya/yb combine stays scalar and reads the results back from scratch), so it
// needs a longer run to pay back and crosses over near m=48 (-11% at m=48, +3.5%
// at m=24). radix-4 non-CUSTOM runs are only ever m<=8, below any crossover, so
// it is left fully scalar.
const (
	minCintBfly3 = 16
	minCintBfly5 = 48
)

// cpxAsInt32 reinterprets a run of kissFFTCpx as the interleaved [r,i,r,i,...]
// []int32 that the cint package operates on. kissFFTCpx is struct{r,i int32}
// (8 bytes, no padding), so a []kissFFTCpx is bit-for-bit a []int32 of twice the
// length; this is a view, not a copy. cint's data model is exactly this layout.
func cpxAsInt32(c []kissFFTCpx) []int32 {
	if len(c) == 0 {
		return nil
	}
	return unsafe.Slice((*int32)(unsafe.Pointer(&c[0])), 2*len(c))
}

// twKey identifies a packed twiddle run by its source table, stride and count.
type twKey struct {
	base   *kissTwiddleCpx
	stride int
	count  int
}

// packedTwiddleCache memoizes the strided-to-contiguous twiddle gathers. The
// gather pattern is data-independent (it depends only on the static mode's
// twiddle table, the stage stride and the run length), so each (table, stride,
// count) tuple is packed exactly once for the process lifetime and every FFT call
// thereafter reuses the contiguous []int16 directly.
var packedTwiddleCache sync.Map // twKey -> []int16

// packedTwiddles returns the contiguous Q15 [tr,ti,tr,ti,...] int16 slice of the
// twiddles tw[j*stride] for j in 0..count-1, the layout cint.Mul consumes. The
// result is cached (see packedTwiddleCache) so the strided gather runs once.
func packedTwiddles(tw []kissTwiddleCpx, stride, count int) []int16 {
	key := twKey{&tw[0], stride, count}
	if v, ok := packedTwiddleCache.Load(key); ok {
		return v.([]int16)
	}
	p := make([]int16, 2*count)
	for j := 0; j < count; j++ {
		c := tw[j*stride]
		p[2*j] = c.r
		p[2*j+1] = c.i
	}
	v, _ := packedTwiddleCache.LoadOrStore(key, p)
	return v.([]int16)
}

// kissFFTCpx mirrors celt/kiss_fft.h kiss_fft_cpx in the FIXED_POINT build: r
// and i are kiss_fft_scalar (opus_int32).
type kissFFTCpx struct {
	r int32
	i int32
}

// S_MUL(a,b) = MULT16_32_Q15(b, a): a is a 32-bit FFT value, b a 16-bit twiddle
// (_kiss_fft_guts.h:61, non-QEXT). The int64 (OPUS_FAST_INT64) form is required.
func sMul(a int32, b int16) int32 { return fixedmath.MULT16_32_Q15(b, a) }

// S_MUL2(a,b) = MULT16_32_Q16(b, a) (_kiss_fft_guts.h:62). Used by the forward
// FFT input scaling.
func sMul2(a int32, b int16) int32 { return fixedmath.MULT16_32_Q16(b, a) }

// cMul mirrors C_MUL(m,a,b): m = a*b where a is a complex FFT value (int32) and
// b a complex twiddle (int16). (_kiss_fft_guts.h:65)
func cMul(a kissFFTCpx, b kissTwiddleCpx) kissFFTCpx {
	return kissFFTCpx{
		r: fixedmath.SUB32_ovflw(sMul(a.r, b.r), sMul(a.i, b.i)),
		i: fixedmath.ADD32_ovflw(sMul(a.r, b.i), sMul(a.i, b.r)),
	}
}

// cAdd mirrors C_ADD(res,a,b): res = a + b. (_kiss_fft_guts.h:84)
func cAdd(a, b kissFFTCpx) kissFFTCpx {
	return kissFFTCpx{fixedmath.ADD32_ovflw(a.r, b.r), fixedmath.ADD32_ovflw(a.i, b.i)}
}

// cSub mirrors C_SUB(res,a,b): res = a - b. (_kiss_fft_guts.h:87)
func cSub(a, b kissFFTCpx) kissFFTCpx {
	return kissFFTCpx{fixedmath.SUB32_ovflw(a.r, b.r), fixedmath.SUB32_ovflw(a.i, b.i)}
}

// cMulByScalar mirrors C_MULBYSCALAR(c,s): c.r=S_MUL(c.r,s); c.i=S_MUL(c.i,s),
// with s a 16-bit twiddle scalar. (_kiss_fft_guts.h:73)
func cMulByScalar(c kissFFTCpx, s int16) kissFFTCpx {
	return kissFFTCpx{sMul(c.r, s), sMul(c.i, s)}
}

// fft_downshift scales the working buffer down by up to `step` bits, spending
// from the remaining `total` budget, so the fixed-point FFT does not overflow.
// (celt/kiss_fft.c:539)
func fftDownshift(x []kissFFTCpx, N int, total *int, step int) {
	shift := fixedmath.IMIN(step, *total)
	*total -= shift
	if shift == 1 {
		for i := 0; i < N; i++ {
			x[i].r = fixedmath.SHR32(x[i].r, 1)
			x[i].i = fixedmath.SHR32(x[i].i, 1)
		}
	} else if shift > 0 {
		for i := 0; i < N; i++ {
			x[i].r = fixedmath.PSHR32(x[i].r, shift)
			x[i].i = fixedmath.PSHR32(x[i].i, shift)
		}
	}
}

// kf_bfly2 is the radix-2 butterfly. Under non-CUSTOM_MODES m is always 4 (the
// radix-2 stage follows a radix-4), so only that branch exists. fout is the
// working buffer positioned at the stage's Fout. (celt/kiss_fft.c:52)
func kfBfly2(fout []kissFFTCpx, m, N int) {
	_ = m
	// tw = QCONST32(0.7071067812f, COEF_SHIFT-1) stored in a celt_coef (int16).
	tw := int16(fixedmath.QCONST32(0.7071067812, coefShift-1))
	pos := 0
	for i := 0; i < N; i++ {
		// Fout2 = Fout + 4.
		var t kissFFTCpx
		t = fout[pos+4]
		fout[pos+4] = cSub(fout[pos+0], t)
		fout[pos+0] = cAdd(fout[pos+0], t)

		t.r = sMul(fixedmath.ADD32_ovflw(fout[pos+5].r, fout[pos+5].i), tw)
		t.i = sMul(fixedmath.SUB32_ovflw(fout[pos+5].i, fout[pos+5].r), tw)
		fout[pos+5] = cSub(fout[pos+1], t)
		fout[pos+1] = cAdd(fout[pos+1], t)

		t.r = fout[pos+6].i
		t.i = fixedmath.NEG32_ovflw(fout[pos+6].r)
		fout[pos+6] = cSub(fout[pos+2], t)
		fout[pos+2] = cAdd(fout[pos+2], t)

		t.r = sMul(fixedmath.SUB32_ovflw(fout[pos+7].i, fout[pos+7].r), tw)
		t.i = sMul(fixedmath.NEG32_ovflw(fixedmath.ADD32_ovflw(fout[pos+7].i, fout[pos+7].r)), tw)
		fout[pos+7] = cSub(fout[pos+3], t)
		fout[pos+3] = cAdd(fout[pos+3], t)
		pos += 8
	}
}

// kf_bfly4 is the radix-4 butterfly, with a degenerate m==1 branch (all twiddles
// are 1) used for the final stage. fstride is fstride[i]<<st.shift; N and mm are
// the stage's fstride[i] and the outer m2. (celt/kiss_fft.c:108)
func kfBfly4(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	if m == 1 {
		// Degenerate case where all the twiddles are 1.
		pos := 0
		for i := 0; i < N; i++ {
			var scratch0, scratch1 kissFFTCpx
			scratch0 = cSub(fout[pos+0], fout[pos+2])
			fout[pos+0] = cAdd(fout[pos+0], fout[pos+2])
			scratch1 = cAdd(fout[pos+1], fout[pos+3])
			fout[pos+2] = cSub(fout[pos+0], scratch1)
			fout[pos+0] = cAdd(fout[pos+0], scratch1)
			scratch1 = cSub(fout[pos+1], fout[pos+3])

			fout[pos+1].r = fixedmath.ADD32_ovflw(scratch0.r, scratch1.i)
			fout[pos+1].i = fixedmath.SUB32_ovflw(scratch0.i, scratch1.r)
			fout[pos+3].r = fixedmath.SUB32_ovflw(scratch0.r, scratch1.i)
			fout[pos+3].i = fixedmath.ADD32_ovflw(scratch0.i, scratch1.r)
			pos += 4
		}
	} else {
		m2 := 2 * m
		m3 := 3 * m
		for i := 0; i < N; i++ {
			pos := i * mm
			tw1, tw2, tw3 := 0, 0, 0
			// m is guaranteed to be a multiple of 4.
			for j := 0; j < m; j++ {
				var scratch [6]kissFFTCpx
				scratch[0] = cMul(fout[pos+m], st.twiddles[tw1])
				scratch[1] = cMul(fout[pos+m2], st.twiddles[tw2])
				scratch[2] = cMul(fout[pos+m3], st.twiddles[tw3])

				scratch[5] = cSub(fout[pos], scratch[1])
				fout[pos] = cAdd(fout[pos], scratch[1])
				scratch[3] = cAdd(scratch[0], scratch[2])
				scratch[4] = cSub(scratch[0], scratch[2])
				fout[pos+m2] = cSub(fout[pos], scratch[3])
				tw1 += fstride
				tw2 += fstride * 2
				tw3 += fstride * 3
				fout[pos] = cAdd(fout[pos], scratch[3])

				fout[pos+m].r = fixedmath.ADD32_ovflw(scratch[5].r, scratch[4].i)
				fout[pos+m].i = fixedmath.SUB32_ovflw(scratch[5].i, scratch[4].r)
				fout[pos+m3].r = fixedmath.SUB32_ovflw(scratch[5].r, scratch[4].i)
				fout[pos+m3].i = fixedmath.ADD32_ovflw(scratch[5].i, scratch[4].r)
				pos++
			}
		}
	}
}

// kf_bfly3 is the radix-3 butterfly. epi3.i is the fixed constant
// -QCONST32(0.86602540f, COEF_SHIFT-1). (celt/kiss_fft.c:180)
//
// Small runs do not amortize the cint call and scratch-setup overhead (the
// crossover sits near m=16 for this two-C_MUL stage, -25% at m=16, +2% at m=8),
// so they keep the scalar inner loop. See BenchmarkBfly3_*.
func kfBfly3(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	if m < minCintBfly3 {
		kfBfly3Scalar(fout, fstride, st, m, N, mm)
		return
	}
	kfBfly3Cint(fout, fstride, st, m, N, mm)
}

func kfBfly3Scalar(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	m2 := 2 * m
	epi3i := int16(-fixedmath.QCONST32(0.86602540, coefShift-1))
	for i := 0; i < N; i++ {
		pos := i * mm
		tw1, tw2 := 0, 0
		for k := 0; k < m; k++ {
			var scratch [5]kissFFTCpx
			scratch[1] = cMul(fout[pos+m], st.twiddles[tw1])
			scratch[2] = cMul(fout[pos+m2], st.twiddles[tw2])

			scratch[3] = cAdd(scratch[1], scratch[2])
			scratch[0] = cSub(scratch[1], scratch[2])
			tw1 += fstride
			tw2 += fstride * 2

			// HALF_OF(x) = x>>1.
			fout[pos+m].r = fixedmath.SUB32_ovflw(fout[pos].r, scratch[3].r>>1)
			fout[pos+m].i = fixedmath.SUB32_ovflw(fout[pos].i, scratch[3].i>>1)

			scratch[0] = cMulByScalar(scratch[0], epi3i)

			fout[pos] = cAdd(fout[pos], scratch[3])

			fout[pos+m2].r = fixedmath.ADD32_ovflw(fout[pos+m].r, scratch[0].i)
			fout[pos+m2].i = fixedmath.SUB32_ovflw(fout[pos+m].i, scratch[0].r)

			fout[pos+m].r = fixedmath.SUB32_ovflw(fout[pos+m].r, scratch[0].i)
			fout[pos+m].i = fixedmath.ADD32_ovflw(fout[pos+m].i, scratch[0].r)
			pos++
		}
	}
}

// kfBfly3Cint vectorizes the two C_MULs plus the add/sub/scale via cint, then
// finishes each block with the scalar cross-lane combine. The inner k-loop
// touches disjoint fout indices per k (pos+k, pos+m+k, pos+m2+k), and the two
// per-k C_MUL inputs (the m- and m2-groups) are read before any combine writes
// them, so a whole block splits cleanly into a vectorized phase and a scalar
// cross-lane combine. The three groups are the contiguous runs fout[pos:pos+3m].
func kfBfly3Cint(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	m2 := 2 * m
	epi3i := int16(-fixedmath.QCONST32(0.86602540, coefShift-1))

	tw1p := packedTwiddles(st.twiddles, fstride, m)   // twiddles[j*fstride]
	tw2p := packedTwiddles(st.twiddles, fstride*2, m) // twiddles[j*2*fstride]

	var s1a, s2a, s3a, s0a [maxFFTRun]kissFFTCpx
	s1i := cpxAsInt32(s1a[:m])
	s2i := cpxAsInt32(s2a[:m])
	s3 := s3a[:m]
	s0 := s0a[:m]
	s3i := cpxAsInt32(s3)
	s0i := cpxAsInt32(s0)

	for i := 0; i < N; i++ {
		pos := i * mm
		foutM := cpxAsInt32(fout[pos+m : pos+m+m])    // scratch[1] source
		foutM2 := cpxAsInt32(fout[pos+m2 : pos+m2+m]) // scratch[2] source

		cint.Mul(s1i, foutM, tw1p)  // scratch[1] = C_MUL(fout[pos+m], tw1)
		cint.Mul(s2i, foutM2, tw2p) // scratch[2] = C_MUL(fout[pos+m2], tw2)
		cint.Add(s3i, s1i, s2i)     // scratch[3] = scratch[1] + scratch[2]
		cint.Sub(s0i, s1i, s2i)     // scratch[0] = scratch[1] - scratch[2]
		cint.MulByScalar(s0i, epi3i)

		for k := 0; k < m; k++ {
			p := pos + k
			// HALF_OF(x) = x>>1 (arithmetic). Computed from the pre-add fout[pos].
			fmr := fixedmath.SUB32_ovflw(fout[p].r, s3[k].r>>1)
			fmi := fixedmath.SUB32_ovflw(fout[p].i, s3[k].i>>1)

			fout[p] = cAdd(fout[p], s3[k])

			fout[pos+m2+k].r = fixedmath.ADD32_ovflw(fmr, s0[k].i)
			fout[pos+m2+k].i = fixedmath.SUB32_ovflw(fmi, s0[k].r)
			fout[pos+m+k].r = fixedmath.SUB32_ovflw(fmr, s0[k].i)
			fout[pos+m+k].i = fixedmath.ADD32_ovflw(fmi, s0[k].r)
		}
	}
}

// kf_bfly5 is the radix-5 butterfly. ya and yb are the fixed constants derived
// from the fifth roots of unity. (celt/kiss_fft.c:239)
//
// radix-5 vectorizes only its four C_MULs; the large ya/yb combine stays scalar
// and reads the results back from scratch, so it needs a longer run than radix-3
// to pay back and crosses over near m=48 (-11% at m=48, +3.5% at m=24). See
// BenchmarkBfly5_*.
func kfBfly5(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	if m < minCintBfly5 {
		kfBfly5Scalar(fout, fstride, st, m, N, mm)
		return
	}
	kfBfly5Cint(fout, fstride, st, m, N, mm)
}

// bfly5Consts returns the fifth-root-of-unity twiddle constants ya, yb shared by
// the scalar and cint radix-5 cores.
func bfly5Consts() (ya, yb kissTwiddleCpx) {
	ya = kissTwiddleCpx{
		r: int16(fixedmath.QCONST32(0.30901699, coefShift-1)),
		i: int16(-fixedmath.QCONST32(0.95105652, coefShift-1)),
	}
	yb = kissTwiddleCpx{
		r: int16(-fixedmath.QCONST32(0.80901699, coefShift-1)),
		i: int16(-fixedmath.QCONST32(0.58778525, coefShift-1)),
	}
	return ya, yb
}

func kfBfly5Scalar(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	ya, yb := bfly5Consts()
	tw := st.twiddles
	for i := 0; i < N; i++ {
		f0 := i * mm
		f1 := f0 + m
		f2 := f0 + 2*m
		f3 := f0 + 3*m
		f4 := f0 + 4*m
		for u := 0; u < m; u++ {
			var scratch [13]kissFFTCpx
			scratch[0] = fout[f0]

			scratch[1] = cMul(fout[f1], tw[u*fstride])
			scratch[2] = cMul(fout[f2], tw[2*u*fstride])
			scratch[3] = cMul(fout[f3], tw[3*u*fstride])
			scratch[4] = cMul(fout[f4], tw[4*u*fstride])

			scratch[7] = cAdd(scratch[1], scratch[4])
			scratch[10] = cSub(scratch[1], scratch[4])
			scratch[8] = cAdd(scratch[2], scratch[3])
			scratch[9] = cSub(scratch[2], scratch[3])

			fout[f0].r = fixedmath.ADD32_ovflw(fout[f0].r, fixedmath.ADD32_ovflw(scratch[7].r, scratch[8].r))
			fout[f0].i = fixedmath.ADD32_ovflw(fout[f0].i, fixedmath.ADD32_ovflw(scratch[7].i, scratch[8].i))

			scratch[5].r = fixedmath.ADD32_ovflw(scratch[0].r, fixedmath.ADD32_ovflw(sMul(scratch[7].r, ya.r), sMul(scratch[8].r, yb.r)))
			scratch[5].i = fixedmath.ADD32_ovflw(scratch[0].i, fixedmath.ADD32_ovflw(sMul(scratch[7].i, ya.r), sMul(scratch[8].i, yb.r)))

			scratch[6].r = fixedmath.ADD32_ovflw(sMul(scratch[10].i, ya.i), sMul(scratch[9].i, yb.i))
			scratch[6].i = fixedmath.NEG32_ovflw(fixedmath.ADD32_ovflw(sMul(scratch[10].r, ya.i), sMul(scratch[9].r, yb.i)))

			fout[f1] = cSub(scratch[5], scratch[6])
			fout[f4] = cAdd(scratch[5], scratch[6])

			scratch[11].r = fixedmath.ADD32_ovflw(scratch[0].r, fixedmath.ADD32_ovflw(sMul(scratch[7].r, yb.r), sMul(scratch[8].r, ya.r)))
			scratch[11].i = fixedmath.ADD32_ovflw(scratch[0].i, fixedmath.ADD32_ovflw(sMul(scratch[7].i, yb.r), sMul(scratch[8].i, ya.r)))
			scratch[12].r = fixedmath.SUB32_ovflw(sMul(scratch[9].i, ya.i), sMul(scratch[10].i, yb.i))
			scratch[12].i = fixedmath.SUB32_ovflw(sMul(scratch[10].r, yb.i), sMul(scratch[9].r, ya.i))

			fout[f2] = cAdd(scratch[11], scratch[12])
			fout[f3] = cSub(scratch[11], scratch[12])

			f0++
			f1++
			f2++
			f3++
			f4++
		}
	}
}

// kfBfly5Cint vectorizes the four C_MULs (scratch[1..4]) over the contiguous
// m-length groups via precomputed contiguous twiddle runs. The ya/yb combine
// mixes real and imag lanes with per-lane constants, which cint does not express,
// so it stays a scalar pass reading the C_MUL results from scratch. The four
// C_MUL inputs (groups f1..f4) are fully read before the combine overwrites any
// group, so there is no read-after-write hazard.
func kfBfly5Cint(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	ya, yb := bfly5Consts()
	tw := st.twiddles

	tw1p := packedTwiddles(tw, fstride, m)
	tw2p := packedTwiddles(tw, 2*fstride, m)
	tw3p := packedTwiddles(tw, 3*fstride, m)
	tw4p := packedTwiddles(tw, 4*fstride, m)

	var s1a, s2a, s3a, s4a [maxFFTRun]kissFFTCpx
	s1 := s1a[:m]
	s2 := s2a[:m]
	s3 := s3a[:m]
	s4 := s4a[:m]
	s1i := cpxAsInt32(s1)
	s2i := cpxAsInt32(s2)
	s3i := cpxAsInt32(s3)
	s4i := cpxAsInt32(s4)

	for i := 0; i < N; i++ {
		f0 := i * mm
		f1 := f0 + m
		f2 := f0 + 2*m
		f3 := f0 + 3*m
		f4 := f0 + 4*m

		cint.Mul(s1i, cpxAsInt32(fout[f1:f1+m]), tw1p)
		cint.Mul(s2i, cpxAsInt32(fout[f2:f2+m]), tw2p)
		cint.Mul(s3i, cpxAsInt32(fout[f3:f3+m]), tw3p)
		cint.Mul(s4i, cpxAsInt32(fout[f4:f4+m]), tw4p)

		for u := 0; u < m; u++ {
			var scratch [13]kissFFTCpx
			scratch[0] = fout[f0+u]
			scratch[1] = s1[u]
			scratch[2] = s2[u]
			scratch[3] = s3[u]
			scratch[4] = s4[u]

			scratch[7] = cAdd(scratch[1], scratch[4])
			scratch[10] = cSub(scratch[1], scratch[4])
			scratch[8] = cAdd(scratch[2], scratch[3])
			scratch[9] = cSub(scratch[2], scratch[3])

			fout[f0+u].r = fixedmath.ADD32_ovflw(scratch[0].r, fixedmath.ADD32_ovflw(scratch[7].r, scratch[8].r))
			fout[f0+u].i = fixedmath.ADD32_ovflw(scratch[0].i, fixedmath.ADD32_ovflw(scratch[7].i, scratch[8].i))

			scratch[5].r = fixedmath.ADD32_ovflw(scratch[0].r, fixedmath.ADD32_ovflw(sMul(scratch[7].r, ya.r), sMul(scratch[8].r, yb.r)))
			scratch[5].i = fixedmath.ADD32_ovflw(scratch[0].i, fixedmath.ADD32_ovflw(sMul(scratch[7].i, ya.r), sMul(scratch[8].i, yb.r)))

			scratch[6].r = fixedmath.ADD32_ovflw(sMul(scratch[10].i, ya.i), sMul(scratch[9].i, yb.i))
			scratch[6].i = fixedmath.NEG32_ovflw(fixedmath.ADD32_ovflw(sMul(scratch[10].r, ya.i), sMul(scratch[9].r, yb.i)))

			fout[f1+u] = cSub(scratch[5], scratch[6])
			fout[f4+u] = cAdd(scratch[5], scratch[6])

			scratch[11].r = fixedmath.ADD32_ovflw(scratch[0].r, fixedmath.ADD32_ovflw(sMul(scratch[7].r, yb.r), sMul(scratch[8].r, ya.r)))
			scratch[11].i = fixedmath.ADD32_ovflw(scratch[0].i, fixedmath.ADD32_ovflw(sMul(scratch[7].i, yb.r), sMul(scratch[8].i, ya.r)))
			scratch[12].r = fixedmath.SUB32_ovflw(sMul(scratch[9].i, ya.i), sMul(scratch[10].i, yb.i))
			scratch[12].i = fixedmath.SUB32_ovflw(sMul(scratch[10].r, yb.i), sMul(scratch[9].r, ya.i))

			fout[f2+u] = cAdd(scratch[11], scratch[12])
			fout[f3+u] = cSub(scratch[11], scratch[12])
		}
	}
}

// opus_fft_impl is the shared in-place FFT engine used by both the forward and
// inverse transforms (direction is handled by the caller via twiddle
// conjugation and bitrev). downshift is the fixed-point scaling budget spent by
// fft_downshift across the stages. (celt/kiss_fft.c:562)
func opusFFTImpl(st *kissFFTState, fout []kissFFTCpx, downshift int) {
	var fstride [maxFactors]int
	// st.shift can be -1.
	shift := 0
	if st.shift > 0 {
		shift = st.shift
	}

	fstride[0] = 1
	L := 0
	var m, m2 int
	for {
		p := int(st.factors[2*L])
		m = int(st.factors[2*L+1])
		fstride[L+1] = fstride[L] * p
		L++
		if m == 1 {
			break
		}
	}
	m = int(st.factors[2*L-1])
	for i := L - 1; i >= 0; i-- {
		if i != 0 {
			m2 = int(st.factors[2*i-1])
		} else {
			m2 = 1
		}
		switch st.factors[2*i] {
		case 2:
			fftDownshift(fout, st.nfft, &downshift, 1)
			kfBfly2(fout, m, fstride[i])
		case 4:
			fftDownshift(fout, st.nfft, &downshift, 2)
			kfBfly4(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		case 3:
			fftDownshift(fout, st.nfft, &downshift, 2)
			kfBfly3(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		case 5:
			fftDownshift(fout, st.nfft, &downshift, 3)
			kfBfly5(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		}
		m = m2
	}
	fftDownshift(fout, st.nfft, &downshift, downshift)
}

// opus_ifft is the inverse complex FFT used by the CELT decoder path. It
// bit-reverses the input, conjugates, runs the shared engine with no extra
// downscaling (downshift 0), then conjugates back. fin and fout must not alias.
// (celt/kiss_fft.c:638)
func opusIFFT(st *kissFFTState, fin, fout []kissFFTCpx) {
	// Bit-reverse the input.
	for i := 0; i < st.nfft; i++ {
		fout[st.bitrev[i]] = fin[i]
	}
	for i := 0; i < st.nfft; i++ {
		fout[i].i = -fout[i].i
	}
	opusFFTImpl(st, fout, 0)
	for i := 0; i < st.nfft; i++ {
		fout[i].i = -fout[i].i
	}
}

// opus_fft is the forward complex FFT. It bit-reverses and pre-scales the input
// by st.scale (S_MUL2), then runs the shared engine with the scale_shift-1
// budget. fin and fout must not alias. (celt/kiss_fft.c:615)
func opusFFT(st *kissFFTState, fin, fout []kissFFTCpx) {
	scale := st.scale
	scaleShift := st.scaleShift - 1
	// Bit-reverse the input.
	for i := 0; i < st.nfft; i++ {
		x := fin[i]
		fout[st.bitrev[i]].r = sMul2(x.r, scale)
		fout[st.bitrev[i]].i = sMul2(x.i, scale)
	}
	opusFFTImpl(st, fout, scaleShift)
}

// InverseFFT drives opus_ifft on mode48000_960's kfft[idx] (idx 0..3, with nfft
// 480/240/120/60) over the complex input inR/inI (each length nfft) and returns
// the complex output (each length nfft). It is exported only so the refc cgo
// differential harness (internal/reftest/oracle) can drive the pure-Go FFT
// against libopus; it is not part of the decoder API.
func InverseFFT(idx int, inR, inI []int32) (outR, outI []int32) {
	st := mode48000_960.mdct.kfft[idx]
	fin := make([]kissFFTCpx, st.nfft)
	fout := make([]kissFFTCpx, st.nfft)
	for i := 0; i < st.nfft; i++ {
		fin[i] = kissFFTCpx{inR[i], inI[i]}
	}
	opusIFFT(st, fin, fout)
	outR = make([]int32, st.nfft)
	outI = make([]int32, st.nfft)
	for i := 0; i < st.nfft; i++ {
		outR[i] = fout[i].r
		outI[i] = fout[i].i
	}
	return outR, outI
}

// ForwardFFT drives opus_fft on mode48000_960's kfft[idx] over the complex input
// inR/inI (each length nfft) and returns the complex output. Exported only for
// the refc cgo differential harness; not part of the decoder API.
func ForwardFFT(idx int, inR, inI []int32) (outR, outI []int32) {
	st := mode48000_960.mdct.kfft[idx]
	fin := make([]kissFFTCpx, st.nfft)
	fout := make([]kissFFTCpx, st.nfft)
	for i := 0; i < st.nfft; i++ {
		fin[i] = kissFFTCpx{inR[i], inI[i]}
	}
	opusFFT(st, fin, fout)
	outR = make([]int32, st.nfft)
	outI = make([]int32, st.nfft)
	for i := 0; i < st.nfft; i++ {
		outR[i] = fout[i].r
		outI[i] = fout[i].i
	}
	return outR, outI
}

// FFTStateNFFT returns the FFT length of mode48000_960's kfft[idx] (480/240/
// 120/60 for idx 0..3). Exported for the refc differential harness.
func FFTStateNFFT(idx int) int { return mode48000_960.mdct.kfft[idx].nfft }

// FFTImplWithDownshift runs opus_fft_impl directly on mode48000_960's kfft[idx]
// with an explicit downshift budget over the given complex buffer (already in
// working order, not bit-reversed). It exists so the refc differential harness
// can drive the per-stage fixed-point downshift path (docs/hard-parts.md section
// 8) directly against libopus; it is not part of the decoder API.
func FFTImplWithDownshift(idx int, inR, inI []int32, downshift int) (outR, outI []int32) {
	st := mode48000_960.mdct.kfft[idx]
	buf := make([]kissFFTCpx, st.nfft)
	for i := 0; i < st.nfft; i++ {
		buf[i] = kissFFTCpx{inR[i], inI[i]}
	}
	opusFFTImpl(st, buf, downshift)
	outR = make([]int32, st.nfft)
	outI = make([]int32, st.nfft)
	for i := 0; i < st.nfft; i++ {
		outR[i] = buf[i].r
		outI[i] = buf[i].i
	}
	return outR, outI
}
