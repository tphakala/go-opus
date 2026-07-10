//go:build refc

package oracle

import (
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This differential test pins the pure-Go CELT PVQ decode (internal/celt cwrs.go
// / vq.go: DecodePulses, AlgUnquant, ExpRotation, RenormaliseVector, PVQV) to the
// pinned libopus C oracle (celt/cwrs.c, celt/vq.c). The Go encoder does not exist
// yet, so the C alg_quant (or a raw ec_enc_uint index) PRODUCES each coded
// codeword; both C and Go then DECODE the same bitstream and every output is
// asserted bit-identical (int32 element-wise) with matching range-decoder end
// state (tell + rng).

// pvqNs is the representative grid of band sizes N (CELT band widths at LM 0-3,
// small to the largest standard-mode split). N>176 needs the CUSTOM_MODES
// extra-rows PVQ table, which this non-CUSTOM_MODES oracle build does not carry.
var pvqNs = []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 22, 24, 32, 44, 48, 64, 88, 96, 128, 144, 176}

// pvqSpreads is SPREAD_NONE/LIGHT/NORMAL/AGGRESSIVE (celt/bands.h:68-71).
var pvqSpreads = []int{0, 1, 2, 3}

// pvqGains are representative decode band gains (Q31), all positive.
var pvqGains = []int32{2147483647, 1073741824, 1500000000, 268435456}

// sampleKs returns a representative set of pulse counts for a band of size N:
// 1, a couple of interior values, and the table-valid cap. Deduplicated.
func sampleKs(maxK int) []int {
	if maxK <= 0 {
		return nil
	}
	set := map[int]bool{1: true, maxK: true}
	for _, k := range []int{2, 3, maxK / 4, maxK / 2, (3 * maxK) / 4, maxK - 1} {
		if k >= 1 && k <= maxK {
			set[k] = true
		}
	}
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// pvqBs returns the block counts B to test for a band of size N: 1, plus 2 and 4
// where they divide N. B is the exp_rotation stride and the collapse-mask block
// count, so it must divide N.
func pvqBs(n int) []int {
	bs := []int{1}
	if n%2 == 0 {
		bs = append(bs, 2)
	}
	if n%4 == 0 {
		bs = append(bs, 4)
	}
	return bs
}

// genVec builds a random vector of N celt_norm (int32) samples in a moderate
// range, guaranteed not all-zero. The exact magnitude does not affect decode
// correctness (only which codeword the C encoder picks), so a plain random
// direction gives good codeword coverage.
func genVec(r *rand.Rand, n int) []int32 {
	x := make([]int32, n)
	nonzero := false
	for i := range x {
		x[i] = int32(r.Intn(1<<21) - (1 << 20))
		if x[i] != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		x[0] = 1 << 20
	}
	return x
}

// TestPVQVMatchesRecurrence cross-checks the Go V(N,K) table lookup against the
// independent O(NK) recurrence in the shim, over every table-valid (N,K).
func TestPVQVMatchesRecurrence(t *testing.T) {
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		for k := 1; k <= maxK; k++ {
			got := celt.PVQV(n, k)
			want := cPVQTrueV(n, k)
			if want == 0 {
				t.Fatalf("N=%d K=%d: recurrence says not representable but K<=maxK", n, k)
			}
			if got != want {
				t.Fatalf("PVQV(%d,%d)=%d, recurrence=%d", n, k, got, want)
			}
		}
	}
}

// TestDecodePulsesMatchesC drives DecodePulses/cwrsi directly: a random codeword
// index is range-encoded on the C side, then decoded by both C decode_pulses and
// Go DecodePulses; the pulse vector, Ryy and decoder tell/rng must all match.
func TestDecodePulsesMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x5EED01))
	cases := 0
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		for _, k := range sampleKs(maxK) {
			ft := celt.PVQV(n, k)
			// A few random indices spanning the codebook, plus the endpoints.
			idxs := make([]uint32, 0, 7)
			idxs = append(idxs, 0, ft-1, ft/2)
			for s := 0; s < 4; s++ {
				idxs = append(idxs, uint32(r.Int63())%ft)
			}
			for _, idx := range idxs {
				buf := cPVQEncodeIndex(idx, ft)

				yC, ryyC, tellC, rngC := cPVQDecodePulses(buf, n, k)

				yGo := make([]int32, n)
				var dec rangecoding.Decoder
				dec.Init(buf)
				ryyGo := celt.DecodePulses(yGo, n, k, &dec)

				if ryyGo != ryyC {
					t.Fatalf("N=%d K=%d idx=%d: Ryy Go=%d C=%d", n, k, idx, ryyGo, ryyC)
				}
				if !equalInt32(yGo, yC) {
					t.Fatalf("N=%d K=%d idx=%d: pulses Go=%v C=%v", n, k, idx, yGo, yC)
				}
				if got := dec.Tell(); got != tellC {
					t.Fatalf("N=%d K=%d idx=%d: tell Go=%d C=%d", n, k, idx, got, tellC)
				}
				if got := dec.Rng(); got != rngC {
					t.Fatalf("N=%d K=%d idx=%d: rng Go=%d C=%d", n, k, idx, got, rngC)
				}
				// Sanity: decoded pulses must sum in |.| to K.
				if s := absSum(yGo); s != k {
					t.Fatalf("N=%d K=%d idx=%d: |pulses| sum=%d != K", n, k, idx, s)
				}
				cases++
			}
		}
	}
	t.Logf("decode_pulses differential cases: %d", cases)
}

// TestAlgUnquantMatchesC is the primary check: for the full N/K/spread/B/gain
// grid, the C alg_quant encodes a random input vector into a PVQ codeword, then
// both C alg_unquant and Go AlgUnquant decode the SAME bitstream. The normalised
// output vectors must be bit-identical (int32 element-wise), the collapse masks
// equal, and the range-decoder tell/rng match.
func TestAlgUnquantMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x5EED02))
	cases := 0
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		for _, k := range sampleKs(maxK) {
			for _, spread := range pvqSpreads {
				for _, b := range pvqBs(n) {
					for trial := 0; trial < 2; trial++ {
						xin := genVec(r, n)
						packet, cmEnc := cPVQAlgQuant(xin, n, k, spread, b)
						_ = cmEnc // encode-side collapse mask; decode side is compared below

						for _, gain := range pvqGains {
							xC, cmC, tellC, rngC := cPVQAlgUnquant(packet, n, k, spread, b, gain)

							xGo := make([]int32, n)
							var dec rangecoding.Decoder
							dec.Init(packet)
							cmGo := celt.AlgUnquant(xGo, n, k, spread, b, &dec, gain)

							if !equalInt32(xGo, xC) {
								t.Fatalf("N=%d K=%d spread=%d B=%d gain=%d: X Go=%v C=%v",
									n, k, spread, b, gain, xGo, xC)
							}
							if cmGo != cmC {
								t.Fatalf("N=%d K=%d spread=%d B=%d gain=%d: collapse Go=%d C=%d",
									n, k, spread, b, gain, cmGo, cmC)
							}
							if got := dec.Tell(); got != tellC {
								t.Fatalf("N=%d K=%d spread=%d B=%d: tell Go=%d C=%d",
									n, k, spread, b, got, tellC)
							}
							if got := dec.Rng(); got != rngC {
								t.Fatalf("N=%d K=%d spread=%d B=%d: rng Go=%d C=%d",
									n, k, spread, b, got, rngC)
							}
							cases++
						}
					}
				}
			}
		}
	}
	t.Logf("alg_unquant differential cases: %d", cases)
}

// TestExpRotationMatchesC drives exp_rotation directly (both directions) over the
// grid, asserting bit-identical rotated buffers. Then it runs the mutation check:
// bumping the spread by one perturbs the derived rotation coefficients (factor ->
// gain -> theta -> c,s), so the Go result under the wrong spread must diverge from
// the correct C result, confirming the bit-exact assertion actually catches a
// rotation change.
func TestExpRotationMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x5EED03))
	cases := 0
	mutationCaught := false
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		for _, k := range sampleKs(maxK) {
			for _, spread := range pvqSpreads {
				for _, b := range pvqBs(n) {
					if n%b != 0 {
						continue
					}
					for _, dir := range []int{-1, 1} {
						xin := genVec(r, n)

						want := cPVQExpRotation(xin, n, dir, b, k, spread)
						got := append([]int32(nil), xin...)
						celt.ExpRotation(got, n, dir, b, k, spread)
						if !equalInt32(got, want) {
							t.Fatalf("N=%d K=%d spread=%d B=%d dir=%d: exp_rotation Go=%v C=%v",
								n, k, spread, b, dir, got, want)
						}
						cases++

						// Mutation: a wrong spread must change the rotation (unless
						// the rotation is a no-op for this case, i.e. 2K>=len or
						// SPREAD_NONE on both).
						if spread < 3 && 2*k < n/b {
							mutant := append([]int32(nil), xin...)
							celt.ExpRotation(mutant, n, dir, b, k, spread+1)
							if !equalInt32(mutant, want) {
								mutationCaught = true
							}
						}
					}
				}
			}
		}
	}
	if !mutationCaught {
		t.Fatal("mutation check never observed a divergence; the harness may be vacuous")
	}
	t.Logf("exp_rotation differential cases: %d (mutation divergence observed)", cases)
}

// TestRenormaliseVectorMatchesC checks RenormaliseVector against C over random
// vectors and gains.
func TestRenormaliseVectorMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x5EED04))
	for _, n := range pvqNs {
		for trial := 0; trial < 4; trial++ {
			xin := genVec(r, n)
			for _, gain := range pvqGains {
				want := cPVQRenormalise(xin, n, gain)
				got := append([]int32(nil), xin...)
				celt.RenormaliseVector(got, n, gain)
				if !equalInt32(got, want) {
					t.Fatalf("N=%d gain=%d: renormalise Go=%v C=%v", n, gain, got, want)
				}
			}
		}
	}
}

// TestComparatorCatchesPerturbation is a harness self-check: a single-element
// perturbation of a correct C output must be flagged by the bit-exact comparator,
// proving the differential assertions are not vacuous.
func TestComparatorCatchesPerturbation(t *testing.T) {
	r := rand.New(rand.NewSource(0x5EED05))
	n, k, spread, b := 96, 3, 3, 1
	xin := genVec(r, n)
	packet, _ := cPVQAlgQuant(xin, n, k, spread, b)
	xC, _, _, _ := cPVQAlgUnquant(packet, n, k, spread, b, 2147483647)

	xGo := make([]int32, n)
	var dec rangecoding.Decoder
	dec.Init(packet)
	celt.AlgUnquant(xGo, n, k, spread, b, &dec, 2147483647)
	if !equalInt32(xGo, xC) {
		t.Fatalf("baseline decode diverged: Go=%v C=%v", xGo, xC)
	}
	// Perturb one nonzero element and confirm the comparator flags it.
	mut := append([]int32(nil), xC...)
	for i := range mut {
		if mut[i] != 0 {
			mut[i]++
			break
		}
	}
	if equalInt32(mut, xC) {
		t.Fatal("comparator failed to detect a one-element perturbation")
	}
}

func equalInt32(a, b []int32) bool {
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

func absSum(y []int32) int {
	s := 0
	for _, v := range y {
		if v < 0 {
			s -= int(v)
		} else {
			s += int(v)
		}
	}
	return s
}
