//go:build refc

package oracle

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/fixedmath"
)

// This differential test pins the pure-Go CELT encoder FRONT-HALF stages
// (internal/celt celt_encoder.go: celt_preemphasis, transient_analysis,
// patch_transient_decision, compute_mdcts, tone_detect, run_prefilter, the CP8a
// scope) to the pinned libopus C oracle (celt/celt_encoder.c stage functions,
// driven through celtenc_shim.h). Each stage is fed the SAME inputs on both sides
// and every output is asserted bit-identical. run_prefilter carries cross-frame
// state (in_mem, prefilter_mem, prefilter_period/gain/tapset), so it is exercised
// as a multi-frame sequence with per-frame comparison of both the returned values
// and the evolved state buffers.

// eqI32 fails the test if two int32 slices differ.
func eqI32(t *testing.T, label string, got, want []int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length Go=%d C=%d", label, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: mismatch at %d: Go=%d C=%d", label, i, got[i], want[i])
		}
	}
}

// --- mode geometry sanity ----------------------------------------------------

func TestCeltencModeGeometry(t *testing.T) {
	if got, want := cCeltencOverlap(), 120; got != want {
		t.Fatalf("overlap: C=%d want %d", got, want)
	}
	if got, want := cCeltencNbebands(), 21; got != want {
		t.Fatalf("nbEBands: C=%d want %d", got, want)
	}
	if got, want := cCeltencMaxperiod(), 1024; got != want {
		t.Fatalf("COMBFILTER_MAXPERIOD: C=%d want %d", got, want)
	}
	if got, want := cCeltencShortmdct(), 120; got != want {
		t.Fatalf("shortMdctSize: C=%d want %d", got, want)
	}
}

// --- celt_preemphasis --------------------------------------------------------

func TestCeltencPreemphasisMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8A01))
	for _, cc := range []int{1, 2} {
		const N = 960
		var goMem, cMem int32
		// Multi-frame: thread the filter memory across frames.
		for frame := 0; frame < 6; frame++ {
			pcm := make([]int16, N*cc)
			for i := range pcm {
				pcm[i] = int16(r.Intn(65536) - 32768)
			}
			goOut, goMemNext := celt.CeltPreemphasis(pcm, N, cc, 1, goMem)
			cOut, cMemNext := cCeltencPreemphasis(pcm, N, cc, 1, 0, cMem)
			eqI32(t, "preemph out", goOut, cOut)
			if goMemNext != cMemNext {
				t.Fatalf("cc=%d frame=%d preemph mem: Go=%d C=%d", cc, frame, goMemNext, cMemNext)
			}
			goMem, cMem = goMemNext, cMemNext
		}
	}
}

// --- transient_analysis ------------------------------------------------------

// genTransientIn builds a C*(N+overlap) celt_sig buffer. amp sets the base
// amplitude; if burst>0 a high-energy burst is injected mid-frame to drive the
// transient decision.
func genTransientIn(r *rand.Rand, C, length int, amp, burst int32) []int32 {
	in := make([]int32, C*length)
	for c := 0; c < C; c++ {
		for i := 0; i < length; i++ {
			v := int32(r.Int63n(int64(2*amp+1))) - amp
			if burst > 0 && i >= length/2 && i < length/2+40 {
				v += int32(r.Int63n(int64(2*burst+1))) - burst
			}
			in[c*length+i] = v
		}
	}
	return in
}

func TestCeltencTransientAnalysisMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x77A))
	const N = 960
	const overlap = 120
	length := N + overlap
	sawTransient, sawFlat := false, false
	for _, cc := range []int{1, 2} {
		for _, tc := range []struct {
			amp, burst int32
			allowWeak  int
		}{
			{1 << 14, 0, 0},
			{1 << 20, 0, 0},
			{1 << 12, 1 << 24, 0},
			{1 << 22, 1 << 26, 0},
			{1 << 13, 1 << 20, 1},
			{1 << 24, 0, 1},
		} {
			in := genTransientIn(r, cc, length, tc.amp, tc.burst)
			// Non-tonal so the tone guard does not fire.
			goIs, goTf, goChan, goWeak := celt.TransientAnalysis(in, length, cc, tc.allowWeak, -1, 0)
			cIs, cTf, cChan, cWeak := cCeltencTransient(in, length, cc, tc.allowWeak, -1, 0)
			if goIs != cIs || int32(goTf) != cTf || goChan != cChan || goWeak != cWeak {
				t.Fatalf("cc=%d amp=%d burst=%d weak=%d: Go(is=%d tf=%d chan=%d weak=%d) C(is=%d tf=%d chan=%d weak=%d)",
					cc, tc.amp, tc.burst, tc.allowWeak, goIs, goTf, goChan, goWeak, cIs, cTf, cChan, cWeak)
			}
			if goIs != 0 {
				sawTransient = true
			} else {
				sawFlat = true
			}
		}
	}
	// Also exercise the tone guard (toneishness high, tone_freq low -> not transient).
	in := genTransientIn(r, 1, length, 1<<20, 1<<25)
	goIs, _, _, _ := celt.TransientAnalysis(in, length, 1, 0, fixedmath.QCONST16(0.01, 13), fixedmath.QCONST32(0.99, 29))
	cIs, _, _, _ := cCeltencTransient(in, length, 1, 0, int(fixedmath.QCONST16(0.01, 13)), fixedmath.QCONST32(0.99, 29))
	if goIs != cIs {
		t.Fatalf("tone-guard transient: Go=%d C=%d", goIs, cIs)
	}
	if !sawTransient || !sawFlat {
		t.Fatalf("coverage: sawTransient=%v sawFlat=%v", sawTransient, sawFlat)
	}
}

// --- patch_transient_decision ------------------------------------------------

func TestCeltencPatchTransientMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x9A7C))
	const nbEBands = 21
	ran := false
	for _, cc := range []int{1, 2} {
		// Two regimes: full-range Q24 log energies (which exercise the opus_val16
		// x1/x2 truncation of large glog values), and tiny values (no truncation).
		for iter := 0; iter < 60; iter++ {
			newE := make([]int32, cc*nbEBands)
			oldE := make([]int32, cc*nbEBands)
			for i := range newE {
				if iter%2 == 0 {
					// Q24 domain, [-30, +12] dB (large: truncated to int16 in C).
					newE[i] = int32((r.Float64()*42 - 30) * float64(int32(1)<<24))
					oldE[i] = int32((r.Float64()*42 - 30) * float64(int32(1)<<24))
				} else {
					// Small values below the int16 truncation boundary.
					newE[i] = int32(r.Intn(1 << 16))
					oldE[i] = int32(r.Intn(1 << 16))
				}
			}
			goD := celt.PatchTransientDecision(newE, oldE, nbEBands, 0, nbEBands, cc)
			cD := cCeltencPatchTransient(newE, oldE, nbEBands, 0, nbEBands, cc)
			if goD != cD {
				t.Fatalf("cc=%d iter=%d patch_transient: Go=%d C=%d", cc, iter, goD, cD)
			}
			ran = true
		}
	}
	// NOTE: the C declares the per-band accumulator inputs x1/x2 as opus_val16, so
	// MAXG(0, glog) is truncated to 16 bits before SUB32 sign-extends it. With
	// 32-bit glog (this frozen config), each term is thus capped at ~2^15, so the
	// summed/averaged mean_diff can never exceed GCONST(1)=2^24 and the function
	// returns 0 for any input. The value of this test is that the Go port
	// replicates the truncation bit-for-bit (a naive int32 x1/x2 diverges here).
	if !ran {
		t.Fatal("patch_transient test did not run")
	}
}

// --- compute_mdcts -----------------------------------------------------------

// genMdctIn builds a CC*(N+overlap) celt_sig buffer within the C-safe magnitude
// (~2^28); a sinusoid plus noise so the transform output is nontrivial.
func genMdctIn(r *rand.Rand, CC, N, overlap int, amp float64) []int32 {
	buf := make([]int32, CC*(N+overlap))
	for c := 0; c < CC; c++ {
		phase := r.Float64() * 2 * math.Pi
		freq := 0.01 + 0.2*r.Float64()
		for i := 0; i < N+overlap; i++ {
			s := amp * math.Sin(phase+freq*float64(i))
			s += (r.Float64()*2 - 1) * amp * 0.1
			buf[c*(N+overlap)+i] = int32(s)
		}
	}
	return buf
}

func TestCeltencComputeMdctsMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x3DC7))
	const overlap = 120
	for _, tc := range []struct {
		LM, C, CC, shortBlocks int
	}{
		{3, 1, 1, 0}, // 20 ms long block, mono
		{3, 2, 2, 0}, // 20 ms long block, stereo
		{3, 1, 2, 0}, // 20 ms long block, stereo downmix (CC=2,C=1)
		{0, 1, 1, 0}, // 2.5 ms long block, mono
		{3, 1, 1, 8}, // short blocks (M=8), mono
		{3, 2, 2, 8}, // short blocks (M=8), stereo
		{2, 1, 1, 4}, // short blocks (M=4), mono
	} {
		N := 120 << tc.LM
		amp := math.Pow(2, 20+float64(r.Intn(6))) // 2^20..2^25, all < 2^28
		in := genMdctIn(r, tc.CC, N, overlap, amp)
		goOut := celt.ComputeMdcts(tc.shortBlocks, in, tc.C, tc.CC, tc.LM, 1)
		cOut := cCeltencComputeMdcts(tc.shortBlocks, in, tc.C, tc.CC, tc.LM, 1, tc.CC*N)
		eqI32(t, "compute_mdcts out", goOut, cOut)
	}
}

// --- tone_detect -------------------------------------------------------------

func TestCeltencToneDetectMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x70E))
	const overlap = 120
	sawTonal, sawNontonal := false, false
	for _, cc := range []int{1, 2} {
		for iter := 0; iter < 8; iter++ {
			N := 960
			total := N + overlap
			in := make([]int32, cc*total)
			tonal := iter%2 == 0
			for c := 0; c < cc; c++ {
				if tonal {
					// A near-pure tone -> tone_detect should return a real freq.
					freq := 0.02 + 0.15*r.Float64()
					phase := r.Float64() * 2 * math.Pi
					amp := math.Pow(2, 22)
					for i := 0; i < total; i++ {
						in[c*total+i] = int32(amp * math.Sin(phase+freq*float64(i)))
					}
				} else {
					amp := int64(1) << 22
					for i := 0; i < total; i++ {
						in[c*total+i] = int32(r.Int63n(2*amp+1) - amp)
					}
				}
			}
			goFreq, goTone := celt.ToneDetect(in, cc, total)
			cFreq, cTone := cCeltencToneDetect(in, cc, total)
			if int(goFreq) != cFreq || goTone != cTone {
				t.Fatalf("cc=%d iter=%d tone_detect: Go(freq=%d tone=%d) C(freq=%d tone=%d)",
					cc, iter, goFreq, goTone, cFreq, cTone)
			}
			if goTone > 0 {
				sawTonal = true
			} else {
				sawNontonal = true
			}
		}
	}
	if !sawTonal || !sawNontonal {
		t.Fatalf("coverage: sawTonal=%v sawNontonal=%v", sawTonal, sawNontonal)
	}
}

// --- run_prefilter (multi-frame cross-frame state) ---------------------------

// genPrefilterIn builds a CC*(N+overlap) celt_sig frame with a sustained periodic
// component (so the pitch estimator finds a lag) plus light noise.
func genPrefilterIn(r *rand.Rand, CC, N, overlap int, period float64, amp float64) []int32 {
	total := N + overlap
	buf := make([]int32, CC*total)
	for c := 0; c < CC; c++ {
		for i := 0; i < total; i++ {
			ph := 2 * math.Pi * float64(i) / period
			s := amp * (math.Sin(ph) + 0.3*math.Sin(2*ph))
			s += (r.Float64()*2 - 1) * amp * 0.05
			buf[c*total+i] = int32(s)
		}
	}
	return buf
}

func TestCeltencRunPrefilterMatchesC(t *testing.T) {
	const N = 960
	const overlap = 120
	const maxPeriod = 1024

	type frameCfg struct {
		complexity, enabled, lossRate, nbAvail int
		tfEstimate                             int
		toneFreq                               int
		toneishness                            int32
		tapsetIn                               int
	}

	for _, cc := range []int{1, 2} {
		r := rand.New(rand.NewSource(int64(0x9F17 + cc)))
		goEnc := celt.NewEncoder(cc)
		// Carried scalar prefilter state and C-side carried buffers.
		period, gainv, tapset := 0, 0, 0
		cInMem := make([]int32, cc*overlap)
		cPrefilterMem := make([]int32, cc*maxPeriod)

		frames := []frameCfg{
			{5, 1, 0, 100, 0, -1, 0, 0},             // pitch-search path
			{5, 1, 0, 100, 0, -1, 0, 1},             // continuity, tapset=1
			{10, 1, 3, 100, 0, -1, 0, 0},            // loss_rate>2 gain halving
			{10, 1, 5, 30, qc16(0.5, 14), -1, 0, 0}, // <35 bytes, loss_rate>4
			{5, 1, 9, 20, 0, -1, 0, 0},              // <25 bytes, loss_rate>8 gain 0
			{5, 1, 0, 100, 0, int(fixedmath.QCONST16(0.05, 13)), fixedmath.QCONST32(0.995, 29), 0}, // tone-dominated
			{5, 0, 0, 100, 0, -1, 0, 0},              // disabled -> gain 0
			{4, 1, 0, 100, 0, -1, 0, 0},              // complexity<5 -> gain 0
			{5, 1, 0, 100, qc16(0.99, 14), -1, 0, 0}, // strong transient tf_estimate
		}

		for fi, fc := range frames {
			periodLen := 180.0 + 40*r.Float64()
			amp := math.Pow(2, 18+float64(r.Intn(4)))
			base := genPrefilterIn(r, cc, N, overlap, periodLen, amp)
			inGo := append([]int32(nil), base...)
			inC := append([]int32(nil), base...)

			goEnc.SetComplexity(fc.complexity)
			goEnc.SetLossRate(fc.lossRate)
			goEnc.SetPrefilterState(period, int16(gainv), tapset)

			gPitch, gGain, gQg, gPfOn := goEnc.RunPrefilter(inGo, goEnc.PrefilterMem(), cc, N,
				fc.tapsetIn, fc.enabled, fc.complexity, int16(fc.tfEstimate), fc.nbAvail,
				int16(fc.toneFreq), fc.toneishness)
			gPeriodMut := goEnc.PrefilterPeriod()

			cPitch, cGain, cQg, cPfOn, cPeriodMut := cCeltencRunPrefilter(cc, N, fc.complexity,
				fc.lossRate, period, gainv, tapset, fc.tapsetIn, fc.enabled, fc.tfEstimate,
				fc.nbAvail, fc.toneFreq, fc.toneishness, inC, cInMem, cPrefilterMem)

			if gPitch != cPitch || int(gGain) != cGain || gQg != cQg || gPfOn != cPfOn || gPeriodMut != cPeriodMut {
				t.Fatalf("cc=%d frame=%d run_prefilter: Go(pitch=%d gain=%d qg=%d pf=%d periodMut=%d) C(pitch=%d gain=%d qg=%d pf=%d periodMut=%d)",
					cc, fi, gPitch, gGain, gQg, gPfOn, gPeriodMut, cPitch, cGain, cQg, cPfOn, cPeriodMut)
			}
			eqI32(t, "run_prefilter in", inGo, inC)
			eqI32(t, "run_prefilter in_mem", goEnc.InMem(), cInMem)
			eqI32(t, "run_prefilter prefilter_mem", goEnc.PrefilterMem(), cPrefilterMem)

			// Driver post-update: carry pitch_index / gain1 / prefilter_tapset.
			period = gPitch
			gainv = int(gGain)
			tapset = fc.tapsetIn
		}
	}
}

// qc16 is a small local helper: QCONST16 as an int for the frame table.
func qc16(x float64, bits int) int { return int(fixedmath.QCONST16(x, bits)) }

// --- remove_doubling ---------------------------------------------------------

// genRemoveDoublingBuf builds the (maxperiod+N)/2 int16 half-rate analysis buffer
// remove_doubling reads. kind 0 = white noise, 1 = periodic (period p), 2 =
// periodic with a strong sub-harmonic (drives the T/k doubling search), 3 = DC
// (guarantees best_yy<=best_xy so pg=Q15ONE).
func genRemoveDoublingBuf(r *rand.Rand, n, kind, p int, amp int) []int16 {
	x := make([]int16, n)
	switch kind {
	case 0:
		for i := range x {
			x[i] = int16(r.Intn(2*amp+1) - amp)
		}
	case 1:
		for i := range x {
			x[i] = int16(float64(amp) * math.Sin(2*math.Pi*float64(i)/float64(p)))
		}
	case 2:
		for i := range x {
			x[i] = int16(float64(amp) * (math.Sin(2*math.Pi*float64(i)/float64(p)) +
				0.6*math.Sin(2*math.Pi*float64(i)/float64(p/2))))
		}
	default:
		for i := range x {
			x[i] = int16(amp)
		}
	}
	return x
}

func TestCeltencRemoveDoublingMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x2D0B))
	const maxperiod = 1024
	const minperiod = 15
	const N = 960
	const bufLen = (maxperiod + N) / 2
	const q15one = 32767

	sawQ15One := false
	sawContEffect := false
	sawGain := false
	ran := 0

	// DC buffer amplitude kept small so sum(x^2) over N/2 stays in int32.
	bufs := []struct {
		kind, p, amp int
	}{
		{0, 0, 6000}, {0, 0, 20000}, {1, 200, 9000}, {1, 96, 9000},
		{2, 200, 9000}, {2, 320, 8000}, {3, 0, 1500},
	}
	for _, bc := range bufs {
		x := genRemoveDoublingBuf(r, bufLen, bc.kind, bc.p, bc.amp)
		for _, T0 := range []int{40, 100, 200, 320, 512, 800, 1000} {
			for _, prevPeriod := range []int{0, T0 / 2, T0, T0 + 2, 96, 200} {
				var pgAtGain0, t0AtGain0 int
				for gi, prevGain := range []int{0, q15one / 2, q15one} {
					goPg, goT0 := celt.RemoveDoubling(x, maxperiod, minperiod, N, T0, prevPeriod, int16(prevGain))
					cPg, cT0 := cCeltencRemoveDoubling(x, maxperiod, minperiod, N, T0, prevPeriod, prevGain)
					if int(goPg) != cPg || goT0 != cT0 {
						t.Fatalf("kind=%d T0=%d prevPeriod=%d prevGain=%d: Go(pg=%d T0=%d) C(pg=%d T0=%d)",
							bc.kind, T0, prevPeriod, prevGain, goPg, goT0, cPg, cT0)
					}
					ran++
					if goPg == q15one {
						sawQ15One = true
					}
					if goPg > 0 {
						sawGain = true
					}
					if gi == 0 {
						pgAtGain0, t0AtGain0 = int(goPg), goT0
					} else if prevGain == q15one && (int(goPg) != pgAtGain0 || goT0 != t0AtGain0) {
						// prevGain changed the outcome -> the continuity (cont) branch fired.
						sawContEffect = true
					}
				}
			}
		}
	}
	// Continuity probe. The cont term (pitch.c:517-524) only changes the result
	// when a large prevGain lowers a sub-multiple's acceptance threshold enough to
	// flip the T selection, which needs g1 in a narrow window. Construction: a pure
	// period-300 sinusoid so the halved coarse lag T0h=300 (from T0=600) has g0~1,
	// while the k=6 sub-multiple T1=(2*300+6)/12=50 has autocorr cos(2pi*50/300)=0.5,
	// which sits between the cont=0 threshold (0.7*g0) that rejects it and the
	// cont=Q15ONE threshold (0.3) that accepts it. prevPeriod=100 (halved 50) aligns
	// cont with that T1, so prevGain flips T from 600 to ~99. Verified bit-exact
	// against C for both prevGain extremes; a couple of neighbours are swept so a
	// small fixed-point shift in g1 does not desensitise the probe.
	for _, pc := range []struct {
		amp        float64
		T0, prevPP int
	}{{2500, 600, 100}, {2500, 600, 66}, {1500, 500, 80}} {
		x := make([]int16, bufLen)
		for i := range x {
			x[i] = int16(pc.amp * math.Sin(2*math.Pi*float64(i)/300))
		}
		if pc.T0 == 500 {
			for i := range x {
				x[i] = int16(pc.amp * math.Sin(2*math.Pi*float64(i)/250))
			}
		}
		pg0, t00 := celt.RemoveDoubling(x, maxperiod, minperiod, N, pc.T0, pc.prevPP, 0)
		cpg0, ct00 := cCeltencRemoveDoubling(x, maxperiod, minperiod, N, pc.T0, pc.prevPP, 0)
		pg1, t01 := celt.RemoveDoubling(x, maxperiod, minperiod, N, pc.T0, pc.prevPP, q15one)
		cpg1, ct01 := cCeltencRemoveDoubling(x, maxperiod, minperiod, N, pc.T0, pc.prevPP, q15one)
		if int(pg0) != cpg0 || t00 != ct00 || int(pg1) != cpg1 || t01 != ct01 {
			t.Fatalf("cont probe amp=%.0f T0=%d prevPP=%d: Go(pg0=%d T0=%d pg1=%d T1=%d) C(pg0=%d T0=%d pg1=%d T1=%d)",
				pc.amp, pc.T0, pc.prevPP, pg0, t00, pg1, t01, cpg0, ct00, cpg1, ct01)
		}
		if pg0 != pg1 || t00 != t01 {
			sawContEffect = true
		}
	}

	if ran == 0 {
		t.Fatal("remove_doubling test did not run")
	}
	if !sawQ15One {
		t.Fatal("coverage: pg=Q15ONE branch (best_yy<=best_xy) never hit")
	}
	if !sawGain {
		t.Fatal("coverage: no nonzero pitch gain produced")
	}
	if !sawContEffect {
		t.Fatal("coverage: prevGain never changed the outcome (continuity branch not exercised)")
	}
}

// --- run_prefilter targeted branch coverage ----------------------------------

// runPrefilterOnce drives one run_prefilter frame on freshly-seeded state, asserts
// the Go and C results (and the mutated in/in_mem/prefilter_mem) are bit-exact,
// and returns the Go outputs.
func runPrefilterOnce(t *testing.T, cc, N, complexity, lossRate, period, gainv, tapset, tapsetIn,
	enabled, tfEstimate, nbAvail, toneFreq int, toneishness int32,
	base, inMemSeed, preMemSeed []int32) (pitch, gain, qg, pfOn int) {
	t.Helper()
	goEnc := celt.NewEncoder(cc)
	goEnc.SetComplexity(complexity)
	goEnc.SetLossRate(lossRate)
	goEnc.SetPrefilterState(period, int16(gainv), tapset)
	copy(goEnc.InMem(), inMemSeed)
	copy(goEnc.PrefilterMem(), preMemSeed)
	inGo := append([]int32(nil), base...)
	gPitch, gGain, gQg, gPfOn := goEnc.RunPrefilter(inGo, goEnc.PrefilterMem(), cc, N, tapsetIn,
		enabled, complexity, int16(tfEstimate), nbAvail, int16(toneFreq), toneishness)

	inC := append([]int32(nil), base...)
	cInMem := append([]int32(nil), inMemSeed...)
	cPreMem := append([]int32(nil), preMemSeed...)
	cPitch, cGain, cQg, cPfOn, _ := cCeltencRunPrefilter(cc, N, complexity, lossRate, period, gainv,
		tapset, tapsetIn, enabled, tfEstimate, nbAvail, toneFreq, toneishness, inC, cInMem, cPreMem)

	if gPitch != cPitch || int(gGain) != cGain || gQg != cQg || gPfOn != cPfOn {
		t.Fatalf("run_prefilter branch: Go(pitch=%d gain=%d qg=%d pf=%d) C(pitch=%d gain=%d qg=%d pf=%d)",
			gPitch, gGain, gQg, gPfOn, cPitch, cGain, cQg, cPfOn)
	}
	eqI32(t, "branch in", inGo, inC)
	eqI32(t, "branch in_mem", goEnc.InMem(), cInMem)
	eqI32(t, "branch prefilter_mem", goEnc.PrefilterMem(), cPreMem)
	return gPitch, int(gGain), gQg, gPfOn
}

func TestCeltencRunPrefilterBranches(t *testing.T) {
	r := rand.New(rand.NewSource(0x5A17))
	const N = 960
	const overlap = 120
	const maxPeriod = 1024
	const minPeriod = 15
	tone := fixedmath.QCONST32(0.995, 29)

	periodic := func(cc int, p float64, amp float64) []int32 {
		buf := make([]int32, cc*(N+overlap))
		for c := 0; c < cc; c++ {
			for i := 0; i < N+overlap; i++ {
				buf[c*(N+overlap)+i] = int32(amp * math.Sin(2*math.Pi*float64(i)/p))
			}
		}
		return buf
	}

	inMem1 := make([]int32, overlap)
	preMem1 := make([]int32, maxPeriod)
	sig := periodic(1, 220, math.Pow(2, 20))

	// Tone-fold: toneFreq >= QCONST16(3.1416,13) triggers the pi-fold, which then
	// lands in the low-freq branch -> pitch = COMBFILTER_MINPERIOD (=15). Without
	// the fold, toneFreq=26000 would give multiple=9, pitch=18, so pitch==15 proves
	// the fold branch (celt_encoder.c:1444) fired.
	pitch, _, _, _ := runPrefilterOnce(t, 1, N, 5, 0, 0, 0, 0, 0, 1, 0, 100, 26000, tone, sig, inMem1, preMem1)
	if pitch != minPeriod {
		t.Fatalf("tone-fold branch: pitch=%d want %d (fold+low-freq)", pitch, minPeriod)
	}

	// multiple++ doubling loop: toneFreq=6000 -> multiple=2, pitch=(51472*2+3000)/6000=17.
	pitch, _, _, _ = runPrefilterOnce(t, 1, N, 5, 0, 0, 0, 0, 0, 1, 0, 100, 6000, tone, sig, inMem1, preMem1)
	if pitch != 17 {
		t.Fatalf("tone multiple++ branch: pitch=%d want 17", pitch)
	}

	// Low-freq branch: toneFreq=30 (<= QCONST16(0.006148,13)=50) -> pitch=MINPERIOD.
	pitch, _, _, _ = runPrefilterOnce(t, 1, N, 5, 0, 0, 0, 0, 0, 1, 0, 100, 30, tone, sig, inMem1, preMem1)
	if pitch != minPeriod {
		t.Fatalf("tone low-freq branch: pitch=%d want %d", pitch, minPeriod)
	}

	// Mono cancel_pitch revert with NONZERO previous gain: a tone-dominated frame
	// (gain1=0.75 passes the threshold, so pf_on becomes 1 pre-comb) driven over a
	// WHITE-NOISE signal, which the comb filter makes worse -> after>before ->
	// cancel_pitch=1 -> the revert runs comb_filter with -st->prefilter_gain (seeded
	// nonzero). A final pf_on==0 with gain==0 can only come from that revert here.
	sawCancel := false
	for attempt := 0; attempt < 8 && !sawCancel; attempt++ {
		noise := make([]int32, N+overlap)
		for i := range noise {
			noise[i] = int32(r.Int63n(1<<21) - (1 << 20))
		}
		seededGain := int(fixedmath.QCONST16(0.5, 15))
		_, gain, _, pfOn := runPrefilterOnce(t, 1, N, 5, 0, 100, seededGain, 0, 0, 1, 0, 100, 6000, tone, noise, inMem1, preMem1)
		if pfOn == 0 && gain == 0 {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatal("coverage: mono cancel_pitch revert (nonzero gain) never fired")
	}
}
