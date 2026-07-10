//go:build refc

package oracle

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This differential test pins the pure-Go CELT residual band ENCODE
// (internal/celt bands.go: QuantAllBandsEncode, the CP7 quant_all_bands encode
// path) to the pinned libopus C oracle (celt/bands.c, encode=1). Both sides run
// the identical full encode pipeline (init_caps -> clt_compute_allocation ->
// quant_all_bands -> ec_enc_done) over the SAME random normalized spectrum, band
// energies, allocation inputs and complexity, and every output is asserted
// bit-identical: the finalized bitstream (range-coder head AND raw-bit tail), the
// collapse_masks, the threaded LCG seed, the pre-done range state (tell + rng),
// codedBands/intensity/dual_stereo, and (for the resynth cases: stereo,
// !dual_stereo, complexity>=8) the reconstructed spectrum X. Reuses the decode
// harness helpers (genTfRes/genNormX/genBandE/clampInt/equalBytes/loadEBands/
// bandsCompareXregion/cBandsModeInfo) from bands_test.go and vq_test.go without
// editing them.

// isResynthCase reports whether quant_all_bands runs the decoder-side band
// synthesis on encode (resynth = theta_rdo = encode && stereo && !dual_stereo &&
// complexity>=8), so the reconstructed X is meaningful and comparable.
func isResynthCase(C, dualStereo, complexity int) bool {
	return C == 2 && dualStereo == 0 && complexity >= 8
}

// goBandsEncode replicates the encoder mini-pipeline in pure Go over a fresh
// buffer: it derives bits/anti_collapse_rsv/total_bits exactly as the C oracle
// does, runs celt.ComputeAllocation(encode=1) to emit the skip/intensity/dual
// flags and produce pulses/codedBands/balance, then celt.QuantAllBandsEncode.
// Returns the finalized bitstream and the encode end state. x holds the resynth
// spectrum (only meaningful for the resynth cases).
func goBandsEncode(start, end, LM, C, isTransient, spread, intensityIn, dualStereoIn,
	disableInv, allocTrim, complexity, length int, offsets, tfRes []int, bandE, xin []int32,
	seedIn uint32, nbEBands, N int) (buf []byte, x []int32, collapse []byte, seed uint32,
	tell int, rng uint32, codedBands, intensity, dualStereo int) {
	M := 1 << LM
	buf = make([]byte, length)
	var enc rangecoding.Encoder
	enc.Init(buf)

	totalBitsFrac := (int32(length) * 8) << bandsBitRes
	bits := totalBitsFrac - int32(enc.TellFrac()) - 1
	antiCollapseRsv := 0
	if isTransient != 0 && LM >= 2 && bits >= int32((LM+2)<<bandsBitRes) {
		antiCollapseRsv = 1 << bandsBitRes
	}
	bits -= int32(antiCollapseRsv)

	cap := make([]int, nbEBands)
	celt.InitCaps(cap, LM, C)
	intensity = intensityIn
	dualStereo = dualStereoIn
	pulses := make([]int, nbEBands)
	ebits := make([]int, nbEBands)
	finePrio := make([]int, nbEBands)
	var balance int
	codedBands, balance = celt.ComputeAllocation(start, end, offsets, cap, allocTrim,
		&intensity, &dualStereo, int(bits), pulses, ebits, finePrio, C, LM, &enc, nil, 1, 0, 0)

	x = make([]int32, C*N)
	copy(x, xin)
	var Y []int32
	if C == 2 {
		Y = x[N:]
	}
	collapse = make([]byte, C*nbEBands)
	seed = seedIn
	shortBlocks := 0
	if isTransient != 0 {
		shortBlocks = M
	}
	totalBits := int32(length)*(8<<bandsBitRes) - int32(antiCollapseRsv)
	celt.QuantAllBandsEncode(start, end, x[:N], Y, collapse, bandE, pulses, shortBlocks, spread,
		dualStereo, intensity, tfRes, totalBits, int32(balance), &enc, LM, codedBands, &seed,
		complexity, disableInv)

	tell = enc.Tell()
	rng = enc.Rng()
	enc.EncDone()
	return buf, x, collapse, seed, tell, rng, codedBands, intensity, dualStereo
}

// firstByteDiff returns the index of the first differing byte, or -1.
func firstByteDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// encLabel names an encode case, including complexity.
func encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length int) string {
	return fmt.Sprintf("LM=%d C=%d tr=%d spread=%d cx=%d dinv=%d end=%d len=%d",
		LM, C, isTransient, spread, complexity, disableInv, end, length)
}

// TestQuantAllBandsEncodeMatchesC is the primary encode check: over a grid of
// LM, channels, transient/short-block, spread, complexity (below and at/above
// the theta-RDO threshold), intensity/dual stereo, disable_inv, band count and
// bitrate, the pure-Go encoder must produce the exact same bitstream and end
// state as the C oracle, and the same reconstruction for the resynth cases.
func TestQuantAllBandsEncodeMatchesC(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0xE0C7))
	cases := 0
	resynthCases := 0

	for _, LM := range []int{0, 1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			for _, isTransient := range []int{0, 1} {
				if isTransient != 0 && LM == 0 {
					continue // the transient flag is only coded for LM>0
				}
				for _, spread := range []int{0, 1, 2, 3} {
					for _, complexity := range []int{0, 5, 10} {
						disableInvs := []int{0}
						if C == 2 {
							disableInvs = []int{0, 1}
						}
						for _, disableInv := range disableInvs {
							for _, end := range []int{nbEBands, nbEBands - 3} {
								lenChoices := []int{
									clampInt(C*N/6, 12, 1275),
									clampInt(C*N/40, 10, 1275),
								}
								for _, length := range lenChoices {
									start := 0
									offsets := make([]int, nbEBands)
									tfRes := genTfRes(r, nbEBands, LM, isTransient)
									intensity := 0
									if C == 2 {
										intensity = start + r.Intn(end-start+1)
									}
									dualStereo := 0
									if C == 2 {
										dualStereo = r.Intn(2)
									}
									allocTrim := r.Intn(11)
									seedIn := r.Uint32()
									bandE := genBandE(r, nbEBands)
									xin := genNormX(r, C, N, M, eb, nbEBands)

									cBuf := make([]byte, length)
									cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
										dualStereo, disableInv, allocTrim, complexity, length, offsets, tfRes,
										bandE, xin, seedIn, cBuf, nbEBands, N)

									gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
										start, end, LM, C, isTransient, spread, intensity, dualStereo,
										disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)

									bound := M * eb[end]
									label := encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length)
									assertEncMatchBuf(t, label, C, N, bound, complexity,
										gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)
									if isResynthCase(C, gDual, complexity) {
										resynthCases++
									}
									cases++
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("quant_all_bands ENCODE differential cases: %d (resynth: %d)", cases, resynthCases)
	if resynthCases == 0 {
		t.Fatal("no resynth (theta-RDO) encode case was exercised; the sweep may be degenerate")
	}
}

// assertEncMatchBuf is assertEncMatch with the C buffer passed explicitly (the
// C oracle writes its finalized bytes into cBuf).
func assertEncMatchBuf(t *testing.T, label string, C, N, bound, complexity int,
	gBuf []byte, gX []int32, gCollapse []byte, gSeed uint32, gTell int, gRng uint32,
	gCoded, gIntensity, gDual int, cBuf []byte, cRes bandsEncResult) {
	t.Helper()
	if d := firstByteDiff(gBuf, cBuf); d >= 0 {
		t.Fatalf("%s: bitstream diverges at byte %d: Go=0x%02x C=0x%02x (len %d)",
			label, d, gBuf[d], cBuf[d], len(gBuf))
	}
	if !equalBytes(gCollapse, cRes.collapseMasks) {
		t.Fatalf("%s: collapse masks Go=%v C=%v", label, gCollapse, cRes.collapseMasks)
	}
	if gSeed != cRes.seed {
		t.Fatalf("%s: seed Go=%d C=%d", label, gSeed, cRes.seed)
	}
	if gTell != cRes.tell {
		t.Fatalf("%s: tell Go=%d C=%d", label, gTell, cRes.tell)
	}
	if gRng != cRes.rng {
		t.Fatalf("%s: rng Go=%d C=%d", label, gRng, cRes.rng)
	}
	if gCoded != cRes.codedBands {
		t.Fatalf("%s: codedBands Go=%d C=%d", label, gCoded, cRes.codedBands)
	}
	if gIntensity != cRes.intensity || gDual != cRes.dualStereo {
		t.Fatalf("%s: intensity/dual Go=(%d,%d) C=(%d,%d)", label,
			gIntensity, gDual, cRes.intensity, cRes.dualStereo)
	}
	if isResynthCase(C, gDual, complexity) {
		bandsCompareXregion(t, gX, cRes.x, C, N, bound, "resynth:"+label)
	}
}

// TestQuantAllBandsEncodeThetaRDO drives the theta-RDO trial loop directly:
// stereo, dual_stereo forced off, intensity forced to the max (so every band
// with a bit budget runs the theta-down / theta-up / splice logic), at
// complexity 8, 9 and 10 over a generous bit budget. It asserts the Go encoder
// matches C bit-for-bit (bitstream head + raw-bit tail + reconstruction) AND is
// a non-vacuity check that the RDO path actually alters the output: for each
// case it re-encodes the SAME input at complexity 7 (RDO off) and requires that
// across the sweep the complexity>=8 bitstream differs from the complexity-7 one
// at least once (proving the trial loop ran and the down/up choice moved bits).
func TestQuantAllBandsEncodeThetaRDO(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x7DA0))
	cases := 0
	rdoAltered := 0

	C := 2
	for _, LM := range []int{1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, isTransient := range []int{0, 1} {
			for _, spread := range []int{0, 2, 3} {
				for trial := 0; trial < 6; trial++ {
					start, end := 0, nbEBands
					disableInv := r.Intn(2)
					// A generous budget so many bands get PVQ bits and i<intensity.
					length := clampInt(C*N/3, 60, 1275)
					offsets := make([]int, nbEBands)
					tfRes := genTfRes(r, nbEBands, LM, isTransient)
					intensity := end // maximal: every coded band can run theta-RDO
					dualStereo := 0  // required for theta_rdo
					allocTrim := r.Intn(11)
					seedIn := r.Uint32()
					bandE := genBandE(r, nbEBands)
					xin := genNormX(r, C, N, M, eb, nbEBands)
					bound := M * eb[end]

					// Reference bitstream with RDO OFF (complexity 7) for the
					// non-vacuity comparison.
					ref7 := make([]byte, length)
					_ = cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
						dualStereo, disableInv, allocTrim, 7, length, offsets, tfRes, bandE, xin, seedIn, ref7, nbEBands, N)

					for _, complexity := range []int{8, 9, 10} {
						cBuf := make([]byte, length)
						cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
							dualStereo, disableInv, allocTrim, complexity, length, offsets, tfRes,
							bandE, xin, seedIn, cBuf, nbEBands, N)

						gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
							start, end, LM, C, isTransient, spread, intensity, dualStereo,
							disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)

						label := "rdo:" + encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length)
						assertEncMatchBuf(t, label, C, N, bound, complexity,
							gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)

						if !equalBytes(cBuf, ref7) {
							rdoAltered++
						}
						cases++
					}
				}
			}
		}
	}
	t.Logf("theta-RDO ENCODE cases: %d (bitstreams altered vs complexity-7: %d)", cases, rdoAltered)
	if rdoAltered == 0 {
		t.Fatal("theta-RDO never altered the bitstream vs the RDO-off baseline; the RDO path may be inactive")
	}
}

// genSpreadWeight builds nbEBands positive spread weights in {1,2,4} (the values
// the encoder derives from tonality), guaranteeing sum>0 for spreading_decision.
func genSpreadWeight(r *rand.Rand, nbEBands int) []int {
	sw := make([]int, nbEBands)
	choices := []int{1, 2, 4}
	for i := range sw {
		sw[i] = choices[r.Intn(len(choices))]
	}
	return sw
}

// TestSpreadingDecisionMatchesC pins the encoder-only spreading_decision
// (bands.c:470) to the C oracle over a grid of LM, channels, band count,
// update_hf and running state, asserting the returned SPREAD_* decision and the
// read-modify-write average / hf_average / tapset_decision all match. It is a
// non-vacuity check too: it requires that more than one distinct decision value
// is produced across the sweep.
func TestSpreadingDecisionMatchesC(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x59DEC))
	cases := 0
	seenDecision := map[int]bool{}

	for _, LM := range []int{0, 1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, channels := range []int{1, 2} {
			for _, end := range []int{nbEBands, nbEBands - 3, 3} {
				for _, updateHf := range []int{0, 1} {
					for trial := 0; trial < 4; trial++ {
						xin := genNormX(r, channels, N, M, eb, nbEBands)
						spreadWeight := genSpreadWeight(r, nbEBands)
						lastDecision := r.Intn(4)
						average := r.Intn(512)
						hfAverage := r.Intn(64)
						tapset := r.Intn(3)

						// C (state in by value, out returned).
						cDec, cAvg, cHf, cTap := cSpreadingDecision(xin, average, lastDecision,
							hfAverage, tapset, updateHf, end, channels, M, spreadWeight, nbEBands)

						// Go (state in by pointer, mutated in place).
						gAvg, gHf, gTap := average, hfAverage, tapset
						gDec := celt.SpreadingDecision(xin, &gAvg, lastDecision, &gHf, &gTap,
							updateHf, end, channels, M, spreadWeight)

						label := fmt.Sprintf("LM=%d C=%d end=%d uhf=%d", LM, channels, end, updateHf)
						if gDec != cDec {
							t.Fatalf("%s: decision Go=%d C=%d", label, gDec, cDec)
						}
						if gAvg != cAvg || gHf != cHf || gTap != cTap {
							t.Fatalf("%s: state Go=(avg %d, hf %d, tap %d) C=(avg %d, hf %d, tap %d)",
								label, gAvg, gHf, gTap, cAvg, cHf, cTap)
						}
						seenDecision[gDec] = true
						cases++
					}
				}
			}
		}
	}
	t.Logf("spreading_decision differential cases: %d (distinct decisions: %d)", cases, len(seenDecision))
	if len(seenDecision) < 2 {
		t.Fatalf("spreading_decision produced only %d distinct value(s); the sweep may be degenerate", len(seenDecision))
	}
}

// TestQuantAllBandsEncodeMutation is the non-vacuity check for the main harness:
// the encoded bitstream must be non-trivial and must react to an input change,
// while Go continues to match C. It also self-checks the byte comparator.
func TestQuantAllBandsEncodeMutation(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x31FE))
	mutationObserved := false
	comparatorChecked := false

	for _, LM := range []int{2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			start, end := 0, nbEBands
			isTransient := 1
			spread := 2
			complexity := 10
			disableInv := 0
			length := clampInt(C*N/6, 12, 1275)
			offsets := make([]int, nbEBands)
			tfRes := genTfRes(r, nbEBands, LM, isTransient)
			intensity := 0
			dualStereo := 0
			if C == 2 {
				intensity = end
			}
			allocTrim := 5
			seedIn := r.Uint32()
			bandE := genBandE(r, nbEBands)
			xin := genNormX(r, C, N, M, eb, nbEBands)
			bound := M * eb[end]

			cBuf := make([]byte, length)
			cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, cBuf, nbEBands, N)
			gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
				start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)

			label := "mut:" + encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length)
			assertEncMatchBuf(t, label, C, N, bound, complexity,
				gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)

			// The bitstream must carry real content.
			nonzero := false
			for _, b := range cBuf {
				if b != 0 {
					nonzero = true
					break
				}
			}
			if !nonzero {
				t.Fatalf("%s: encoded bitstream is all zero; harness is vacuous", label)
			}

			// Comparator self-check: a one-byte bump of the C bitstream must be flagged.
			mut := append([]byte(nil), cBuf...)
			mut[0] ^= 0xFF
			if equalBytes(mut, cBuf) {
				t.Fatal("byte comparator failed to detect a one-byte perturbation")
			}
			comparatorChecked = true

			// Real mutation: perturb the input spectrum. Go and C must both change
			// and still agree with each other.
			xin2 := append([]int32(nil), xin...)
			xin2[0] += 1 << 20
			mBuf := make([]byte, length)
			mRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin2, seedIn, mBuf, nbEBands, N)
			gBuf2, gX2, gColl2, gSeed2, gTell2, gRng2, gCoded2, gInt2, gDual2 := goBandsEncode(
				start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin2, seedIn, nbEBands, N)
			assertEncMatchBuf(t, "mut2:"+label, C, N, bound, complexity,
				gBuf2, gX2, gColl2, gSeed2, gTell2, gRng2, gCoded2, gInt2, gDual2, mBuf, mRes)
			if !equalBytes(mBuf, cBuf) {
				mutationObserved = true
			}
		}
	}
	if !comparatorChecked {
		t.Fatal("comparator self-check never ran")
	}
	if !mutationObserved {
		t.Fatal("input mutation never changed the bitstream; the harness may be vacuous")
	}
}

// ---------------------------------------------------------------------------
// Coverage tests for the three trace-clean-but-untested encode branches Fable
// flagged (hybrid start!=0 folding, MIN_STEREO_ENERGY copy-down, and the
// avoid_split_noise theta snap). Oracle-only; no production code is changed.
// ---------------------------------------------------------------------------

// TestQuantAllBandsEncodeHybrid exercises the start!=0 (hybrid) encode path,
// which both encode sweeps otherwise never touch (start=0 makes
// specialHybridFolding a no-op). start=17 makes normOffset != 0 and, because
// band 19 is wider than band 18, drives the specialHybridFolding copy. Stereo
// theta-RDO cases with intensity>start+1 additionally run the i==start+1 refold
// before the round-up trial. Everything is asserted bit-exact vs C.
func TestQuantAllBandsEncodeHybrid(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x11B21D))
	start := 17
	// Witness that specialHybridFolding actually copies (n2 > n1 for this start).
	n1w := eb[start+1] - eb[start]
	n2w := eb[start+2] - eb[start+1]
	if !(n2w > n1w) {
		t.Fatalf("start=%d does not exercise the specialHybridFolding copy (n1=%d n2=%d)", start, n1w, n2w)
	}
	cases := 0
	rdoRefoldCases := 0

	for _, LM := range []int{1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			for _, isTransient := range []int{0, 1} {
				for _, complexity := range []int{0, 10} {
					for trial := 0; trial < 4; trial++ {
						end := nbEBands
						spread := r.Intn(4)
						disableInv := 0
						if C == 2 {
							disableInv = r.Intn(2)
						}
						length := clampInt(C*N/4, 40, 1275)
						offsets := make([]int, nbEBands)
						tfRes := genTfRes(r, nbEBands, LM, isTransient)
						intensity := 0
						dualStereo := 0
						if C == 2 {
							intensity = end // so band start+1 < intensity runs theta-RDO
						}
						allocTrim := r.Intn(11)
						seedIn := r.Uint32()
						bandE := genBandE(r, nbEBands)
						xin := genNormX(r, C, N, M, eb, nbEBands)

						cBuf := make([]byte, length)
						cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
							dualStereo, disableInv, allocTrim, complexity, length, offsets, tfRes,
							bandE, xin, seedIn, cBuf, nbEBands, N)
						gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
							start, end, LM, C, isTransient, spread, intensity, dualStereo,
							disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)

						bound := M * eb[end]
						label := "hybrid:" + encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length)
						assertEncMatchBuf(t, label, C, N, bound, complexity,
							gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)
						if isResynthCase(C, gDual, complexity) && intensity > start+1 {
							rdoRefoldCases++
						}
						cases++
					}
				}
			}
		}
	}
	t.Logf("hybrid (start=17) ENCODE cases: %d (theta-RDO i==start+1 refold: %d)", cases, rdoRefoldCases)
	if rdoRefoldCases == 0 {
		t.Fatal("no hybrid theta-RDO case exercised the i==start+1 refold path")
	}
}

// genBandENearSilent builds 2*nbEBands band energies mostly above the floor but
// with a few coded bands pushed below MIN_STEREO_ENERGY (2) on one channel
// (covering left-silent, right-silent and the both-equal tie), returning the
// count of coded bands that carry a near-silent channel.
func genBandENearSilent(r *rand.Rand, nbEBands, start, end int) ([]int32, int) {
	be := make([]int32, 2*nbEBands)
	for i := range be {
		be[i] = int32(100 + r.Intn(1<<22))
	}
	silent := 0
	mark := func(band, left, right int) {
		if band >= start && band < end {
			be[band] = int32(left)
			be[nbEBands+band] = int32(right)
			if left < 2 || right < 2 {
				silent++
			}
		}
	}
	mark(start, 1, 500000)   // left near-silent  -> copy X <- Y
	mark(start+1, 500000, 1) // right near-silent -> copy Y <- X
	mark(start+2, 1, 1)      // tie (both below the floor)
	return be, silent
}

// TestQuantAllBandsEncodeMinStereoEnergy exercises the MIN_STEREO_ENERGY
// copy-down in quant_band_stereo, which genBandE (energies >= 100) never
// triggers. It injects near-silent channels into a few coded stereo bands and
// asserts Go==C bit-exact, plus a differential: removing the near-silent floor
// (bumping those bands above 2) must change the bitstream, confirming the
// near-silent handling is live.
func TestQuantAllBandsEncodeMinStereoEnergy(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x51E27))
	C := 2
	cases := 0
	silentTotal := 0
	differed := 0
	for _, LM := range []int{1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, isTransient := range []int{0, 1} {
			for _, complexity := range []int{0, 5, 10} {
				for trial := 0; trial < 3; trial++ {
					start, end := 0, nbEBands
					spread := r.Intn(4)
					disableInv := r.Intn(2)
					length := clampInt(C*N/6, 40, 1275)
					offsets := make([]int, nbEBands)
					tfRes := genTfRes(r, nbEBands, LM, isTransient)
					intensity := end
					dualStereo := 0
					allocTrim := r.Intn(11)
					seedIn := r.Uint32()
					xin := genNormX(r, C, N, M, eb, nbEBands)
					bandE, silent := genBandENearSilent(r, nbEBands, start, end)

					cBuf := make([]byte, length)
					cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
						dualStereo, disableInv, allocTrim, complexity, length, offsets, tfRes,
						bandE, xin, seedIn, cBuf, nbEBands, N)
					gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
						start, end, LM, C, isTransient, spread, intensity, dualStereo,
						disableInv, allocTrim, complexity, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)
					bound := M * eb[end]
					label := "minstereo:" + encLabel(LM, C, isTransient, spread, complexity, disableInv, end, length)
					assertEncMatchBuf(t, label, C, N, bound, complexity,
						gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)

					// Differential: same input, but the near-silent bands bumped
					// above the floor so the copy-down cannot fire.
					bandEHi := append([]int32(nil), bandE...)
					for _, band := range []int{start, start + 1, start + 2} {
						if band < end {
							bandEHi[band] = int32(1000 + band)
							bandEHi[nbEBands+band] = int32(2000 + band)
						}
					}
					hiBuf := make([]byte, length)
					_ = cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
						dualStereo, disableInv, allocTrim, complexity, length, offsets, tfRes,
						bandEHi, xin, seedIn, hiBuf, nbEBands, N)
					if !equalBytes(cBuf, hiBuf) {
						differed++
					}
					silentTotal += silent
					cases++
				}
			}
		}
	}
	t.Logf("MIN_STEREO_ENERGY ENCODE cases: %d (near-silent coded bands: %d, bitstream changed vs floor-free: %d)",
		cases, silentTotal, differed)
	if silentTotal == 0 {
		t.Fatal("no near-silent coded band was injected; the harness is vacuous")
	}
	if differed == 0 {
		t.Fatal("MIN_STEREO_ENERGY copy-down never changed the bitstream vs floor-free bandE")
	}
}

// Test-local verbatim copies of the compute_theta snap-predicate math (proven
// == C through the decode differential). Used only to independently confirm the
// avoid_split_noise branch condition holds for the crafted inputs.
const (
	snBitRes       = 3
	snQThetaOffset = 4
)

func snFracMul16(a, b int32) int32 {
	return (16384 + int32(int16(a))*int32(int16(b))) >> 15
}

func snBitexactCos(x int16) int16 {
	tmp := (4096 + int32(x)*int32(x)) >> 13
	x2 := int16(tmp)
	x2 = int16((32767 - int32(x2)) + snFracMul16(int32(x2), -7651+snFracMul16(int32(x2), 8277+snFracMul16(-626, int32(x2)))))
	return 1 + x2
}

func snBitexactLog2tan(isin, icos int) int {
	lc := fixedmath.EC_ILOG(uint32(icos))
	ls := fixedmath.EC_ILOG(uint32(isin))
	icos <<= 15 - lc
	isin <<= 15 - ls
	return (ls-lc)*(1<<11) +
		int(snFracMul16(int32(isin), snFracMul16(int32(isin), -2597)+7932)) -
		int(snFracMul16(int32(icos), snFracMul16(int32(icos), -2597)+7932))
}

// snComputeQn is compute_qn for the mono split (stereo=0, so no N==2 case).
func snComputeQn(N, b, offset, pulseCap int) int {
	exp2Table8 := [8]int16{16384, 17866, 19483, 21247, 23170, 25267, 27554, 30048}
	var qn, qb int
	N2 := 2*N - 1
	qb = int(fixedmath.Celt_sudiv(int32(b+N2*offset), int32(N2)))
	qb = fixedmath.IMIN(b-pulseCap-(4<<snBitRes), qb)
	qb = fixedmath.IMIN(8<<snBitRes, qb)
	if qb < (1 << snBitRes >> 1) {
		qn = 1
	} else {
		qn = int(exp2Table8[qb&0x7]) >> (14 - (qb >> snBitRes))
		qn = (qn + 1) >> 1 << 1
	}
	return qn
}

// snDeinterleave is the hadamard==0 branch of deinterleave_hadamard (a transpose;
// no ordery table). For a transient first band with tf_res==0 it is the exact
// reorder quant_band applies to X before the split.
func snDeinterleave(X []int32, N0, stride int) []int32 {
	N := N0 * stride
	tmp := make([]int32, N)
	for i := 0; i < stride; i++ {
		for j := 0; j < N0; j++ {
			tmp[i*N0+j] = X[j*stride+i]
		}
	}
	return tmp
}

// firstBandBudget replicates quant_all_bands's per-band budget b for i==start
// (the first band), which quant_partition passes unchanged to compute_theta and
// the snap compares delta against.
func firstBandBudget(start, end, LM, C, isTransient, length int, offsets []int,
	allocTrim, intensityIn, dualStereoIn, nbEBands int) (b0, codedBands int) {
	buf := make([]byte, length)
	var enc rangecoding.Encoder
	enc.Init(buf)
	totalBitsFrac := (int32(length) * 8) << bandsBitRes
	bits := totalBitsFrac - int32(enc.TellFrac()) - 1
	antiCollapseRsv := 0
	if isTransient != 0 && LM >= 2 && bits >= int32((LM+2)<<bandsBitRes) {
		antiCollapseRsv = 1 << bandsBitRes
	}
	bits -= int32(antiCollapseRsv)
	cap := make([]int, nbEBands)
	celt.InitCaps(cap, LM, C)
	intensity := intensityIn
	dualStereo := dualStereoIn
	pulses := make([]int, nbEBands)
	ebits := make([]int, nbEBands)
	finePrio := make([]int, nbEBands)
	cb, balance := celt.ComputeAllocation(start, end, offsets, cap, allocTrim,
		&intensity, &dualStereo, int(bits), pulses, ebits, finePrio, C, LM, &enc, nil, 1, 0, 0)
	codedBands = cb
	tell := int32(enc.TellFrac())
	totalBits := int32(length)*(8<<bandsBitRes) - int32(antiCollapseRsv)
	remainingBits := totalBits - tell - 1
	if start <= codedBands-1 {
		currBalance := fixedmath.Celt_sudiv(int32(balance), int32(fixedmath.IMIN(3, codedBands-start)))
		b0 = fixedmath.IMAX(0, fixedmath.IMIN(16383, fixedmath.IMIN(int(remainingBits)+1, pulses[start]+int(currBalance))))
	}
	return b0, codedBands
}

// craftFirstBand fills band `bandStart` (mono) so that after the transient
// deinterleave transpose the two split halves have a strong energy skew: the
// mid (first) half is the strong one when midStrong is true, the side (second)
// half otherwise. width is the band width in shortMdctSize units, N0==width and
// stride==M for a transient band with tf_res==0.
func craftFirstBand(xin []int32, bandStart, M, width int, midStrong bool, large, small int32) {
	N := M * width
	splitN := N / 2
	// Desired post-deinterleave layout T, then invert the transpose to fill band.
	T := make([]int32, N)
	for p := 0; p < N; p++ {
		strongHalf := p < splitN
		v := small
		if strongHalf == midStrong {
			v = large
		}
		T[p] = v
	}
	band := xin[M*bandStart:]
	stride := M
	N0 := width
	for i := 0; i < stride; i++ {
		for j := 0; j < N0; j++ {
			band[j*stride+i] = T[i*N0+j]
		}
	}
}

// TestQuantAllBandsEncodeAvoidSplitNoise exercises the avoid_split_noise theta
// snap (delta>*b -> itheta=qn, delta<-*b -> itheta=0). The snap only reaches a
// wide first band whose small budget can be exceeded by delta, so it uses hybrid
// starts on the widest bands (19, 20) that actually split, mono (stereo==0),
// transient (B>1 -> avoid_split_noise==1), with tf_res[start]=0 so the pre-split
// reorder is the plain deinterleave transpose, and tight packets (6-40 bytes) so
// the band budget b lands in the (cacheMax+12, |delta|) window. It sweeps skew
// and packet size, asserts Go==C bit-exact on every case, and independently
// evaluates the snap predicate (verbatim compute_qn / bitexact_cos /
// bitexact_log2tan math over the actual first-band budget and StereoItheta) to
// witness that both snap directions actually fire.
func TestQuantAllBandsEncodeAvoidSplitNoise(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0xA5D17))
	C := 1
	start := 17 // the real hybrid start; start>17 overflows specialHybridFolding
	end := nbEBands
	width := eb[start+1] - eb[start] // band 17 width (8)
	isTransient := 1
	spread := 2
	disableInv := 0
	allocTrim := 5
	cases := 0
	snapUp := 0   // delta > b  -> itheta = qn
	snapDown := 0 // delta < -b -> itheta = 0

	// The snap only reaches a wide first band whose small budget b can be
	// exceeded by delta. Band 0 (width 1) never qualifies, and CELT-only always
	// uses start=0, so this is the hybrid band-17 path. A large dynalloc boost on
	// band 17 with a tight packet pins b at remaining_bits so it lands in the
	// (cacheMax+12, |delta|) window; sweeping the boost and packet size walks b
	// across it. It fires for LM=3 (splitN=32, delta large enough); LM=2 stays
	// below the split threshold but is kept for extra bit-exact coverage.
	for _, LM := range []int{2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		bandN := M * width
		splitN := bandN / 2
		splitLM := LM - 1
		pulseCap := cBandsLogN(start) + splitLM*(1<<snBitRes)
		offset := (pulseCap >> 1) - snQThetaOffset
		cacheMax := cBandsCacheMax(LM, start)

		for _, midStrong := range []bool{true, false} {
			for _, ratio := range []int{4, 6, 8, 11, 16} {
				for _, boost := range []int{140, 180, 220, 260, 300, 340} {
					for _, length := range []int{12, 16, 20, 24, 30, 40, 50} {
						offsets := make([]int, nbEBands)
						offsets[start] = boost
						tfRes := genTfRes(r, nbEBands, LM, isTransient)
						tfRes[start] = 0 // transpose-only reorder for the first band
						intensity := 0
						dualStereo := 0
						seedIn := r.Uint32()
						bandE := genBandE(r, nbEBands)
						xin := genNormX(r, C, N, M, eb, nbEBands)
						large := int32(1 << 22)
						small := large / int32(ratio)
						craftFirstBand(xin, start, M, width, midStrong, large, small)

						b0, _ := firstBandBudget(start, end, LM, C, isTransient, length, offsets,
							allocTrim, intensity, dualStereo, nbEBands)

						cBuf := make([]byte, length)
						cRes := cBandsEncPipeline(start, end, LM, C, isTransient, spread, intensity,
							dualStereo, disableInv, allocTrim, 5, length, offsets, tfRes,
							bandE, xin, seedIn, cBuf, nbEBands, N)
						gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsEncode(
							start, end, LM, C, isTransient, spread, intensity, dualStereo,
							disableInv, allocTrim, 5, length, offsets, tfRes, bandE, xin, seedIn, nbEBands, N)
						bound := M * eb[end]
						label := fmt.Sprintf("avoidsplit:LM=%d mid=%v ratio=%d boost=%d len=%d", LM, midStrong, ratio, boost, length)
						assertEncMatchBuf(t, label, C, N, bound, 5,
							gBuf, gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual, cBuf, cRes)

						// Witness: does band `start`'s top-level split hit the snap?
						if bandN > 2 && b0 > cacheMax+12 {
							bandX := append([]int32(nil), xin[M*start:M*start+bandN]...)
							tmp := snDeinterleave(bandX, width, M)
							ithetaRaw := int(celt.StereoItheta(tmp[:splitN], tmp[splitN:2*splitN], 0, splitN) >> 16)
							qn := snComputeQn(splitN, b0, offset, pulseCap)
							if qn != 1 {
								ithetaQ := (ithetaRaw*qn + 8192) >> 14
								if ithetaQ > 0 && ithetaQ < qn {
									unquantized := int(fixedmath.Celt_udiv(uint32(ithetaQ*16384), uint32(qn)))
									imid := int(snBitexactCos(int16(unquantized)))
									iside := int(snBitexactCos(int16(16384 - unquantized)))
									delta := int(snFracMul16(int32((splitN-1)<<7), int32(snBitexactLog2tan(iside, imid))))
									if delta > b0 {
										snapUp++
									}
									if delta < -b0 {
										snapDown++
									}
								}
							}
						}
						cases++
					}
				}
			}
		}
	}
	t.Logf("avoid_split_noise ENCODE cases: %d (snap->qn: %d, snap->0: %d)", cases, snapUp, snapDown)
	if snapUp == 0 || snapDown == 0 {
		t.Fatalf("avoid_split_noise snap not witnessed in both directions (up=%d down=%d)", snapUp, snapDown)
	}
}
