//go:build refc

package oracle

import (
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This is the Checkpoint 3 gate for the CELT FORWARD transforms: the pure-Go
// internal/celt forward FFT (opus_fft) and forward MDCT (clt_mdct_forward),
// ported in kiss_fft.go and mdct.go, are driven over the SAME frozen static
// mode48000_960 that the C oracle uses (opus_custom_mode_create(48000,960)
// returns the static mode under non-CUSTOM_MODES). Every output value must be
// bit-identical between the two implementations, across the four FFT lengths
// (480/240/120/60), LM = 0..3 (frame sizes 120/240/480/960), and both the mono
// (stride 1) and interleaved stereo (stride 2) forward-MDCT call shapes.
//
// The per-stage fixed-point downshift (fft_downshift) inside the FFT is the
// rounding-brittle part flagged in docs/hard-parts.md section 8. It is localized
// by TestForwardFFTImplStageLocalize, which drives opus_fft_impl directly across
// the whole downshift budget for each nfft. Helpers newSeededRand / randScaled
// are shared with mdct_test.go (same package).

// fwdMDCTScaleBits sweeps forward-MDCT input magnitudes from tiny (large FFT
// headroom, hence a small scale_shift-headroom downshift) up to 2^26. It stops
// short of full scale on purpose: the window/fold and pre-rotation compute f and
// yr/yi at full int32 precision with no protective downscale, so |in| much
// beyond ~2^28 would overflow int32 in the C oracle (signed-overflow UB there),
// which is outside the encoder's operational range anyway. 2^26 keeps every
// intermediate in range while still sweeping the whole headroom/downshift ladder
// (larger magnitude => smaller headroom => larger FFT downshift).
var fwdMDCTScaleBits = []int{2, 6, 10, 14, 18, 22, 26}

// --- Forward MDCT ----------------------------------------------------------

func TestForwardMDCTBitExact(t *testing.T) {
	const overlap = 120 // mode48000_960 overlap
	for lm := 0; lm < 4; lm++ {
		n2 := 120 << uint(lm) // output coefficient count = shortMdctSize<<LM
		inlen := n2 + overlap // forward MDCT reads N2+overlap time-domain samples
		for _, stride := range []int{1, 2} {
			r := newSeededRand(uint64(0x5f5 + lm*8 + stride))
			for _, bits := range fwdMDCTScaleBits {
				const trials = 40
				for trial := 0; trial < trials; trial++ {
					in := make([]int32, inlen)
					for i := range in {
						in[i] = randScaled(r, bits)
					}
					goOut := celt.ForwardMDCT(lm, stride, in)
					cOut := cMDCTForward(lm, stride, in)
					if len(goOut) != len(cOut) {
						t.Fatalf("fwdmdct lm %d stride %d bits %d trial %d: len go %d c %d",
							lm, stride, bits, trial, len(goOut), len(cOut))
					}
					for i := range goOut {
						if goOut[i] != cOut[i] {
							t.Fatalf("fwdmdct lm %d stride %d bits %d trial %d: out[%d] go=%d c=%d",
								lm, stride, bits, trial, i, goOut[i], cOut[i])
						}
					}
				}
			}
			// Boundary vectors.
			for name, in := range fwdMDCTBoundaryVectors(inlen) {
				goOut := celt.ForwardMDCT(lm, stride, in)
				cOut := cMDCTForward(lm, stride, in)
				if len(goOut) != len(cOut) {
					t.Fatalf("fwdmdct/%s lm %d stride %d: len go %d c %d", name, lm, stride, len(goOut), len(cOut))
				}
				for i := range goOut {
					if goOut[i] != cOut[i] {
						t.Fatalf("fwdmdct/%s lm %d stride %d: out[%d] go=%d c=%d",
							name, lm, stride, i, goOut[i], cOut[i])
					}
				}
			}
		}
	}
}

// --- Forward FFT -----------------------------------------------------------

// TestForwardFFTBitExactCP3 re-affirms the forward opus_fft path (used by the
// forward MDCT) end-to-end across the four nfft sizes. Full-scale inputs are
// safe here: opus_fft prescales by st.scale before the engine runs.
func TestForwardFFTBitExactCP3(t *testing.T) {
	for idx := 0; idx < 4; idx++ {
		nfft := celt.FFTStateNFFT(idx)
		if got := cFFTNFFTFwd(idx); got != nfft {
			t.Fatalf("idx %d: Go nfft %d != C nfft %d", idx, nfft, got)
		}
		r := newSeededRand(uint64(0x6f6 + idx))
		for _, bits := range []int{4, 10, 16, 22, 28, 31} {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				inR := make([]int32, nfft)
				inI := make([]int32, nfft)
				for i := 0; i < nfft; i++ {
					inR[i] = randScaled(r, bits)
					inI[i] = randScaled(r, bits)
				}
				goR, goI := celt.ForwardFFT(idx, inR, inI)
				cR, cI := cFFTFwd(idx, inR, inI)
				for i := range goR {
					if goR[i] != cR[i] || goI[i] != cI[i] {
						t.Fatalf("fft idx %d bits %d trial %d: out[%d] go=(%d,%d) c=(%d,%d)",
							idx, bits, trial, i, goR[i], goI[i], cR[i], cI[i])
					}
				}
			}
		}
	}
}

// TestForwardFFTImplStageLocalize localizes the per-stage fixed-point downshift
// (fft_downshift), the trap flagged in docs/hard-parts.md section 8. The four
// nfft sizes have DISTINCT radix sequences, applied in reverse factor order by
// opus_fft_impl, each with its own downshift step:
//
//	nfft 480: radices 4,2,4,3,5  steps 2,1,2,2,3  (sum 10)
//	nfft 240: radices 4,4,3,5    steps 2,2,2,3    (sum 9)
//	nfft 120: radices 4,2,3,5    steps 2,1,2,3    (sum 8)
//	nfft  60: radices 4,3,5      steps 2,2,3      (sum 7)
//
// The C stage boundaries are not directly reachable without editing kiss_fft.c
// (forbidden), so instead of tapping between butterflies we sweep the whole
// downshift budget from 0 up past each nfft's step total. Because each stage
// consumes min(step, remaining_budget) before its butterfly, sweeping the budget
// makes it run out at every stage boundary in turn; a wrong downshift on any one
// butterfly would then diverge from C at some budget value. Full-scale inputs
// maximise the magnitude each stage shifts, so a lost/extra bit shows up.
func TestForwardFFTImplStageLocalize(t *testing.T) {
	stepTotal := []int{10, 9, 8, 7} // by idx (nfft 480/240/120/60)
	for idx := 0; idx < 4; idx++ {
		nfft := celt.FFTStateNFFT(idx)
		r := newSeededRand(uint64(0x7f7 + idx))
		maxDS := stepTotal[idx] + 2
		for ds := 0; ds <= maxDS; ds++ {
			const trials = 20
			for trial := 0; trial < trials; trial++ {
				inR := make([]int32, nfft)
				inI := make([]int32, nfft)
				for i := 0; i < nfft; i++ {
					inR[i] = randScaled(r, 31)
					inI[i] = randScaled(r, 31)
				}
				goR, goI := celt.FFTImplWithDownshift(idx, inR, inI, ds)
				cR, cI := cFFTImplFwd(idx, inR, inI, ds)
				for i := range goR {
					if goR[i] != cR[i] || goI[i] != cI[i] {
						t.Fatalf("fftimpl-stage idx %d ds %d trial %d: out[%d] go=(%d,%d) c=(%d,%d)",
							idx, ds, trial, i, goR[i], goI[i], cR[i], cI[i])
					}
				}
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

// fwdMDCTBoundaryVectors returns time-domain forward-MDCT inputs of length n.
// Magnitudes are capped at 2^26 for the same overflow reason as fwdMDCTScaleBits.
func fwdMDCTBoundaryVectors(n int) map[string][]int32 {
	const bound = 1 << 26
	zeros := make([]int32, n)
	impulse := make([]int32, n)
	impulse[n/2] = bound
	alt := make([]int32, n)
	mx := make([]int32, n)
	mn := make([]int32, n)
	ramp := make([]int32, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			alt[i] = bound
		} else {
			alt[i] = -bound
		}
		mx[i] = bound
		mn[i] = -bound
		ramp[i] = int32(((i % 128) - 64) << 18)
	}
	return map[string][]int32{
		"zeros": zeros, "impulse": impulse, "alt": alt, "max": mx, "min": mn, "ramp": ramp,
	}
}
