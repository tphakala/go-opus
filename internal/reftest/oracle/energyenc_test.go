//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// CP4 differential tests: the pure-Go CELT ENCODE-side band-energy path
// (internal/celt bands.go computeBandEnergies/normaliseBands, quant_bands.go
// amp2Log2/quantCoarseEnergy/quantFineEnergy/quantEnergyFinalise, laplace.go
// ecLaplaceEncode) against the pinned libopus C oracle. The coarse-energy tests
// run multi-frame sequences and compare a per-frame state snapshot (oldEBands,
// error, delayedIntra) plus the emitted bytes and the intra/inter RDO decision,
// because the inter path predicts from the previous frame (docs/hard-parts.md 5).

// ---------------------------------------------------------------------------
// ec_laplace_encode
// ---------------------------------------------------------------------------

// randLaplaceValue draws a Laplace symbol biased toward small magnitudes but
// reaching deep into the flat PDF tail (the ndi clamp + *value write-back path).
func randLaplaceValue(r *rand.Rand) int32 {
	switch r.Intn(10) {
	case 0:
		return 0
	case 1, 2:
		return int32(r.Intn(5) - 2) // -2..2
	case 3, 4, 5:
		return int32(r.Intn(41) - 20) // -20..20
	case 6, 7:
		return int32(r.Intn(201) - 100) // -100..100
	default:
		return int32(r.Intn(4001) - 2000) // deep tail
	}
}

// TestEcLaplaceEncodeDifferential drives ecLaplaceEncode against ec_laplace_encode
// over random symbol sequences with fs/decay spanning the e_prob_model ranges
// (fs = p<<7 for p in 1..255, decay = d<<6 for d in 1..179, i.e. up to 11456),
// plus explicit extremes. It asserts the finalized bytes, the write-back values,
// and the pre-flush range register (rng/val) all match bit-for-bit.
func TestEcLaplaceEncodeDifferential(t *testing.T) {
	const (
		nbBytes = 64
		trials  = 3000
	)
	// Explicit edge cases prepended: symbol/fs/decay at the boundaries.
	type edge struct {
		val  int32
		p, d int
	}
	edges := []edge{
		{0, 1, 1}, {0, 255, 179}, {1, 1, 1}, {-1, 255, 179},
		{15, 21, 6}, {16, 21, 179}, {-16, 200, 6}, {100, 1, 1},
		{-100, 255, 1}, {2000, 1, 179}, {-2000, 255, 179}, {7, 128, 128},
	}

	run := func(t *testing.T, values []int32, fss []uint32, decays []int32) {
		t.Helper()
		res := cEELaplaceSeq(values, fss, decays, nbBytes)

		buf := make([]byte, nbBytes)
		var enc rangecoding.Encoder
		enc.Init(buf)
		goVals := make([]int32, len(values))
		for i := range values {
			v := int(values[i])
			celt.EcLaplaceEncode(&enc, &v, fss[i], int(decays[i]))
			goVals[i] = int32(v)
		}
		rng, valReg, rb := enc.Rng(), enc.Val(), enc.RangeBytes()
		enc.EncDone()

		if rng != res.rng || valReg != res.valReg || rb != res.rangeBytes {
			t.Fatalf("range state mismatch: go(rng=%d val=%d rb=%d) c(rng=%d val=%d rb=%d)\ninputs v=%v fs=%v decay=%v",
				rng, valReg, rb, res.rng, res.valReg, res.rangeBytes, values, fss, decays)
		}
		for i := range goVals {
			if goVals[i] != res.values[i] {
				t.Fatalf("write-back value[%d] mismatch: go=%d c=%d (in=%d fs=%d decay=%d)",
					i, goVals[i], res.values[i], values[i], fss[i], decays[i])
			}
		}
		if !bytes.Equal(buf, res.packet) {
			t.Fatalf("packet bytes mismatch\n go=%x\n  c=%x\ninputs v=%v fs=%v decay=%v",
				buf, res.packet, values, fss, decays)
		}
	}

	// Edge cases, each as a single-symbol sequence.
	for i, e := range edges {
		e := e
		t.Run(fmt.Sprintf("edge%d", i), func(t *testing.T) {
			run(t, []int32{e.val}, []uint32{uint32(e.p) << 7}, []int32{int32(e.d) << 6})
		})
	}

	// Randomized multi-symbol sequences.
	for trial := 0; trial < trials; trial++ {
		trial := trial
		r := rand.New(rand.NewSource(int64(trial)*2654435761 + 1))
		n := 1 + r.Intn(16)
		values := make([]int32, n)
		fss := make([]uint32, n)
		decays := make([]int32, n)
		for i := 0; i < n; i++ {
			values[i] = randLaplaceValue(r)
			fss[i] = uint32(1+r.Intn(255)) << 7   // 1..255
			decays[i] = int32(1+r.Intn(179)) << 6 // 1..179 -> up to 11456
		}
		t.Run(fmt.Sprintf("rand%d", trial), func(t *testing.T) {
			run(t, values, fss, decays)
		})
	}
}

// ---------------------------------------------------------------------------
// compute_band_energies / normalise_bands / amp2Log2
// ---------------------------------------------------------------------------

// randSig fills X with random celt_sig values shaped like an MDCT spectrum.
// mode 0: moderate random; 1: all zero (EPSILON path); 2: sparse (some whole
// bands zero -> EPSILON, others large); 3: large magnitude toward the
// C-well-defined ceiling (the shift=0 region and celt_sqrt32 near its top).
// Mode 3 caps |X| at 2^26, the same bound CP3 established as safe against the C
// signed-overflow in celt_ilog2's maxval+(maxval>>14)+1 argument.
func randSig(r *rand.Rand, n, mode int) []int32 {
	X := make([]int32, n)
	switch mode {
	case 1:
		// all zero
	case 2:
		for i := range X {
			if r.Intn(4) == 0 {
				X[i] = int32(r.Intn(1<<22) - (1 << 21))
			}
		}
	case 3:
		for i := range X {
			X[i] = int32(r.Intn(1<<27) - (1 << 26)) // [-2^26, 2^26)
		}
	default:
		for i := range X {
			X[i] = int32(r.Intn(1<<24) - (1 << 23))
		}
	}
	return X
}

func TestComputeBandEnergiesDifferential(t *testing.T) {
	nb := cEENbEBands()
	sms := cEEShortMdctSize()
	for lm := 0; lm <= 3; lm++ {
		for _, channels := range []int{1, 2} {
			for mode := 0; mode <= 3; mode++ {
				lm, channels, mode := lm, channels, mode
				t.Run(fmt.Sprintf("LM%d/C%d/mode%d", lm, channels, mode), func(t *testing.T) {
					r := rand.New(rand.NewSource(int64(lm*911 + channels*31 + mode*7 + 1)))
					N := sms << lm
					X := randSig(r, channels*N, mode)

					cE := cEEComputeBandEnergies(lm, channels, nb, X)

					goE := make([]int32, channels*nb)
					xcopy := append([]int32(nil), X...)
					celt.ComputeBandEnergies(xcopy, goE, nb, channels, lm)

					for i := range goE {
						if goE[i] != cE[i] {
							t.Fatalf("bandE[%d] (band %d ch %d) go=%d c=%d",
								i, i%nb, i/nb, goE[i], cE[i])
						}
					}

					// normalise_bands over the same spectrum + energies.
					cX := cEENormaliseBands(lm, channels, nb, X, cE)
					goX := make([]int32, channels*N)
					freqCopy := append([]int32(nil), X...)
					celt.NormaliseBands(freqCopy, goX, goE, nb, channels, 1<<lm)
					for i := range goX {
						if goX[i] != cX[i] {
							t.Fatalf("normX[%d] go=%d c=%d", i, goX[i], cX[i])
						}
					}

					// amp2Log2 over the energies, with effEnd both == and < end.
					for _, effEnd := range []int{nb, nb - 3} {
						cL := cEEAmp2Log2(effEnd, nb, channels, cE)
						goL := make([]int32, channels*nb)
						celt.Amp2Log2(effEnd, nb, cE, goL, channels)
						for i := range goL {
							if goL[i] != cL[i] {
								t.Fatalf("bandLogE[%d] (effEnd=%d) go=%d c=%d", i, effEnd, goL[i], cL[i])
							}
						}
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// quant_coarse_energy (two-pass RDO) and the full coarse+fine+finalise path
// ---------------------------------------------------------------------------

// goEncCoarse mirrors the C oracle_ee_coarse: one frame of quantCoarseEnergy with
// the given carried prediction base and RDO bias, plus the decoded intra flag.
// start/end/effEnd select the coded band window; preBits emits that many bits
// before coarse energy so ec_tell>0 at entry.
func goEncCoarse(lm, channels, nbBytes, forceIntra, twoPass, lossRate, lfe, start, end, effEnd, preBits int,
	eIn, oldIn []int32, delayedIn int32) energyEncResult {
	nb := cEENbEBands()
	n := channels * nb
	buf := make([]byte, nbBytes)
	old := append([]int32(nil), oldIn...)
	errArr := make([]int32, n)
	delayed := delayedIn

	var enc rangecoding.Encoder
	enc.Init(buf)
	for k := 0; k < preBits; k++ {
		enc.EncBitLogp(0, 1)
	}
	celt.QuantCoarseEnergy(start, end, effEnd, eIn, old, uint32(nbBytes*8), errArr, &enc,
		channels, lm, nbBytes, forceIntra, &delayed, twoPass, lossRate, lfe)
	enc.EncDone()

	return energyEncResult{
		packet:     buf,
		oldOut:     old,
		errOut:     errArr,
		delayedOut: delayed,
		intraDec:   decIntra(buf, nbBytes, preBits),
	}
}

// goEncFull mirrors the C oracle_ee_full: coarse+fine+finalise for one frame.
func goEncFull(lm, channels, nbBytes, forceIntra, twoPass, lossRate, lfe int,
	eIn, oldIn []int32, delayedIn int32, fineQuant, finePriority []int) energyEncResult {
	nb := cEENbEBands()
	n := channels * nb
	buf := make([]byte, nbBytes)
	old := append([]int32(nil), oldIn...)
	errArr := make([]int32, n)
	delayed := delayedIn
	budget := int32(nbBytes * 8)

	var enc rangecoding.Encoder
	enc.Init(buf)
	celt.QuantCoarseEnergy(0, nb, nb, eIn, old, uint32(budget), errArr, &enc,
		channels, lm, nbBytes, forceIntra, &delayed, twoPass, lossRate, lfe)
	celt.QuantFineEnergy(0, nb, old, errArr, fineQuant, &enc, channels)
	bitsLeft := int(budget) - enc.Tell()
	celt.QuantEnergyFinalise(0, nb, old, errArr, fineQuant, finePriority, bitsLeft, &enc, channels)
	enc.EncDone()

	return energyEncResult{
		packet:     buf,
		oldOut:     old,
		errOut:     errArr,
		delayedOut: delayed,
		intraDec:   decIntra(buf, nbBytes, 0),
	}
}

// decIntra reads back the coarse-energy intra flag the way the decoder does,
// after consuming preBits leading range-coded bits (the ones goEncCoarse emitted
// before coarse energy).
func decIntra(buf []byte, nbBytes, preBits int) int {
	var dec rangecoding.Decoder
	dec.Init(buf)
	for k := 0; k < preBits; k++ {
		dec.DecBitLogp(1)
	}
	budget := int32(nbBytes * 8)
	if int32(dec.Tell())+3 <= budget {
		return dec.DecBitLogp(3)
	}
	return 0
}

// compareFrame asserts the Go and C per-frame outputs are bit-identical. error[]
// is only compared within the coded window [start,end) per channel: outside it C
// copies the uninitialized error_intra VARDECL when intra wins (never read
// downstream, since fine/finalise also work on [start,end)), so those entries are
// genuinely undefined and not part of the contract. oldEBands is compared in full
// because both sides preserve the input for bands outside the window.
func compareFrame(t *testing.T, tag string, g, c energyEncResult, start, end, nb, channels int) {
	t.Helper()
	if g.intraDec != c.intraDec {
		t.Fatalf("%s: intra flag go=%d c=%d", tag, g.intraDec, c.intraDec)
	}
	if g.delayedOut != c.delayedOut {
		t.Fatalf("%s: delayedIntra go=%d c=%d", tag, g.delayedOut, c.delayedOut)
	}
	for i := range g.oldOut {
		if g.oldOut[i] != c.oldOut[i] {
			t.Fatalf("%s: oldEBands[%d] go=%d c=%d", tag, i, g.oldOut[i], c.oldOut[i])
		}
	}
	for ch := 0; ch < channels; ch++ {
		for i := start; i < end; i++ {
			idx := i + ch*nb
			if g.errOut[idx] != c.errOut[idx] {
				t.Fatalf("%s: error[%d] (band %d ch %d) go=%d c=%d", tag, idx, i, ch, g.errOut[idx], c.errOut[idx])
			}
		}
	}
	if !bytes.Equal(g.packet, c.packet) {
		t.Fatalf("%s: packet bytes differ\n go=%x\n  c=%x", tag, g.packet, c.packet)
	}
}

// freshReset is -GCONST(28.f): the log energy a band resets to.
const freshResetEnc = int32(-(28 << 24))

// coarseCfg is one multi-frame configuration. start/end/effEnd select the coded
// band window (coarse path only; the full path always uses 0..nbEBands), and
// preBits emits leading bits so ec_tell>0 at entry.
type coarseCfg struct {
	lm, channels, nbBytes int
	twoPass, forceIntra   int
	lossRate, lfe         int
	start, end, effEnd    int
	preBits               int
}

// runCoarseSequence runs a jumpy-then-stable multi-frame sequence on both sides,
// carrying oldEBands + delayedIntra independently, and returns the set of intra
// decisions observed on force_intra=0 two_pass frames (to prove both RDO
// branches, and thus both splice paths, are exercised).
func runCoarseSequence(t *testing.T, cfg coarseCfg, seed int64, full bool) map[int]bool {
	t.Helper()
	nb := cEENbEBands()
	n := cfg.channels * nb
	r := rand.New(rand.NewSource(seed))

	// Independent carried state for each side, both starting from a reset.
	goOld := make([]int32, n)
	cOld := make([]int32, n)
	for i := range goOld {
		goOld[i] = freshResetEnc
		cOld[i] = freshResetEnc
	}
	var goDelayed, cDelayed int32

	// A slowly drifting base energy per band so inter-prediction usually wins,
	// with periodic big jumps so intra sometimes wins.
	base := make([]float64, n)
	for i := range base {
		base[i] = -14 + r.Float64()*34
	}

	seen := map[int]bool{}
	const frames = 12
	for f := 0; f < frames; f++ {
		if f%4 == 3 {
			for i := range base {
				base[i] = -14 + r.Float64()*34 // jump
			}
		} else {
			for i := range base {
				base[i] += r.Float64()*2 - 1 // drift
			}
		}
		eBands := make([]int32, n)
		for i := range eBands {
			eBands[i] = int32(base[i] * (1 << 24))
		}

		var fq32, fp32 []int32
		var fineQuant, finePriority []int
		if full {
			fineQuant = make([]int, nb)
			finePriority = make([]int, nb)
			fq32 = make([]int32, nb)
			fp32 = make([]int32, nb)
			for i := 0; i < nb; i++ {
				fineQuant[i] = r.Intn(9)
				finePriority[i] = r.Intn(2)
				fq32[i] = int32(fineQuant[i])
				fp32[i] = int32(finePriority[i])
			}
		}

		var gr, cr energyEncResult
		if full {
			gr = goEncFull(cfg.lm, cfg.channels, cfg.nbBytes, cfg.forceIntra, cfg.twoPass,
				cfg.lossRate, cfg.lfe, eBands, goOld, goDelayed, fineQuant, finePriority)
			cr = cEEFull(cfg.lm, cfg.channels, cfg.nbBytes, cfg.forceIntra, cfg.twoPass,
				cfg.lossRate, cfg.lfe, eBands, cOld, cDelayed, fq32, fp32)
		} else {
			gr = goEncCoarse(cfg.lm, cfg.channels, cfg.nbBytes, cfg.forceIntra, cfg.twoPass,
				cfg.lossRate, cfg.lfe, cfg.start, cfg.end, cfg.effEnd, cfg.preBits, eBands, goOld, goDelayed)
			cr = cEECoarse(cfg.lm, cfg.channels, cfg.nbBytes, cfg.forceIntra, cfg.twoPass,
				cfg.lossRate, cfg.lfe, cfg.start, cfg.end, cfg.effEnd, cfg.preBits, eBands, cOld, cDelayed)
		}

		compareFrame(t, fmt.Sprintf("frame%d", f), gr, cr, cfg.start, cfg.end, nb, cfg.channels)

		if cfg.twoPass != 0 && cfg.forceIntra == 0 {
			seen[cr.intraDec] = true
		}

		// Carry each side's own reconstruction forward.
		goOld, goDelayed = gr.oldOut, gr.delayedOut
		cOld, cDelayed = cr.oldOut, cr.delayedOut
	}
	return seen
}

// TestQuantCoarseEnergyMultiFrame runs quant_coarse_energy over multi-frame
// sequences across (LM, channels, two_pass, force_intra, loss_rate, lfe, budget),
// comparing the emitted bytes, the intra/inter decision, and the reconstructed
// oldEBands + error + delayedIntra against C every frame. It explicitly asserts
// that the two-pass RDO picks intra on some frames and inter on others, so both
// sides of the snapshot/splice are proven byte-exact.
func TestQuantCoarseEnergyMultiFrame(t *testing.T) {
	nb := cEENbEBands()
	globalSeen := map[int]bool{}
	seed := int64(1)
	for lm := 0; lm <= 3; lm++ {
		for _, channels := range []int{1, 2} {
			for _, twoPass := range []int{0, 1} {
				for _, forceIntra := range []int{0, 1} {
					for _, lossRate := range []int{0, 25} {
						for _, nbBytes := range []int{8, 20, 60, 200} {
							cfg := coarseCfg{lm, channels, nbBytes, twoPass, forceIntra, lossRate, 0, 0, nb, nb, 0}
							seed++
							name := fmt.Sprintf("LM%d/C%d/2p%d/fi%d/lr%d/%dB",
								lm, channels, twoPass, forceIntra, lossRate, nbBytes)
							s := seed
							t.Run(name, func(t *testing.T) {
								for k := range runCoarseSequence(t, cfg, s, false) {
									globalSeen[k] = true
								}
							})
						}
					}
				}
			}
		}
	}
	// lfe path: energy decay is clamped to GCONST(3.f) and qi<=0 for i>=2.
	for _, channels := range []int{1, 2} {
		cfg := coarseCfg{3, channels, 60, 1, 0, 0, 1, 0, nb, nb, 0}
		seed++
		t.Run(fmt.Sprintf("lfe/C%d", channels), func(t *testing.T) {
			runCoarseSequence(t, cfg, seed, false)
		})
	}
	if !globalSeen[0] || !globalSeen[1] {
		t.Fatalf("two-pass RDO did not exercise both branches: seen=%v (want both intra-wins and inter-wins)", globalSeen)
	}
}

// TestQuantEnergyFullMultiFrame runs the full coarse+fine+finalise energy path
// over multi-frame sequences, driving quant_fine_energy and quant_energy_finalise
// from the coarse output and carrying the post-finalise oldEBands across frames
// exactly as celt_encoder.c does.
func TestQuantEnergyFullMultiFrame(t *testing.T) {
	nb := cEENbEBands()
	seed := int64(500000)
	for lm := 0; lm <= 3; lm++ {
		for _, channels := range []int{1, 2} {
			for _, twoPass := range []int{0, 1} {
				for _, nbBytes := range []int{16, 40, 120, 400} {
					cfg := coarseCfg{lm, channels, nbBytes, twoPass, 0, 0, 0, 0, nb, nb, 0}
					seed++
					s := seed
					t.Run(fmt.Sprintf("LM%d/C%d/2p%d/%dB", lm, channels, twoPass, nbBytes),
						func(t *testing.T) {
							runCoarseSequence(t, cfg, s, true)
						})
				}
			}
		}
	}
}

// TestQuantCoarseEnergyWindowed drives quant_coarse_energy over non-full band
// windows: hybrid (start=17, only the top CELT bands coded), narrowband
// (end < nbEBands), and effEnd < end (which only loss_distortion sees, so it
// shows up in delayedIntra). This exercises the i!=start bits_left fallback
// gating (quant_bands.c:217), the 3*C*(end-i) budget math over a narrow window,
// and the band-window indexing, all compared byte-exact + full state every frame.
func TestQuantCoarseEnergyWindowed(t *testing.T) {
	nb := cEENbEBands()
	windows := []struct {
		name               string
		start, end, effEnd int
	}{
		{"hybrid17", 17, nb, nb},
		{"nb13", 0, 13, 13},
		{"nb17", 0, 17, 17},
		{"nb19", 0, 19, 19},
		{"effEnd", 0, nb, nb - 4},         // effEnd < end (loss_distortion window)
		{"hybrid+effEnd", 17, nb, nb - 2}, // start!=0 and effEnd<end together
	}
	seed := int64(900000)
	for _, w := range windows {
		for lm := 0; lm <= 3; lm++ {
			for _, channels := range []int{1, 2} {
				for _, twoPass := range []int{0, 1} {
					for _, nbBytes := range []int{20, 120} {
						cfg := coarseCfg{lm, channels, nbBytes, twoPass, 0, 25, 0,
							w.start, w.end, w.effEnd, 0}
						seed++
						s := seed
						t.Run(fmt.Sprintf("%s/LM%d/C%d/2p%d/%dB", w.name, lm, channels, twoPass, nbBytes),
							func(t *testing.T) {
								runCoarseSequence(t, cfg, s, false)
							})
					}
				}
			}
		}
	}
}

// TestQuantCoarseEnergyNonzeroTell drives quant_coarse_energy with ec_tell>0 at
// entry (preBits range-coded bits emitted first, as silence/postfilter/transient
// flags do in the real encoder). The "normal" cases keep tell+3<=budget so the
// intra flag is still written; the "forced" tiny-budget cases push tell+3>budget,
// which disables two_pass and forces intra=0 (quant_bands.c:281) AND skips the
// intra-flag write inside the impl (quant_bands.c:168). The forced cases assert
// the decoded intra is always 0.
func TestQuantCoarseEnergyNonzeroTell(t *testing.T) {
	nb := cEENbEBands()

	// Nonzero entry tell, but budget still ample: intra flag written normally.
	for _, lm := range []int{0, 3} {
		for _, channels := range []int{1, 2} {
			for _, preBits := range []int{5, 20} {
				cfg := coarseCfg{lm, channels, 60, 1, 0, 0, 0, 0, nb, nb, preBits}
				s := int64(700000 + lm*1000 + channels*100 + preBits)
				t.Run(fmt.Sprintf("normal/LM%d/C%d/pre%d", lm, channels, preBits),
					func(t *testing.T) {
						runCoarseSequence(t, cfg, s, false)
					})
			}
		}
	}

	// Tiny budget: tell+3 > budget forces two_pass=0 and intra=0, and the impl
	// skips the intra-flag write. preBits is chosen so 1+preBits+3 > nbBytes*8
	// with margin, while the pre-bits still fit in the buffer.
	forced := []struct{ nbBytes, preBits int }{{3, 22}, {4, 30}, {6, 45}}
	for _, channels := range []int{1, 2} {
		for _, tc := range forced {
			cfg := coarseCfg{3, channels, tc.nbBytes, 1, 0, 0, 0, 0, nb, nb, tc.preBits}
			s := int64(800000 + channels*1000 + tc.nbBytes*100 + tc.preBits)
			t.Run(fmt.Sprintf("forced/C%d/%dB/pre%d", channels, tc.nbBytes, tc.preBits),
				func(t *testing.T) {
					seen := runCoarseSequence(t, cfg, s, false)
					if seen[1] || !seen[0] {
						t.Fatalf("forced tiny-budget case: expected intra always 0, got seen=%v", seen)
					}
				})
		}
	}
}
