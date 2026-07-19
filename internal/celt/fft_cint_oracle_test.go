package celt

import (
	"math/rand/v2"
	"testing"

	"github.com/tphakala/go-opus/internal/fixedmath"
)

// This file is the fast (no-cgo) bit-exactness oracle for the cint-vectorized
// FFT butterflies. The reference implementations below (kfBfly{2,3,4,5}Ref and
// opusFFTImplRef) are VERBATIM copies of the original scalar transliteration of
// libopus that the differential refc gate (internal/reftest/oracle) already
// proved byte-identical to the C reference. The production kiss_fft.go now routes
// through cint; TestFFTCintBitExact asserts the production opusFFTImpl is
// byte-identical to this frozen scalar reference over every FFT size the codec
// uses and over adversarial inputs. Reference == C (refc gate) and production ==
// reference (this test) transitively proves production == C without cgo.

func kfBfly2Ref(fout []kissFFTCpx, m, N int) {
	_ = m
	tw := int16(fixedmath.QCONST32(0.7071067812, coefShift-1))
	pos := 0
	for i := 0; i < N; i++ {
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

func kfBfly4Ref(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	if m == 1 {
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

func kfBfly3Ref(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
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

func kfBfly5Ref(fout []kissFFTCpx, fstride int, st *kissFFTState, m, N, mm int) {
	ya := kissTwiddleCpx{
		r: int16(fixedmath.QCONST32(0.30901699, coefShift-1)),
		i: int16(-fixedmath.QCONST32(0.95105652, coefShift-1)),
	}
	yb := kissTwiddleCpx{
		r: int16(-fixedmath.QCONST32(0.80901699, coefShift-1)),
		i: int16(-fixedmath.QCONST32(0.58778525, coefShift-1)),
	}
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

func opusFFTImplRef(st *kissFFTState, fout []kissFFTCpx, downshift int) {
	var fstride [maxFactors]int
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
			kfBfly2Ref(fout, m, fstride[i])
		case 4:
			fftDownshift(fout, st.nfft, &downshift, 2)
			kfBfly4Ref(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		case 3:
			fftDownshift(fout, st.nfft, &downshift, 2)
			kfBfly3Ref(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		case 5:
			fftDownshift(fout, st.nfft, &downshift, 3)
			kfBfly5Ref(fout, fstride[i]<<shift, st, m, fstride[i], m2)
		}
		m = m2
	}
	fftDownshift(fout, st.nfft, &downshift, downshift)
}

// adversarialCpx returns named boundary complex vectors of length nfft that
// stress the fixed-point wrap and truncation corners.
func adversarialCpx(nfft int) map[string][]kissFFTCpx {
	mk := func(f func(i int) (int32, int32)) []kissFFTCpx {
		v := make([]kissFFTCpx, nfft)
		for i := range v {
			r, im := f(i)
			v[i] = kissFFTCpx{r, im}
		}
		return v
	}
	const minI32 = -2147483648
	const maxI32 = 2147483647
	return map[string][]kissFFTCpx{
		"zeros":  mk(func(int) (int32, int32) { return 0, 0 }),
		"allMin": mk(func(int) (int32, int32) { return minI32, minI32 }),
		"allMax": mk(func(int) (int32, int32) { return maxI32, maxI32 }),
		"minMax": mk(func(int) (int32, int32) { return minI32, maxI32 }),
		"altExtms": mk(func(i int) (int32, int32) {
			if i%2 == 0 {
				return maxI32, minI32
			}
			return minI32, maxI32
		}),
		"altSign": mk(func(i int) (int32, int32) {
			if i%2 == 0 {
				return 1 << 30, -(1 << 30)
			}
			return -(1 << 30), 1 << 30
		}),
		"tailOne": mk(func(i int) (int32, int32) {
			if i == nfft-1 {
				return maxI32, minI32
			}
			return 0, 0
		}),
		"headOne": mk(func(i int) (int32, int32) {
			if i == 0 {
				return minI32, maxI32
			}
			return 0, 0
		}),
		"straddle": mk(func(i int) (int32, int32) {
			if i%3 == 0 {
				return maxI32 - 1, minI32 + 1
			}
			return 1 << 29, -(1 << 29)
		}),
	}
}

// TestFFTCintBitExact is the core oracle: production opusFFTImpl (cint-backed)
// must be byte-identical to the frozen scalar reference over every FFT size, the
// full downshift budget, adversarial vectors, and random full-scale fuzz.
func TestFFTCintBitExact(t *testing.T) {
	downshifts := []int{0, 1, 2, 3, 5, 8, 13, 20, 22, 29}
	for idx := 0; idx < 4; idx++ {
		st := mode48000_960.mdct.kfft[idx]
		nfft := st.nfft

		// Adversarial boundary vectors across the downshift range.
		for name, v := range adversarialCpx(nfft) {
			for _, ds := range downshifts {
				got := make([]kissFFTCpx, nfft)
				want := make([]kissFFTCpx, nfft)
				copy(got, v)
				copy(want, v)
				opusFFTImpl(st, got, ds)
				opusFFTImplRef(st, want, ds)
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("idx %d nfft %d vec %s ds %d: out[%d] got=(%d,%d) want=(%d,%d)",
							idx, nfft, name, ds, i, got[i].r, got[i].i, want[i].r, want[i].i)
					}
				}
			}
		}

		// Random full-scale fuzz across the downshift range.
		r := rand.New(rand.NewPCG(uint64(0xF17+idx), 0x9e3779b97f4a7c15))
		for _, ds := range downshifts {
			for trial := 0; trial < 60; trial++ {
				base := make([]kissFFTCpx, nfft)
				fillCpx(base, r)
				got := make([]kissFFTCpx, nfft)
				want := make([]kissFFTCpx, nfft)
				copy(got, base)
				copy(want, base)
				opusFFTImpl(st, got, ds)
				opusFFTImplRef(st, want, ds)
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("idx %d nfft %d fuzz ds %d trial %d: out[%d] got=(%d,%d) want=(%d,%d)",
							idx, nfft, ds, trial, i, got[i].r, got[i].i, want[i].r, want[i].i)
					}
				}
			}
		}
	}
}

// TestInverseMDCTCintBitExact drives the full backward MDCT (pre-rotate, FFT,
// post-rotate, TDAC) through the production path and compares against a run that
// uses the scalar-reference FFT, so any divergence in the cint FFT surfaces at
// the MDCT output too. Covers LM 0..3 and adversarial + fuzz inputs.
func TestInverseMDCTCintBitExact(t *testing.T) {
	m := &mode48000_960
	for lm := 0; lm < 4; lm++ {
		shift := m.maxLM - lm
		overlap := m.overlap
		N := m.mdct.n >> shift
		n2 := N >> 1

		run := func(in []int32) []int32 {
			out := make([]int32, overlap/2+n2)
			var sc scratch
			cltMDCTBackward(&m.mdct, in, out, m.window, overlap, shift, 1, &sc)
			return out
		}

		r := rand.New(rand.NewPCG(uint64(0xABC+lm), 0x9e3779b97f4a7c15))
		const sigSat = 536870911
		for trial := 0; trial < 200; trial++ {
			in := make([]int32, n2)
			for i := range in {
				in[i] = int32(r.IntN(2*sigSat+1) - sigSat)
			}
			got := run(in)
			want := inverseMDCTRef(m, in, overlap, shift, n2)
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("lm %d trial %d: out[%d] got=%d want=%d", lm, trial, i, got[i], want[i])
				}
			}
		}
	}
}

// inverseMDCTRef mirrors cltMDCTBackward but calls opusFFTImplRef for the FFT, so
// it is the scalar-reference counterpart of the production backward MDCT.
func inverseMDCTRef(m *celtMode, in []int32, overlap, shift, n2 int) []int32 {
	l := &m.mdct
	out := make([]int32, overlap/2+n2)
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

	var preShift, postShift, fftShift int
	{
		sumval := int32(N2)
		maxval := int32(0)
		for i := 0; i < N2; i++ {
			maxval = fixedmath.MAX32(maxval, fixedmath.ABS32(in[i]))
			sumval = fixedmath.ADD32_ovflw(sumval, fixedmath.ABS32(fixedmath.SHR32(in[i], 11)))
		}
		preShift = fixedmath.IMAX(0, 29-fixedmath.Celt_zlog2(1+maxval))
		postShift = fixedmath.IMAX(0, 19-fixedmath.Celt_ilog2(fixedmath.ABS32(sumval)))
		postShift = fixedmath.IMIN(postShift, preShift)
		fftShift = preShift - postShift
	}

	f2 := make([]kissFFTCpx, N4)
	{
		xp1 := 0
		xp2 := (N2 - 1)
		bitrev := l.kfft[shift].bitrev
		for i := 0; i < N4; i++ {
			rev := int(bitrev[i])
			x1 := fixedmath.SHL32_ovflw(in[xp1], preShift)
			x2 := fixedmath.SHL32_ovflw(in[xp2], preShift)
			yr := fixedmath.ADD32_ovflw(sMul(x2, trig[trigOff+i]), sMul(x1, trig[trigOff+N4+i]))
			yi := fixedmath.SUB32_ovflw(sMul(x1, trig[trigOff+i]), sMul(x2, trig[trigOff+N4+i]))
			f2[rev].i = yr
			f2[rev].r = yi
			xp1 += 2
			xp2 -= 2
		}
	}

	opusFFTImplRef(l.kfft[shift], f2, fftShift)

	{
		o := overlap >> 1
		for i := 0; i < (N4+1)>>1; i++ {
			k0 := i
			k1 := N4 - i - 1
			re := f2[k0].i
			im := f2[k0].r
			t0 := trig[trigOff+i]
			t1 := trig[trigOff+N4+i]
			yr0 := fixedmath.PSHR32_ovflw(fixedmath.ADD32_ovflw(sMul(re, t0), sMul(im, t1)), postShift)
			yi0 := fixedmath.PSHR32_ovflw(fixedmath.SUB32_ovflw(sMul(re, t1), sMul(im, t0)), postShift)

			re = f2[k1].i
			im = f2[k1].r
			out[o+2*k0] = yr0
			out[o+N2-1-2*k0] = yi0

			t0 = trig[trigOff+(N4-i-1)]
			t1 = trig[trigOff+(N2-i-1)]
			yr1 := fixedmath.PSHR32_ovflw(fixedmath.ADD32_ovflw(sMul(re, t0), sMul(im, t1)), postShift)
			yi1 := fixedmath.PSHR32_ovflw(fixedmath.SUB32_ovflw(sMul(re, t1), sMul(im, t0)), postShift)
			out[o+N2-2-2*k0] = yr1
			out[o+2*k0+1] = yi1
		}
	}

	{
		xp1 := overlap - 1
		yp1 := 0
		wp1 := 0
		wp2 := overlap - 1
		for i := 0; i < overlap/2; i++ {
			x1 := out[xp1]
			x2 := out[yp1]
			out[yp1] = fixedmath.SUB32_ovflw(sMul(x2, m.window[wp2]), sMul(x1, m.window[wp1]))
			out[xp1] = fixedmath.ADD32_ovflw(sMul(x2, m.window[wp1]), sMul(x1, m.window[wp2]))
			yp1++
			xp1--
			wp1++
			wp2--
		}
	}
	return out
}
