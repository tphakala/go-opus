//go:build refc

package oracle

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This differential test pins the pure-Go CELT residual band decode
// (internal/celt bands.go: QuantAllBands, DenormaliseBands, AntiCollapse) to the
// pinned libopus C oracle (celt/bands.c). quant_all_bands interleaves the range
// coder, so the C side runs the mini decoder pipeline (init_caps ->
// clt_compute_allocation -> quant_all_bands) with encode=1 to PRODUCE a
// bitstream from a random normalized X[], then both C and Go decode the SAME
// bitstream and every output is asserted bit-identical: X[] (int32
// element-wise), the collapse_masks, the threaded LCG seed, and the
// range-decoder end state (tell + rng), plus codedBands/intensity/dual_stereo.

// bandsBitRes mirrors BITRES (celt/entcode.h): the eighth-bit fixed-point unit.
const bandsBitRes = 3

// tfSelectTable mirrors tf_select_table (celt/celt.c): the per-(LM, isTransient)
// tf_change values, used to generate valid tf_res[] the way tf_decode would.
var tfSelectTable = [4][8]int{
	{0, -1, 0, -1, 0, -1, 0, -1},
	{0, -1, 0, -2, 1, 0, 1, -1},
	{0, -2, 0, -3, 2, 0, 1, -1},
	{0, -2, 0, -3, 3, 0, 1, -1},
}

// loadEBands returns eBands[0..nbEBands] (the nbEBands+1 band boundaries) from
// the C oracle mode.
func loadEBands(nbEBands int) []int {
	eb := make([]int, nbEBands+1)
	for i := range eb {
		eb[i] = cBandsEBand(i)
	}
	return eb
}

// genTfRes produces a valid tf_res[] for (LM, isTransient) by mimicking
// tf_decode: one tf_select for the frame, a random tf_changed per band.
func genTfRes(r *rand.Rand, nbEBands, LM, isTransient int) []int {
	tfSelect := r.Intn(2)
	tf := make([]int, nbEBands)
	for i := range tf {
		tfChanged := r.Intn(2)
		tf[i] = tfSelectTable[LM][4*isTransient+2*tfSelect+tfChanged]
	}
	return tf
}

// genNormX fills a channels*N celt_norm buffer with random per-band vectors,
// guaranteeing each band is non-zero so the C alg_quant never sees a degenerate
// input. The exact magnitudes only steer which codewords the encoder picks; both
// decoders read back the same bitstream regardless.
func genNormX(r *rand.Rand, channels, N, M int, eb []int, nbEBands int) []int32 {
	x := make([]int32, channels*N)
	for c := 0; c < channels; c++ {
		for i := 0; i < nbEBands; i++ {
			lo := c*N + M*eb[i]
			hi := c*N + M*eb[i+1]
			nonzero := false
			for j := lo; j < hi; j++ {
				x[j] = int32(r.Intn(1<<24) - (1 << 23))
				if x[j] != 0 {
					nonzero = true
				}
			}
			if !nonzero && hi > lo {
				x[lo] = 1 << 22
			}
		}
	}
	return x
}

// genBandE builds 2*nbEBands positive band energies above MIN_STEREO_ENERGY (2)
// for the stereo encode path.
func genBandE(r *rand.Rand, nbEBands int) []int32 {
	be := make([]int32, 2*nbEBands)
	for i := range be {
		be[i] = int32(100 + r.Intn(1<<22))
	}
	return be
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// goBandsDecode replicates the decoder mini-pipeline in pure Go over buf: it
// derives bits/anti_collapse_rsv/total_bits exactly as celt_decoder.c does, runs
// celt.ComputeAllocation to consume the skip/intensity/dual flags and produce
// pulses/codedBands/balance, then celt.QuantAllBands. Returns the decoded state.
func goBandsDecode(buf []byte, start, end, LM, C, isTransient, spread, intensityIn,
	dualStereoIn, disableInv, allocTrim, length int, offsets, tfRes []int, seedIn uint32,
	nbEBands, N int) (x []int32, collapse []byte, seed uint32, tell int, rng uint32,
	codedBands, intensity, dualStereo int) {
	M := 1 << LM
	var dec rangecoding.Decoder
	dec.Init(buf)

	totalBitsFrac := (int32(length) * 8) << bandsBitRes
	bits := totalBitsFrac - int32(dec.TellFrac()) - 1
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
		&intensity, &dualStereo, int(bits), pulses, ebits, finePrio, C, LM, nil, &dec, 0, 0, 0)

	x = make([]int32, C*N)
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
	celt.QuantAllBands(start, end, x[:N], Y, collapse, nil, pulses, shortBlocks, spread,
		dualStereo, intensity, tfRes, totalBits, int32(balance), &dec, LM, codedBands, &seed, disableInv)

	return x, collapse, seed, dec.Tell(), dec.Rng(), codedBands, intensity, dualStereo
}

// bandsCompareXregion checks the decoded X per channel over the processed region
// [0, M*eb[end]) (bands beyond end are untouched and identically zero on both
// sides).
func bandsCompareXregion(t *testing.T, gotX, wantX []int32, C, N, bound int, label string) {
	t.Helper()
	for c := 0; c < C; c++ {
		g := gotX[c*N : c*N+bound]
		w := wantX[c*N : c*N+bound]
		if !equalInt32(g, w) {
			for i := range g {
				if g[i] != w[i] {
					t.Fatalf("%s: X channel %d diverges at sample %d: Go=%d C=%d", label, c, i, g[i], w[i])
				}
			}
		}
	}
}

// TestQuantAllBandsMatchesC is the primary check: for a grid over LM, channels,
// transient/short-block, spread, intensity/dual stereo, disable_inv, band count
// and bitrate, the C encoder produces a bitstream, then C and Go both decode it
// and the decoded spectra, collapse masks, LCG seed and range-decoder end state
// must all match bit-for-bit.
func TestQuantAllBandsMatchesC(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x8A4D5))
	cases := 0

	for _, LM := range []int{0, 1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			for _, isTransient := range []int{0, 1} {
				if isTransient != 0 && LM == 0 {
					continue // the transient flag is only coded for LM>0
				}
				disableInvs := []int{0}
				if C == 2 {
					disableInvs = []int{0, 1}
				}
				for _, spread := range []int{0, 1, 2, 3} {
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

								buf := make([]byte, length)
								_ = cBandsPipeline(1, start, end, LM, C, isTransient, spread, intensity,
									dualStereo, disableInv, allocTrim, length, offsets, tfRes, bandE, xin, seedIn, buf, nbEBands, N)

								cDec := cBandsPipeline(0, start, end, LM, C, isTransient, spread, intensity,
									dualStereo, disableInv, allocTrim, length, offsets, tfRes, nil, nil, seedIn, buf, nbEBands, N)

								gX, gCollapse, gSeed, gTell, gRng, gCoded, gIntensity, gDual := goBandsDecode(
									buf, start, end, LM, C, isTransient, spread, intensity, dualStereo,
									disableInv, allocTrim, length, offsets, tfRes, seedIn, nbEBands, N)

								bound := M * eb[end]
								label := labelOf(LM, C, isTransient, spread, disableInv, end, length)
								bandsCompareXregion(t, gX, cDec.x, C, N, bound, label)
								if !equalBytes(gCollapse, cDec.collapseMasks) {
									t.Fatalf("%s: collapse masks Go=%v C=%v", label, gCollapse, cDec.collapseMasks)
								}
								if gSeed != cDec.seed {
									t.Fatalf("%s: seed Go=%d C=%d", label, gSeed, cDec.seed)
								}
								if gTell != cDec.tell {
									t.Fatalf("%s: tell Go=%d C=%d", label, gTell, cDec.tell)
								}
								if gRng != cDec.rng {
									t.Fatalf("%s: rng Go=%d C=%d", label, gRng, cDec.rng)
								}
								if gCoded != cDec.codedBands {
									t.Fatalf("%s: codedBands Go=%d C=%d", label, gCoded, cDec.codedBands)
								}
								if gIntensity != cDec.intensity || gDual != cDec.dualStereo {
									t.Fatalf("%s: intensity/dual Go=(%d,%d) C=(%d,%d)", label,
										gIntensity, gDual, cDec.intensity, cDec.dualStereo)
								}
								cases++
							}
						}
					}
				}
			}
		}
	}
	t.Logf("quant_all_bands differential cases: %d", cases)
}

func labelOf(LM, C, isTransient, spread, disableInv, end, length int) string {
	return fmt.Sprintf("LM=%d C=%d tr=%d spread=%d dinv=%d end=%d len=%d",
		LM, C, isTransient, spread, disableInv, end, length)
}

// TestQuantAllBandsWithBoosts exercises dynalloc boosts (offsets>0), which pin
// skip_start and change the per-band allocation, over a smaller grid.
func TestQuantAllBandsWithBoosts(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0xB0057))
	cases := 0
	for _, LM := range []int{2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			for trial := 0; trial < 6; trial++ {
				start, end := 0, nbEBands
				isTransient := r.Intn(2)
				spread := r.Intn(4)
				disableInv := r.Intn(2)
				length := clampInt(C*N/6, 12, 1275)
				offsets := make([]int, nbEBands)
				// Boost a couple of random bands by one quanta each.
				for k := 0; k < 2; k++ {
					bi := start + r.Intn(end-start)
					width := C * (eb[bi+1] - eb[bi]) << LM
					quanta := clampInt(width<<bandsBitRes, 6<<bandsBitRes, 1<<20)
					if width > quanta {
						quanta = width
					}
					if width < 6<<bandsBitRes {
						quanta = clampInt(width<<bandsBitRes, width, 6<<bandsBitRes)
					}
					offsets[bi] += quanta
				}
				tfRes := genTfRes(r, nbEBands, LM, isTransient)
				intensity := 0
				dualStereo := 0
				if C == 2 {
					intensity = start + r.Intn(end-start+1)
					dualStereo = r.Intn(2)
				}
				allocTrim := r.Intn(11)
				seedIn := r.Uint32()
				bandE := genBandE(r, nbEBands)
				xin := genNormX(r, C, N, M, eb, nbEBands)

				buf := make([]byte, length)
				_ = cBandsPipeline(1, start, end, LM, C, isTransient, spread, intensity, dualStereo,
					disableInv, allocTrim, length, offsets, tfRes, bandE, xin, seedIn, buf, nbEBands, N)
				cDec := cBandsPipeline(0, start, end, LM, C, isTransient, spread, intensity, dualStereo,
					disableInv, allocTrim, length, offsets, tfRes, nil, nil, seedIn, buf, nbEBands, N)
				gX, gCollapse, gSeed, gTell, gRng, gCoded, _, _ := goBandsDecode(buf, start, end, LM, C,
					isTransient, spread, intensity, dualStereo, disableInv, allocTrim, length, offsets, tfRes, seedIn, nbEBands, N)

				bound := M * eb[end]
				label := labelOf(LM, C, isTransient, spread, disableInv, end, length)
				bandsCompareXregion(t, gX, cDec.x, C, N, bound, "boost:"+label)
				if !equalBytes(gCollapse, cDec.collapseMasks) {
					t.Fatalf("boost %s: collapse masks differ", label)
				}
				if gSeed != cDec.seed || gTell != cDec.tell || gRng != cDec.rng || gCoded != cDec.codedBands {
					t.Fatalf("boost %s: seed/tell/rng/codedBands Go=(%d,%d,%d,%d) C=(%d,%d,%d,%d)",
						label, gSeed, gTell, gRng, gCoded, cDec.seed, cDec.tell, cDec.rng, cDec.codedBands)
				}
				cases++
			}
		}
	}
	t.Logf("quant_all_bands boosted differential cases: %d", cases)
}

// TestQuantAllBandsMutation is the non-vacuity check: decoding the SAME bitstream
// with a perturbed decode parameter (spread, disable_inv or seed) must diverge
// from the correct C decode, proving the bit-exact assertions actually catch a
// behavioral change. It also self-checks the comparator on a one-element bump.
func TestQuantAllBandsMutation(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	eb := loadEBands(nbEBands)
	r := rand.New(rand.NewSource(0x11FE))
	mutationCaught := false
	comparatorChecked := false

	for _, LM := range []int{2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, C := range []int{1, 2} {
			start, end := 0, nbEBands
			isTransient := 1
			spread := 2
			disableInv := 0
			// A low bitrate so several bands collapse and get LCG folding/noise,
			// making the seed mutation observable.
			length := clampInt(C*N/30, 12, 1275)
			offsets := make([]int, nbEBands)
			tfRes := genTfRes(r, nbEBands, LM, isTransient)
			intensity := 0
			dualStereo := 0
			if C == 2 {
				intensity = start + r.Intn(end-start+1)
			}
			allocTrim := 5
			seedIn := r.Uint32()
			bandE := genBandE(r, nbEBands)
			xin := genNormX(r, C, N, M, eb, nbEBands)

			buf := make([]byte, length)
			_ = cBandsPipeline(1, start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, length, offsets, tfRes, bandE, xin, seedIn, buf, nbEBands, N)
			cDec := cBandsPipeline(0, start, end, LM, C, isTransient, spread, intensity, dualStereo,
				disableInv, allocTrim, length, offsets, tfRes, nil, nil, seedIn, buf, nbEBands, N)

			bound := M * eb[end]

			// Baseline: Go must match C.
			gX, _, _, _, _, _, _, _ := goBandsDecode(buf, start, end, LM, C, isTransient, spread,
				intensity, dualStereo, disableInv, allocTrim, length, offsets, tfRes, seedIn, nbEBands, N)
			bandsCompareXregion(t, gX, cDec.x, C, N, bound, "mutation-baseline")

			// Comparator self-check: a one-element bump of the C result must be flagged.
			mut := append([]int32(nil), cDec.x...)
			for i := 0; i < bound; i++ {
				mut[i]++
				break
			}
			if equalInt32(mut[:bound], cDec.x[:bound]) {
				t.Fatal("comparator failed to detect a one-element perturbation")
			}
			comparatorChecked = true

			// Real mutation: decode with a different seed. Any LCG-folded band diverges.
			mX, _, _, _, _, _, _, _ := goBandsDecode(buf, start, end, LM, C, isTransient, spread,
				intensity, dualStereo, disableInv, allocTrim, length, offsets, tfRes, seedIn+1, nbEBands, N)
			for c := 0; c < C && !mutationCaught; c++ {
				if !equalInt32(mX[c*N:c*N+bound], cDec.x[c*N:c*N+bound]) {
					mutationCaught = true
				}
			}
			// Also mutate the spread; the exp_rotation coefficients change.
			if spread < 3 {
				sX, _, _, _, _, _, _, _ := goBandsDecode(buf, start, end, LM, C, isTransient, spread+1,
					intensity, dualStereo, disableInv, allocTrim, length, offsets, tfRes, seedIn, nbEBands, N)
				for c := 0; c < C && !mutationCaught; c++ {
					if !equalInt32(sX[c*N:c*N+bound], cDec.x[c*N:c*N+bound]) {
						mutationCaught = true
					}
				}
			}
		}
	}
	if !comparatorChecked {
		t.Fatal("comparator self-check never ran")
	}
	if !mutationCaught {
		t.Fatal("mutation check never observed a divergence; the harness may be vacuous")
	}
}

// TestDenormaliseBandsMatchesC checks DenormaliseBands against C over random
// normalized spectra and per-band log energies (spanning the extremes that
// exercise the shift>=31 (g=0) and shift<0 (g=max) branches), plus downsample
// and silence variations, and hybrid (start!=0) starts.
func TestDenormaliseBandsMatchesC(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	r := rand.New(rand.NewSource(0xDE0))
	cases := 0
	for _, LM := range []int{0, 1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		for _, downsample := range []int{1, 2} {
			for _, silence := range []int{0, 1} {
				starts := []int{0, 17}
				for _, start := range starts {
					ends := []int{nbEBands}
					if start == 0 {
						ends = []int{nbEBands, nbEBands - 4}
					} else {
						ends = []int{nbEBands, start + 1}
					}
					for _, end := range ends {
						for trial := 0; trial < 3; trial++ {
							X := make([]int32, N)
							for i := range X {
								X[i] = int32(r.Intn(1<<25) - (1 << 24))
							}
							bandLogE := make([]int32, nbEBands)
							for i := range bandLogE {
								// Q(DB_SHIFT) log energy across [-20, 28), hitting both gain clamps.
								bandLogE[i] = int32(r.Intn(48<<24) - (20 << 24))
							}
							want := cDenormaliseBands(X, bandLogE, start, end, LM, downsample, silence, N)
							got := make([]int32, N)
							celt.DenormaliseBands(X, got, bandLogE, start, end, M, downsample, silence)
							if !equalInt32(got, want) {
								for i := range got {
									if got[i] != want[i] {
										t.Fatalf("LM=%d start=%d end=%d ds=%d sil=%d: freq diverges at %d Go=%d C=%d",
											LM, start, end, downsample, silence, i, got[i], want[i])
									}
								}
							}
							cases++
						}
					}
				}
			}
		}
	}
	t.Logf("denormalise_bands differential cases: %d", cases)
}

// TestAntiCollapseMatchesC checks AntiCollapse against C over random spectra,
// collapse masks (with zero bits so the noise fill triggers), pulses and
// prev/cur log energies, for both encode flags. It requires at least one case
// where the fill actually changed the spectrum (non-vacuity).
func TestAntiCollapseMatchesC(t *testing.T) {
	nbEBands, _, shortMdctSize := cBandsModeInfo()
	r := rand.New(rand.NewSource(0xAC1))
	cases := 0
	fillObserved := false
	for _, LM := range []int{0, 1, 2, 3} {
		M := 1 << LM
		N := M * shortMdctSize
		size := N
		for _, C := range []int{1, 2} {
			for _, end := range []int{nbEBands, nbEBands - 5} {
				start := 0
				for trial := 0; trial < 5; trial++ {
					X := make([]int32, C*size)
					for i := range X {
						X[i] = int32(r.Intn(1<<24) - (1 << 23))
					}
					masks := make([]byte, C*nbEBands)
					for i := range masks {
						// Low (1<<LM) bits matter; keep some zero so bands collapse.
						masks[i] = byte(r.Intn(1 << M))
					}
					logE := make([]int32, 2*nbEBands)
					prev1 := make([]int32, 2*nbEBands)
					prev2 := make([]int32, 2*nbEBands)
					for i := 0; i < 2*nbEBands; i++ {
						logE[i] = int32(r.Intn(40<<24) - (12 << 24))
						prev1[i] = int32(r.Intn(40<<24) - (12 << 24))
						prev2[i] = int32(r.Intn(40<<24) - (12 << 24))
					}
					pulses := make([]int, nbEBands)
					for i := range pulses {
						pulses[i] = r.Intn(400)
					}
					seed := r.Uint32()
					for _, encode := range []int{0, 1} {
						want := cAntiCollapse(X, masks, LM, C, size, start, end, logE, prev1, prev2, pulses, seed, encode, nbEBands)
						got := append([]int32(nil), X...)
						celt.AntiCollapse(got, masks, LM, C, size, start, end, logE, prev1, prev2, pulses, seed, encode)
						if !equalInt32(got, want) {
							for i := range got {
								if got[i] != want[i] {
									t.Fatalf("LM=%d C=%d end=%d enc=%d: X diverges at %d Go=%d C=%d",
										LM, C, end, encode, i, got[i], want[i])
								}
							}
						}
						if !equalInt32(want, X) {
							fillObserved = true
						}
						cases++
					}
				}
			}
		}
	}
	if !fillObserved {
		t.Fatal("anti_collapse never changed the spectrum; the harness may be vacuous")
	}
	t.Logf("anti_collapse differential cases: %d", cases)
}
