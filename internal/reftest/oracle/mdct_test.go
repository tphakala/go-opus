//go:build refc

package oracle

import (
	"math/rand/v2"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This is the phase-2 gate for the CELT inverse transforms: the pure-Go
// internal/celt inverse FFT (opus_ifft) and inverse MDCT (clt_mdct_backward),
// ported in kiss_fft.go and mdct.go, are driven over the SAME frozen static
// mode48000_960 that the C oracle uses (opus_custom_mode_create(48000,960)
// returns the static mode under non-CUSTOM_MODES). Every output value must be
// bit-identical between the two implementations, across the four FFT lengths
// (480/240/120/60) and LM = 0..3 (frame sizes 120/240/480/960), including the
// per-stage fixed-point downshift path exercised by large-headroom (small
// magnitude) inputs. See docs/hard-parts.md section 8.

// scaleBits sweeps input magnitudes from tiny (huge FFT headroom, exercising the
// per-stage fft_downshift) up to near full-scale (no downshift).
var scaleBits = []int{2, 6, 10, 14, 18, 22, 26, 30}

// randScaled returns a random int32 in [-(1<<bits), (1<<bits)].
func randScaled(r *rand.Rand, bits int) int32 {
	span := int64(1) << uint(bits)
	return int32(r.Int64N(2*span+1) - span)
}

func newSeededRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
}

// --- Inverse FFT -----------------------------------------------------------

func TestInverseFFTBitExact(t *testing.T) {
	for idx := 0; idx < 4; idx++ {
		nfft := celt.FFTStateNFFT(idx)
		if got := cFFTNFFT(idx); got != nfft {
			t.Fatalf("idx %d: Go nfft %d != C nfft %d", idx, nfft, got)
		}
		r := newSeededRand(uint64(0x1f1 + idx))
		for _, bits := range scaleBits {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				inR := make([]int32, nfft)
				inI := make([]int32, nfft)
				for i := 0; i < nfft; i++ {
					inR[i] = randScaled(r, bits)
					inI[i] = randScaled(r, bits)
				}
				goR, goI := celt.InverseFFT(idx, inR, inI)
				cR, cI := cIFFT(idx, inR, inI)
				assertCpxEqual(t, "ifft", idx, bits, trial, goR, goI, cR, cI)
			}
		}
		// Boundary vectors.
		for name, v := range fftBoundaryVectors(nfft) {
			goR, goI := celt.InverseFFT(idx, v.re, v.im)
			cR, cI := cIFFT(idx, v.re, v.im)
			assertCpxEqualNamed(t, "ifft/"+name, idx, goR, goI, cR, cI)
		}
	}
}

func TestForwardFFTBitExact(t *testing.T) {
	for idx := 0; idx < 4; idx++ {
		nfft := celt.FFTStateNFFT(idx)
		r := newSeededRand(uint64(0x2f2 + idx))
		for _, bits := range scaleBits {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				inR := make([]int32, nfft)
				inI := make([]int32, nfft)
				for i := 0; i < nfft; i++ {
					inR[i] = randScaled(r, bits)
					inI[i] = randScaled(r, bits)
				}
				goR, goI := celt.ForwardFFT(idx, inR, inI)
				cR, cI := cFFT(idx, inR, inI)
				assertCpxEqual(t, "fft", idx, bits, trial, goR, goI, cR, cI)
			}
		}
		for name, v := range fftBoundaryVectors(nfft) {
			goR, goI := celt.ForwardFFT(idx, v.re, v.im)
			cR, cI := cFFT(idx, v.re, v.im)
			assertCpxEqualNamed(t, "fft/"+name, idx, goR, goI, cR, cI)
		}
	}
}

// TestFFTImplDownshiftBitExact drives opus_fft_impl directly across the whole
// downshift range (0..29) with full-scale random inputs. This is the focused
// gate on the per-stage fixed-point downshift mapping (fft_downshift), the trap
// flagged in docs/hard-parts.md section 8: the FFT-level differential test
// (opus_ifft) only ever runs with downshift 0, so the >0 budget the inverse
// MDCT spends inside the FFT is exercised here.
func TestFFTImplDownshiftBitExact(t *testing.T) {
	for idx := 0; idx < 4; idx++ {
		nfft := celt.FFTStateNFFT(idx)
		r := newSeededRand(uint64(0x4f4 + idx))
		for _, ds := range []int{0, 1, 2, 3, 5, 8, 13, 20, 22, 29} {
			const trials = 30
			for trial := 0; trial < trials; trial++ {
				inR := make([]int32, nfft)
				inI := make([]int32, nfft)
				for i := 0; i < nfft; i++ {
					inR[i] = randScaled(r, 31)
					inI[i] = randScaled(r, 31)
				}
				goR, goI := celt.FFTImplWithDownshift(idx, inR, inI, ds)
				cR, cI := cFFTImpl(idx, inR, inI, ds)
				for i := range goR {
					if goR[i] != cR[i] || goI[i] != cI[i] {
						t.Fatalf("fftimpl idx %d ds %d trial %d: out[%d] go=(%d,%d) c=(%d,%d)",
							idx, ds, trial, i, goR[i], goI[i], cR[i], cI[i])
					}
				}
			}
		}
	}
}

// --- Inverse MDCT ----------------------------------------------------------

func TestInverseMDCTBitExact(t *testing.T) {
	for lm := 0; lm < 4; lm++ {
		n2 := 120 << uint(lm) // input coefficient count = shortMdctSize<<LM
		r := newSeededRand(uint64(0x3f3 + lm))
		for _, bits := range scaleBits {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				in := make([]int32, n2)
				for i := 0; i < n2; i++ {
					in[i] = randScaled(r, bits)
				}
				goOut := celt.InverseMDCT(lm, in)
				cOut := cMDCTBackward(lm, in)
				assertRealEqual(t, "mdct", lm, bits, trial, goOut, cOut)
			}
		}
		// Boundary vectors: zeros, impulse, alternating, all-max, all-min.
		for name, in := range mdctBoundaryVectors(n2) {
			goOut := celt.InverseMDCT(lm, in)
			cOut := cMDCTBackward(lm, in)
			if len(goOut) != len(cOut) {
				t.Fatalf("mdct/%s lm %d: len mismatch go %d c %d", name, lm, len(goOut), len(cOut))
			}
			for i := range goOut {
				if goOut[i] != cOut[i] {
					t.Fatalf("mdct/%s lm %d: out[%d] go=%d c=%d", name, lm, i, goOut[i], cOut[i])
				}
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

type cpxVec struct {
	re, im []int32
}

func fftBoundaryVectors(nfft int) map[string]cpxVec {
	zeros := cpxVec{make([]int32, nfft), make([]int32, nfft)}
	ones := cpxVec{make([]int32, nfft), make([]int32, nfft)}
	alt := cpxVec{make([]int32, nfft), make([]int32, nfft)}
	extremes := cpxVec{make([]int32, nfft), make([]int32, nfft)}
	impulse := cpxVec{make([]int32, nfft), make([]int32, nfft)}
	impulse.re[0] = 1 << 20
	for i := 0; i < nfft; i++ {
		ones.re[i] = 1
		ones.im[i] = -1
		if i%2 == 0 {
			alt.re[i] = 1 << 24
			alt.im[i] = -(1 << 24)
		} else {
			alt.re[i] = -(1 << 24)
			alt.im[i] = 1 << 24
		}
		if i%2 == 0 {
			extremes.re[i] = 0x7fffffff
			extremes.im[i] = -0x80000000
		} else {
			extremes.re[i] = -0x80000000
			extremes.im[i] = 0x7fffffff
		}
	}
	return map[string]cpxVec{
		"zeros": zeros, "ones": ones, "alt": alt, "extremes": extremes, "impulse": impulse,
	}
}

// sigSat mirrors celt/arch.h SIG_SAT (2^29-1), the saturation limit the decoder
// applies to the denormalised MDCT input, so it is the true operational maximum
// magnitude the inverse MDCT ever sees.
const sigSat = 536870911

func mdctBoundaryVectors(n2 int) map[string][]int32 {
	zeros := make([]int32, n2)
	impulse := make([]int32, n2)
	impulse[0] = 1 << 20
	alt := make([]int32, n2)
	mx := make([]int32, n2)
	mn := make([]int32, n2)
	for i := 0; i < n2; i++ {
		if i%2 == 0 {
			alt[i] = 1 << 24
		} else {
			alt[i] = -(1 << 24)
		}
		// The headroom scan in clt_mdct_backward computes 1+maxval; a maxval of
		// INT_MAX (|in| == 2^31-1) overflows int32, which is signed-overflow UB
		// in libopus itself (GCC then folds pre/post_shift to nonsense). The
		// decoder never feeds such values: it saturates to +-SIG_SAT. Cap the
		// boundary vectors there, the true operational extreme.
		mx[i] = sigSat
		mn[i] = -sigSat
	}
	return map[string][]int32{
		"zeros": zeros, "impulse": impulse, "alt": alt, "max": mx, "min": mn,
	}
}

func assertCpxEqual(t *testing.T, tag string, idx, bits, trial int, goR, goI, cR, cI []int32) {
	t.Helper()
	for i := range goR {
		if goR[i] != cR[i] || goI[i] != cI[i] {
			t.Fatalf("%s idx %d bits %d trial %d: out[%d] go=(%d,%d) c=(%d,%d)",
				tag, idx, bits, trial, i, goR[i], goI[i], cR[i], cI[i])
		}
	}
}

func assertCpxEqualNamed(t *testing.T, tag string, idx int, goR, goI, cR, cI []int32) {
	t.Helper()
	for i := range goR {
		if goR[i] != cR[i] || goI[i] != cI[i] {
			t.Fatalf("%s idx %d: out[%d] go=(%d,%d) c=(%d,%d)",
				tag, idx, i, goR[i], goI[i], cR[i], cI[i])
		}
	}
}

func assertRealEqual(t *testing.T, tag string, lm, bits, trial int, goOut, cOut []int32) {
	t.Helper()
	if len(goOut) != len(cOut) {
		t.Fatalf("%s lm %d bits %d trial %d: len go %d != c %d", tag, lm, bits, trial, len(goOut), len(cOut))
	}
	for i := range goOut {
		if goOut[i] != cOut[i] {
			t.Fatalf("%s lm %d bits %d trial %d: out[%d] go=%d c=%d", tag, lm, bits, trial, i, goOut[i], cOut[i])
		}
	}
}
