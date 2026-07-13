//go:build refc

package oracle

import (
	"bytes"
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This differential test pins the pure-Go CELT encoder TIME-FREQUENCY stage
// (internal/celt/celt_encoder_tf.go: l1_metric, tf_analysis, tf_encode; the CP8b
// "TF" unit) to the pinned libopus C oracle (celt/celt_encoder.c:650/:663/:824,
// driven through celtenc_shim.h). Both sides get identical inputs and every
// output is asserted bit-identical.
//
// tf_encode is a RANGE-CODER WRITER, so it is not enough to compare its return:
// the test compares (a) the encoded bytes, (b) the coder state (tell / rng / val
// / error) after the call, and (c) the tf_res array, which tf_encode MUTATES IN
// PLACE twice (budget-forced bands at :849, then the tf_select_table remap of
// EVERY entry at :859-860). The remapped array is what quant_all_bands consumes
// downstream, so a mismatch there would corrupt the bitstream even if the bits
// tf_encode itself wrote were right.
//
// tf_analysis is stateless, so a randomized sweep over its input space is the
// right shape; it is driven ONLY with start=0. The C reads importance[0..len)
// while dynalloc_analysis only defines importance[start..end), so start>0 would
// have the C read uninitialized stack (see celt_encoder.c:751 and the comment on
// tfAnalysis in celt_encoder_tf.go). That is unreachable in the frozen config
// because the enable_tf_analysis gate (celt_encoder.c:2210) requires !hybrid and
// start>0 only happens in hybrid. tf_encode never reads importance, so IT is
// swept with start>0 as well.

// tfEqInts fails the test if two int slices differ.
func tfEqInts(t *testing.T, label string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length Go=%d C=%d", label, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: mismatch at %d: Go=%v C=%v", label, i, got, want)
		}
	}
}

// tfShortMdct is the frozen 48 kHz mode's shortMdctSize; N (== the tf_analysis
// N0 argument) is shortMdctSize<<LM, matching the caller at celt_encoder.c:2258.
const tfShortMdct = 120

// tfNbEBands is the frozen mode's nbEBands (== effEBands, so effEnd tops out
// here).
const tfNbEBands = 21

// tfGenX builds one channel-interleaved celt_norm buffer (C*N0 int32, NORM_SHIFT
// = 24 so the natural magnitude is ~1<<24). The shapes deliberately span the
// decision space of tf_analysis's per-band L1 search: a transient-shaped signal
// makes coarse frequency / fine time resolution win (driving best_level toward
// the isTransient branches, including the -1 case), while a tonal one makes the
// opposite win, and that spread is what makes selcost[1] < selcost[0] reachable
// (i.e. tf_select == 1).
func tfGenX(r *rand.Rand, total, N0, kind int) []int32 {
	x := make([]int32, total)
	const amp = 1 << 24
	switch kind {
	case 0: // white noise at full normalized scale
		for i := range x {
			x[i] = int32(r.Intn(2*amp) - amp)
		}
	case 1: // impulsive: energy packed into one short block, near-silence elsewhere
		for i := range x {
			x[i] = int32(r.Intn(2049) - 1024)
		}
		blk := N0 / 8
		if blk < 1 {
			blk = 1
		}
		off := r.Intn(8) * blk
		for c := 0; c*N0 < total; c++ {
			for i := off; i < off+blk && i < N0; i++ {
				x[c*N0+i] = int32(r.Intn(2*amp) - amp)
			}
		}
	case 2: // tonal: a sinusoid (favours fine frequency resolution)
		f := 0.01 + r.Float64()*0.4
		ph := r.Float64() * 6.283
		for c := 0; c*N0 < total; c++ {
			for i := 0; i < N0 && c*N0+i < total; i++ {
				x[c*N0+i] = int32(float64(amp) * math.Sin(f*float64(i)+ph))
			}
		}
	case 3: // low-level noise (small L1, exercises the bias term's sign)
		for i := range x {
			x[i] = int32(r.Intn(513) - 256)
		}
	case 4: // alternating sign: maximal haar1 high-band response
		for i := range x {
			v := int32(r.Intn(amp))
			if i&1 == 1 {
				v = -v
			}
			x[i] = v
		}
	case 5: // ramp plus noise
		for i := range x {
			x[i] = int32((i%N0)*(amp/N0)) + int32(r.Intn(1<<16)-(1<<15))
		}
	case 6:
		// HIGH-AMPLITUDE probe of l1_metric's width. C's ABS16 (arch.h:227) is a
		// type-agnostic macro and here its operand is SHR32(tmp[i], NORM_SHIFT-14),
		// an opus_val32 -- so the C does NOT narrow to 16 bits. That is only
		// OBSERVABLE once |SHR32(x,10)| can exceed 32767, i.e. |x| > 2^25: at the
		// natural celt_norm scale (~2^24) a 16-bit ABS would give identical
		// results and the ABS32-vs-ABS16 choice would be untestable. So this kind
		// drives |x| up to 2^28, which pins the port to the C's 32-bit width.
		//
		// 2^28 is the ceiling on purpose: tf_analysis applies haar1 cumulatively
		// at most LM+1 = 4 times, and each pass can grow a coefficient by sqrt(2),
		// so |x| <= 2^28 keeps every intermediate below 2^30 and the C's int32
		// arithmetic well clear of signed overflow (which would be UB, not a
		// contract this test could pin).
		const hiAmp = 1 << 28
		for i := range x {
			x[i] = int32(r.Intn(2*hiAmp) - hiAmp)
		}
	}
	return x
}

// --- tf_analysis (celt_encoder.c:663) ----------------------------------------

func TestCeltencTfAnalysisMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8B7F01))

	// lambda at the call site is IMAX(80, 20480/effectiveBytes + 2)
	// (celt_encoder.c:2257), so >= 80 and large for tiny frames. Small values are
	// also swept: they make path switching cheap and so drive the Viterbi
	// backward pass through mixed tf_res outputs.
	lambdas := []int{0, 1, 5, 80, 100, 200, 500, 1000, 2050, 20482}

	var (
		sawTfSel0, sawTfSel1       bool
		sawTransient, sawNonTrans  bool
		sawTfResMixed              bool
		sawTfResAll0, sawTfResAll1 bool
		sawLM                      [4]bool
		sawChan1                   bool
		sawHiAmp                   bool
		sawTfEstExtreme            bool
	)

	for _, LM := range []int{0, 1, 2, 3} {
		N0 := tfShortMdct << LM
		for _, C := range []int{1, 2} {
			for _, isTransient := range []int{0, 1} {
				for kind := 0; kind <= 6; kind++ {
					for rep := 0; rep < 4; rep++ {
						x := tfGenX(r, C*N0, N0, kind)
						if kind == 6 {
							sawHiAmp = true
						}
						// effEnd = IMIN(end, effEBands); len >= 2 (the C reads
						// eBands[len-1] and metric[0] / tf_res[len-1]).
						length := 2 + r.Intn(tfNbEBands-1)
						tfChan := r.Intn(C)
						if tfChan == 1 {
							sawChan1 = true
						}
						lambda := lambdas[r.Intn(len(lambdas))]
						// tf_estimate is opus_val16 Q14 in [0, 1<<14] at the call
						// site (transient_analysis output), which is the common case.
						// Every 5th rep instead uses an EXTREME opus_val16, including
						// negatives and the int16 endpoints: bias is computed as
						// MULT16_16_Q14(QCONST16(.04,15), MAX16(-QCONST16(.25,14),
						// QCONST16(.5,14)-tf_estimate)) (celt_encoder.c:682), where
						// QCONST16(.5,14)-tf_estimate is int arithmetic (both operands
						// promote) but MULT16_16 then casts to opus_val16. Only an
						// out-of-range tf_estimate makes that narrowing observable, so
						// these values pin where the 16-bit cast actually sits.
						var tfEstimate int16
						if rep == 3 {
							extremes := []int16{-32768, -20000, -1, 0, 16384, 32767}
							tfEstimate = extremes[r.Intn(len(extremes))]
							sawTfEstExtreme = true
						} else {
							tfEstimate = int16(r.Intn(1 << 14))
						}
						// importance is dynalloc_analysis's output
						// (celt_encoder.c:1186): PSHR32(13*celt_exp2_db(MING(follower,
						// GCONST(4))),16), i.e. ~[0,208]; the lfe / small-frame branch
						// sets a flat 13. Both shapes are swept. Keeping it in the
						// real range keeps the int cost accumulators far from 32-bit
						// wrap, where Go's 64-bit int could otherwise diverge from C.
						importance := make([]int, length)
						if rep == 0 {
							for i := range importance {
								importance[i] = 13
							}
						} else {
							for i := range importance {
								importance[i] = r.Intn(209)
							}
						}

						goSel, goRes := celt.TfAnalysis(length, isTransient, lambda, x,
							N0, LM, tfEstimate, tfChan, importance)
						cSel, cRes := cCeltencTfAnalysis(length, isTransient, lambda, x,
							N0, LM, int(tfEstimate), tfChan, importance)

						if goSel != cSel {
							t.Fatalf("tf_analysis tf_select: LM=%d C=%d isTransient=%d kind=%d len=%d lambda=%d tfChan=%d tfEst=%d: Go=%d C=%d",
								LM, C, isTransient, kind, length, lambda, tfChan, tfEstimate, goSel, cSel)
						}
						tfEqInts(t, "tf_analysis tf_res", goRes, cRes)

						if cSel == 0 {
							sawTfSel0 = true
						} else {
							sawTfSel1 = true
						}
						if isTransient != 0 {
							sawTransient = true
						} else {
							sawNonTrans = true
						}
						sawLM[LM] = true
						n0, n1 := 0, 0
						for _, v := range cRes {
							if v == 0 {
								n0++
							} else {
								n1++
							}
						}
						switch {
						case n0 > 0 && n1 > 0:
							sawTfResMixed = true
						case n1 == 0:
							sawTfResAll0 = true
						default:
							sawTfResAll1 = true
						}
					}
				}
			}
		}
	}

	// Non-vacuity: every documented branch must have actually executed.
	if !sawTfSel0 {
		t.Fatal("non-vacuity: tf_analysis never returned tf_select=0")
	}
	if !sawTfSel1 {
		t.Fatal("non-vacuity: tf_analysis never returned tf_select=1 (celt_encoder.c:772 never fired)")
	}
	if !sawTransient {
		t.Fatal("non-vacuity: isTransient=1 never exercised")
	}
	if !sawNonTrans {
		t.Fatal("non-vacuity: isTransient=0 never exercised")
	}
	if !sawTfResMixed {
		t.Fatal("non-vacuity: the Viterbi backward pass never produced a mixed tf_res (no path switching)")
	}
	if !sawTfResAll0 {
		t.Fatal("non-vacuity: never saw an all-zero tf_res")
	}
	if !sawTfResAll1 {
		t.Fatal("non-vacuity: never saw an all-one tf_res")
	}
	if !sawChan1 {
		t.Fatal("non-vacuity: tf_chan=1 (right channel) never exercised")
	}
	if !sawHiAmp {
		t.Fatal("non-vacuity: l1_metric was never driven past the 16-bit range (ABS32-vs-ABS16 untested)")
	}
	if !sawTfEstExtreme {
		t.Fatal("non-vacuity: the bias 16-bit cast chain was never driven with an out-of-range tf_estimate")
	}
	for LM := range sawLM {
		if !sawLM[LM] {
			t.Fatalf("non-vacuity: LM=%d never exercised", LM)
		}
	}
}

// --- tf_encode (celt_encoder.c:824) ------------------------------------------

// tfEncodeCase is one tf_encode configuration. Both sides are driven with it and
// every output (bytes, coder state, mutated tf_res) is compared.
type tfEncodeCase struct {
	start, end  int
	isTransient int
	LM          int
	tfSelect    int
	prefillBits int
	prefillPat  uint32
	bufLen      int
	tfRes       []int
	note        string
}

// tfCResult is a C tf_encode result plus the finalized bitstream the wrapper
// wrote into the caller's buffer (celtencTfEncodeResult itself does not carry
// the bytes, since the C wrapper mutates the buffer in place).
type tfCResult struct {
	celtencTfEncodeResult
	buf []byte
}

// tfRunEncode drives one case through the C oracle and returns its result. It is
// also the primitive the non-vacuity probes below are built on: every flag is
// derived from ORACLE outputs, never from a re-implementation of the budget
// logic in the test.
func tfRunEncode(c tfEncodeCase) tfCResult {
	buf := make([]byte, c.bufLen)
	res := cCeltencTfEncode(c.start, c.end, c.isTransient, c.tfRes, c.LM, c.tfSelect,
		c.prefillBits, c.prefillPat, buf)
	return tfCResult{celtencTfEncodeResult: res, buf: buf}
}

// tfForcedFired reports whether tf_encode's budget-exhaustion path
// (celt_encoder.c:849, `tf_res[i] = curr`) fired for this case.
//
// It is an exact, oracle-derived probe, not a guess: forcing is MONOTONE in i
// (once tell+logp > budget the loop takes the else branch, which advances
// neither tell nor logp, and logp is already at its steady 4/5 value from the
// second band on), so the path fired iff the LAST band was forced. A band that
// is actually coded always leaves a fingerprint on tf_res, because every
// tf_select_table column pair (celt.c:320) maps tf_res 0 and 1 to DIFFERENT
// values for every LM/isTransient/tf_select. So flipping the last band's
// requested decision changes the C's output tf_res iff that band was coded;
// if the output is unchanged, the band was forced.
func tfForcedFired(c tfEncodeCase) bool {
	if c.end <= c.start {
		return false
	}
	base := tfRunEncode(c)
	flip := c
	flip.tfRes = append([]int(nil), c.tfRes...)
	flip.tfRes[c.end-1] ^= 1
	other := tfRunEncode(flip)
	return base.tfRes[c.end-1] == other.tfRes[c.end-1]
}

// tfSelectWasCoded reports whether tf_encode actually wrote the tf_select bit
// (celt_encoder.c:856) rather than falling into the else that zeroes tf_select
// (:858). Again oracle-derived: when the bit IS coded, the guard at :855
// guarantees tf_select_table differs between tf_select 0 and 1 for the bands in
// play, so running the same case with tf_select=0 vs tf_select=1 must produce
// different output; when it is NOT coded, tf_select is forced to 0 and the two
// runs are identical.
func tfSelectWasCoded(c tfEncodeCase) bool {
	a := c
	a.tfSelect = 0
	b := c
	b.tfSelect = 1
	ra := tfRunEncode(a)
	rb := tfRunEncode(b)
	if !bytes.Equal(ra.buf, rb.buf) {
		return true
	}
	for i := range ra.tfRes {
		if ra.tfRes[i] != rb.tfRes[i] {
			return true
		}
	}
	return false
}

func TestCeltencTfEncodeMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8B7F02))

	var (
		sawForced, sawAllCoded       bool
		sawRsv0, sawRsv1             bool
		sawSelCoded, sawSelZeroed    bool
		sawTransient, sawNonTrans    bool
		sawTfChanged0, sawTfChanged1 bool
		sawStartPos                  bool
		sawErr                       bool
		sawTfSelIn0, sawTfSelIn1     bool
		sawLM                        [4]bool
	)

	var cases []tfEncodeCase

	// Randomized sweep over the whole input space.
	for _, LM := range []int{0, 1, 2, 3} {
		for _, isTransient := range []int{0, 1} {
			for _, tfSelect := range []int{0, 1} {
				for rep := 0; rep < 24; rep++ {
					start := 0
					// tf_encode never reads importance, so start>0 is safe here
					// (unlike tf_analysis) and is swept: it is the real hybrid-mode
					// coded window.
					if rep%3 == 1 {
						start = 1 + r.Intn(3)
					}
					end := start + 1 + r.Intn(tfNbEBands-start)
					// bufLen drives budget = storage*8. Small buffers force the
					// :849 unaffordable-band path; large ones code every band.
					var bufLen int
					switch rep % 4 {
					case 0:
						bufLen = 1 + r.Intn(3) // tiny: budget exhaustion
					case 1:
						bufLen = 1 + r.Intn(8)
					default:
						bufLen = 8 + r.Intn(120) // roomy: all bands coded
					}
					// prefill advances `tell` before tf_encode, sweeping the
					// starting point of the unsigned budget comparison.
					prefillBits := 0
					if rep%2 == 1 {
						prefillBits = r.Intn(80)
					}
					tfRes := make([]int, tfNbEBands)
					for i := range tfRes {
						tfRes[i] = r.Intn(2)
					}
					cases = append(cases, tfEncodeCase{
						start: start, end: end, isTransient: isTransient, LM: LM,
						tfSelect: tfSelect, prefillBits: prefillBits,
						prefillPat: r.Uint32(), bufLen: bufLen, tfRes: tfRes,
						note: "sweep",
					})
				}
			}
		}
	}

	// Targeted cases for the documented edge branches.
	allZero := make([]int, tfNbEBands)
	allOne := make([]int, tfNbEBands)
	for i := range allOne {
		allOne[i] = 1
	}
	cases = append(cases,
		// tf_changed stays 0 (every coded band is 0) -> the :855 guard compares
		// tf_select_table[LM][4*iT+0] vs [4*iT+2].
		tfEncodeCase{start: 0, end: 21, isTransient: 0, LM: 3, tfSelect: 1, bufLen: 64, tfRes: allZero, note: "tf_changed=0"},
		tfEncodeCase{start: 0, end: 21, isTransient: 1, LM: 3, tfSelect: 1, bufLen: 64, tfRes: allZero, note: "tf_changed=0 transient"},
		// tf_changed becomes 1.
		tfEncodeCase{start: 0, end: 21, isTransient: 1, LM: 2, tfSelect: 1, bufLen: 64, tfRes: allOne, note: "tf_changed=1"},
		// LM=0 -> tf_select_rsv is 0 regardless of budget (:838).
		tfEncodeCase{start: 0, end: 21, isTransient: 1, LM: 0, tfSelect: 1, bufLen: 64, tfRes: allOne, note: "LM=0, rsv=0"},
		// Minimum storage: budget = 8 bits, so almost every band is unaffordable.
		tfEncodeCase{start: 0, end: 21, isTransient: 0, LM: 3, tfSelect: 1, bufLen: 1, tfRes: allOne, note: "bufLen=1"},
		tfEncodeCase{start: 0, end: 21, isTransient: 1, LM: 1, tfSelect: 0, bufLen: 1, tfRes: allOne, note: "bufLen=1 transient"},
		// Prefill the coder to the brim so tell is already at/over budget and even
		// the FIRST band is unaffordable (and tf_select_rsv is 0 by budget, not LM).
		tfEncodeCase{start: 0, end: 21, isTransient: 1, LM: 3, tfSelect: 1, prefillBits: 60, prefillPat: 0xA5A5A5A5, bufLen: 2, tfRes: allOne, note: "tell >= budget"},
		tfEncodeCase{start: 2, end: 21, isTransient: 0, LM: 2, tfSelect: 1, prefillBits: 24, prefillPat: 0xFFFFFFFF, bufLen: 4, tfRes: allOne, note: "start>0 + tight budget"},
	)

	for ci, c := range cases {
		goRes := celt.TfEncode(c.start, c.end, c.isTransient, c.tfRes, c.LM, c.tfSelect,
			c.prefillBits, c.prefillPat, c.bufLen)
		cRes := tfRunEncode(c)

		if goRes.TellBefore != cRes.tellBefore {
			t.Fatalf("[%d %s] tell before tf_encode: Go=%d C=%d (LM=%d start=%d end=%d iT=%d sel=%d buf=%d prefill=%d)",
				ci, c.note, goRes.TellBefore, cRes.tellBefore, c.LM, c.start, c.end, c.isTransient, c.tfSelect, c.bufLen, c.prefillBits)
		}
		tfEqInts(t, "tf_encode tf_res (mutated in place)", goRes.TfRes, cRes.tfRes)
		if !bytes.Equal(goRes.Buf, cRes.buf) {
			t.Fatalf("[%d %s] encoded bytes: Go=%x C=%x (LM=%d start=%d end=%d iT=%d sel=%d buf=%d prefill=%d)",
				ci, c.note, goRes.Buf, cRes.buf, c.LM, c.start, c.end, c.isTransient, c.tfSelect, c.bufLen, c.prefillBits)
		}
		if goRes.Tell != cRes.tell {
			t.Fatalf("[%d %s] tell after tf_encode: Go=%d C=%d", ci, c.note, goRes.Tell, cRes.tell)
		}
		if goRes.Rng != cRes.rng {
			t.Fatalf("[%d %s] range coder rng: Go=%08x C=%08x", ci, c.note, goRes.Rng, cRes.rng)
		}
		if goRes.Val != cRes.val {
			t.Fatalf("[%d %s] range coder val: Go=%08x C=%08x", ci, c.note, goRes.Val, cRes.val)
		}
		if goRes.ErrFlag != cRes.errFlag {
			t.Fatalf("[%d %s] range coder error flag: Go=%d C=%d", ci, c.note, goRes.ErrFlag, cRes.errFlag)
		}

		// Non-vacuity bookkeeping (all derived from the C oracle).
		if tfForcedFired(c) {
			sawForced = true
		} else {
			sawAllCoded = true
		}
		if tfSelectWasCoded(c) {
			sawSelCoded = true
		} else {
			sawSelZeroed = true
		}
		if c.LM > 0 {
			sawRsv1 = true
		} else {
			sawRsv0 = true
		}
		if c.isTransient != 0 {
			sawTransient = true
		} else {
			sawNonTrans = true
		}
		if c.tfSelect != 0 {
			sawTfSelIn1 = true
		} else {
			sawTfSelIn0 = true
		}
		if c.start > 0 {
			sawStartPos = true
		}
		if cRes.errFlag != 0 {
			sawErr = true
		}
		sawLM[c.LM] = true
		// tf_changed is the OR of the coded tf_res values. A case whose coded
		// bands are all 0 has tf_changed=0.
		changed := false
		for i := c.start; i < c.end; i++ {
			if c.tfRes[i] != 0 {
				changed = true
			}
		}
		if changed {
			sawTfChanged1 = true
		} else {
			sawTfChanged0 = true
		}
	}

	if !sawForced {
		t.Fatal("non-vacuity: the unaffordable-band path (celt_encoder.c:849, tf_res[i]=curr) never fired")
	}
	if !sawAllCoded {
		t.Fatal("non-vacuity: no case coded every band (budget never sufficed)")
	}
	if !sawRsv1 {
		t.Fatal("non-vacuity: tf_select_rsv=1 never happened")
	}
	if !sawRsv0 {
		t.Fatal("non-vacuity: tf_select_rsv=0 (LM=0) never happened")
	}
	if !sawSelCoded {
		t.Fatal("non-vacuity: the tf_select bit (celt_encoder.c:856) was never actually coded")
	}
	if !sawSelZeroed {
		t.Fatal("non-vacuity: the tf_select=0 fallback (celt_encoder.c:858) never fired")
	}
	if !sawTransient || !sawNonTrans {
		t.Fatal("non-vacuity: isTransient did not take both values")
	}
	if !sawTfSelIn0 || !sawTfSelIn1 {
		t.Fatal("non-vacuity: the tf_select input did not take both values")
	}
	if !sawTfChanged0 {
		t.Fatal("non-vacuity: tf_changed=0 never happened")
	}
	if !sawTfChanged1 {
		t.Fatal("non-vacuity: tf_changed=1 never happened")
	}
	if !sawStartPos {
		t.Fatal("non-vacuity: start>0 never exercised")
	}
	if !sawErr {
		t.Fatal("non-vacuity: the range coder never hit its storage limit (error flag never set)")
	}
	for LM := range sawLM {
		if !sawLM[LM] {
			t.Fatalf("non-vacuity: LM=%d never exercised", LM)
		}
	}
}
