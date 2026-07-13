//go:build refc

package oracle

import (
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This differential test pins the pure-Go CELT encoder DYNALLOC unit
// (internal/celt/celt_encoder_dynalloc.go: median_of_5, median_of_3,
// dynalloc_analysis; CP8b unit DYNALLOC) to the pinned libopus C oracle
// (celt/celt_encoder.c:990/:1029/:1049, driven through celtenc_shim.h). Both
// sides are fed the SAME inputs and every DEFINED output is asserted bit-identical.
//
// dynalloc_analysis is stateless (no cross-frame state), so it is driven as a
// randomized sweep over a table of frame configs rather than a sequence. What it
// DOES have is a structural branch and a pile of input-dependent sub-branches, so
// the table sweeps LM 0..3, C 1..2, start 0 and >0, several `end`s, isTransient,
// vbr/constrained_vbr, lfe, lsb_depth, the tone-compensation trigger, and
// effectiveBytes straddling the 30+5*LM dynalloc threshold. Non-vacuity flags
// below prove every documented branch actually fired.
//
// OUTPUT WINDOWS (asymmetric in the C, and load-bearing):
//   - offsets is OPUS_CLEAR'd over [0,nbEBands) at :1066  -> compare in full.
//   - spread_weight is written over [0,end)      at :1112  -> compare [0,end).
//   - importance is written ONLY over [start,end) (:1187 / :1268) -> compare
//     [start,end) ONLY. Outside that window the C leaves the caller's buffer
//     untouched (in the real encoder that is uninitialized ALLOC stack), so
//     comparing [0,nbEBands) would be asserting on undefined data.

// eqDynallocInts fails the test if two []int slices differ over [lo,hi).
func eqDynallocInts(t *testing.T, label string, got, want []int, lo, hi int) {
	t.Helper()
	for i := lo; i < hi; i++ {
		if got[i] != want[i] {
			t.Fatalf("%s: mismatch at %d (window [%d,%d)): Go=%d C=%d",
				label, i, lo, hi, got[i], want[i])
		}
	}
}

// dynEBands is the frozen 48 kHz / 960 mode's eBands table (celt/static_modes_fixed.h
// eBands5ms, nbEBands+1 = 22 entries). The test only needs it to classify the
// dynalloc `width` branch (:1241-1252) for the non-vacuity flags.
var dynEBands = [22]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 14, 16, 20, 24, 28, 34, 40, 48, 60, 78, 100}

// dbQ24 converts a dB value to celt_glog (Q(DB_SHIFT=24)).
func dbQ24(x float64) int32 { return int32(x * 16777216) }

// dynSpectrum builds one C*nbEBands celt_glog band-log-energy contour. shape
// selects the character of the spectrum, which is what steers the interesting
// dynalloc branches:
//
//	"flat"        - almost no structure: follower tracks bandLogE, tiny boosts.
//	"tilt"        - monotonically falling: `last` stays low, backward pass bites.
//	"spiky"       - isolated loud bands: the median filter and the boost path bite.
//	"bandlimited" - loud low bands, dead high bands: deep masking -> spread_weight
//	                shifts of 1..5, and a big maxDepth-vs-sig spread.
//	"random"      - uniform noise over the full realistic range.
//	"quiet"       - every band far below the noise floor, so maxDepth is NEVER
//	                raised above its -GCONST(31.9f) initialiser (:1068). This is
//	                the only way to observe that constant at all.
//	"quirklast"   - channel 0 rises 1 dB/band (so `last` ends up at end-1) and
//	                channel 1 falls steeply with NO band ever rising by more than
//	                GCONST(.5f). `last` is declared OUTSIDE the per-channel loop in
//	                the C (:1123) and is NOT reset, so channel 1 inherits channel
//	                0's `last` and runs a backward pass it "should not" run. Needs
//	                a slope > 2 dB/band for the backward pass (f[i+1]+GCONST(2.f))
//	                to actually bite. Must be used with noJitter (bandLogE3 comes
//	                from bandLogE2, which must stay monotone).
func dynSpectrum(r *rand.Rand, shape string, nbEBands, channels int) []int32 {
	out := make([]int32, channels*nbEBands)
	slope := 4.0 + r.Float64()*4 // quirklast: 4..8 dB/band
	for c := 0; c < channels; c++ {
		base := 4 + r.Float64()*10
		for i := 0; i < nbEBands; i++ {
			var v float64
			switch shape {
			case "flat":
				v = base + r.Float64()*0.6 - 0.3
			case "tilt":
				v = base - 0.7*float64(i) + r.Float64()*1.5
			case "spiky":
				v = base - 0.2*float64(i) + r.Float64()*2
				if r.Intn(4) == 0 {
					v += 6 + r.Float64()*8
				}
			case "bandlimited":
				v = base + r.Float64()*2
				if i >= 9 {
					v -= 4 * float64(i-8)
				}
			case "quiet":
				v = -60 - r.Float64()*10
			case "quirklast":
				if c == 0 {
					v = float64(i) // strictly rising 1 dB/band > GCONST(.5f)
				} else {
					v = 24 - slope*float64(i) // strictly falling, never rises
				}
			default: // "random"
				v = -6 + r.Float64()*24
			}
			out[c*nbEBands+i] = dbQ24(v)
		}
	}
	return out
}

// dynJitter returns a copy of src with a small per-band perturbation, used to
// derive bandLogE2 / oldBandE from bandLogE the way the real encoder's
// pre-/post-transient energies differ.
func dynJitter(r *rand.Rand, src []int32, ampDB float64) []int32 {
	out := make([]int32, len(src))
	for i := range src {
		out[i] = src[i] + dbQ24((r.Float64()*2-1)*ampDB)
	}
	return out
}

type dynCfg struct {
	name string
	// Geometry.
	channels int // C (coded channels)
	LM       int
	start    int
	end      int
	// Encoder knobs.
	lsbDepth       int
	isTransient    int
	vbr            int
	constrainedVbr int
	lfe            int
	// effectiveBytes = 30 + 5*LM + effBytesDelta, so the sign of effBytesDelta
	// picks the side of the dynalloc threshold at :1121.
	effBytesDelta int
	// Tone compensation (:1206).
	toneFreq    int
	toneishness int32
	// Spectrum shape fed to dynSpectrum, and whether surround_dynalloc is nonzero.
	shape    string
	surround bool
	// noJitter makes bandLogE2 / oldBandE exact copies of bandLogE, which the
	// "quirklast" shape needs (bandLogE3 is copied from bandLogE2 at :1129, so
	// the monotonicity the shape encodes must survive into bandLogE2).
	noJitter bool
}

// qconst98Q29 is the C's QCONST32(.98f, 29) tone threshold (:1206), evaluated with
// the C's float32 literal semantics: 526133504. toneishness must be strictly
// greater than this for the tone-compensation block to run.
const qconst98Q29 = int32(526133504)

func dynCfgs() []dynCfg {
	return []dynCfg{
		// --- boost block ON (effectiveBytes >= 30+5*LM, !lfe) -------------------
		{"mono/LM3/full/vbr/spiky", 1, 3, 0, 21, 24, 0, 1, 0, 0, 200, 0, 0, "spiky", false, false},
		{"mono/LM3/full/vbr/transient", 1, 3, 0, 21, 24, 1, 1, 0, 0, 400, 0, 0, "spiky", false, false},
		{"stereo/LM3/full/vbr/spiky", 2, 3, 0, 21, 24, 0, 1, 0, 0, 600, 0, 0, "spiky", false, false},
		{"stereo/LM3/full/cbr", 2, 3, 0, 21, 24, 0, 0, 0, 0, 300, 0, 0, "spiky", false, false},
		{"stereo/LM3/full/cvbr", 2, 3, 0, 21, 16, 0, 1, 1, 0, 300, 0, 0, "spiky", false, false},
		{"stereo/LM3/full/cvbr/transient", 2, 3, 0, 21, 16, 1, 1, 1, 0, 300, 0, 0, "spiky", false, false},
		{"mono/LM0/full/vbr", 1, 0, 0, 21, 24, 0, 1, 0, 0, 60, 0, 0, "spiky", false, false},
		{"stereo/LM0/full/cbr", 2, 0, 0, 21, 24, 0, 0, 0, 0, 90, 0, 0, "tilt", false, false},
		{"mono/LM1/full/vbr", 1, 1, 0, 21, 24, 0, 1, 0, 0, 120, 0, 0, "random", false, false},
		{"stereo/LM2/full/vbr", 2, 2, 0, 21, 8, 0, 1, 0, 0, 250, 0, 0, "random", false, false},
		{"mono/LM2/bandlimited", 1, 2, 0, 21, 24, 0, 1, 0, 0, 250, 0, 0, "bandlimited", false, false},
		{"stereo/LM3/bandlimited", 2, 3, 0, 21, 24, 0, 1, 0, 0, 250, 0, 0, "bandlimited", false, false},
		{"mono/LM3/flat", 1, 3, 0, 21, 24, 0, 1, 0, 0, 250, 0, 0, "flat", false, false},
		// end variants (narrower coded bandwidth).
		{"mono/LM3/end13", 1, 3, 0, 13, 24, 0, 1, 0, 0, 200, 0, 0, "spiky", false, false},
		{"stereo/LM3/end17", 2, 3, 0, 17, 24, 0, 1, 0, 0, 200, 0, 0, "spiky", false, false},
		{"mono/LM2/end19", 1, 2, 0, 19, 16, 0, 1, 0, 0, 200, 0, 0, "tilt", false, false},
		// start>0 (the hybrid window): importance/offsets only defined on [start,end).
		{"stereo/LM3/start17", 2, 3, 17, 21, 24, 0, 1, 0, 0, 300, 0, 0, "spiky", false, false},
		{"mono/LM3/start17/cbr", 1, 3, 17, 21, 24, 0, 0, 0, 0, 300, 0, 0, "spiky", false, false},
		{"stereo/LM0/start17", 2, 0, 17, 21, 24, 0, 1, 0, 0, 100, 0, 0, "random", false, false},
		{"mono/LM2/start9", 1, 2, 9, 21, 24, 0, 1, 0, 0, 200, 0, 0, "spiky", false, false},
		// surround_dynalloc floor.
		{"stereo/LM3/surround", 2, 3, 0, 21, 24, 0, 1, 0, 0, 300, 0, 0, "tilt", true, false},
		{"mono/LM3/surround/start17", 1, 3, 17, 21, 24, 0, 1, 0, 0, 300, 0, 0, "flat", true, false},
		// tone compensation ON (toneishness > QCONST32(.98f,29)).
		// NOTE the "flat" shape here is load-bearing, not decorative: on a spiky
		// spectrum follower[i] is already at or above GCONST(4) before :1206, so the
		// MING(follower[i], GCONST(4)) clamp at :1238 swallows the entire tone boost
		// and freq_bin becomes unobservable in the outputs. On a flat spectrum
		// follower[i] ~= 0, so the +2/+1/+1/+.5 rings around freq_bin (:1213-1216)
		// land distinct values in distinct bands and offsets/tot_boost pin freq_bin
		// exactly. (Verified: with spiky spectra a wrong freq_bin shift survives.)
		{"mono/LM3/tone/low", 1, 3, 0, 21, 24, 0, 1, 0, 0, 300, 800, 530000000, "flat", false, false},
		{"stereo/LM3/tone/mid", 2, 3, 0, 21, 24, 0, 1, 0, 0, 300, 6000, 536870911, "flat", false, false},
		{"mono/LM3/tone/high", 1, 3, 0, 21, 24, 0, 1, 0, 0, 300, 24000, 528000000, "flat", false, false},
		{"mono/LM2/tone/b13", 1, 2, 0, 21, 24, 0, 1, 0, 0, 300, 3300, 530000000, "flat", false, false},
		{"mono/LM1/tone/b18", 1, 1, 0, 21, 24, 0, 1, 0, 0, 300, 11000, 530000000, "flat", false, false},
		{"stereo/LM3/tone/spiky", 2, 3, 0, 21, 24, 0, 1, 0, 0, 300, 6000, 536870911, "spiky", false, false},
		// tone_freq values that sit exactly on a freq_bin rounding boundary: with
		// QCONST16(120/M_PI,9)==19557 they round to bin 6 / bin 28, with 19558 they
		// round to 7 / 29. Without these, a 1-LSB error in that constant is absorbed
		// by the bin quantisation and is unobservable. (Verified: a +1 constant
		// survives the rest of the sweep.)
		{"mono/LM3/tone/binedge6", 1, 3, 0, 21, 24, 0, 1, 0, 0, 300, 1394, 530000000, "flat", false, false},
		{"mono/LM3/tone/binedge28", 1, 3, 0, 21, 24, 0, 1, 0, 0, 300, 6112, 530000000, "flat", false, false},
		{"mono/LM3/tone/end13high", 1, 3, 0, 13, 24, 0, 1, 0, 0, 300, 20000, 527000000, "flat", false, false},
		{"stereo/LM3/tone/start17high", 2, 3, 17, 21, 24, 0, 1, 0, 0, 300, 25000, 536000000, "flat", false, false},
		// tone threshold exactly NOT met (== is not >): must behave as tone-off.
		{"mono/LM3/tone/boundary", 1, 3, 0, 21, 24, 0, 1, 0, 0, 300, 12000, qconst98Q29, "spiky", false, false},
		// tot_boost cap break (:1254-1260): CBR/CVBR-non-transient + tiny budget +
		// spiky spectrum -> boost_bits blows through 2*effectiveBytes/3.
		{"mono/LM3/cap/cbr", 1, 3, 0, 21, 24, 0, 0, 0, 0, 0, 0, 0, "spiky", false, false},
		{"stereo/LM3/cap/cbr", 2, 3, 0, 21, 24, 0, 0, 0, 0, 2, 0, 0, "spiky", false, false},
		{"stereo/LM3/cap/cvbr", 2, 3, 0, 21, 24, 0, 1, 1, 0, 5, 0, 0, "spiky", false, false},
		{"mono/LM0/cap/cbr", 1, 0, 0, 21, 24, 0, 0, 0, 0, 1, 0, 0, "spiky", false, false},
		// --- boost block OFF ----------------------------------------------------
		// effectiveBytes below the 30+5*LM threshold (lfe=0).
		{"mono/LM3/below", 1, 3, 0, 21, 24, 0, 1, 0, 0, -1, 0, 0, "spiky", false, false},
		{"stereo/LM0/below", 2, 0, 0, 21, 24, 0, 1, 0, 0, -1, 0, 0, "spiky", false, false},
		{"stereo/LM3/below/start17", 2, 3, 17, 21, 24, 0, 0, 0, 0, -30, 0, 0, "spiky", false, false},
		{"mono/LM1/below/transient", 1, 1, 0, 17, 8, 1, 1, 0, 0, -10, 0, 0, "random", false, false},
		// lfe=1 (budget is fine, but lfe short-circuits the whole boost block).
		{"mono/LM3/lfe", 1, 3, 0, 21, 24, 0, 1, 0, 1, 300, 0, 0, "spiky", false, false},
		{"stereo/LM3/lfe/start17", 2, 3, 17, 21, 24, 0, 0, 0, 1, 300, 0, 0, "spiky", false, false},
		{"mono/LM0/lfe/tone", 1, 0, 0, 21, 24, 1, 1, 0, 1, 100, 9000, 535000000, "spiky", false, false},
		// --- maxDepth floor -----------------------------------------------------
		// Every band is far below the noise floor, so maxDepth never leaves its
		// -GCONST(31.9f) initialiser (:1068). Without this the constant is dead
		// weight and a wrong value is unobservable.
		{"mono/LM3/quiet", 1, 3, 0, 21, 8, 0, 1, 0, 0, 300, 0, 0, "quiet", false, false},
		{"stereo/LM0/quiet", 2, 0, 0, 21, 24, 0, 0, 0, 0, 100, 0, 0, "quiet", false, false},
		{"stereo/LM3/quiet/below", 2, 3, 0, 21, 16, 0, 1, 0, 0, -5, 0, 0, "quiet", false, false},
		// --- `last` carried across channels (:1123) -----------------------------
		// Channel 0 rises every band (last -> end-1); channel 1 never rises, so with
		// the C's non-reset `last` it runs a backward pass (:1148) it would not run
		// if `last` were per-channel. noJitter keeps bandLogE2 monotone.
		{"stereo/LM3/quirklast", 2, 3, 0, 21, 8, 0, 1, 0, 0, 400, 0, 0, "quirklast", false, true},
		{"stereo/LM2/quirklast/cbr", 2, 2, 0, 21, 8, 0, 0, 0, 0, 400, 0, 0, "quirklast", false, true},
		{"stereo/LM3/quirklast/end17", 2, 3, 0, 17, 8, 0, 1, 0, 0, 400, 0, 0, "quirklast", false, true},
	}
}

// dynFlags tracks the branches the sweep must prove it exercised. Every field is
// checked at the end of the test; a false one fails the run (a test that passes
// without exercising the branch is a FAILING test).
type dynFlags struct {
	boostBlock    bool // :1121 taken (effectiveBytes >= 30+5*LM && !lfe)
	skipBytes     bool // :1266 else taken because effectiveBytes < 30+5*LM
	skipLfe       bool // :1266 else taken because lfe (budget was sufficient)
	lm0           bool // :1130 LM==0 max-with-oldBandE path
	lmNonzero     bool // LM>0 (that path skipped)
	mono          bool // C==1 (:1176 else)
	stereo        bool // C==2 (:1091 mask max, :1167 cross-talk)
	start0        bool // start==0
	startPos      bool // start>0 (importance window offset)
	transient     bool
	nonTransient  bool
	cbrHalve      bool // :1193 (!vbr||constrained_vbr) && !isTransient
	noCbrHalve    bool
	toneBoost     bool // :1206 toneishness > QCONST32(.98f,29)
	toneBandBoost bool // :1213 freq_bin lands inside a coded band
	toneEndBoost  bool // :1218 freq_bin >= eBands[end]
	toneOff       bool
	capBreak      bool // :1254-1260 tot_boost cap hit, loop broken early
	noCapBreak    bool
	widthSmall    bool // :1242 width<6
	widthMid      bool // :1249 6<=width<=48
	widthLarge    bool // :1246 width>48
	offsetNonzero bool // dynalloc actually boosted a band
	offsetAllZero bool // and a frame where it boosted nothing
	surroundFloor bool // surround_dynalloc[i] raised follower[i]
	importGt13    bool // importance above the 13 floor
	import13      bool // importance == 13 (both in and out of the boost block)
	spread32      bool // spread_weight == 32 (shift 0)
	spreadLt32    bool // spread_weight < 32 (shift 1..5)
	spread1       bool // spread_weight == 1 (shift 5, the SMR clamp floor)
	maxDepthFloor bool // maxDepth never rose above -GCONST(31.9f) (:1068)
	lastCarry     bool // ch1 has no rising band while ch0's `last`>0 (:1123 quirk)
}

// gconstHalfQ24 is GCONST(.5f) (:1144), the "band rises" threshold that drives
// `last`. Used by the non-vacuity bookkeeping only.
const gconstHalfQ24 = int32(8388608)

// maxDepthInit is -GCONST(31.9f) (:1068) with the C's float32 literal semantics.
const maxDepthInit = int32(-535193184)

func TestCeltencDynallocAnalysisMatchesC(t *testing.T) {
	const nbEBands = 21
	if got := cCeltencNbebands(); got != nbEBands {
		t.Fatalf("nbEBands: C=%d want %d", got, nbEBands)
	}
	r := rand.New(rand.NewSource(0xD7A110C))
	var fl dynFlags

	for _, cfg := range dynCfgs() {
		effectiveBytes := 30 + 5*cfg.LM + cfg.effBytesDelta
		// 8 independent random spectra per config: dynalloc_analysis is stateless,
		// so this is a sweep, not a sequence, but every draw is compared in full.
		for draw := 0; draw < 8; draw++ {
			bandLogE := dynSpectrum(r, cfg.shape, nbEBands, cfg.channels)
			var bandLogE2, oldBandE []int32
			if cfg.noJitter {
				bandLogE2 = append([]int32(nil), bandLogE...)
				oldBandE = append([]int32(nil), bandLogE...)
			} else {
				bandLogE2 = dynJitter(r, bandLogE, 1.0)
				oldBandE = dynJitter(r, bandLogE, 2.5)
			}
			surround := make([]int32, nbEBands)
			if cfg.surround {
				for i := range surround {
					surround[i] = dbQ24(r.Float64() * 3.5)
				}
			}

			goRes := celt.DynallocAnalysis(bandLogE, bandLogE2, oldBandE, nbEBands,
				cfg.start, cfg.end, cfg.channels, cfg.lsbDepth, cfg.isTransient,
				cfg.vbr, cfg.constrainedVbr, cfg.LM, effectiveBytes, cfg.lfe,
				surround, int16(cfg.toneFreq), cfg.toneishness)

			cRes := cCeltencDynallocAnalysis(bandLogE, bandLogE2, oldBandE, nbEBands,
				cfg.start, cfg.end, cfg.channels, cfg.lsbDepth, cfg.isTransient,
				cfg.vbr, cfg.constrainedVbr, cfg.LM, effectiveBytes, cfg.lfe,
				surround, cfg.toneFreq, cfg.toneishness)

			label := cfg.name
			if goRes.MaxDepth != cRes.maxDepth {
				t.Fatalf("%s draw %d: maxDepth Go=%d C=%d", label, draw, goRes.MaxDepth, cRes.maxDepth)
			}
			if goRes.TotBoost != cRes.totBoost {
				t.Fatalf("%s draw %d: tot_boost Go=%d C=%d", label, draw, goRes.TotBoost, cRes.totBoost)
			}
			// offsets: OPUS_CLEAR'd over the whole array, so fully defined.
			eqDynallocInts(t, label+" offsets", goRes.Offsets, cRes.offsets, 0, nbEBands)
			// spread_weight: written over [0,end).
			eqDynallocInts(t, label+" spread_weight", goRes.SpreadWeight, cRes.spreadWeight, 0, cfg.end)
			// importance: written ONLY over [start,end). Outside that the C never
			// touches the buffer, so it must NOT be compared.
			eqDynallocInts(t, label+" importance", goRes.Importance, cRes.importance, cfg.start, cfg.end)

			// ---- non-vacuity bookkeeping (driven off the C's own outputs) -------
			boostOn := effectiveBytes >= 30+5*cfg.LM && cfg.lfe == 0
			if cRes.maxDepth == maxDepthInit {
				fl.maxDepthFloor = true
			}
			// `last` (:1123) is NOT reset between channels. Its precondition: some
			// band of channel 0 rises by more than GCONST(.5f) (so last>0 when the
			// channel-1 pass starts) while NO band of channel 1 does (so channel 1
			// never overwrites it and inherits channel 0's value).
			if boostOn && cfg.channels == 2 {
				ch0Last := 0
				ch1Rise := false
				for i := 1; i < cfg.end; i++ {
					if bandLogE2[i] > bandLogE2[i-1]+gconstHalfQ24 {
						ch0Last = i
					}
					if bandLogE2[nbEBands+i] > bandLogE2[nbEBands+i-1]+gconstHalfQ24 {
						ch1Rise = true
					}
				}
				if ch0Last > 0 && !ch1Rise {
					fl.lastCarry = true
				}
			}
			if boostOn {
				fl.boostBlock = true
			} else if cfg.lfe != 0 {
				fl.skipLfe = true
			} else {
				fl.skipBytes = true
			}
			if cfg.LM == 0 {
				fl.lm0 = true
			} else {
				fl.lmNonzero = true
			}
			if cfg.channels == 1 {
				fl.mono = true
			} else {
				fl.stereo = true
			}
			if cfg.start == 0 {
				fl.start0 = true
			} else {
				fl.startPos = true
			}
			if cfg.isTransient != 0 {
				fl.transient = true
			} else {
				fl.nonTransient = true
			}
			if boostOn {
				if (cfg.vbr == 0 || cfg.constrainedVbr != 0) && cfg.isTransient == 0 {
					fl.cbrHalve = true
				} else {
					fl.noCbrHalve = true
				}
				if cfg.toneishness > qconst98Q29 {
					fl.toneBoost = true
					// freq_bin = PSHR32(tone_freq*QCONST16(120/M_PI,9), 13+9) (:1208).
					freqBin := int((int32(int16(cfg.toneFreq))*19557 + (1 << 21)) >> 22)
					if freqBin >= dynEBands[cfg.end] {
						fl.toneEndBoost = true
					}
					for i := cfg.start; i < cfg.end; i++ {
						if freqBin >= dynEBands[i] && freqBin <= dynEBands[i+1] {
							fl.toneBandBoost = true
						}
					}
				} else {
					fl.toneOff = true
				}
				// The cap break (:1257-1259) sets tot_boost to exactly
				// ((2*effectiveBytes/3)<<BITRES<<3), and it can only be reached when
				// the CBR/CVBR-non-transient guard at :1254 holds.
				capEligible := cfg.vbr == 0 || (cfg.constrainedVbr != 0 && cfg.isTransient == 0)
				capBits := int32((2 * effectiveBytes / 3) << 3 << 3)
				if capEligible && capBits > 0 && cRes.totBoost == capBits {
					fl.capBreak = true
				} else {
					fl.noCapBreak = true
				}
				anyOffset := false
				for i := cfg.start; i < cfg.end; i++ {
					if cRes.offsets[i] != 0 {
						anyOffset = true
					}
					width := cfg.channels * (dynEBands[i+1] - dynEBands[i]) << cfg.LM
					switch {
					case width < 6:
						fl.widthSmall = true
					case width > 48:
						fl.widthLarge = true
					default:
						fl.widthMid = true
					}
				}
				if anyOffset {
					fl.offsetNonzero = true
				} else {
					fl.offsetAllZero = true
				}
				if cfg.surround {
					// surround_dynalloc[i] can only raise follower[i], which can only
					// raise importance[i] above what a zero surround would give. Rerun
					// the Go side with a zeroed surround and look for a difference.
					zero := make([]int32, nbEBands)
					ref := celt.DynallocAnalysis(bandLogE, bandLogE2, oldBandE, nbEBands,
						cfg.start, cfg.end, cfg.channels, cfg.lsbDepth, cfg.isTransient,
						cfg.vbr, cfg.constrainedVbr, cfg.LM, effectiveBytes, cfg.lfe,
						zero, int16(cfg.toneFreq), cfg.toneishness)
					for i := cfg.start; i < cfg.end; i++ {
						if ref.Importance[i] != goRes.Importance[i] || ref.Offsets[i] != goRes.Offsets[i] {
							fl.surroundFloor = true
						}
					}
				}
			}
			for i := cfg.start; i < cfg.end; i++ {
				if cRes.importance[i] == 13 {
					fl.import13 = true
				}
				if cRes.importance[i] > 13 {
					fl.importGt13 = true
				}
			}
			for i := 0; i < cfg.end; i++ {
				switch cRes.spreadWeight[i] {
				case 32:
					fl.spread32 = true
				case 1:
					fl.spread1 = true
					fl.spreadLt32 = true
				default:
					fl.spreadLt32 = true
				}
			}
		}
	}

	// ---- non-vacuity guards: every documented branch MUST have fired ----------
	checks := []struct {
		ok   bool
		name string
	}{
		{fl.boostBlock, "boost block taken (effectiveBytes >= 30+5*LM && !lfe, :1121)"},
		{fl.skipBytes, "boost block skipped via effectiveBytes < 30+5*LM (:1266)"},
		{fl.skipLfe, "boost block skipped via lfe (:1121/:1266)"},
		{fl.lm0, "LM==0 max-with-oldBandE path (:1130)"},
		{fl.lmNonzero, "LM>0 (no oldBandE max)"},
		{fl.mono, "C==1 follower path (:1176)"},
		{fl.stereo, "C==2 mask max (:1091) + cross-talk (:1167)"},
		{fl.start0, "start==0"},
		{fl.startPos, "start>0 (importance window offset)"},
		{fl.transient, "isTransient==1"},
		{fl.nonTransient, "isTransient==0"},
		{fl.cbrHalve, "CBR/CVBR non-transient dynalloc halving (:1193)"},
		{fl.noCbrHalve, "no dynalloc halving"},
		{fl.toneBoost, "tone compensation taken (:1206)"},
		{fl.toneBandBoost, "tone freq_bin inside a coded band (:1213)"},
		{fl.toneEndBoost, "tone freq_bin >= eBands[end] (:1218)"},
		{fl.toneOff, "tone compensation not taken"},
		{fl.capBreak, "tot_boost cap break (:1254-1260)"},
		{fl.noCapBreak, "no cap break"},
		{fl.widthSmall, "width<6 boost (:1242)"},
		{fl.widthMid, "6<=width<=48 boost (:1249)"},
		{fl.widthLarge, "width>48 boost (:1246)"},
		{fl.offsetNonzero, "a band actually boosted (offsets[i]!=0)"},
		{fl.offsetAllZero, "a frame with no boost at all"},
		{fl.surroundFloor, "surround_dynalloc raised follower (:1183)"},
		{fl.importGt13, "importance > 13 (:1187)"},
		{fl.import13, "importance == 13"},
		{fl.spread32, "spread_weight == 32 (shift 0, :1112)"},
		{fl.spreadLt32, "spread_weight < 32 (shift 1..5)"},
		{fl.spread1, "spread_weight == 1 (shift 5, SMR clamped at -GCONST(5))"},
		{fl.maxDepthFloor, "maxDepth stayed at its -GCONST(31.9f) initialiser (:1068)"},
		{fl.lastCarry, "`last` carried from channel 0 into channel 1 (:1123 quirk)"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Fatalf("non-vacuity: branch never exercised: %s", c.name)
		}
		t.Logf("branch exercised: %s", c.name)
	}
	t.Logf("non-vacuity: all %d documented branches fired", len(checks))
}

// TestCeltencDynallocMedianMatchesC pins median_of_5 (:990) and median_of_3
// (:1029) directly. They are static and have no C wrapper of their own, so they
// are driven THROUGH dynalloc_analysis: with LM>0, C==1, a large budget and a
// zeroed surround, the follower's median-filter stage (:1155-1162) is the only
// thing that can lift f[i] above the forward/backward recursion, so any median
// disagreement shows up as an offsets/importance mismatch. The spectra here are
// built from a tiny alphabet of energies so ties, duplicates and equal-neighbour
// cases (the branch-heavy corners of both medians) are hit densely.
func TestCeltencDynallocMedianMatchesC(t *testing.T) {
	const nbEBands = 21
	r := rand.New(rand.NewSource(0x1E01A5))
	alphabet := []float64{-2, 0, 0.5, 1, 1.5, 4, 4, 12}
	for iter := 0; iter < 400; iter++ {
		bandLogE := make([]int32, nbEBands)
		for i := range bandLogE {
			bandLogE[i] = dbQ24(alphabet[r.Intn(len(alphabet))])
		}
		bandLogE2 := make([]int32, nbEBands)
		for i := range bandLogE2 {
			bandLogE2[i] = dbQ24(alphabet[r.Intn(len(alphabet))])
		}
		oldBandE := make([]int32, nbEBands)
		copy(oldBandE, bandLogE2)
		surround := make([]int32, nbEBands)

		goRes := celt.DynallocAnalysis(bandLogE, bandLogE2, oldBandE, nbEBands,
			0, nbEBands, 1, 24, 0, 1, 0, 3, 400, 0, surround, 0, 0)
		cRes := cCeltencDynallocAnalysis(bandLogE, bandLogE2, oldBandE, nbEBands,
			0, nbEBands, 1, 24, 0, 1, 0, 3, 400, 0, surround, 0, 0)

		if goRes.MaxDepth != cRes.maxDepth {
			t.Fatalf("iter %d: maxDepth Go=%d C=%d", iter, goRes.MaxDepth, cRes.maxDepth)
		}
		if goRes.TotBoost != cRes.totBoost {
			t.Fatalf("iter %d: tot_boost Go=%d C=%d", iter, goRes.TotBoost, cRes.totBoost)
		}
		eqDynallocInts(t, "median offsets", goRes.Offsets, cRes.offsets, 0, nbEBands)
		eqDynallocInts(t, "median importance", goRes.Importance, cRes.importance, 0, nbEBands)
		eqDynallocInts(t, "median spread_weight", goRes.SpreadWeight, cRes.spreadWeight, 0, nbEBands)
	}
}
