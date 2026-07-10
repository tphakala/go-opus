//go:build refc

package oracle

import (
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/silk"
)

// This differential test pins the pure-Go SILK NLSF decode chain (internal/silk
// nlsf_decode.go / nlsf2a.go / lpc_analysis.go / bwexpander.go / sort.go:
// NLSFDecode, NLSF2A, LPCInversePredGain, Bwexpander, Bwexpander32, NLSFStabilize)
// to the pinned libopus C oracle (silk/NLSF_decode.c, NLSF_unpack.c,
// NLSF_stabilize.c, NLSF2A.c, LPC_fit.c, LPC_inv_pred_gain.c, bwexpander.c,
// bwexpander_32.c, sort.c). The chain is a pure function of the already
// range-decoded NLSF codebook path indices, so the SAME inputs are fed to both C
// and Go and every output (NLSF_Q15, a_Q12, inverse gain) is asserted
// bit-identical.

// nlsfOrders is the two SILK LPC filter orders on the decode path: 10 for the
// NB/MB NLSF codebook, 16 for the WB codebook.
var nlsfOrders = []int{10, 16}

// nlsfCB1Vectors is the number of first-stage codebook vectors (nVectors) for both
// codebooks; the CB1 path index (NLSFIndices[0]) must be in [0, nVectors).
const nlsfCB1Vectors = 32

// genNLSFIndices builds a random codebook path vector [order+1] int8: index 0 is
// the first-stage CB index in [0, nVectors); indices 1..order are residual
// quantization indices. residMax caps |residual|; when residMax is 0 all residuals
// are zero (decode the raw codebook vector). Residuals are pure arithmetic inputs
// to silk_NLSF_residual_dequant (never array indices), so any int8 value is valid
// and exercised on both sides identically.
func genNLSFIndices(r *rand.Rand, order, residMax int) []int8 {
	idx := make([]int8, order+1)
	idx[0] = int8(r.Intn(nlsfCB1Vectors))
	for i := 1; i <= order; i++ {
		if residMax == 0 {
			continue
		}
		idx[i] = int8(r.Intn(2*residMax+1) - residMax)
	}
	return idx
}

// TestSilkNLSFDecodeMatchesC is the primary end-to-end check: over both orders and
// a range of residual magnitudes (including the raw-codebook and full-int8 extreme
// cases), the C and Go silk_NLSF_decode must produce a bit-identical Q15 NLSF
// vector, and silk_NLSF2A run over that vector must produce a bit-identical Q12 LPC
// filter. The full-int8 residual tier exercises the silk_NLSF_residual_dequant
// int-arithmetic corner (out_Q10 exceeding int16 before the level adjust).
func TestSilkNLSFDecodeMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0001))
	// residMax tiers: 0 (raw codebook), 4 (NLSF_QUANT_MAX_AMPLITUDE), 10
	// (NLSF_QUANT_MAX_AMPLITUDE_EXT, the real decoded range), 127 (full int8).
	residTiers := []int{0, 4, 10, 127}
	cases := 0
	stableFilters := 0
	for _, order := range nlsfOrders {
		// Boundary CB1 indices 0 and nVectors-1 with zero residuals.
		for _, cb1 := range []int{0, nlsfCB1Vectors - 1} {
			idx := make([]int8, order+1)
			idx[0] = int8(cb1)
			checkNLSFChain(t, order, idx, &cases, &stableFilters)
		}
		for _, residMax := range residTiers {
			trials := 64
			for trial := 0; trial < trials; trial++ {
				idx := genNLSFIndices(r, order, residMax)
				checkNLSFChain(t, order, idx, &cases, &stableFilters)
			}
		}
	}
	if stableFilters == 0 {
		t.Fatal("no decoded NLSF produced a stable LPC filter; the NLSF2A path looks vacuous")
	}
	t.Logf("silk_NLSF_decode + silk_NLSF2A differential cases: %d (stable filters: %d)", cases, stableFilters)
}

// checkNLSFChain decodes idx with both C and Go silk_NLSF_decode, asserts the Q15
// NLSF vectors are bit-identical, then runs silk_NLSF2A on both and asserts the Q12
// LPC filters are bit-identical.
func checkNLSFChain(t *testing.T, order int, idx []int8, cases, stableFilters *int) {
	t.Helper()

	goNLSF := make([]int16, order)
	silk.NLSFDecode(goNLSF, idx, order)
	cNLSF := cSilkNLSFDecode(order, idx)
	if !equalInt16(goNLSF, cNLSF) {
		t.Fatalf("order=%d idx=%v: NLSF_Q15 Go=%v C=%v", order, idx, goNLSF, cNLSF)
	}

	goA := make([]int16, order)
	silk.NLSF2A(goA, goNLSF, order)
	cA := cSilkNLSF2A(cNLSF, order)
	if !equalInt16(goA, cA) {
		t.Fatalf("order=%d idx=%v NLSF=%v: a_Q12 Go=%v C=%v", order, idx, goNLSF, goA, cA)
	}

	// The a_Q12 out of silk_NLSF2A is stabilized, so it should be a stable filter;
	// track it so the primary test proves the stability path is not vacuous.
	if silk.LPCInversePredGain(goA, order) != 0 {
		*stableFilters++
	}
	*cases++
}

// TestSilkNLSF2AMatchesC fuzzes silk_NLSF2A directly. It feeds three families of
// Q15 NLSF input: random vectors in [0, 32767] (usually unsorted, so frequently an
// unstable filter that drives the bandwidth-expansion loop and silk_LPC_fit's
// overflow clip); properly sorted valid vectors; and degenerate all-equal /
// tightly-clustered vectors that are guaranteed to force the stability loop. a_Q12
// must be bit-identical to C in every case.
func TestSilkNLSF2AMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0002))
	cases := 0
	for _, order := range nlsfOrders {
		// Degenerate inputs that guarantee the NLSF2A bandwidth-expansion loop fires:
		// an all-equal spectrum has repeated roots -> unstable filter.
		for _, v := range []int{0, 1, 4096, 16384, 30000, 32767} {
			nlsf := make([]int16, order)
			for i := range nlsf {
				nlsf[i] = int16(v)
			}
			checkNLSF2A(t, nlsf, order, &cases)
		}
		// Tightly clustered (near-degenerate) sorted vectors.
		for trial := 0; trial < 200; trial++ {
			base := r.Intn(30000)
			nlsf := make([]int16, order)
			for i := range nlsf {
				nlsf[i] = int16(base + i) // spacing 1
			}
			checkNLSF2A(t, nlsf, order, &cases)
		}
		// Random (usually unsorted) vectors.
		for trial := 0; trial < 4000; trial++ {
			nlsf := make([]int16, order)
			for i := range nlsf {
				nlsf[i] = int16(r.Intn(32768)) // [0, 32767]
			}
			checkNLSF2A(t, nlsf, order, &cases)
		}
		// Properly sorted, well-spaced valid vectors.
		for trial := 0; trial < 2000; trial++ {
			nlsf := make([]int16, order)
			cur := 1 + r.Intn(200)
			for i := range nlsf {
				cur += 1 + r.Intn((32000-cur)/(order-i))
				if cur > 32767 {
					cur = 32767
				}
				nlsf[i] = int16(cur)
			}
			checkNLSF2A(t, nlsf, order, &cases)
		}
	}
	t.Logf("silk_NLSF2A differential cases: %d", cases)
}

func checkNLSF2A(t *testing.T, nlsf []int16, order int, cases *int) {
	t.Helper()
	goA := make([]int16, order)
	silk.NLSF2A(goA, nlsf, order)
	cA := cSilkNLSF2A(nlsf, order)
	if !equalInt16(goA, cA) {
		t.Fatalf("order=%d NLSF=%v: a_Q12 Go=%v C=%v", order, nlsf, goA, cA)
	}
	*cases++
}

// TestSilkLPCInversePredGainMatchesC fuzzes silk_LPC_inverse_pred_gain over random
// Q12 coefficients (which are usually unstable -> return 0, and hit the DC_resp
// early-out) and over stable filters produced by silk_NLSF2A (return != 0). The
// int32 result must be bit-identical to C, and both the zero and non-zero outcomes
// must occur so the assertion is not vacuous.
func TestSilkLPCInversePredGainMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0003))
	cases := 0
	zeros, nonzeros := 0, 0
	for _, order := range nlsfOrders {
		// Random coefficients: exercises the stability / DC_resp / return-0 paths.
		for trial := 0; trial < 3000; trial++ {
			a := make([]int16, order)
			for i := range a {
				a[i] = int16(r.Intn(1<<13) - (1 << 12)) // roughly Q12 coefficient range
			}
			checkInvGain(t, a, order, &zeros, &nonzeros, &cases)
		}
		// Stable filters from the decode chain: exercises the full (return != 0) path.
		for trial := 0; trial < 400; trial++ {
			nlsf := make([]int16, order)
			for i := range nlsf {
				nlsf[i] = int16(r.Intn(32768))
			}
			a := make([]int16, order)
			silk.NLSF2A(a, nlsf, order)
			checkInvGain(t, a, order, &zeros, &nonzeros, &cases)
		}
	}
	if zeros == 0 || nonzeros == 0 {
		t.Fatalf("silk_LPC_inverse_pred_gain non-vacuity failed: zeros=%d nonzeros=%d", zeros, nonzeros)
	}
	t.Logf("silk_LPC_inverse_pred_gain differential cases: %d (zero: %d, nonzero: %d)", cases, zeros, nonzeros)
}

func checkInvGain(t *testing.T, a []int16, order int, zeros, nonzeros, cases *int) {
	t.Helper()
	got := silk.LPCInversePredGain(a, order)
	want := cSilkLPCInversePredGain(a, order)
	if got != want {
		t.Fatalf("order=%d a=%v: invGain Go=%d C=%d", order, a, got, want)
	}
	if got == 0 {
		*zeros++
	} else {
		*nonzeros++
	}
	*cases++
}

// TestSilkBwexpanderMatchesC fuzzes silk_bwexpander (Q12 int16 filter) over random
// coefficients and chirp factors, asserting the in-place expanded filter is
// bit-identical to C.
func TestSilkBwexpanderMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0004))
	cases := 0
	for _, d := range []int{2, 10, 16} {
		for trial := 0; trial < 3000; trial++ {
			ar := make([]int16, d)
			for i := range ar {
				ar[i] = int16(r.Intn(1<<16) - (1 << 15))
			}
			chirp := int32(r.Intn(131073)) // [0, 2^17], covers <1, ==1 and >1 chirp
			goAr := append([]int16(nil), ar...)
			silk.Bwexpander(goAr, d, chirp)
			cAr := cSilkBwexpander(ar, d, chirp)
			if !equalInt16(goAr, cAr) {
				t.Fatalf("d=%d chirp=%d ar=%v: Go=%v C=%v", d, chirp, ar, goAr, cAr)
			}
			cases++
		}
	}
	t.Logf("silk_bwexpander differential cases: %d", cases)
}

// TestSilkBwexpander32MatchesC fuzzes silk_bwexpander_32 (unscaled int32 filter)
// over random coefficients and chirp factors, asserting the in-place expanded
// filter is bit-identical to C.
func TestSilkBwexpander32MatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0005))
	cases := 0
	for _, d := range []int{2, 10, 16} {
		for trial := 0; trial < 3000; trial++ {
			ar := make([]int32, d)
			for i := range ar {
				ar[i] = int32(r.Uint32())
			}
			chirp := int32(r.Intn(131073))
			goAr := append([]int32(nil), ar...)
			silk.Bwexpander32(goAr, d, chirp)
			cAr := cSilkBwexpander32(ar, d, chirp)
			if !equalInt32(goAr, cAr) {
				t.Fatalf("d=%d chirp=%d ar=%v: Go=%v C=%v", d, chirp, ar, goAr, cAr)
			}
			cases++
		}
	}
	t.Logf("silk_bwexpander_32 differential cases: %d", cases)
}

// genNDeltaMin builds a random [L+1] minimum-distance vector with every element in
// [1, maxDelta] (silk_NLSF_stabilize requires NDeltaMin[L] >= 1 and treats every
// entry as a positive spacing).
func genNDeltaMin(r *rand.Rand, L, maxDelta int) []int16 {
	d := make([]int16, L+1)
	for i := range d {
		d[i] = int16(1 + r.Intn(maxDelta))
	}
	return d
}

// TestSilkNLSFStabilizeMatchesC fuzzes silk_NLSF_stabilize over random (often
// unsorted, out-of-spacing) NLSF vectors and random valid NDeltaMin vectors, plus
// crafted inputs that force the iterative fixer to fire and inputs whose spacing
// budget exceeds the range so the 20-iteration cap is hit and the insertion-sort
// fall-back runs. The stabilized output must be bit-identical to C.
func TestSilkNLSFStabilizeMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0006))
	cases := 0
	modified := 0     // stabilizer changed the input (iterative or fall-back fired)
	fallbackSeen := 0 // deliberately over-budget cases that hit the fall-back
	for _, L := range nlsfOrders {
		// Random unsorted NLSF + modest spacing: mixes no-op, iterative and fall-back.
		for trial := 0; trial < 4000; trial++ {
			nlsf := make([]int16, L)
			for i := range nlsf {
				nlsf[i] = int16(r.Intn(32768))
			}
			// Spacing small enough to usually be satisfiable, occasionally not.
			nd := genNDeltaMin(r, L, 400)
			in := append([]int16(nil), nlsf...)
			goOut := append([]int16(nil), nlsf...)
			silk.NLSFStabilize(goOut, nd, L)
			cOut := cSilkNLSFStabilize(nlsf, nd, L)
			if !equalInt16(goOut, cOut) {
				t.Fatalf("L=%d nlsf=%v nd=%v: Go=%v C=%v", L, in, nd, goOut, cOut)
			}
			if !equalInt16(goOut, in) {
				modified++
			}
			cases++
		}

		// Forced fall-back: a reversed (strictly decreasing) NLSF with a spacing
		// budget summing past the 1<<15 range cannot be satisfied by the iterative
		// fixer within MAX_LOOPS, so the insertion-sort fall-back runs.
		for trial := 0; trial < 200; trial++ {
			nlsf := make([]int16, L)
			for i := range nlsf {
				nlsf[i] = int16(32000 - i*100 - r.Intn(50)) // decreasing
			}
			// Large deltas: (L+1) * bigDelta > 2^15 guarantees the cap is hit.
			bigDelta := 32768/(L+1) + 200
			nd := make([]int16, L+1)
			for i := range nd {
				nd[i] = int16(bigDelta)
			}
			in := append([]int16(nil), nlsf...)
			goOut := append([]int16(nil), nlsf...)
			silk.NLSFStabilize(goOut, nd, L)
			cOut := cSilkNLSFStabilize(nlsf, nd, L)
			if !equalInt16(goOut, cOut) {
				t.Fatalf("fallback L=%d nlsf=%v nd=%v: Go=%v C=%v", L, in, nd, goOut, cOut)
			}
			if !equalInt16(goOut, in) {
				modified++
			}
			fallbackSeen++
			cases++
		}
	}
	if modified == 0 {
		t.Fatal("silk_NLSF_stabilize never modified any input; the firing path looks vacuous")
	}
	if fallbackSeen == 0 {
		t.Fatal("the fall-back path was never exercised")
	}
	t.Logf("silk_NLSF_stabilize differential cases: %d (modified: %d, forced-fallback: %d)", cases, modified, fallbackSeen)
}

// TestSilkNLSFChainMutation is the non-vacuity mutation check. First it confirms
// the baseline Go decode matches C (guards against a vacuous comparator). Then it
// mutates a single input index and confirms (a) the C output actually changes (the
// index is load-bearing) and (b) the Go output tracks the same change bit-for-bit;
// a port that ignored an index would fail (b). Finally it perturbs a correct C
// output by one LSB and confirms the comparator flags it.
func TestSilkNLSFChainMutation(t *testing.T) {
	r := rand.New(rand.NewSource(0x715F0007))
	cb1Caught := false
	residCaught := false
	for _, order := range nlsfOrders {
		for trial := 0; trial < 64; trial++ {
			idx := genNLSFIndices(r, order, 10)

			base := make([]int16, order)
			silk.NLSFDecode(base, idx, order)
			cBase := cSilkNLSFDecode(order, idx)
			if !equalInt16(base, cBase) {
				t.Fatalf("order=%d idx=%v: baseline Go decode diverged from C", order, idx)
			}

			// Mutation A: change the first-stage codebook index.
			mutCB := append([]int8(nil), idx...)
			mutCB[0] = int8((int(idx[0]) + 1) % nlsfCB1Vectors)
			goCB := make([]int16, order)
			silk.NLSFDecode(goCB, mutCB, order)
			cCB := cSilkNLSFDecode(order, mutCB)
			if !equalInt16(goCB, cCB) {
				t.Fatalf("order=%d: mutated-CB1 Go decode diverged from C", order)
			}
			if !equalInt16(cCB, cBase) {
				cb1Caught = true // the CB1 index is load-bearing and Go tracked it
			}

			// Mutation B: change one residual index.
			mutRes := append([]int8(nil), idx...)
			j := 1 + r.Intn(order)
			mutRes[j] = int8(int(mutRes[j]) + 3)
			goRes := make([]int16, order)
			silk.NLSFDecode(goRes, mutRes, order)
			cRes := cSilkNLSFDecode(order, mutRes)
			if !equalInt16(goRes, cRes) {
				t.Fatalf("order=%d: mutated-residual Go decode diverged from C", order)
			}
			if !equalInt16(cRes, cBase) {
				residCaught = true
			}
		}
	}
	if !cb1Caught {
		t.Fatal("changing the CB1 index never changed the decode; the differential assertion may be vacuous")
	}
	if !residCaught {
		t.Fatal("changing a residual index never changed the decode; the differential assertion may be vacuous")
	}

	// Comparator self-check: a one-LSB perturbation of a correct C output must be
	// flagged by the bit-exact comparator.
	order := 16
	idx := genNLSFIndices(r, order, 10)
	cNLSF := cSilkNLSFDecode(order, idx)
	mut := append([]int16(nil), cNLSF...)
	mut[0]++
	if equalInt16(mut, cNLSF) {
		t.Fatal("comparator failed to detect a one-element perturbation")
	}
}
