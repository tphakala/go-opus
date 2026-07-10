//go:build refc

package oracle

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// q24 returns a random celt_glog (Q24) value uniformly in [lo, hi] log-energy
// units, matching the range of realistic bandLogE values.
func q24(r *rand.Rand, lo, hi float64) int32 {
	return int32((lo + r.Float64()*(hi-lo)) * (1 << 24))
}

// freshReset is -GCONST(28.f): the log-energy a decoder resets a band to. Used
// for the "fresh frame" cross-frame-state case.
const freshReset = int32(-(28 << 24))

// TestEnergyDecodeDifferential drives the pure-Go CELT band-energy decode
// (internal/celt unquant_coarse/fine/finalise, via laplace.go) against the
// pinned libopus C oracle. For every (LM, intra, channels, cross-frame-state,
// budget) combination it: generates random true energies and prior oldEBands;
// runs the C encoder to emit a bitstream and capture C's reconstructed
// oldEBands; runs the C decoder over that bitstream; then runs the Go decoder
// over the SAME bitstream seeded with the SAME prior state, and asserts the Go
// oldEBands is bit-exact against both C reconstructions. This exercises the
// inter/intra prediction filter (docs/hard-parts.md 5: oldEBands is cross-frame
// state) and the full coarse (Laplace) + fine + finalise pipeline.
func TestEnergyDecodeDifferential(t *testing.T) {
	nb := cEnergyNbEBands()
	if nb != 21 {
		t.Fatalf("mode nbEBands = %d, want 21", nb)
	}

	// A spread of packet sizes: the small ones drive the tight-budget coarse
	// branches (small_energy_icdf, single-bit, and the budget-exhausted qi=-1
	// path); the large ones exercise the full fine-energy and finalise bits.
	budgets := []int{8, 12, 20, 40, 120, 250}

	stateCases := []struct {
		name  string
		fresh bool
	}{
		{"fresh", true},  // oldEBands = -GCONST(28): a reset frame.
		{"prior", false}, // oldEBands = random prior energies: inter-prediction path.
	}

	for lm := 0; lm <= 3; lm++ {
		for intra := 0; intra <= 1; intra++ {
			for _, channels := range []int{1, 2} {
				for _, sc := range stateCases {
					for _, nbBytes := range budgets {
						name := fmt.Sprintf("LM%d/intra%d/C%d/%s/%dB",
							lm, intra, channels, sc.name, nbBytes)
						t.Run(name, func(t *testing.T) {
							runEnergyCase(t, lm, intra, channels, nbBytes, sc.fresh, nb)
						})
					}
				}
			}
		}
	}
}

func runEnergyCase(t *testing.T, lm, intra, channels, nbBytes int, fresh bool, nb int) {
	t.Helper()

	// Deterministic per-case seed so failures reproduce.
	seed := int64(lm*100003 + intra*10007 + channels*1009 + nbBytes*13)
	if fresh {
		seed += 7
	}
	r := rand.New(rand.NewSource(seed))

	n := channels * nb
	eBands := make([]int32, n)
	oldInit := make([]int32, n)
	for i := range eBands {
		eBands[i] = q24(r, -14, 20)
		if fresh {
			oldInit[i] = freshReset
		} else {
			oldInit[i] = q24(r, -14, 20)
		}
	}

	// Random but valid fine-quant / priority tables (rate.c would normally
	// produce these; the test just needs identical arrays on both sides).
	fineQuant := make([]int, nb)
	finePriority := make([]int, nb)
	fq32 := make([]int32, nb)
	fp32 := make([]int32, nb)
	for i := 0; i < nb; i++ {
		fineQuant[i] = r.Intn(9) // 0..MAX_FINE_BITS
		finePriority[i] = r.Intn(2)
		fq32[i] = int32(fineQuant[i])
		fp32[i] = int32(finePriority[i])
	}

	res := cEnergyRoundtrip(lm, channels, intra, nbBytes, eBands, oldInit, fq32, fp32)

	// Sanity: the C codec itself must round-trip (encoder reconstruction ==
	// decoder reconstruction). A failure here means the shim is set up wrong,
	// not that the Go port is wrong.
	for i := 0; i < n; i++ {
		if res.oldEnc[i] != res.oldDec[i] {
			t.Fatalf("C encode/decode disagree at band %d (ch %d): enc=%d dec=%d",
				i%nb, i/nb, res.oldEnc[i], res.oldDec[i])
		}
	}

	// Go decode over the exact same buffer, mirroring the decoder pipeline: read
	// the intra-energy flag first (celt_decoder.c:1359), then the three unquant
	// steps.
	var dec rangecoding.Decoder
	dec.Init(res.packet)
	budget := int32(len(res.packet) * 8)
	tell := int32(dec.Tell())
	intraGo := 0
	if tell+3 <= budget {
		intraGo = dec.DecBitLogp(3)
	}
	if intraGo != res.intraDec {
		t.Fatalf("intra flag mismatch: go=%d c=%d", intraGo, res.intraDec)
	}

	goOld := make([]int32, n)
	copy(goOld, oldInit)
	celt.UnquantCoarseEnergy(0, nb, goOld, intraGo, &dec, channels, lm)
	celt.UnquantFineEnergy(0, nb, goOld, fineQuant, &dec, channels)
	bitsLeft := int(budget) - dec.Tell()
	celt.UnquantEnergyFinalise(0, nb, goOld, fineQuant, finePriority, bitsLeft, &dec, channels)

	for i := 0; i < n; i++ {
		if goOld[i] != res.oldDec[i] {
			t.Fatalf("Go vs C decode mismatch at band %d (ch %d): go=%d c=%d (intra=%d)",
				i%nb, i/nb, goOld[i], res.oldDec[i], intraGo)
		}
	}
}
