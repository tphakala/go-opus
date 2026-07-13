//go:build refc

package oracle

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// CP8c wave 1. This file pins the pieces the encoder DRIVER will be assembled
// from, and the oracle machinery the driver test (wave 3) will be compared
// against:
//
//   - hysteresis_decision (bands.c:46), used at celt_encoder.c:2403 to update
//     st->intensity: a full differential sweep, Go vs C.
//   - compute_vbr (celt_encoder.c:1604): a full bit-exact differential sweep of
//     the Go port (internal/celt/celt_encoder_vbr.go) against the C, added in
//     wave 2.
//   - the encoder HANDLE (create / configure / encode / dump-state / destroy):
//     cross-checked byte-for-byte against the already-trusted CBR encoder in
//     celtdec_shim.h, and used to prove the full state dump reads every field of
//     the canonical order, including the six VBR/stereo fields that were missing
//     from celt.EncoderState until now (a broken VBR loop used to pass a
//     state-hash comparison silently).

const vbrFrameSize = 960 // LM=3 at 48 kHz

// --- hysteresis_decision ----------------------------------------------------

// intensityThresholds / intensityHisteresis are the ONLY tables
// hysteresis_decision is called with in this build (celt_encoder.c:2393-2397,
// used at :2403 for st->intensity). The spread/tapset tables at :2329-2332 sit
// inside an `#if 0` block and are dead. Both are opus_val16.
var (
	intensityThresholds = []int16{1, 2, 3, 4, 5, 6, 7, 8, 16, 24, 36, 44, 50, 56, 62, 67, 72, 79, 88, 106, 134}
	intensityHisteresis = []int16{1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 3, 3, 4, 5, 6, 8, 8}
)

// TestCeltencHysteresisDecisionVsC drives hysteresis_decision (bands.c:46)
// through the live intensity table plus synthetic tables, every prev in [0,N],
// and a val sweep covering below-first-threshold, exactly-on-threshold, inside
// each hysteresis band, above-last-threshold and the int16 extremes.
//
// The "promotion" table is the important one: C's usual arithmetic conversions
// evaluate thresholds[prev]+hysteresis[prev] (and the matching subtraction) in
// int, so 32757+32767 does NOT wrap to a negative int16. A Go port that keeps
// those sums in int16 diverges exactly there.
func TestCeltencHysteresisDecisionVsC(t *testing.T) {
	tables := []struct {
		name       string
		thresholds []int16
		hysteresis []int16
	}{
		{"intensity", intensityThresholds, intensityHisteresis},
		{"single", []int16{0}, []int16{5}},
		{"negative", []int16{-1000, 500}, []int16{300, 200}},
		{"hysteresis-wider-than-gaps", []int16{10, 20, 30}, []int16{100, 100, 100}},
		{"promotion", []int16{math.MinInt16, 0, math.MaxInt16 - 10}, []int16{32767, 1, 32767}},
	}

	var sawZero, sawN, sawClampUp, sawClampDown, sawNoClamp bool

	r := rand.New(rand.NewSource(0x8C1A))
	for _, tb := range tables {
		N := len(tb.thresholds)
		// Build the val sweep: every threshold and its immediate neighbourhood,
		// every hysteresis boundary, the int16 extremes, plus random values.
		var vals []int
		for i := 0; i < N; i++ {
			th := int(tb.thresholds[i])
			hy := int(tb.hysteresis[i])
			vals = append(vals, th-hy-1, th-hy, th-hy+1, th-1, th, th+1, th+hy-1, th+hy, th+hy+1)
		}
		vals = append(vals, math.MinInt16, math.MinInt16+1, -1, 0, 1, math.MaxInt16-1, math.MaxInt16)
		for i := 0; i < 256; i++ {
			vals = append(vals, r.Intn(1<<17)-(1<<16)) // spills past int16 to exercise the cast
		}

		for _, v := range vals {
			for prev := 0; prev <= N; prev++ {
				want := cCeltencHysteresisDecision(v, tb.thresholds, tb.hysteresis, N, prev)
				// The Go seam takes the already-truncated opus_val16, exactly as
				// the C call site does ((opus_val16)(equiv_rate/1000) at :2403).
				got := celt.HysteresisDecision(int16(v), tb.thresholds, tb.hysteresis, N, prev)
				if got != want {
					t.Fatalf("%s: hysteresis_decision(val=%d (int16 %d), N=%d, prev=%d) Go=%d C=%d",
						tb.name, v, int16(v), N, prev, got, want)
				}
				switch {
				case got == 0:
					sawZero = true
				case got == N:
					sawN = true
				}
				// Non-vacuity: prove both hysteresis clamps fired at least once.
				// Recompute the unclamped index the way the C's first loop does.
				raw := N
				for i := 0; i < N; i++ {
					if int16(v) < tb.thresholds[i] {
						raw = i
						break
					}
				}
				switch {
				case raw > prev && got == prev:
					sawClampDown = true // i>prev clamped back to prev
				case raw < prev && got == prev:
					sawClampUp = true // i<prev clamped back to prev
				case raw == got && raw != prev:
					sawNoClamp = true
				}
			}
		}
	}

	if !sawZero || !sawN {
		t.Fatalf("vacuous sweep: index 0 seen=%v, index N seen=%v", sawZero, sawN)
	}
	if !sawClampDown || !sawClampUp || !sawNoClamp {
		t.Fatalf("vacuous sweep: clamp(i>prev) seen=%v, clamp(i<prev) seen=%v, unclamped seen=%v",
			sawClampDown, sawClampUp, sawNoClamp)
	}
}

// --- compute_vbr: bit-exact differential sweep (CP8c wave 2) -----------------

// vbrCase is one compute_vbr argument set (celt_encoder.c:1604). pitchChange is
// passed to the C only: under DISABLE_FLOAT_API it is `(void)pitch_change;`
// (:1672) and so cannot affect the result, which is why the Go port drops it.
// The sweep proves that by re-running the C with the bit flipped.
type vbrCase struct {
	baseTarget      int32
	LM              int
	bitrate         int32 // the caller's equiv_rate (:2458)
	lastCodedBands  int   // 0 means "use nbEBands" (:1622)
	C               int
	intensity       int
	constrainedVbr  int
	stereoSaving    int16 // opus_val16
	totBoost        int
	tfEstimate      int16 // opus_val16
	maxDepth        int32 // celt_glog, Q24
	lfe             int
	hasSurroundMask int
	surroundMasking int32 // celt_glog, Q24
	temporalVbr     int32 // celt_glog, Q24
	pitchChange     int
}

// vbrCoverage is the non-vacuity ledger: every documented branch and every clamp
// of celt_encoder.c:1604-1716 must be seen firing BOTH ways over the sweep, or
// the test fails as vacuous.
type vbrCoverage struct {
	mono, stereo             bool // C==1 / C==2 (:1635)
	ssClamped, ssUnclamped   bool // the MIN16 at :1644
	fracWon, savingWon       bool // the MIN32 at :1646
	ssWrapped                bool // stereo_saving-QCONST16(.1,8) wrapped past INT16_MIN
	tfWrapped                bool // tf_estimate-tf_calibration wrapped past INT16_MIN
	surroundOn, surroundOff  bool // the has_surround_mask && !lfe block (:1675)
	surrQuarter, surrTarget  bool // the IMAX at :1679
	lfeOn, lfeOff            bool
	lfeMasksSurround         bool // lfe==1 with has_surround_mask==1: block skipped
	floorQuarter, floorDepth bool // the IMAX at :1691
	floorApplied, floorFree  bool // the IMIN at :1692
	constrainedOn, constrOff bool // the constrained-VBR block (:1698)
	temporalOn, temporalOff  bool // the temporal-VBR block (:1703)
	amountZero, amountMax    bool // the IMAX(0,.) / IMIN(32000,.) clamps at :1707
	amountMid                bool
	tvbrNeg, tvbrPos         bool // temporal_vbr of both signs reached tvbr_factor
	doubleCapped, uncapped   bool // the IMIN at :1713
	negTarget                bool // a negative target reached the /4 vs >>2 divides
	minTarget, maxTarget     int32
	seen                     map[int32]bool
}

func (cov *vbrCoverage) note(tc vbrCase, target int32, w celt.ComputeVBRWitness) {
	if cov.seen == nil {
		cov.seen = map[int32]bool{}
		cov.minTarget, cov.maxTarget = target, target
	}
	cov.seen[target] = true
	if target < cov.minTarget {
		cov.minTarget = target
	}
	if target > cov.maxTarget {
		cov.maxTarget = target
	}
	switch tc.C {
	case 1:
		cov.mono = true
	case 2:
		cov.stereo = true
	}
	if w.StereoBlock {
		if w.StereoSavingClamped {
			cov.ssClamped = true
		} else {
			cov.ssUnclamped = true
		}
		if w.StereoFracWon {
			cov.fracWon = true
		} else {
			cov.savingWon = true
		}
		// QCONST16(0.1f,8) == 26: a stereo_saving below -32742 makes the C's
		// int-valued subtraction fall out of opus_val16 and wrap positive inside
		// MULT16_16 (the sign of the stereo saving flips).
		if int32(min(tc.stereoSaving, 256))-26 < math.MinInt16 {
			cov.ssWrapped = true
		}
	}
	// QCONST16(0.044f,14) == 721, same wrap hazard at :1653.
	if int32(tc.tfEstimate)-721 < math.MinInt16 {
		cov.tfWrapped = true
	}
	if w.SurroundBlock {
		cov.surroundOn = true
		if w.SurroundQuarterWon {
			cov.surrQuarter = true
		} else {
			cov.surrTarget = true
		}
	} else {
		cov.surroundOff = true
		if tc.hasSurroundMask == 1 && tc.lfe == 1 {
			cov.lfeMasksSurround = true
		}
	}
	if tc.lfe == 1 {
		cov.lfeOn = true
	} else {
		cov.lfeOff = true
	}
	if w.FloorQuarterWon {
		cov.floorQuarter = true
	} else {
		cov.floorDepth = true
	}
	if w.FloorApplied {
		cov.floorApplied = true
	} else {
		cov.floorFree = true
	}
	if w.ConstrainedBlock {
		cov.constrainedOn = true
	} else {
		cov.constrOff = true
	}
	if w.TemporalBlock {
		cov.temporalOn = true
		switch {
		case 96000-tc.bitrate <= 0:
			cov.amountZero = true
		case 96000-tc.bitrate >= 32000:
			cov.amountMax = true
		default:
			cov.amountMid = true
		}
		if w.TvbrFactor < 0 {
			cov.tvbrNeg = true
		}
		if w.TvbrFactor > 0 {
			cov.tvbrPos = true
		}
	} else {
		cov.temporalOff = true
	}
	if w.DoubleCapped {
		cov.doubleCapped = true
	} else {
		cov.uncapped = true
	}
	if target < 0 {
		cov.negTarget = true
	}
}

func (cov *vbrCoverage) check(t *testing.T) {
	t.Helper()
	flags := []struct {
		name string
		got  bool
	}{
		{"C==1", cov.mono},
		{"C==2", cov.stereo},
		{"stereo_saving MIN16 clamped (:1644)", cov.ssClamped},
		{"stereo_saving MIN16 not clamped (:1644)", cov.ssUnclamped},
		{"MIN32 max_frac side won (:1646)", cov.fracWon},
		{"MIN32 stereo_saving side won (:1646)", cov.savingWon},
		{"stereo_saving-QCONST16(.1,8) wrapped out of opus_val16 (:1647)", cov.ssWrapped},
		{"tf_estimate-tf_calibration wrapped out of opus_val16 (:1653)", cov.tfWrapped},
		{"surround block ran (:1675)", cov.surroundOn},
		{"surround block skipped (:1675)", cov.surroundOff},
		{"lfe==1 suppressed the surround block (:1675)", cov.lfeMasksSurround},
		{"lfe==1", cov.lfeOn},
		{"lfe==0", cov.lfeOff},
		{"IMAX target/4 won (:1679)", cov.surrQuarter},
		{"IMAX surround_target won (:1679)", cov.surrTarget},
		{"IMAX target>>2 won (:1691)", cov.floorQuarter},
		{"IMAX floor_depth won (:1691)", cov.floorDepth},
		{"floor_depth clamped target (:1692)", cov.floorApplied},
		{"floor_depth did not clamp target (:1692)", cov.floorFree},
		{"constrained-VBR block ran (:1698)", cov.constrainedOn},
		{"constrained-VBR block skipped (:1698)", cov.constrOff},
		{"temporal-VBR block ran (:1703)", cov.temporalOn},
		{"temporal-VBR block skipped (:1703)", cov.temporalOff},
		{"amount clamped to 0 (:1707)", cov.amountZero},
		{"amount clamped to 32000 (:1707)", cov.amountMax},
		{"amount unclamped (:1707)", cov.amountMid},
		{"tvbr_factor < 0 (:1708)", cov.tvbrNeg},
		{"tvbr_factor > 0 (:1708)", cov.tvbrPos},
		{"2*base_target cap fired (:1713)", cov.doubleCapped},
		{"2*base_target cap did not fire (:1713)", cov.uncapped},
		{"negative target (exercises /4 vs >>2)", cov.negTarget},
	}
	for _, f := range flags {
		if !f.got {
			t.Fatalf("vacuous sweep: branch never exercised: %s", f.name)
		}
	}
	// The output must actually move: a compute_vbr that returned a constant would
	// satisfy every equality check above.
	if len(cov.seen) < 5000 {
		t.Fatalf("vacuous sweep: compute_vbr produced only %d distinct targets", len(cov.seen))
	}
	if cov.minTarget >= 0 || cov.maxTarget < 500000 {
		t.Fatalf("vacuous sweep: target range [%d, %d] is too narrow", cov.minTarget, cov.maxTarget)
	}
	t.Logf("coverage: %d distinct targets over [%d, %d], all %d branches fired",
		len(cov.seen), cov.minTarget, cov.maxTarget, len(flags))
}

// runVbrCase runs one case through both implementations and requires bit-exact
// agreement. It also re-runs the C with pitch_change flipped, which must not
// change the target (:1672).
func runVbrCase(t *testing.T, tc vbrCase, cov *vbrCoverage) {
	t.Helper()
	want := cCeltencComputeVbr(tc.baseTarget, tc.LM, tc.bitrate, tc.lastCodedBands, tc.C,
		tc.intensity, tc.constrainedVbr, tc.stereoSaving, tc.totBoost, tc.tfEstimate,
		tc.pitchChange, tc.maxDepth, tc.lfe, tc.hasSurroundMask, tc.surroundMasking,
		tc.temporalVbr)
	got, w := celt.ComputeVBR(tc.baseTarget, tc.LM, tc.bitrate, tc.lastCodedBands, tc.C,
		tc.intensity, tc.constrainedVbr, tc.stereoSaving, tc.totBoost, tc.tfEstimate,
		tc.maxDepth, tc.lfe, tc.hasSurroundMask, tc.surroundMasking, tc.temporalVbr)
	if got != want {
		t.Fatalf("compute_vbr Go=%d C=%d for %+v\n  witness: %+v", got, want, tc, w)
	}
	// pitch_change is dead in this build; prove the Go port may drop it.
	flipped := cCeltencComputeVbr(tc.baseTarget, tc.LM, tc.bitrate, tc.lastCodedBands, tc.C,
		tc.intensity, tc.constrainedVbr, tc.stereoSaving, tc.totBoost, tc.tfEstimate,
		1-tc.pitchChange, tc.maxDepth, tc.lfe, tc.hasSurroundMask, tc.surroundMasking,
		tc.temporalVbr)
	if flipped != want {
		t.Fatalf("compute_vbr depends on pitch_change (%d vs %d) for %+v", flipped, want, tc)
	}
	cov.note(tc, got, w)
}

// The parameter pools. Each is a corner list: the values that sit exactly on a
// clamp, a wrap or a comparison boundary in celt_encoder.c:1604-1716, plus a few
// ordinary ones. The sweep draws from these and from the full random ranges.
var (
	// base_target is `vbr_rate - ((40*C+20)<<BITRES)` (:2448) and genuinely goes
	// NEGATIVE at low rates, which is the only way to tell `target/4` (:1679,
	// truncates toward zero) apart from `target>>2` (:1691, floors).
	vbrBaseTargets = []int32{-1000000, -100000, -8003, -4001, -1, 0, 1, 3, 100, 1000,
		8000, 40000, 81600, 200000, 500000, 1000000}
	// 96000-bitrate is clamped to [0, 32000] at :1707, so 64000 and 96000 are the
	// two boundaries.
	vbrBitrates = []int32{0, 1, 6000, 32000, 63999, 64000, 64001, 90000, 95999, 96000,
		96001, 128000, 300000, 510000}
	// QCONST16(.2f,14) == 3277 is the temporal-VBR gate (:1703);
	// QCONST16(.044f,14) == 721 is tf_calibration (:1653). -32768 is the opus_val16
	// extreme that makes tf_estimate-tf_calibration wrap.
	vbrTfEstimates = []int16{math.MinInt16, -32048, -32047, -1000, -1, 0, 1, 720, 721, 722,
		3276, 3277, 3278, 8192, 16384, 32767}
	// QCONST16(1.f,8) == 256 is the MIN16 clamp at :1644; QCONST16(0.1f,8) == 26
	// makes stereo_saving-26 wrap out of opus_val16 below -32742.
	vbrStereoSavings = []int16{math.MinInt16, -32743, -32742, -32741, -20000, -1000, -256,
		-26, -1, 0, 1, 100, 255, 256, 257, 1000, 32767}
	vbrTotBoosts = []int{0, 1, 19, 100, 500, 2000, 5000, 20000}
	// maxDepth comes out of dynalloc_analysis, which seeds it to -GCONST(31.9f)
	// (:1068) and MAXGs it with bandLogE-noise_floor, so the reachable range is
	// about [-31.9, +40] dB in Q24. The pool spans that and a bit beyond (-32<<24
	// sits just below the seed).
	vbrMaxDepths = []int32{-32 << 24, -12 << 24, -1 << 24, -1, 0, 1,
		1 << 24, 4 << 24, 12 << 24, 20 << 24, 31 << 24, 40 << 24}
	// surround_masking is <= 0 in the encoder (it is a masking allowance), but the
	// pool sweeps both signs; temporal_vbr takes both signs by construction (:2186).
	vbrSurroundMaskings = []int32{-40 << 24, -20 << 24, -5 << 24, -1 << 24, -(1 << 20), 0,
		1 << 20, 1 << 24, 5 << 24, 20 << 24}
	vbrTemporalVbrs = []int32{-16 << 24, -8 << 24, -2 << 24, -1 << 24, -(1 << 22), 0,
		1 << 22, 1 << 24, 2 << 24, 8 << 24, 16 << 24}
)

// TestCeltencVbrVsC is the bit-exact differential sweep of compute_vbr
// (celt_encoder.c:1604-1716) against the C oracle. compute_vbr is a pure function
// of its arguments (it reads no st-> field), so a broad randomized sweep over the
// corner pools is the right shape of test; the vbrCoverage ledger then fails the
// test if any documented branch or clamp never fired.
//
// The sweep was mutation-tested: swapping `target/4` for `target>>2` at :1679,
// saturating instead of truncating any of the four opus_val16 hazards (:1647,
// :1653, :1677, :1708), dropping the C==2 coded_bins term (:1625) and reading
// eBands[nbEBands-1] instead of [nbEBands-2] (:1685) are each caught.
//
// ONE LINE IS NOT PINNED BY THIS TEST, and honesty demands saying so: the
// `target>>2` at :1691 cannot be told apart from `target/4`, because the IMAX it
// feeds is consumed by IMIN(target, floor_depth) and for target < 0 both forms
// stay >= target, so the IMIN returns target either way. Swapping them does NOT
// fail this sweep. The Go port keeps C's spelling; that choice rests on the
// source, not on this test.
func TestCeltencVbrVsC(t *testing.T) {
	var cov vbrCoverage
	r := rand.New(rand.NewSource(0x8C1B))

	pick := func(n int) int { return r.Intn(n) }
	for i := 0; i < 300000; i++ {
		tc := vbrCase{
			baseTarget:      vbrBaseTargets[pick(len(vbrBaseTargets))],
			LM:              pick(4),
			bitrate:         vbrBitrates[pick(len(vbrBitrates))],
			lastCodedBands:  pick(22), // 0 means nbEBands (:1622)
			C:               1 + pick(2),
			intensity:       pick(22),
			constrainedVbr:  pick(2),
			stereoSaving:    vbrStereoSavings[pick(len(vbrStereoSavings))],
			totBoost:        vbrTotBoosts[pick(len(vbrTotBoosts))],
			tfEstimate:      vbrTfEstimates[pick(len(vbrTfEstimates))],
			maxDepth:        vbrMaxDepths[pick(len(vbrMaxDepths))],
			lfe:             pick(2),
			hasSurroundMask: pick(2),
			surroundMasking: vbrSurroundMaskings[pick(len(vbrSurroundMaskings))],
			temporalVbr:     vbrTemporalVbrs[pick(len(vbrTemporalVbrs))],
			pitchChange:     pick(2),
		}
		// Every third case draws the continuous parameters from their full ranges
		// instead of the corner pools, so the sweep is not only corners.
		if i%3 == 0 {
			tc.baseTarget = int32(r.Intn(1200001) - 200000)
			tc.bitrate = int32(r.Intn(520000))
			tc.stereoSaving = int16(r.Intn(1 << 16))
			tc.tfEstimate = int16(r.Intn(1 << 16))
			tc.totBoost = r.Intn(20001)
			// The full reachable maxDepth range, continuous, in Q24.
			tc.maxDepth = int32(r.Intn(72<<24) - (32 << 24))
			tc.surroundMasking = int32(r.Intn(60<<24) - (40 << 24))
			tc.temporalVbr = int32(r.Intn(32<<24) - (16 << 24))
		}
		runVbrCase(t, tc, &cov)
	}

	// Targeted cases for the corners a uniform draw is unlikely to hit often, so
	// the coverage ledger below cannot be satisfied by luck alone.
	targeted := []vbrCase{
		// target/4 wins the surround IMAX (:1679): a deeply negative masking
		// pushes surround_target far below a quarter of a positive target.
		{baseTarget: 80000, LM: 3, bitrate: 96000, C: 2, intensity: 21, stereoSaving: 256,
			maxDepth: 20 << 24, hasSurroundMask: 1, surroundMasking: -40 << 24},
		// surround_target wins the same IMAX with a positive masking.
		{baseTarget: 80000, LM: 3, bitrate: 96000, C: 2, intensity: 21, stereoSaving: 256,
			maxDepth: 20 << 24, hasSurroundMask: 1, surroundMasking: 5 << 24},
		// lfe suppresses the surround block and re-enables constrained VBR (:1675, :1698).
		{baseTarget: 80000, LM: 3, bitrate: 96000, C: 1, maxDepth: 20 << 24, lfe: 1,
			hasSurroundMask: 1, constrainedVbr: 1, surroundMasking: -20 << 24},
		// target>>2 wins the floor IMAX (:1691): maxDepth at its floor.
		{baseTarget: 200000, LM: 3, bitrate: 6000, C: 1, maxDepth: -32 << 24,
			tfEstimate: 3000, temporalVbr: 8 << 24},
		// floor_depth clamps target hard (:1692).
		{baseTarget: 500000, LM: 0, bitrate: 6000, C: 1, maxDepth: 1, tfEstimate: 3000},
		// The 2*base_target cap (:1713) with a tiny base and a huge dynalloc boost.
		{baseTarget: 100, LM: 3, bitrate: 6000, C: 1, totBoost: 20000, tfEstimate: 16384,
			maxDepth: 40 << 24},
		// stereo_saving at INT16_MIN: the :1647 subtraction wraps positive.
		{baseTarget: 80000, LM: 3, bitrate: 32000, C: 2, intensity: 21,
			stereoSaving: math.MinInt16, maxDepth: 20 << 24},
		// tf_estimate at INT16_MIN: the :1653 subtraction wraps positive.
		{baseTarget: 80000, LM: 3, bitrate: 32000, C: 1, tfEstimate: math.MinInt16,
			maxDepth: 20 << 24},
		// Negative base_target: the only way /4 (:1679) and >>2 (:1691) can differ.
		{baseTarget: -8003, LM: 1, bitrate: 6000, C: 2, intensity: 3, stereoSaving: 100,
			maxDepth: -1, hasSurroundMask: 1, surroundMasking: -1},
		{baseTarget: -8003, LM: 1, bitrate: 6000, C: 2, intensity: 3, stereoSaving: 100,
			maxDepth: -1, constrainedVbr: 1, temporalVbr: -8 << 24},
	}
	for _, tc := range targeted {
		runVbrCase(t, tc, &cov)
	}

	cov.check(t)
}

// --- the encoder handle ------------------------------------------------------

// vbrPCM builds an interleaved int16 test signal: a couple of tones plus noise,
// with a transient burst in the middle frames.
func vbrPCM(r *rand.Rand, frames, frameSize, channels int, transientFrame int) []int16 {
	pcm := make([]int16, frames*frameSize*channels)
	ph := 0.0
	for f := 0; f < frames; f++ {
		amp := 6000.0
		if f == transientFrame {
			amp = 26000.0
		}
		for n := 0; n < frameSize; n++ {
			ph += 2 * math.Pi * 440 / 48000
			v := amp * math.Sin(ph)
			if f == transientFrame && n > frameSize/2 {
				v = amp * (2*r.Float64() - 1)
			}
			for c := 0; c < channels; c++ {
				s := v*(1.0-0.3*float64(c)) + 300*r.NormFloat64()
				i := (f*frameSize+n)*channels + c
				pcm[i] = int16(math.Max(-32768, math.Min(32767, s)))
			}
		}
	}
	return pcm
}

// TestCeltencHandleMatchesRawEncoder cross-checks the NEW handle
// (oracle_celtenc_h_create + configure) against the ALREADY-TRUSTED CBR encoder
// in celtdec_shim.h (oracle_celtenc_create), which the CELT decoder differential
// test has used since CP2. With celtencDefaultConfig (which is exactly the
// post-celt_encoder_init state) the two must produce byte-identical packets and
// the same final range on every frame. This is what proves create/configure does
// not perturb the encoder.
func TestCeltencHandleMatchesRawEncoder(t *testing.T) {
	const frames = 8
	for _, channels := range []int{1, 2} {
		for _, complexity := range []int{0, 5, 10} {
			r := rand.New(rand.NewSource(0x8C1C))
			pcm := vbrPCM(r, frames, vbrFrameSize, channels, 4)

			ref, err := newCCELTEncoder(channels, complexity)
			if err != nil {
				t.Fatalf("newCCELTEncoder: %v", err)
			}
			h, err := newCCeltencHandle(channels)
			if err != nil {
				ref.Close()
				t.Fatalf("newCCeltencHandle: %v", err)
			}
			cfg := celtencDefaultConfig(channels)
			cfg.Complexity = complexity
			if err := h.Configure(cfg); err != nil {
				ref.Close()
				h.Close()
				t.Fatalf("Configure: %v", err)
			}

			for f := 0; f < frames; f++ {
				lo := f * vbrFrameSize * channels
				frame := pcm[lo : lo+vbrFrameSize*channels]
				const nbBytes = 120

				want, err := ref.Encode(frame, vbrFrameSize, nbBytes)
				if err != nil {
					t.Fatalf("ch=%d cx=%d frame %d: ref encode: %v", channels, complexity, f, err)
				}
				ret, got, _ := h.Encode(frame, vbrFrameSize, nbBytes)
				if ret < 0 {
					t.Fatalf("ch=%d cx=%d frame %d: handle encode returned %d", channels, complexity, f, ret)
				}
				if len(got) != len(want) {
					t.Fatalf("ch=%d cx=%d frame %d: packet len handle=%d ref=%d",
						channels, complexity, f, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("ch=%d cx=%d frame %d: packet byte %d handle=%#02x ref=%#02x",
							channels, complexity, f, i, got[i], want[i])
					}
				}
			}
			ref.Close()
			h.Close()
		}
	}
}

// TestCeltencHandleStateDump proves the state dump reads every field of the
// canonical order and that the cross-frame state actually EVOLVES, in particular
// the six fields (vbr_reservoir, vbr_drift, vbr_offset, vbr_count,
// stereo_saving, intensity) that celt.EncoderState omitted before CP8c. It also
// proves the hash now covers them: perturbing any one of the six must change
// EncoderState.Hash().
func TestCeltencHandleStateDump(t *testing.T) {
	const frames = 12
	channels := 2
	r := rand.New(rand.NewSource(0x8C1D))
	pcm := vbrPCM(r, frames, vbrFrameSize, channels, 5)

	h, err := newCCeltencHandle(channels)
	if err != nil {
		t.Fatalf("newCCeltencHandle: %v", err)
	}
	defer h.Close()

	// Constrained VBR at a real bitrate: this is the configuration that runs the
	// whole vbr_reservoir / vbr_drift / vbr_offset / vbr_count loop
	// (celt_encoder.c:2435-2533).
	cfg := celtencDefaultConfig(channels)
	cfg.VBR = 1
	cfg.VBRConstraint = 1
	cfg.Bitrate = 64000
	cfg.Complexity = 10
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	init := h.State()
	if got := len(init.InMem); got != channels*cCeltencOverlap() {
		t.Fatalf("in_mem dump length %d, want %d", got, channels*cCeltencOverlap())
	}
	if got := len(init.PrefilterMem); got != channels*cCeltencMaxperiod() {
		t.Fatalf("prefilter_mem dump length %d, want %d", got, channels*cCeltencMaxperiod())
	}
	if got := len(init.OldLogE); got != channels*cCeltencNbebands() {
		t.Fatalf("oldLogE dump length %d, want %d", got, channels*cCeltencNbebands())
	}
	// celt_encoder_init seeds oldLogE/oldLogE2 to -GCONST(28) = -(28<<24).
	if init.OldLogE[0] != -(28 << 24) {
		t.Fatalf("oldLogE[0] after init = %d, want %d", init.OldLogE[0], -(28 << 24))
	}
	if init.VbrCount != 0 || init.VbrReservoir != 0 || init.VbrDrift != 0 {
		t.Fatalf("VBR state not zero after init: %+v", init)
	}

	var sawVbrCount, sawReservoir, sawDrift, sawOffset, sawIntensity, sawStereoSaving bool
	prev := init
	for f := 0; f < frames; f++ {
		lo := f * vbrFrameSize * channels
		ret, pkt, rng := h.Encode(pcm[lo:lo+vbrFrameSize*channels], vbrFrameSize, 1275)
		if ret <= 0 {
			t.Fatalf("frame %d: celt_encode_with_ec returned %d", f, ret)
		}
		if len(pkt) != ret {
			t.Fatalf("frame %d: packet len %d != ret %d", f, len(pkt), ret)
		}
		st := h.State()
		if st.Rng != rng {
			t.Fatalf("frame %d: dumped rng %#x != OPUS_GET_FINAL_RANGE %#x", f, st.Rng, rng)
		}
		if st.VbrCount != prev.VbrCount {
			sawVbrCount = true
		}
		if st.VbrReservoir != prev.VbrReservoir {
			sawReservoir = true
		}
		if st.VbrDrift != prev.VbrDrift {
			sawDrift = true
		}
		if st.VbrOffset != prev.VbrOffset {
			sawOffset = true
		}
		if st.Intensity != prev.Intensity {
			sawIntensity = true
		}
		if st.StereoSaving != prev.StereoSaving {
			sawStereoSaving = true
		}
		if st.Hash() == prev.Hash() {
			t.Fatalf("frame %d: state hash did not change across a frame", f)
		}
		prev = st
	}

	// NON-VACUITY: every one of the six formerly-unhashed fields must have moved.
	if !sawVbrCount || !sawReservoir || !sawDrift || !sawOffset {
		t.Fatalf("VBR loop never exercised: count=%v reservoir=%v drift=%v offset=%v",
			sawVbrCount, sawReservoir, sawDrift, sawOffset)
	}
	if !sawIntensity || !sawStereoSaving {
		t.Fatalf("stereo state never exercised: intensity=%v stereo_saving=%v",
			sawIntensity, sawStereoSaving)
	}

	// The hash must be sensitive to each of the six. Perturb one field at a time
	// and require the hash to move: this is the regression guard for the hole
	// CP8c closed.
	base := prev.Hash()
	mutators := []struct {
		name string
		mut  func(s *celt.EncoderState)
	}{
		{"VbrReservoir", func(s *celt.EncoderState) { s.VbrReservoir++ }},
		{"VbrDrift", func(s *celt.EncoderState) { s.VbrDrift++ }},
		{"VbrOffset", func(s *celt.EncoderState) { s.VbrOffset++ }},
		{"VbrCount", func(s *celt.EncoderState) { s.VbrCount++ }},
		{"StereoSaving", func(s *celt.EncoderState) { s.StereoSaving++ }},
		{"Intensity", func(s *celt.EncoderState) { s.Intensity++ }},
	}
	for _, m := range mutators {
		s := prev
		m.mut(&s)
		if s.Hash() == base {
			t.Fatalf("EncoderState.Hash() ignores %s: a broken VBR loop would pass silently", m.name)
		}
	}
}

// TestCeltencHandleDeterministic drives two independently created handles with
// the same config and input and requires identical packets, rng and state on
// every frame, across CBR / VBR / constrained-VBR / LFE / forced-intra configs.
// It is the harness self-test the wave-3 driver comparison depends on.
func TestCeltencHandleDeterministic(t *testing.T) {
	const frames = 6
	cfgs := []struct {
		name     string
		channels int
		mut      func(c *celtencConfig)
		nbBytes  int
	}{
		{"cbr-mono", 1, func(c *celtencConfig) {}, 96},
		{"vbr-unconstrained-stereo", 2, func(c *celtencConfig) {
			c.VBR, c.VBRConstraint, c.Bitrate = 1, 0, 96000
		}, 1275},
		{"vbr-constrained-stereo", 2, func(c *celtencConfig) {
			c.VBR, c.VBRConstraint, c.Bitrate = 1, 1, 32000
		}, 1275},
		{"lfe-mono", 1, func(c *celtencConfig) { c.LFE, c.Bitrate, c.VBR = 1, 24000, 1 }, 1275},
		{"forced-intra-mono", 1, func(c *celtencConfig) { c.ForceIntra, c.DisablePrefilter = 1, 1 }, 96},
		{"narrow-band-stereo", 2, func(c *celtencConfig) { c.Start, c.End, c.DisableInv = 1, 17, 1 }, 96},
	}

	for _, tc := range cfgs {
		r := rand.New(rand.NewSource(0x8C1E))
		pcm := vbrPCM(r, frames, vbrFrameSize, tc.channels, 3)

		mk := func() *cCeltencHandle {
			h, err := newCCeltencHandle(tc.channels)
			if err != nil {
				t.Fatalf("%s: newCCeltencHandle: %v", tc.name, err)
			}
			cfg := celtencDefaultConfig(tc.channels)
			tc.mut(&cfg)
			if err := h.Configure(cfg); err != nil {
				h.Close()
				t.Fatalf("%s: Configure: %v", tc.name, err)
			}
			return h
		}
		a, b := mk(), mk()

		for f := 0; f < frames; f++ {
			lo := f * vbrFrameSize * tc.channels
			frame := pcm[lo : lo+vbrFrameSize*tc.channels]
			retA, pktA, rngA := a.Encode(frame, vbrFrameSize, tc.nbBytes)
			retB, pktB, rngB := b.Encode(frame, vbrFrameSize, tc.nbBytes)
			if retA != retB || rngA != rngB {
				t.Fatalf("%s frame %d: ret %d/%d rng %#x/%#x", tc.name, f, retA, retB, rngA, rngB)
			}
			if retA <= 0 {
				t.Fatalf("%s frame %d: celt_encode_with_ec returned %d", tc.name, f, retA)
			}
			for i := range pktA {
				if pktA[i] != pktB[i] {
					t.Fatalf("%s frame %d: packet byte %d differs (%#02x vs %#02x)",
						tc.name, f, i, pktA[i], pktB[i])
				}
			}
			if a.State().Hash() != b.State().Hash() {
				t.Fatalf("%s frame %d: state hash differs across identical encoders", tc.name, f)
			}
		}
		a.Close()
		b.Close()
	}
}
