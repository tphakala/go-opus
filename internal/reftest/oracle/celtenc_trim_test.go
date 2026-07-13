//go:build refc

package oracle

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/fixedmath"
)

// This differential test pins the pure-Go CELT encoder allocation-trim analysis
// stages (internal/celt celt_encoder_trim.go: alloc_trim_analysis at
// celt_encoder.c:865 and stereo_analysis at :957, the CP8b "TRIM" unit) to the
// pinned libopus C oracle, driven through celtenc_shim.h.
//
// alloc_trim_analysis MUTATES st->stereo_saving at :919 (C==2 only), so it is
// cross-frame state and is exercised as a MULTI-FRAME SEQUENCE: stereo_saving is
// threaded through both the Go and the C side and compared after every frame.
// stereo_analysis is stateless, so it gets a randomized sweep instead.
//
// eqI32 is shared with celtenc_test.go.

// trimEBands is the 48 kHz / 960 mode's eBand table (static_modes_fixed.h
// eband5ms), duplicated here only so the test can lay out per-band spectra.
var trimEBands = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 14, 16, 20, 24, 28, 34, 40, 48, 60, 78, 100}

const (
	trimNbEBands = 21
	trimDbShift  = 24 // DB_SHIFT
	trimNormShif = 24 // NORM_SHIFT
)

// trimNormX builds a channels*N0 celt_norm spectrum shaped the way normalise_bands
// leaves it: every band of every channel has unit L2 norm in Q(NORM_SHIFT), so a
// perfectly correlated band yields celt_inner_prod_norm_shift == 2^28 and
// SHR32(partial,18) == 1024 == QCONST16(1,10). corr sets the inter-channel
// correlation of the right channel (channel 1) with the left; amp scales the whole
// spectrum so the test can push SHR32(partial,18) past what a normalized band
// produces and drive the ADD16/EXTRACT16 16-bit truncations in the C.
func trimNormX(r *rand.Rand, channels, LM, N0 int, corr, amp float64) []int32 {
	x := make([]int32, channels*N0)
	beta := math.Sqrt(math.Max(0, 1-corr*corr))
	for b := 0; b+1 < len(trimEBands); b++ {
		lo := trimEBands[b] << LM
		hi := trimEBands[b+1] << LM
		n := hi - lo
		l := make([]float64, n)
		rr := make([]float64, n)
		for j := 0; j < n; j++ {
			u := r.NormFloat64()
			v := r.NormFloat64()
			l[j] = u
			rr[j] = corr*u + beta*v
		}
		norm := func(v []float64) {
			var e float64
			for _, s := range v {
				e += s * s
			}
			if e <= 0 {
				return
			}
			e = math.Sqrt(e)
			for j := range v {
				v[j] /= e
			}
		}
		norm(l)
		norm(rr)
		for j := 0; j < n; j++ {
			x[lo+j] = int32(math.Round(l[j] * amp * float64(int32(1)<<trimNormShif)))
			if channels == 2 {
				x[N0+lo+j] = int32(math.Round(rr[j] * amp * float64(int32(1)<<trimNormShif)))
			}
		}
	}
	return x
}

// trimBandLogE builds a channels*nbEBands celt_glog (Q24 dB) band-energy vector
// with a linear spectral tilt, which is exactly what alloc_trim_analysis's `diff`
// accumulator at :926 measures. A negative tilt drives diff negative, which is what
// exercises the truncate-toward-zero division at :929 and the SHR32/`/6` mix at :931.
func trimBandLogE(r *rand.Rand, channels int, base, tilt, jitter float64) []int32 {
	b := make([]int32, channels*trimNbEBands)
	for c := 0; c < channels; c++ {
		for i := 0; i < trimNbEBands; i++ {
			db := base + tilt*float64(i) + jitter*(r.Float64()*2-1)
			b[i+c*trimNbEBands] = int32(math.Round(db * float64(int32(1)<<trimDbShift)))
		}
	}
	return b
}

// trimFrame is one frame of the alloc_trim_analysis sequence. The comment on each
// table entry names the branch it targets.
type trimFrame struct {
	name         string
	channels     int
	LM           int
	end          int
	intensity    int
	equivRate    int32
	surroundTrim int32 // celt_glog, Q24
	tfEstimate   int16 // opus_val16, Q14
	corr         float64
	amp          float64
	tilt         float64 // dB per band
	base         float64
	jitter       float64
}

func trimFrames() []trimFrame {
	f := []trimFrame{
		// --- mono (C==1): the whole :884-920 stereo block is skipped ---------
		{"mono/LM3/rate32k/flat", 1, 3, 21, 0, 32000, 0, 0, 0, 1, 0, 18, 0.4},
		{"mono/LM3/rate72k/mid-rate-interp", 1, 3, 21, 0, 72000, 0, 0, 0, 1, 0.05, 18, 0.2},
		{"mono/LM3/rate96k/high-rate", 1, 3, 21, 0, 96000, 0, 0, 0, 1, -0.05, 18, 0.2},
		// tilt clamps: |tiltRaw| > QCONST16(2,8) saturates MIN32/MAX32 at :931.
		{"mono/LM3/tilt-up-clamp-hi", 1, 3, 21, 0, 96000, 0, 0, 0, 1, 0.9, 12, 0.1},
		{"mono/LM3/tilt-down-clamp-lo", 1, 3, 21, 0, 96000, 0, 0, 0, 1, -0.9, 24, 0.1},
		// small tilts leave the MIN32/MAX32 unclamped (the middle branch).
		{"mono/LM3/tilt-tiny-pos", 1, 3, 21, 0, 96000, 0, 0, 0, 1, 0.02, 18, 0.02},
		{"mono/LM3/tilt-tiny-neg", 1, 3, 21, 0, 96000, 0, 0, 0, 1, -0.02, 18, 0.02},
		// surround_trim / tf_estimate arms (:932, :933), both signs.
		{"mono/LM2/surround-neg/trim-index-hi", 1, 2, 21, 0, 96000, -5 << trimDbShift, 0, 0, 1, -0.02, 18, 0.05},
		{"mono/LM2/surround-pos/trim-index-lo", 1, 2, 21, 0, 32000, 4 << trimDbShift, 12000, 0, 1, 0.9, 18, 0.05},
		{"mono/LM1/surround-neg-2", 1, 1, 17, 0, 88000, -3 << trimDbShift, 4096, 0, 1, 0.1, 15, 0.3},
		{"mono/LM0/tf-estimate-max", 1, 0, 13, 0, 64000, 0, 16384, 0, 1, 0.3, 15, 0.3},
		{"mono/LM0/end13", 1, 0, 13, 0, 48000, 1 << trimDbShift, 8192, 0, 1, -0.3, 15, 0.3},

		// --- stereo (C==2): the :884-920 block runs and mutates stereo_saving --
		// intensity<=8 -> the :899 refinement loop body never runs (minXC==sum).
		{"stereo/LM3/uncorr/intensity8", 2, 3, 21, 8, 96000, 0, 0, 0.0, 1, 0.05, 18, 0.3},
		{"stereo/LM3/uncorr/intensity0", 2, 3, 21, 0, 96000, 0, 0, 0.0, 1, -0.05, 18, 0.3},
		// intensity>8 -> the :899 refinement loop runs.
		{"stereo/LM3/corr0.99/intensity21", 2, 3, 21, 21, 96000, 0, 0, 0.99, 1, 0.02, 18, 0.2},
		{"stereo/LM3/corr0.95/intensity15", 2, 3, 21, 15, 72000, 0, 0, 0.95, 1, -0.02, 18, 0.2},
		{"stereo/LM3/corr0.70/intensity12", 2, 3, 21, 12, 48000, 0, 0, 0.70, 1, 0.3, 18, 0.2},
		{"stereo/LM3/corr0.30/intensity10", 2, 3, 21, 10, 96000, 0, 0, 0.30, 1, -0.3, 18, 0.2},
		{"stereo/LM3/anticorr/intensity18", 2, 3, 21, 18, 96000, 0, 0, -0.95, 1, 0.0, 18, 0.2},
		// amp>1: pushes SHR32(partial,18) above what a unit-norm band gives, so the
		// EXTRACT16 narrowing and the ADD16 16-bit wrap at :894 are exercised for real.
		// A fully correlated band contributes amp^2*1024 per band, so amp=2.6 gives
		// ~6.9k per band and the eight-band ADD16 accumulator wraps int16 mid-loop.
		// amp stays below sqrt(8) so celt_inner_prod_norm_shift's own opus_val64 ->
		// opus_val32 narrowing does not also overflow.
		// amp=2.75 is the narrow window in which the wrap is not merely present but
		// OBSERVABLE: the true 8-band accumulation lands in (57344, 65528], so the
		// wrapped int16 lands in (-8192,-8] and MIN16(QCONST16(1,10), ABS16(sum/8))
		// yields < 1024, whereas a widened (non-C) accumulator would saturate at
		// 1024. A port that accumulated `sum` in int32 gives a different trim_index.
		{"stereo/LM3/corr1.0/amp2.75/add16-wrap-observable", 2, 3, 21, 21, 96000, 0, 0, 1.0, 2.75, 0.02, 18, 0.1},
		{"stereo/LM2/corr1.0/amp2.75/add16-wrap-observable", 2, 2, 21, 16, 72000, 0, 0, 1.0, 2.75, -0.02, 18, 0.1},
		{"stereo/LM3/corr1.0/amp2.6/add16-wrap", 2, 3, 21, 21, 96000, 0, 0, 1.0, 2.6, 0.02, 18, 0.1},
		{"stereo/LM1/corr1.0/amp2.5/add16-wrap", 2, 1, 21, 16, 72000, 0, 0, 1.0, 2.5, -0.02, 18, 0.1},
		{"stereo/LM1/corr0.9/amp1.5", 2, 1, 17, 14, 60000, 0, 2048, 0.9, 1.5, 0.1, 18, 0.2},
		{"stereo/LM0/corr0.5/amp1.2", 2, 0, 13, 11, 40000, 0, 6000, 0.5, 1.2, -0.1, 18, 0.2},
		// stereo + surround/tf, saturating trim_index at both ends of :949.
		{"stereo/LM3/corr1.0/surround-pos/index0", 2, 3, 21, 21, 32000, 5 << trimDbShift, 16384, 1.0, 1, 0.9, 18, 0.05},
		{"stereo/LM3/uncorr/surround-neg/index10", 2, 3, 21, 8, 96000, -6 << trimDbShift, 0, 0.0, 1, -0.9, 18, 0.05},
		{"stereo/LM2/end17/corr0.8", 2, 2, 17, 12, 78000, -1 << trimDbShift, 1024, 0.8, 1, 0.05, 18, 0.2},
		{"stereo/LM1/end13/corr-0.6", 2, 1, 13, 10, 66000, 2 << trimDbShift, 512, -0.6, 1, -0.05, 18, 0.2},

		// --- opus_val16 (int16) truncation of `trim` -------------------------
		// trim is opus_val16, so EVERY `trim -= ...` at :931-933 truncates to 16
		// bits. surround_trim is a celt_glog (int32) and SHR16(surround_trim,16) is
		// a full 32-bit shift, so an extreme surround_trim drives |trim| past 32767
		// and the C's int16 assignment WRAPS. These two frames are out of the range
		// the real encoder ever produces, but they are legal C inputs and they are
		// the only way to prove the int16 model is load-bearing rather than
		// decorative: without it Go and C diverge by ~65536 in trim.
		{"stereo/int16-wrap/surround-max", 2, 3, 21, 21, 32000, math.MaxInt32, 16384, 1.0, 1, 0.9, 18, 0.05},
		{"mono/int16-wrap/surround-min", 1, 3, 21, 0, 96000, math.MinInt32, 0, 0, 1, -0.9, 18, 0.05},
	}
	// A long, strongly correlated stereo run: -HALF16(logXC2) sits well above 0, so
	// MIN16 at :919 keeps picking the (*stereo_saving + QCONST16(.25,8)) arm and
	// stereo_saving climbs by 64 per frame until it saturates against the
	// -HALF16(logXC2) arm. This is what makes the sequence non-vacuous.
	for i := 0; i < 24; i++ {
		f = append(f, trimFrame{
			name: "stereo/ramp/corr0.995", channels: 2, LM: 3, end: 21, intensity: 21,
			equivRate: 96000, corr: 0.995, amp: 1, tilt: 0.01, base: 18, jitter: 0.1,
		})
	}
	// Then a few uncorrelated frames: -HALF16(logXC2) collapses to ~0, so MIN16
	// picks the other arm and stereo_saving is yanked back down.
	for i := 0; i < 4; i++ {
		f = append(f, trimFrame{
			name: "stereo/collapse/uncorr", channels: 2, LM: 3, end: 21, intensity: 20,
			equivRate: 96000, corr: 0.0, amp: 1, tilt: -0.01, base: 18, jitter: 0.1,
		})
	}
	// A mono frame in the middle of the sequence must leave stereo_saving alone.
	f = append(f, trimFrame{
		name: "mono/holds-stereo-saving", channels: 1, LM: 3, end: 21,
		equivRate: 96000, corr: 0, amp: 1, tilt: 0.0, base: 18, jitter: 0.1,
	})
	return f
}

func TestCeltencAllocTrimAnalysisMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8B_7811))

	var (
		sawEquivLow, sawEquivMid, sawEquivHigh bool
		sawMono, sawStereo                     bool
		sawMinXCLoop, sawNoMinXCLoop           bool
		sawSumSat, sawSumBelowSat              bool
		sawStereoTrimClamp, sawStereoTrimFree  bool
		sawSSLeftArm, sawSSRightArm            bool
		sawSSRose, sawSSFell, sawSSHeld        bool
		sawDiffPos, sawDiffNeg                 bool
		sawDivTruncNeg                         bool
		sawShrFloorNeg                         bool
		sawDiv6TruncNeg                        bool
		sawTiltClampLo, sawTiltClampHi         bool
		sawTiltFree                            bool
		sawNegTrim                             bool
		sawIndexSatLo, sawIndexSatHi           bool
		sawIndexMid                            bool
		sawAdd16Wrap, sawAdd16WrapObservable   bool
		sawTrimInt16Wrap                       bool
	)
	seen := map[int]bool{}

	frames := trimFrames()
	// Three passes over the table with fresh random draws, threading stereo_saving
	// across every frame of the whole sequence on BOTH sides.
	for pass := 0; pass < 3; pass++ {
		var goSS, cSS int16
		for fi, f := range frames {
			N0 := 120 << f.LM
			x := trimNormX(r, f.channels, f.LM, N0, f.corr, f.amp)
			bandLogE := trimBandLogE(r, f.channels, f.base, f.tilt, f.jitter)

			ssIn := goSS
			if cSS != goSS {
				t.Fatalf("pass %d frame %d (%s): stereo_saving desync before call: Go=%d C=%d",
					pass, fi, f.name, goSS, cSS)
			}

			goTrim, goSSOut, w := celt.AllocTrimAnalysis(x, bandLogE, f.end, f.LM, f.channels, N0,
				goSS, f.tfEstimate, f.intensity, f.surroundTrim, f.equivRate)
			cTrim, cSSOut := cCeltencAllocTrimAnalysis(x, bandLogE, f.end, f.LM, f.channels, N0,
				int32(cSS), int(f.tfEstimate), f.intensity, f.surroundTrim, f.equivRate)

			if goTrim != cTrim {
				t.Fatalf("pass %d frame %d (%s): trim_index Go=%d C=%d (stereo_saving in=%d)",
					pass, fi, f.name, goTrim, cTrim, ssIn)
			}
			if int32(goSSOut) != cSSOut {
				t.Fatalf("pass %d frame %d (%s): stereo_saving Go=%d C=%d (in=%d)",
					pass, fi, f.name, goSSOut, cSSOut, ssIn)
			}
			goSS = goSSOut
			cSS = int16(cSSOut)

			// --- non-vacuity witnesses (derived from the real call's internals) ---
			seen[goTrim] = true
			switch {
			case f.equivRate < 64000:
				sawEquivLow = true
			case f.equivRate < 80000:
				sawEquivMid = true
			default:
				sawEquivHigh = true
			}
			if w.StereoBlock {
				sawStereo = true
				if w.MinXCLoop {
					sawMinXCLoop = true
				} else {
					sawNoMinXCLoop = true
				}
				if w.Sum == fixedmath.QCONST16(1.0, 10) {
					sawSumSat = true
				} else {
					sawSumBelowSat = true
				}
				// :918 MAX16(-QCONST16(4,8), MULT16_16_Q15(QCONST16(.75,15), logXC))
				stereoTerm := fixedmath.MULT16_16_Q15(fixedmath.QCONST16(0.75, 15), w.LogXC)
				if stereoTerm < -int32(fixedmath.QCONST16(4.0, 8)) {
					sawStereoTrimClamp = true
				} else {
					sawStereoTrimFree = true
				}
				// :919 MIN16(*stereo_saving + QCONST16(.25,8), -HALF16(logXC2))
				left := int32(ssIn) + int32(fixedmath.QCONST16(0.25, 8))
				right := -int32(fixedmath.HALF16(w.LogXC2))
				if left < right {
					sawSSLeftArm = true
				} else {
					sawSSRightArm = true
				}
				switch {
				case goSSOut > ssIn:
					sawSSRose = true
				case goSSOut < ssIn:
					sawSSFell = true
				}
				// ADD16 16-bit wrap inside the :889-895 accumulation: recompute the
				// eight band terms in full precision (a coverage witness only). Each
				// term is EXTRACT16(SHR32(partial,18)), and C accumulates them with
				// ADD16, i.e. mod 2^16; mod-2^16 addition is associative, so the C's
				// final `sum` is exactly int16(acc).
				var acc int64
				for b := 0; b < 8; b++ {
					lo := trimEBands[b] << f.LM
					n := (trimEBands[b+1] - trimEBands[b]) << f.LM
					var ip int64
					for j := 0; j < n; j++ {
						ip += int64(x[lo+j]) * int64(x[N0+lo+j])
					}
					acc += int64(int32(ip>>(2*(trimNormShif-14))) >> 18)
				}
				if acc > math.MaxInt16 || acc < math.MinInt16 {
					sawAdd16Wrap = true
					// Is the wrap OBSERVABLE, i.e. does it change `sum` after
					// :896-897 (MULT16_16_Q15 by 1/8, ABS16, MIN16 against
					// QCONST16(1,10))? Only then would a widened accumulator be
					// caught by the differential compare.
					withWrap := fixedmath.MIN16(int32(fixedmath.QCONST16(1.0, 10)),
						fixedmath.ABS16(int16(fixedmath.MULT16_16_Q15(fixedmath.QCONST16(1.0/8, 15), int16(acc)))))
					wide := (int64(fixedmath.QCONST16(1.0/8, 15)) * acc) >> 15
					if wide < 0 {
						wide = -wide
					}
					if wide > int64(fixedmath.QCONST16(1.0, 10)) {
						wide = int64(fixedmath.QCONST16(1.0, 10))
					}
					if int64(withWrap) != wide {
						sawAdd16WrapObservable = true
					}
					if int32(int16(acc)) != int32(w.Sum) && withWrap != int32(w.Sum) {
						t.Fatalf("pass %d frame %d (%s): witness disagrees with the port: sum=%d want %d",
							pass, fi, f.name, w.Sum, withWrap)
					}
				}
			} else {
				sawMono = true
				if goSSOut == ssIn {
					sawSSHeld = true
				}
			}
			// :929 diff /= C*(end-1): C truncation toward zero on a negative value.
			div := int32(f.channels * (f.end - 1))
			if w.DiffPre < 0 && w.DiffPre%div != 0 {
				sawDivTruncNeg = true
			}
			switch {
			case w.Diff > 0:
				sawDiffPos = true
			case w.Diff < 0:
				sawDiffNeg = true
			}
			// :931 SHR32(diff+QCONST32(1,DB_SHIFT-5), DB_SHIFT-13) floors; the /6
			// that follows truncates toward zero. Both must be exercised on negatives.
			shrIn := w.Diff + fixedmath.QCONST32(1.0, trimDbShift-5)
			if shrIn < 0 && shrIn&((1<<(trimDbShift-13))-1) != 0 {
				sawShrFloorNeg = true
			}
			shrOut := fixedmath.SHR32(shrIn, trimDbShift-13)
			if shrOut < 0 && shrOut%6 != 0 {
				sawDiv6TruncNeg = true
			}
			switch {
			case w.TiltRaw < -int32(fixedmath.QCONST16(2.0, 8)):
				sawTiltClampLo = true
			case w.TiltRaw > int32(fixedmath.QCONST16(2.0, 8)):
				sawTiltClampHi = true
			default:
				sawTiltFree = true
			}
			if w.Trim < 0 {
				sawNegTrim = true // PSHR32(trim,8) at :945 floors a negative trim
			}
			// The int16 truncation of `trim` at :931-933: redo :931-933 in int32
			// (mod-2^16 arithmetic is associative, so int16(wide) == w.Trim always;
			// wide != w.Trim exactly when at least one C assignment wrapped).
			tiltTerm := fixedmath.MAX32(-int32(fixedmath.QCONST16(2.0, 8)),
				fixedmath.MIN32(int32(fixedmath.QCONST16(2.0, 8)), w.TiltRaw))
			wide := int32(w.TrimBase) - tiltTerm -
				fixedmath.SHR32(f.surroundTrim, trimDbShift-8) -
				2*int32(fixedmath.SHR16(f.tfEstimate, 14-8))
			if wide != int32(w.Trim) {
				sawTrimInt16Wrap = true
			}
			if int16(wide) != w.Trim {
				t.Fatalf("pass %d frame %d (%s): witness disagrees with the port: int16(wide)=%d trim=%d",
					pass, fi, f.name, int16(wide), w.Trim)
			}
			switch {
			case w.TrimIndexRaw < 0:
				sawIndexSatLo = true
			case w.TrimIndexRaw > 10:
				sawIndexSatHi = true
			default:
				sawIndexMid = true
			}
		}
	}

	// --- non-vacuity guards: every documented branch must have fired ---------
	guards := []struct {
		ok   bool
		what string
	}{
		{sawEquivLow, "equiv_rate < 64000 (:878)"},
		{sawEquivMid, "64000 <= equiv_rate < 80000 (:880)"},
		{sawEquivHigh, "equiv_rate >= 80000 (trim stays QCONST16(5,8))"},
		{sawMono, "C==1 (stereo block skipped)"},
		{sawStereo, "C==2 (stereo block at :884)"},
		{sawMinXCLoop, "intensity>8: minXC refinement loop body ran (:899)"},
		{sawNoMinXCLoop, "intensity<=8: minXC refinement loop body skipped"},
		{sawSumSat, "sum saturated at QCONST16(1,10) (:897)"},
		{sawSumBelowSat, "sum below the QCONST16(1,10) saturation (:897)"},
		{sawStereoTrimClamp, "MAX16(-QCONST16(4,8), ...) clamped at :918"},
		{sawStereoTrimFree, "MAX16(-QCONST16(4,8), ...) unclamped at :918"},
		{sawSSLeftArm, "MIN16 at :919 picked *stereo_saving+QCONST16(.25,8)"},
		{sawSSRightArm, "MIN16 at :919 picked -HALF16(logXC2)"},
		{sawSSRose, "stereo_saving increased across a frame"},
		{sawSSFell, "stereo_saving decreased across a frame"},
		{sawSSHeld, "C==1 left stereo_saving untouched"},
		{sawDiffPos, "diff > 0 (:929)"},
		{sawDiffNeg, "diff < 0 (:929)"},
		{sawDivTruncNeg, "diff /= C*(end-1) truncated a NEGATIVE, non-exact quotient (:929)"},
		{sawShrFloorNeg, "SHR32 floored a negative, non-exact value (:931)"},
		{sawDiv6TruncNeg, "/6 truncated a negative, non-exact quotient toward zero (:931)"},
		{sawTiltClampLo, "tilt term clamped at -QCONST16(2,8) (:931)"},
		{sawTiltClampHi, "tilt term clamped at +QCONST16(2,8) (:931)"},
		{sawTiltFree, "tilt term unclamped (:931)"},
		{sawNegTrim, "trim < 0 at :945 (PSHR32 floors)"},
		{sawIndexSatLo, "trim_index < 0 before IMAX (:949)"},
		{sawIndexSatHi, "trim_index > 10 before IMIN (:949)"},
		{sawIndexMid, "trim_index already in [0,10] before the clamp (:949)"},
		{sawAdd16Wrap, "ADD16 16-bit wrap inside the sum accumulation (:894)"},
		{sawAdd16WrapObservable, "ADD16 wrap CHANGED sum after :896-897 (so a widened accumulator would be caught)"},
		{sawTrimInt16Wrap, "opus_val16 (int16) truncation of trim wrapped at :931-933"},
	}
	for _, g := range guards {
		if !g.ok {
			t.Fatalf("VACUOUS TEST: branch never exercised: %s", g.what)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("VACUOUS TEST: trim_index only took %d distinct values (%v); want >= 5",
			len(seen), seen)
	}
	if !seen[0] || !seen[10] {
		t.Fatalf("VACUOUS TEST: trim_index never hit both saturation ends: seen=%v", seen)
	}
	t.Logf("trim_index values observed: %v", seen)
}

// TestCeltencAllocTrimAnalysisRandomMatchesC is a broad randomized sweep on top of
// the targeted sequence above: it still threads stereo_saving across frames (the
// only cross-frame state), but draws every parameter at random.
//
// It also carries the test's SENSITIVITY guards. trim_index = PSHR32(trim,8) is a
// 256:1 quantizer of trim, so an off-by-one in trim (exactly what a floor/truncate
// mix-up at :929 or :931 produces) is invisible unless some frame lands on a
// PSHR32 boundary. surround_trim is therefore drawn at fine (sub-QCONST16(1,8))
// resolution so trim sweeps every residue mod 256, and the guards below require
// that a +-1 perturbation of trim WOULD have flipped trim_index on some frame,
// including on a frame where the negative, non-exact `/6` at :931 is live.
func TestCeltencAllocTrimAnalysisRandomMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8B_7822))
	// 19 is the wideband end band and is reachable in production. There is no branch
	// on end here, but the weights (2+2*i-end) and the divisor (C*(end-1)) both depend
	// on it, so every reachable end value is swept.
	ends := []int{13, 17, 19, 21}
	rates := []int32{8000, 31999, 32000, 63999, 64000, 64512, 70000, 79999, 80000, 128000, 510000}
	// Small tilts keep the :931 MIN32/MAX32 unclamped, which is the only regime in
	// which the `/6` rounding direction can reach trim at all.
	tiltScales := []float64{0.02, 0.06, 0.12, 0.2, 0.8}

	var (
		sensPlus1  int // a +1 error in trim would have flipped trim_index
		sensMinus1 int // a -1 error in trim would have flipped trim_index
		sensDiv6   int // ... on a frame where the negative non-exact /6 at :931 is live
		frames     int
	)

	for seq := 0; seq < 200; seq++ {
		var goSS, cSS int16
		for frame := 0; frame < 100; frame++ {
			channels := 1 + r.Intn(2)
			lm := r.Intn(4)
			end := ends[r.Intn(len(ends))]
			intensity := r.Intn(trimNbEBands + 1)
			equivRate := rates[r.Intn(len(rates))]
			// Fine-grained: SHR32(surroundTrim, 16) sweeps [-512,511] in unit steps.
			surroundTrim := int32(r.Intn(1<<26)) - (1 << 25)
			tfEstimate := int16(r.Intn(16385))
			corr := r.Float64()*2 - 1
			amp := 0.5 + r.Float64()*1.5
			tilt := (r.Float64()*2 - 1) * tiltScales[r.Intn(len(tiltScales))]
			N0 := 120 << lm

			x := trimNormX(r, channels, lm, N0, corr, amp)
			bandLogE := trimBandLogE(r, channels, 6+r.Float64()*20, tilt, r.Float64())

			goTrim, goSSOut, w := celt.AllocTrimAnalysis(x, bandLogE, end, lm, channels, N0,
				goSS, tfEstimate, intensity, surroundTrim, equivRate)
			cTrim, cSSOut := cCeltencAllocTrimAnalysis(x, bandLogE, end, lm, channels, N0,
				int32(cSS), int(tfEstimate), intensity, surroundTrim, equivRate)

			if goTrim != cTrim || int32(goSSOut) != cSSOut {
				t.Fatalf("seq %d frame %d (C=%d LM=%d end=%d intensity=%d rate=%d surround=%d tf=%d corr=%.3f amp=%.3f tilt=%.3f ssIn=%d): "+
					"trim_index Go=%d C=%d, stereo_saving Go=%d C=%d",
					seq, frame, channels, lm, end, intensity, equivRate, surroundTrim, tfEstimate,
					corr, amp, tilt, goSS, goTrim, cTrim, goSSOut, cSSOut)
			}
			goSS = goSSOut
			cSS = int16(cSSOut)
			frames++

			// --- sensitivity accounting ---
			// trim_index = (trim+128)>>8, then IMAX(0,IMIN(10,.)). A +1 error in trim
			// changes the index iff (trim+128) mod 256 == 255 and the index is <= 9;
			// a -1 error iff (trim+128) mod 256 == 0 and the index is >= 1.
			res := (int32(w.Trim) + 128) & 255
			if res == 255 && w.TrimIndexRaw >= 0 && w.TrimIndexRaw <= 9 {
				sensPlus1++
				// The floor-vs-truncate bug at :931 makes tiltRaw one SMALLER (more
				// negative), hence trim one LARGER. It only reaches trim when the
				// MIN32/MAX32 does not clamp and the quotient is negative and inexact.
				shrOut := fixedmath.SHR32(w.Diff+fixedmath.QCONST32(1.0, trimDbShift-5), trimDbShift-13)
				clamped := w.TiltRaw < -int32(fixedmath.QCONST16(2.0, 8)) ||
					w.TiltRaw > int32(fixedmath.QCONST16(2.0, 8))
				if !clamped && shrOut < 0 && shrOut%6 != 0 {
					sensDiv6++
				}
			}
			if res == 0 && w.TrimIndexRaw >= 1 && w.TrimIndexRaw <= 10 {
				sensMinus1++
			}
		}
	}

	t.Logf("frames=%d sensitivity: +1-detecting=%d -1-detecting=%d /6-detecting=%d",
		frames, sensPlus1, sensMinus1, sensDiv6)
	if sensPlus1 == 0 {
		t.Fatal("INSENSITIVE TEST: no frame where a +1 error in trim would flip trim_index")
	}
	if sensMinus1 == 0 {
		t.Fatal("INSENSITIVE TEST: no frame where a -1 error in trim would flip trim_index")
	}
	if sensDiv6 == 0 {
		t.Fatal("INSENSITIVE TEST: no frame where the negative, non-exact /6 at :931 " +
			"reaches trim_index; a floor/truncate mix-up there would go undetected")
	}
}

// TestCeltencStereoAnalysisMatchesC drives stereo_analysis (celt_encoder.c:957),
// which is stateless and read-only on X. The band loop is hard-coded to i<13, so
// there is no `end` to sweep; LM 0..3 is swept instead (LM<=1 drops 8 thetas at
// :983-984) and the inter-channel balance is swept so BOTH decision outcomes occur
// (matched channels -> joint stereo (0); panned/imbalanced -> dual stereo (1)).
//
// stereo_analysis returns a single BIT, so it is only sensitive to an error in the
// arithmetic when a frame sits close to the decision boundary at :985. The sweep
// therefore does not just draw random spectra: for every LM it BISECTS the
// right-channel gain to land on the boundary and then walks a dense integer
// neighbourhood of it, and it asserts (via the witness) that some frame's decision
// margin |lhs-rhs| came within 1/8192 of lhs. That is tighter than the perturbation
// a mistyped QCONST16(0.707107,15) (23170 -> 23168) would cause, so such an error
// cannot hide.
func TestCeltencStereoAnalysisMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0xC8B_7833))

	var sawZero, sawOne, sawLMLow, sawLMHigh bool
	minMargin := math.MaxFloat64
	calls := 0

	// check runs one frame on both sides, asserts agreement, and records witnesses.
	check := func(x []int32, lm, N0 int, label string) int {
		got, w := celt.StereoAnalysis(x, lm, N0)
		want := cCeltencStereoAnalysis(x, lm, N0)
		if got != want {
			t.Fatalf("stereo_analysis(LM=%d %s): Go=%d C=%d (lhs=%d rhs=%d thetas=%d)",
				lm, label, got, want, w.Lhs, w.Rhs, w.Thetas)
		}
		calls++
		if got == 0 {
			sawZero = true
		} else {
			sawOne = true
		}
		if lm <= 1 {
			sawLMLow = true
		} else {
			sawLMHigh = true
		}
		if w.Lhs > 0 {
			m := math.Abs(float64(w.Lhs)-float64(w.Rhs)) / float64(w.Lhs)
			if m < minMargin {
				minMargin = m
			}
		}
		return got
	}

	// --- broad sweep: correlation x amplitude x panning ----------------------
	corrs := []float64{-1.0, -0.99, -0.8, -0.4, 0.0, 0.2, 0.5, 0.8, 0.95, 0.99, 1.0}
	amps := []float64{0.25, 1.0, 2.0, 6.0}
	for _, lm := range []int{0, 1, 2, 3} {
		N0 := 120 << lm
		for _, corr := range corrs {
			for _, amp := range amps {
				for rep := 0; rep < 4; rep++ {
					x := trimNormX(r, 2, lm, N0, corr, amp)
					// Strongly panned content pushes sumMS/sumLR toward dual stereo.
					if rep >= 2 {
						shift := uint(rep + 2)
						for j := 0; j < N0; j++ {
							x[N0+j] >>= shift
						}
					}
					check(x, lm, N0, "sweep")
				}
			}
		}
	}

	// --- degenerate input: an all-zero spectrum (only EPSILON is left) --------
	for _, lm := range []int{0, 1, 2, 3} {
		N0 := 120 << lm
		check(make([]int32, 2*N0), lm, N0, "all-zero")
	}

	// --- boundary seeking: bisect the right-channel gain, then walk it densely -
	// scale() attenuates channel 1 by num/2^20, which moves sumMS/sumLR smoothly
	// enough to bracket the :985 comparison to within a few parts per million.
	//
	// Resolution matters as much as proximity: lhs and rhs are int32, so a relative
	// arithmetic error only becomes visible once it is worth at least 1 LSB of lhs.
	// The trials therefore use a large amp (sumMS grows with it) and a correlated
	// base (so that gain=1 decides joint and gain=0 decides dual, guaranteeing a
	// bracket), which puts lhs in the tens of thousands rather than the hundreds.
	const gainBits = 20
	bestLhs, bestGap := int64(1), int64(math.MaxInt32)
	for _, lm := range []int{0, 1, 2, 3} {
		N0 := 120 << lm
		for _, corr := range []float64{0.80, 0.92, 0.98} {
			for _, amp := range []float64{1.0, 2.5, 5.0} {
				base := trimNormX(r, 2, lm, N0, corr, amp)
				right := make([]int32, N0)
				copy(right, base[N0:])
				scale := func(num int64) []int32 {
					x := make([]int32, 2*N0)
					copy(x, base[:N0])
					for j := 0; j < N0; j++ {
						x[N0+j] = int32((int64(right[j]) * num) >> gainBits)
					}
					return x
				}
				// The bisection is driven by the C ORACLE, never by the Go code under
				// test: anchoring it on Go would let a buggy Go carry the boundary
				// along with it and the neighbourhood would still agree. Anchored on
				// C, any Go whose boundary sits elsewhere disagrees inside the walk.
				decide := func(num int64) int {
					return cCeltencStereoAnalysis(scale(num), lm, N0)
				}
				lo, hi := int64(0), int64(1)<<gainBits // gain 0 -> dual(1); gain 1 -> joint(0)
				if decide(lo) == decide(hi) {
					continue // no bracket on this draw; the sweep above still covers it
				}
				for hi-lo > 1 {
					mid := (lo + hi) / 2
					if decide(mid) == decide(lo) {
						lo = mid
					} else {
						hi = mid
					}
				}
				// Walk a dense integer neighbourhood of C's flip, checking every one
				// against C. The radius is wide enough to bracket where a slightly
				// wrong Go would place its own boundary (a 1e-4 relative error in the
				// :980 scaling moves it by a few hundred gain steps).
				for num := lo - 1024; num <= hi+1024; num++ {
					if num < 0 || num > int64(1)<<gainBits {
						continue
					}
					x := scale(num)
					check(x, lm, N0, "boundary")
					_, w := celt.StereoAnalysis(x, lm, N0)
					gap := w.Lhs - w.Rhs
					if gap < 0 {
						gap = -gap
					}
					// Track the frame with the finest RELATIVE resolution, i.e. the one
					// minimising (gap+1)/lhs: that ratio is the smallest relative
					// arithmetic error the single output bit can still reveal. Compared
					// by cross-multiplication to stay in integers.
					if w.Lhs > 0 && (int64(gap)+1)*bestLhs < (bestGap+1)*int64(w.Lhs) {
						bestGap, bestLhs = int64(gap), int64(w.Lhs)
					}
				}
			}
		}
	}

	t.Logf("calls=%d min decision margin |lhs-rhs|/lhs = %.3e", calls, minMargin)
	if !sawZero {
		t.Fatal("VACUOUS TEST: stereo_analysis never returned 0 (joint stereo)")
	}
	if !sawOne {
		t.Fatal("VACUOUS TEST: stereo_analysis never returned 1 (dual stereo)")
	}
	if !sawLMLow {
		t.Fatal("VACUOUS TEST: stereo_analysis never ran with LM<=1 (thetas -= 8, :983)")
	}
	if !sawLMHigh {
		t.Fatal("VACUOUS TEST: stereo_analysis never ran with LM>1 (thetas == 13)")
	}
	// Sensitivity. stereo_analysis returns a single BIT, so it can only reveal an
	// arithmetic error of relative size eps if some frame both (a) sits within eps
	// of the :985 boundary and (b) has lhs large enough that eps*lhs is worth at
	// least one int32 LSB. bestGap/bestLhs is the finest such frame the sweep found;
	// requiring bestGap <= 1 and bestLhs >= 32768 means any relative error down to
	// 1/32768 = 3.1e-5 flips the decision on that frame. That is comfortably finer
	// than a mistyped QCONST16(0.707107,15) (23170 -> 23168, i.e. 8.6e-5).
	t.Logf("finest boundary frame: |lhs-rhs|=%d lhs=%d (detects relative errors >= %.2e)",
		bestGap, bestLhs, float64(bestGap+1)/float64(bestLhs))
	if bestGap > 1 || bestLhs < 32768 {
		t.Fatalf("INSENSITIVE TEST: finest boundary frame was |lhs-rhs|=%d with lhs=%d; "+
			"the sweep never combined a razor-thin margin with enough resolution, so small "+
			"arithmetic errors at :980-986 would go undetected", bestGap, bestLhs)
	}
	// Pin the one magic constant the boundary test is calibrated against.
	if got := fixedmath.QCONST16(0.707107, 15); got != 23170 {
		t.Fatalf("QCONST16(0.707107,15) = %d, want 23170", got)
	}
}

// TestCeltencAllocTrimDivisionTruncationMatchesC pins the ONE arithmetic corner the
// randomized sweep above cannot reach often enough to be trusted: `diff /= C*(end-1)`
// at :929 is a C integer division, which truncates TOWARD ZERO on a negative
// dividend, and it must NOT be a shift (which would floor). Downstream, diff is
// scaled by 1/2048 (SHR32) and then by 1/6, so an off-by-one in diff normally
// evaporates long before it reaches trim_index; the probability of a random frame
// exposing it is about 1/3e6.
//
// So the frame is constructed rather than drawn. With C=1 and end=21 the :923-928
// accumulator weights bandLogE[i]>>5 by (2+2*i-end) = 2*i-19, which is exactly 1 at
// i=10, so putting D<<5 in bandLogE[10] and zero everywhere else makes diff (before
// the division) exactly D. D is chosen so that:
//
//	trunc: diff=-546816 -> SHR32 -> -11 -> /6 -> -1 -> trim=1151 -> trim_index=4
//	floor: diff=-546817 -> SHR32 -> -12 -> /6 -> -2 -> trim=1152 -> trim_index=5
//
// surround_trim = 130<<16 puts trim on a PSHR32(.,8) boundary so the one-unit
// difference actually flips the returned trim_index. If the port ever floors that
// division, the C says 4 and Go says 5.
func TestCeltencAllocTrimDivisionTruncationMatchesC(t *testing.T) {
	const (
		end          = 21
		channels     = 1
		lm           = 3
		N0           = 120 << lm
		equivRate    = 96000 // >= 80000, so trim starts at QCONST16(5,8) = 1280
		surroundTrim = 130 << 16
	)
	// D = 20*(2048*-11 - 524288) - 5: negative, and not a multiple of C*(end-1)=20,
	// so truncate-toward-zero and floor really do differ.
	const D = int32(-10936325)

	bandLogE := make([]int32, channels*trimNbEBands)
	bandLogE[10] = D << 5
	x := make([]int32, channels*N0) // C==1: alloc_trim_analysis never reads X

	goTrim, goSS, w := celt.AllocTrimAnalysis(x, bandLogE, end, lm, channels, N0,
		0, 0, 0, surroundTrim, equivRate)
	cTrim, cSS := cCeltencAllocTrimAnalysis(x, bandLogE, end, lm, channels, N0,
		0, 0, 0, surroundTrim, equivRate)

	if goTrim != cTrim || goSS != int16(cSS) {
		t.Fatalf("trim_index Go=%d C=%d, stereo_saving Go=%d C=%d", goTrim, cTrim, goSS, cSS)
	}
	// The construction must have landed, or the test is silently not testing this.
	if w.DiffPre != D {
		t.Fatalf("construction failed: diff before division = %d, want %d", w.DiffPre, D)
	}
	if w.Diff != -546816 {
		t.Fatalf("construction failed: diff after /= %d = %d, want -546816 "+
			"(a FLOORING division would give -546817)", channels*(end-1), w.Diff)
	}
	if w.TiltRaw != -1 {
		t.Fatalf("construction failed: tiltRaw = %d, want -1 (a flooring division "+
			"upstream would give -2)", w.TiltRaw)
	}
	if w.Trim != 1151 || w.TrimIndexRaw != 4 || goTrim != 4 {
		t.Fatalf("construction failed: trim=%d trim_index_raw=%d trim_index=%d; "+
			"want 1151 / 4 / 4 (a flooring division upstream would give 1152 / 5 / 5)",
			w.Trim, w.TrimIndexRaw, goTrim)
	}
	t.Logf("pinned: diffPre=%d diff=%d tiltRaw=%d trim=%d trim_index=%d (C agrees)",
		w.DiffPre, w.Diff, w.TiltRaw, w.Trim, cTrim)
}
