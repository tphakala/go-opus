//go:build refc

package oracle

// Differential test for the pure-Go SILK stereo un-mixing (internal/silk
// stereo_decode_pred.go + stereo_ms_to_lr.go) against the pinned libopus C
// (silk/stereo_decode_pred.c + silk/stereo_MS_to_LR.c).
//
// Two surfaces are covered:
//
//   1. TestSilkStereoDecodePredMatchesC exhaustively walks the ENTIRE predictor index
//      space (ix[n][0] in [0,2], ix[n][1] in [0,4], ix[n][2] in [0,4]) times both
//      mid-only values (11250 cases). Each case is round-tripped through the real C
//      encoder + decoder to produce a genuine bitstream and the C-decoded pred_Q13 /
//      mid-only / coder end state; the pure-Go StereoDecodePred + StereoDecodeMidOnly
//      decode the identical bytes and must agree bit-for-bit on pred_Q13, the mid-only
//      flag, the range decoder rng and ec_tell. This validates the entropy decode (the
//      joint-iCDF split into two coarse indices and the uniform3/uniform5 pairs) and the
//      fixed-point dequant together.
//
//   2. TestSilkStereoMStoLRSequenceMatchesC drives multi-frame SEQUENCES through both
//      the C and Go silk_stereo_MS_to_LR in lockstep, carrying the persistent
//      stereo_dec_state (pred_prev_Q13 + the sMid/sSide one-sample delay) across frames.
//      The predictors change every frame (sourced from the real predictor round-trip and
//      from structured extremes with large sign swings), so the 8 ms interpolation ramp
//      and the delay carry are exercised; the mid/side inputs mix random full-range,
//      near-full-scale constants (to trip the L/R SAT16 clamps), ramps, impulses and
//      low-amplitude noise. After every frame the L/R int16 output and the full stereo
//      state must be bit-identical. NB (8 kHz), MB (12 kHz) and WB (16 kHz) frame lengths
//      at both 20 ms (nb_subfr 4) and 10 ms (nb_subfr 2) are covered.
//
// The two mutation tests prove the assertions are not vacuous: corrupting the predictor
// bitstream, and feeding the un-mixer a differing predictor, both change the compared
// outputs.

import (
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// stereoBuildXBuf lays out one channel's frame samples into a frameLength+2 buffer with
// the samples at [2:frameLength+2], matching silk_stereo_MS_to_LR's expected layout (it
// overwrites [0:2] with the carried delay and writes the output to [1:frameLength+1]).
func stereoBuildXBuf(samples []int16, frameLength int) []int16 {
	x := make([]int16, frameLength+2)
	copy(x[2:], samples)
	return x
}

// stereoRandValidIx returns a random valid predictor index tuple (row-major ix[2][3]):
// ix[n][0] in [0,2], ix[n][1] in [0,4], ix[n][2] in [0,4], which keeps the C encoder's
// celt_assert bounds and the joint index (5*ix[0][2]+ix[1][2] < 25) satisfied.
func stereoRandValidIx(r *rand.Rand) [6]int8 {
	return [6]int8{
		int8(r.Intn(3)), int8(r.Intn(5)), int8(r.Intn(5)),
		int8(r.Intn(3)), int8(r.Intn(5)), int8(r.Intn(5)),
	}
}

// stereoSeqPredictors builds a per-frame predictor sequence. Most frames use realistic
// predictors sourced from the C encode/decode round-trip of random valid indices; every
// fifth frame is a structured extreme (large magnitudes and sign swings) so the delta
// interpolation (which truncates pred_Q13-pred_prev_Q13 to int16 inside silk_SMULBB) is
// pushed through its wrap-around region.
func stereoSeqPredictors(r *rand.Rand, nFrames int) [][2]int32 {
	extremes := [][2]int32{
		{0, 0},
		{27464, 13732}, {-27464, -13732},
		{27464, -13732}, {-27464, 13732},
		{13732, 13732}, {-13732, -13732},
	}
	preds := make([][2]int32, nFrames)
	for i := 0; i < nFrames; i++ {
		if i%5 == 0 {
			preds[i] = extremes[r.Intn(len(extremes))]
			continue
		}
		res := stereoPredRoundtripC(stereoRandValidIx(r), r.Intn(2))
		preds[i] = res.predQ13
	}
	return preds
}

// stereoGenMS generates one frame of mid/side int16 samples of the given kind.
func stereoGenMS(r *rand.Rand, kind, frameLength int) (mid, side []int16) {
	mid = make([]int16, frameLength)
	side = make([]int16, frameLength)
	switch kind {
	case 0: // random full-range
		for i := range mid {
			mid[i] = int16(r.Intn(65536) - 32768)
			side[i] = int16(r.Intn(65536) - 32768)
		}
	case 1: // near-full-scale constants: trips the L = mid+side / R = mid-side SAT16
		mv := int16(30000 - r.Intn(6000))
		sv := int16(30000 - r.Intn(6000))
		if r.Intn(2) == 0 {
			sv = -sv
		}
		for i := range mid {
			mid[i] = mv
			side[i] = sv
		}
	case 2: // ramps
		for i := range mid {
			mid[i] = int16((i*37)%60000 - 30000)
			side[i] = int16((i*53)%60000 - 30000)
		}
	case 3: // silence with full-scale impulses
		for i := range mid {
			if i%17 == 0 {
				mid[i] = 32767
			}
			if i%23 == 0 {
				side[i] = -32768
			}
		}
	default: // low-amplitude noise: exercises the rounding in the prediction add
		for i := range mid {
			mid[i] = int16(r.Intn(9) - 4)
			side[i] = int16(r.Intn(9) - 4)
		}
	}
	return mid, side
}

func TestSilkStereoDecodePredMatchesC(t *testing.T) {
	count := 0
	for a0 := 0; a0 < 3; a0++ {
		for a1 := 0; a1 < 5; a1++ {
			for a2 := 0; a2 < 5; a2++ {
				for b0 := 0; b0 < 3; b0++ {
					for b1 := 0; b1 < 5; b1++ {
						for b2 := 0; b2 < 5; b2++ {
							for mid := 0; mid < 2; mid++ {
								ix := [6]int8{
									int8(a0), int8(a1), int8(a2),
									int8(b0), int8(b1), int8(b2),
								}
								res := stereoPredRoundtripC(ix, mid)

								var dec rangecoding.Decoder
								dec.Init(res.buf)
								var goPred [2]int32
								silk.StereoDecodePred(&dec, &goPred)
								goMid := silk.StereoDecodeMidOnly(&dec)
								goRng := dec.Rng()
								goTell := dec.Tell()

								if goPred != res.predQ13 {
									t.Fatalf("ix=%v mid=%d: pred_Q13 C=%v Go=%v", ix, mid, res.predQ13, goPred)
								}
								if goMid != res.midOnly {
									t.Fatalf("ix=%v mid=%d: mid_only C=%d Go=%d", ix, mid, res.midOnly, goMid)
								}
								if goRng != res.rng {
									t.Fatalf("ix=%v mid=%d: rng C=%d Go=%d", ix, mid, res.rng, goRng)
								}
								if goTell != res.tell {
									t.Fatalf("ix=%v mid=%d: tell C=%d Go=%d", ix, mid, res.tell, goTell)
								}
								count++
							}
						}
					}
				}
			}
		}
	}
	if count != 11250 {
		t.Fatalf("expected 11250 exhaustive combos, ran %d", count)
	}
}

func TestSilkStereoMStoLRSequenceMatchesC(t *testing.T) {
	configs := []struct {
		name    string
		fsKHz   int
		nbSubfr int
	}{
		{"NB_20ms", 8, 4},
		{"MB_20ms", 12, 4},
		{"WB_20ms", 16, 4},
		{"NB_10ms", 8, 2},
		{"MB_10ms", 12, 2},
		{"WB_10ms", 16, 2},
	}
	const nFrames = 32
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			frameLength := cfg.nbSubfr * 5 * cfg.fsKHz
			r := rand.New(rand.NewSource(int64(cfg.fsKHz*1000 + cfg.nbSubfr)))
			preds := stereoSeqPredictors(r, nFrames)

			var cState stereoState
			var goState silk.StereoDecState

			for f := 0; f < nFrames; f++ {
				kind := f % 5
				mid, side := stereoGenMS(r, kind, frameLength)
				pred := preds[f]

				x1c := stereoBuildXBuf(mid, frameLength)
				x2c := stereoBuildXBuf(side, frameLength)
				x1g := stereoBuildXBuf(mid, frameLength)
				x2g := stereoBuildXBuf(side, frameLength)

				cState.msToLR(x1c, x2c, pred, cfg.fsKHz, frameLength)
				goPred := pred
				silk.StereoMStoLR(&goState, x1g, x2g, &goPred, cfg.fsKHz, frameLength)

				// Valid output is x1[1:frameLength+1] (left) and x2[1:frameLength+1] (right).
				for i := 1; i <= frameLength; i++ {
					if x1c[i] != x1g[i] {
						t.Fatalf("%s frame %d kind %d: left[%d] C=%d Go=%d pred=%v",
							cfg.name, f, kind, i-1, x1c[i], x1g[i], pred)
					}
					if x2c[i] != x2g[i] {
						t.Fatalf("%s frame %d kind %d: right[%d] C=%d Go=%d pred=%v",
							cfg.name, f, kind, i-1, x2c[i], x2g[i], pred)
					}
				}
				if cState.predPrevQ13 != goState.PredPrevQ13 {
					t.Fatalf("%s frame %d: pred_prev_Q13 C=%v Go=%v",
						cfg.name, f, cState.predPrevQ13, goState.PredPrevQ13)
				}
				if cState.sMid != goState.SMid {
					t.Fatalf("%s frame %d: sMid C=%v Go=%v", cfg.name, f, cState.sMid, goState.SMid)
				}
				if cState.sSide != goState.SSide {
					t.Fatalf("%s frame %d: sSide C=%v Go=%v", cfg.name, f, cState.sSide, goState.SSide)
				}
			}
		})
	}
}

// TestSilkStereoDecodePredMutationDetected proves the predictor differential comparison
// is not vacuous: corrupting the bitstream changes at least one asserted field.
func TestSilkStereoDecodePredMutationDetected(t *testing.T) {
	ix := [6]int8{2, 3, 4, 1, 1, 2}
	res := stereoPredRoundtripC(ix, 1)

	var dec rangecoding.Decoder
	dec.Init(res.buf)
	var goPred [2]int32
	silk.StereoDecodePred(&dec, &goPred)
	goMid := silk.StereoDecodeMidOnly(&dec)
	if goPred != res.predQ13 || goMid != res.midOnly {
		t.Fatal("baseline C/Go stereo pred unexpectedly differ")
	}

	bad := append([]byte(nil), res.buf...)
	bad[0] ^= 0xff
	var dec2 rangecoding.Decoder
	dec2.Init(bad)
	var badPred [2]int32
	silk.StereoDecodePred(&dec2, &badPred)
	badMid := silk.StereoDecodeMidOnly(&dec2)
	if badPred == res.predQ13 && badMid == res.midOnly &&
		dec2.Rng() == res.rng && dec2.Tell() == res.tell {
		t.Fatal("corrupting the bitstream changed nothing; comparison is vacuous")
	}
}

// TestSilkStereoMStoLRMutationDetected proves the un-mixing differential comparison is
// not vacuous: from an identical populated state, a differing predictor makes the C and
// Go L/R output diverge.
func TestSilkStereoMStoLRMutationDetected(t *testing.T) {
	const fsKHz, frameLength = 16, 320
	r := rand.New(rand.NewSource(4242))

	var cState stereoState
	var goState silk.StereoDecState
	pred := [2]int32{5000, -3000}
	for f := 0; f < 3; f++ {
		mid, side := stereoGenMS(r, 0, frameLength)
		x1c := stereoBuildXBuf(mid, frameLength)
		x2c := stereoBuildXBuf(side, frameLength)
		x1g := stereoBuildXBuf(mid, frameLength)
		x2g := stereoBuildXBuf(side, frameLength)
		goPred := pred
		cState.msToLR(x1c, x2c, pred, fsKHz, frameLength)
		silk.StereoMStoLR(&goState, x1g, x2g, &goPred, fsKHz, frameLength)
		if x1c[1] != x1g[1] {
			t.Fatal("baseline C/Go left output unexpectedly differ")
		}
		pred[0] = -pred[0]
	}

	// From the identical populated state, feed a predictor that differs by 0.5 in Q13
	// (4096) and confirm the output diverges: only the predictor differs, so a match
	// would mean the comparison ignores the predictor.
	mid, side := stereoGenMS(r, 0, frameLength)
	x1c := stereoBuildXBuf(mid, frameLength)
	x2c := stereoBuildXBuf(side, frameLength)
	x1g := stereoBuildXBuf(mid, frameLength)
	x2g := stereoBuildXBuf(side, frameLength)

	cs := cState
	gs := goState
	cPred := [2]int32{4000, 2000}
	gPred := [2]int32{4000 + 4096, 2000}
	cs.msToLR(x1c, x2c, cPred, fsKHz, frameLength)
	silk.StereoMStoLR(&gs, x1g, x2g, &gPred, fsKHz, frameLength)

	diverged := false
	for i := 1; i <= frameLength; i++ {
		if x1c[i] != x1g[i] || x2c[i] != x2g[i] {
			diverged = true
			break
		}
	}
	if !diverged {
		t.Fatal("differing predictors did NOT change the output; comparison is vacuous")
	}
}
