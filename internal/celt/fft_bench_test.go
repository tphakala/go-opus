package celt

import (
	"math/rand/v2"
	"testing"
)

// fillCpx populates a kissFFTCpx slice with pseudo-random full-scale values.
func fillCpx(dst []kissFFTCpx, r *rand.Rand) {
	for i := range dst {
		dst[i] = kissFFTCpx{int32(r.Uint32()), int32(r.Uint32())}
	}
}

// benchFFTImpl runs opus_fft_impl in place; the working buffer is reset from a
// golden copy each iteration (the transform is destructive). The reset copy is
// small relative to the transform and is identical before/after any change, so
// it does not bias the comparison.
func benchFFTImpl(b *testing.B, idx, downshift int) {
	b.Helper()
	st := mode48000_960.mdct.kfft[idx]
	r := rand.New(rand.NewPCG(0x1234+uint64(idx), 0x9e3779b97f4a7c15))
	golden := make([]kissFFTCpx, st.nfft)
	fillCpx(golden, r)
	buf := make([]kissFFTCpx, st.nfft)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf, golden)
		opusFFTImpl(st, buf, downshift)
	}
}

func BenchmarkFFTImpl480(b *testing.B) { benchFFTImpl(b, 0, 6) }
func BenchmarkFFTImpl240(b *testing.B) { benchFFTImpl(b, 1, 6) }
func BenchmarkFFTImpl120(b *testing.B) { benchFFTImpl(b, 2, 6) }
func BenchmarkFFTImpl60(b *testing.B)  { benchFFTImpl(b, 3, 6) }

// benchFFTImplRef is the scalar-reference counterpart of benchFFTImpl, used to
// benchstat the cint production path against the frozen scalar path in the same
// binary under identical conditions.
func benchFFTImplRef(b *testing.B, idx, downshift int) {
	b.Helper()
	st := mode48000_960.mdct.kfft[idx]
	r := rand.New(rand.NewPCG(0x1234+uint64(idx), 0x9e3779b97f4a7c15))
	golden := make([]kissFFTCpx, st.nfft)
	fillCpx(golden, r)
	buf := make([]kissFFTCpx, st.nfft)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf, golden)
		opusFFTImplRef(st, buf, downshift)
	}
}

func BenchmarkFFTImplRef480(b *testing.B) { benchFFTImplRef(b, 0, 6) }
func BenchmarkFFTImplRef240(b *testing.B) { benchFFTImplRef(b, 1, 6) }
func BenchmarkFFTImplRef120(b *testing.B) { benchFFTImplRef(b, 2, 6) }
func BenchmarkFFTImplRef60(b *testing.B)  { benchFFTImplRef(b, 3, 6) }

// benchBfly3 isolates the radix-3 butterfly (cint or scalar) on a fixed buffer,
// reset each iteration. params match a real stage: idx picks (m, N, mm, fstride).
func benchBfly3(b *testing.B, useCint bool, fstride, m, N, mm int) {
	b.Helper()
	st := mode48000_960.mdct.kfft[0]
	n := N*mm + 3*m
	r := rand.New(rand.NewPCG(0x777, 0x9e3779b97f4a7c15))
	golden := make([]kissFFTCpx, n)
	fillCpx(golden, r)
	buf := make([]kissFFTCpx, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf, golden)
		if useCint {
			kfBfly3Cint(buf, fstride, st, m, N, mm)
		} else {
			kfBfly3Scalar(buf, fstride, st, m, N, mm)
		}
	}
}

// idx0 radix-3 stage: fstride=5, m=32, N=5, mm=96 (large run).
func BenchmarkBfly3_m32_cint(b *testing.B) { benchBfly3(b, true, 5, 32, 5, 96) }
func BenchmarkBfly3_m32_ref(b *testing.B)  { benchBfly3(b, false, 5, 32, 5, 96) }

// idx1 radix-3 stage: fstride=5<<1=10, m=16, N=5, mm=48.
func BenchmarkBfly3_m16_cint(b *testing.B) { benchBfly3(b, true, 10, 16, 5, 48) }
func BenchmarkBfly3_m16_ref(b *testing.B)  { benchBfly3(b, false, 10, 16, 5, 48) }

// idx2 radix-3 stage: fstride=5<<2=20, m=8, N=5, mm=24.
func BenchmarkBfly3_m8_cint(b *testing.B) { benchBfly3(b, true, 20, 8, 5, 24) }
func BenchmarkBfly3_m8_ref(b *testing.B)  { benchBfly3(b, false, 20, 8, 5, 24) }

// idx3 radix-3 stage: fstride=5<<3=40, m=4, N=5, mm=12 (small run).
func BenchmarkBfly3_m4_cint(b *testing.B) { benchBfly3(b, true, 40, 4, 5, 12) }
func BenchmarkBfly3_m4_ref(b *testing.B)  { benchBfly3(b, false, 40, 4, 5, 12) }

// benchBfly5 isolates the radix-5 butterfly (cint or scalar). All real radix-5
// stages have N=1, mm=1; the run length is m and the twiddle stride is fstride.
func benchBfly5(b *testing.B, useCint bool, fstride, m int) {
	b.Helper()
	st := mode48000_960.mdct.kfft[0]
	n := 5 * m
	r := rand.New(rand.NewPCG(0x999, 0x9e3779b97f4a7c15))
	golden := make([]kissFFTCpx, n)
	fillCpx(golden, r)
	buf := make([]kissFFTCpx, n)
	// Call the cores directly so each arm measures the intended path regardless
	// of the production m-gate.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf, golden)
		if useCint {
			kfBfly5Cint(buf, fstride, st, m, 1, 1)
		} else {
			kfBfly5Scalar(buf, fstride, st, m, 1, 1)
		}
	}
}

func BenchmarkBfly5_m96_cint(b *testing.B) { benchBfly5(b, true, 1, 96) }
func BenchmarkBfly5_m96_ref(b *testing.B)  { benchBfly5(b, false, 1, 96) }
func BenchmarkBfly5_m48_cint(b *testing.B) { benchBfly5(b, true, 2, 48) }
func BenchmarkBfly5_m48_ref(b *testing.B)  { benchBfly5(b, false, 2, 48) }
func BenchmarkBfly5_m24_cint(b *testing.B) { benchBfly5(b, true, 4, 24) }
func BenchmarkBfly5_m24_ref(b *testing.B)  { benchBfly5(b, false, 4, 24) }
func BenchmarkBfly5_m12_cint(b *testing.B) { benchBfly5(b, true, 8, 12) }
func BenchmarkBfly5_m12_ref(b *testing.B)  { benchBfly5(b, false, 8, 12) }

// benchInverseMDCT runs the full backward MDCT hot path (pre-rotate, FFT,
// post-rotate, TDAC) for the given LM.
func benchInverseMDCT(b *testing.B, lm int) {
	b.Helper()
	m := &mode48000_960
	shift := m.maxLM - lm
	overlap := m.overlap
	N := m.mdct.n >> shift
	N2 := N >> 1
	r := rand.New(rand.NewPCG(0x55+uint64(lm), 0x9e3779b97f4a7c15))
	in := make([]int32, N2)
	for i := range in {
		// Constrain to the decoder's operational range (|x| <= SIG_SAT).
		in[i] = int32(r.IntN(2*536870911+1) - 536870911)
	}
	out := make([]int32, overlap/2+N2)
	var sc scratch
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cltMDCTBackward(&m.mdct, in, out, m.window, overlap, shift, 1, &sc)
	}
}

func BenchmarkInverseMDCT_LM3(b *testing.B) { benchInverseMDCT(b, 3) }
func BenchmarkInverseMDCT_LM2(b *testing.B) { benchInverseMDCT(b, 2) }
func BenchmarkInverseMDCT_LM1(b *testing.B) { benchInverseMDCT(b, 1) }
func BenchmarkInverseMDCT_LM0(b *testing.B) { benchInverseMDCT(b, 0) }
