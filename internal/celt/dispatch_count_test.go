//go:build dispatchcount

package celt

import (
	"math"
	"testing"
)

// These tests run only under -tags dispatchcount (a dedicated CI step). They exist
// because the SIMD differential suites diff a dispatcher against a scalar reference
// that is also the dispatcher's fallback, so a green suite cannot tell a working
// vector kernel from one that silently never ran (issue #72).
//
// The strength of a dispatch counter depends on whether the dispatcher has an
// in-tree fallback BRANCH, and the two classes are deliberately not claimed to be
// equivalent:
//
//   - comb and haar1 route to the vector kernel only past a real branch (comb: the
//     block width and N reach minCombBlock; haar1: stride reaches
//     minHaar1ButterflyStride). The counter sits on the vector side of that branch,
//     so a threshold retune that silently demoted every call into the scalar
//     fallback drops the count to zero and turns the test red. These tests drive
//     both a shape above the branch (counter MUST move) and one below it (counter
//     must NOT move), so the counter is proven to track the branch, not merely the
//     call. This is the case issue #72 was filed for and it is fully closed here.
//
//   - bandGainRequant, celtInnerProd and xcorrKernel have NO in-tree scalar
//     fallback: they delegate to the library unconditionally (past an empty-input
//     guard for the two pitch kernels). Their counter therefore proves only that
//     the differential suites actually EXERCISE the dispatcher (it is not dead
//     code), and for pitch that non-empty input passes the empty-input guard. It
//     canNOT prove the library kernel ran rather than a hypothetical scalar
//     reimplementation, because there is no in-tree branch to bind the counter to;
//     a source edit swapping the library call for the generic would leave the
//     wrapper-entry counter firing. That residual gap is inherent to a
//     branch-free wrapper, not something these tests claim to cover.
//
// The counters are atomic and every check uses a before/after delta, so a stray
// increment from another test cannot make a positive assertion flaky; the negative
// assertions run in serial (non-parallel) tests, before the package's parallel
// pitch differential tests resume, so no concurrent increment lands between the
// two reads.

func TestCombDispatchObserved(t *testing.T) {
	g10, g11, g12 := int16(10048), int16(7112), int16(4248)

	// Above the gate and spanning several blocks: must reach i32.FIRSymValidQ15.
	const vecT, vecN = 120, 480
	buf, xb := combBuf(combValues(1), vecT, vecN)
	out := make([]int32, xb+vecN)
	before := combDispatches.Load()
	combFilterConst(out, xb, buf, xb, vecT, vecN, g10, g11, g12)
	if combDispatches.Load() == before {
		t.Fatalf("comb vector kernel not observed for T=%d N=%d: the dispatcher never reached FIRSymValidQ15", vecT, vecN)
	}

	// Below the gate (block width T-2 and N both under minCombBlock): must take the
	// scalar fallback and leave the counter untouched.
	const fbT, fbN = 15, 8
	fbuf, fxb := combBuf(combValues(2), fbT, fbN)
	fout := make([]int32, fxb+fbN)
	mid := combDispatches.Load()
	combFilterConst(fout, fxb, fbuf, fxb, fbT, fbN, g10, g11, g12)
	if combDispatches.Load() != mid {
		t.Fatalf("comb fallback shape T=%d N=%d wrongly counted as a vector dispatch", fbT, fbN)
	}
}

func TestHaar1DispatchObserved(t *testing.T) {
	// stride >= minHaar1ButterflyStride routes through haar1WideButterfly.
	const wideStride, wideN0 = minHaar1ButterflyStride, 64
	wide := randI32(1, haar1TouchLen(wideN0, wideStride)+8)
	before := haar1Dispatches.Load()
	haar1(wide, wideN0, wideStride)
	if haar1Dispatches.Load() == before {
		t.Fatalf("haar1 wide butterfly not observed for N0=%d stride=%d", wideN0, wideStride)
	}

	// A narrow stride keeps the inlined scalar butterfly: counter must not move.
	const narrowStride, narrowN0 = minHaar1ButterflyStride - 1, 64
	narrow := randI32(2, haar1TouchLen(narrowN0, narrowStride)+8)
	mid := haar1Dispatches.Load()
	haar1(narrow, narrowN0, narrowStride)
	if haar1Dispatches.Load() != mid {
		t.Fatalf("haar1 narrow stride=%d wrongly counted as a wide-butterfly dispatch", narrowStride)
	}
}

func TestBandGainDispatchObserved(t *testing.T) {
	// bandGainRequant has no in-tree fallback branch; the counter only proves the
	// dispatcher is exercised (not dead code), not that i32.GainQ31 ran rather than
	// a scalar reimplementation (see the file header on branch-free wrappers).
	src := randI32(3, 96)
	dst := make([]int32, len(src))
	before := bandGainDispatches.Load()
	bandGainRequant(dst, src, 1<<24, 6, 14)
	if bandGainDispatches.Load() == before {
		t.Fatal("bandGainRequant did not reach i32.GainQ31")
	}
}

func TestMaxabsDispatchObserved(t *testing.T) {
	// celtMaxabs32 / celtMaxabs16 / celtMaxabsRes have no in-tree fallback branch;
	// they delegate to i32.MaxAbs / i16.MaxAbs past a window guard that panics
	// rather than returning, so the counter only proves the dispatcher is exercised
	// (not dead code), not that the vector kernel ran rather than a scalar
	// reimplementation (see the file header on branch-free wrappers).
	x32 := seqFillI32(480)
	before32 := maxabs32Dispatches.Load()
	_ = celtMaxabs32(x32, 0, len(x32))
	if maxabs32Dispatches.Load() == before32 {
		t.Fatal("celtMaxabs32 did not reach i32.MaxAbs")
	}

	x16 := make([]int16, 480)
	for i := range x16 {
		x16[i] = int16(i - 240)
	}
	before16 := maxabs16Dispatches.Load()
	_ = celtMaxabs16(x16, len(x16))
	if maxabs16Dispatches.Load() == before16 {
		t.Fatal("celtMaxabs16 did not reach i16.MaxAbs")
	}
	// celtMaxabsRes shares the i16.MaxAbs counter; a windowed call must also move it.
	midRes := maxabs16Dispatches.Load()
	_ = celtMaxabsRes(x16, 16, 448)
	if maxabs16Dispatches.Load() == midRes {
		t.Fatal("celtMaxabsRes did not reach i16.MaxAbs")
	}
}

func TestPitchDispatchObserved(t *testing.T) {
	x := make([]int16, 480)
	y := make([]int16, 480+3)
	for i := range x {
		x[i] = int16(math.MinInt16 + i%65536)
	}
	for i := range y {
		y[i] = int16(math.MaxInt16 - i%65536)
	}

	// Non-empty input passes the empty-input guard and reaches the DotProduct call
	// site. The counter tracks that guard (the negative case below), not that the
	// vector kernel ran vs a scalar reimplementation (see the file header).
	before := innerProdDispatches.Load()
	_ = celtInnerProd(x, 0, y, 0, 480)
	if innerProdDispatches.Load() == before {
		t.Fatal("celtInnerProd did not reach the post-guard DotProduct call for N=480")
	}
	// N<=0 returns at the empty-input guard, before the call: counter must not move.
	mid := innerProdDispatches.Load()
	_ = celtInnerProd(x, 0, y, 0, 0)
	if innerProdDispatches.Load() != mid {
		t.Fatal("celtInnerProd N=0 incremented past the empty-input guard")
	}

	// Same for xcorrKernel: non-empty input passes the len_<=0 guard.
	var sum [4]int32
	beforeX := xcorrDispatches.Load()
	xcorrKernel(x, y, &sum, 480)
	if xcorrDispatches.Load() == beforeX {
		t.Fatal("xcorrKernel did not reach the post-guard XCorr call for len_=480")
	}
	// len_<=0 returns at the guard, before the call: counter must not move.
	midX := xcorrDispatches.Load()
	xcorrKernel(x, y, &sum, 0)
	if xcorrDispatches.Load() != midX {
		t.Fatal("xcorrKernel len_=0 incremented past the empty-input guard")
	}
}
