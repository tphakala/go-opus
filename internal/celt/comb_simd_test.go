package celt

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// combGainTriples covers the (g10, g11, g12) shapes the codec actually produces
// plus the wrapping edges. The production gains are MULT16_16_P15(g1, combGains
// row) with g1 the Q15 postfilter gain in [0, ~0.9], so all three are positive
// int16 with a dominant center tap; tapsets 1 and 2 have g12 == 0 (and tapset 2
// a small g11). The edge triples (Max/MinInt16, single-nonzero, mixed sign)
// exercise the per-product Q15 truncation and the wrapping pair-adds across the
// whole int16 gain range, which the production values alone do not reach.
var combGainTriples = [][3]int16{
	{10048, 7112, 4248}, // tapset 0, full gain
	{15200, 8784, 0},    // tapset 1, full gain
	{26208, 3280, 0},    // tapset 2, full gain
	{5024, 3556, 2124},  // tapset 0, half gain
	{0, 0, 0},           // no-op
	{26208, 0, 0},       // center only
	{0, 8784, 0},        // one pair only
	{0, 0, 4248},        // outer pair only
	{math.MaxInt16, math.MaxInt16, math.MaxInt16},
	{math.MinInt16, math.MinInt16, math.MinInt16},
	{math.MinInt16, 0, math.MaxInt16},
	{math.MaxInt16, math.MinInt16, 1},
}

// combValues is a per-index generator for the history + input region: a spread
// over the full int32 range so the pair-adds, the Q15 truncations and the
// SATURATE/bias boundary at +-sigSat are all exercised.
func combValues(seed uint32) func(i int) int32 {
	return func(i int) int32 {
		u := uint32(i)*2654435761 + seed*40503 + 1013904223
		return int32(u) // deliberate wrap over the full int32 range
	}
}

// combValuesInBand squeezes combValues into the codec's real dynamic range so
// v = acc + x - 1 never reaches SATURATE. At full range 65 to 88 percent of
// outputs clamp, which collapses a genuine per-sample divergence onto an equal
// clamp value: a one-sample block-width error is invisible at (T=135, N=240)
// full range and visible in band.
func combValuesInBand(seed uint32) func(i int) int32 {
	base := combValues(seed)
	return func(i int) int32 { return base(i) >> 6 }
}

// combBuf builds the x buffer for a (T, N) call: T+2 samples of history so the
// most-negative read x[xb-T-2] is index 0, followed by N input samples, with
// xb = T+2.
func combBuf(gen func(i int) int32, T, N int) (buf []int32, xb int) {
	xb = T + 2
	buf = make([]int32, xb+N)
	for i := range buf {
		buf[i] = gen(i)
	}
	return buf, xb
}

// runCombSep runs the scalar reference and the SIMD dispatcher into two fresh,
// sentinel-filled output buffers with x read-only (the encoder-prefilter /
// decoder-etmp shape), diffing the whole buffer so a stray write past the N
// written outputs or into the sentinel tail is caught as well as any value
// mismatch. It also clones x and asserts neither path wrote into it, so a write
// into the read-only input surfaces at its own call rather than as a cascade
// through the later gain triples that reuse the same buffer.
func runCombSep(t *testing.T, buf []int32, xb, T, N int, g10, g11, g12 int16) {
	t.Helper()
	const sentinel = int32(0x0BADF00D)
	yLen := xb + N + 8
	a := make([]int32, yLen)
	b := make([]int32, yLen)
	for i := range a {
		a[i] = sentinel
		b[i] = sentinel
	}
	xOrig := slices.Clone(buf)
	combFilterConstGeneric(a, xb, buf, xb, T, N, g10, g11, g12)
	combFilterConst(b, xb, buf, xb, T, N, g10, g11, g12)
	if !slices.Equal(buf, xOrig) {
		t.Fatalf("combFilterConst wrote into the read-only input x, T=%d N=%d g=(%d,%d,%d)", T, N, g10, g11, g12)
	}
	if !slices.Equal(a, b) {
		t.Fatalf("combFilterConst (separate) mismatch T=%d N=%d g=(%d,%d,%d):\n scalar=%v\n simd  =%v",
			T, N, g10, g11, g12, a[xb:xb+N], b[xb:xb+N])
	}
}

// runCombInPlace runs both paths with y and x the same slice at the same base:
// the decoder post-filter shape, and the recurrence path where a broken block
// boundary would diverge. History [0,xb) must also survive untouched, so the
// whole buffer is diffed.
func runCombInPlace(t *testing.T, buf []int32, xb, T, N int, g10, g11, g12 int16) {
	t.Helper()
	gen := slices.Clone(buf)
	sim := slices.Clone(buf)
	combFilterConstGeneric(gen, xb, gen, xb, T, N, g10, g11, g12)
	combFilterConst(sim, xb, sim, xb, T, N, g10, g11, g12)
	if !slices.Equal(gen, sim) {
		t.Fatalf("combFilterConst (in-place) mismatch T=%d N=%d g=(%d,%d,%d):\n scalar=%v\n simd  =%v",
			T, N, g10, g11, g12, gen[xb:xb+N], sim[xb:xb+N])
	}
}

// combMaxT is the largest pitch period combFilterConst sees: the callers clamp T
// to [combfilterMinperiod, combfilterMaxperiod-2].
const combMaxT = combfilterMaxperiod - 2

// combTestPeriods spans the min pitch (15), the SIMD gate at block width
// T-2 == minCombBlock (T=34), the combTile boundary at T-2 == combTile (T=258)
// and just past it (T=259) where a block is split, prime-ish widths, and large
// periods where a block covers many vector registers, up to the clamped max
// combMaxT.
var combTestPeriods = []int{15, 16, 17, 31, 32, 33, 34, 63, 120, 255, 258, 259, 512, combMaxT}

// combTestLengths span below the gate, exactly at it (N=32 == minCombBlock) and
// above, include N=122 (an exact multiple of T=63's block width 61, so a
// multi-block call whose final block is full, with no short trailing block, is
// exercised), and full frame sizes that cross several blocks.
var combTestLengths = []int{0, 1, 2, 3, 7, 8, 9, 13, 15, 16, 17, 31, 32, 33, 63, 120, 122, 240, 480, 960}

// assertStraddlesCombGate fails unless the periods x lengths matrix sits on both
// sides of the SIMD dispatch gate and at least one cell reaches the vector path.
// combFilterConst routes to combFilterConstGeneric (the very code that is the
// differential oracle) unless both the block width T-2 and the output length N
// reach minCombBlock, so a matrix with no cell above the gate makes the
// differential compare the oracle with itself and stay green. Raising minCombBlock
// to 100000 makes the vector call dead code and leaves this whole file passing;
// this guard reads the same minCombBlock the dispatcher reads, so it is what turns
// that red. Modelled on the straddle check in haar1_simd_test.go.
func assertStraddlesCombGate(t *testing.T, periods, lengths []int) {
	t.Helper()
	var periodBelow, periodAtOrAbove, lengthBelow, lengthAtOrAbove, reachesVector bool
	for _, T := range periods {
		if T-2 < minCombBlock {
			periodBelow = true
		} else {
			periodAtOrAbove = true
		}
	}
	for _, N := range lengths {
		if N < minCombBlock {
			lengthBelow = true
		} else {
			lengthAtOrAbove = true
		}
	}
	for _, T := range periods {
		for _, N := range lengths {
			if T-2 >= minCombBlock && N >= minCombBlock {
				reachesVector = true
			}
		}
	}
	if !periodBelow || !periodAtOrAbove {
		t.Fatalf("period matrix must straddle the gate (block width T-2 == minCombBlock = %d): below=%v atOrAbove=%v",
			minCombBlock, periodBelow, periodAtOrAbove)
	}
	if !lengthBelow || !lengthAtOrAbove {
		t.Fatalf("length matrix must straddle the gate (N == minCombBlock = %d): below=%v atOrAbove=%v",
			minCombBlock, lengthBelow, lengthAtOrAbove)
	}
	if !reachesVector {
		t.Fatalf("no (T,N) cell reaches the vector path (T-2 >= %d and N >= %d): the differential would compare the oracle with itself",
			minCombBlock, minCombBlock)
	}
}

func TestCombFilterConstMatchesScalar(t *testing.T) {
	assertStraddlesCombGate(t, combTestPeriods, combTestLengths)
	for _, T := range combTestPeriods {
		for ni, N := range combTestLengths {
			seed := uint32(T*131 + ni)
			// Run every cell at full int32 range (for the SATURATE/bias corners)
			// AND squeezed in band, so a genuine per-sample divergence is not
			// collapsed onto a shared clamp value (see combValuesInBand).
			for _, gen := range []func(int) int32{combValues(seed), combValuesInBand(seed)} {
				buf, xb := combBuf(gen, T, N)
				for _, g := range combGainTriples {
					runCombSep(t, buf, xb, T, N, g[0], g[1], g[2])
					runCombInPlace(t, buf, xb, T, N, g[0], g[1], g[2])
				}
			}
		}
	}
}

// TestCombFilterConstExtremes fills the history and input with int32 wrapping
// edges so the pair-adds overflow, the Q15 truncations hit their corners, and
// the SATURATE clamp at +-sigSat and the -1 bias boundary are exercised. The
// saturating range is deliberate here (unlike MatchesScalar), so no in-band pass.
func TestCombFilterConstExtremes(t *testing.T) {
	edges := []int32{math.MinInt32, math.MinInt32 + 1, -1, 0, 1, sigSat - 1, sigSat, sigSat + 1, math.MaxInt32}
	periods := []int{15, 16, 33, 120, 512}
	lengths := []int{1, 2, 8, 16, 17, 33, 240}
	assertStraddlesCombGate(t, periods, lengths)
	for _, T := range periods {
		for _, N := range lengths {
			buflen := T + 2 + N
			allEdge := make([]int32, buflen)
			alt := make([]int32, buflen)
			for i := range allEdge {
				allEdge[i] = edges[i%len(edges)]
				if i%2 == 0 {
					alt[i] = math.MinInt32
				} else {
					alt[i] = math.MaxInt32
				}
			}
			xb := T + 2
			for _, g := range combGainTriples {
				runCombSep(t, allEdge, xb, T, N, g[0], g[1], g[2])
				runCombInPlace(t, allEdge, xb, T, N, g[0], g[1], g[2])
				runCombSep(t, alt, xb, T, N, g[0], g[1], g[2])
				runCombInPlace(t, alt, xb, T, N, g[0], g[1], g[2])
			}
		}
	}
}

// TestCombFilterConstNotVacuous is the negative control. It proves the
// differential tests are not diffing two no-ops: for a nonzero gain the kernel
// must change the buffer, and the in-place recurrence must produce a result
// distinct from the separate-buffer (non-recursive) evaluation, so a SIMD path
// that ignored the recurrence (or the aliasing) would be caught rather than pass.
// (T=120, N=480) routes to the vector kernel and spans several blocks, so this
// controls the SIMD path itself, not the scalar fallback; the opening assertion
// pins that routing so a future minCombBlock change cannot silently demote it.
func TestCombFilterConstNotVacuous(t *testing.T) {
	const T, N = 120, 480
	if T-2 < minCombBlock || N < minCombBlock {
		t.Fatalf("NotVacuous must exercise the vector path: T=%d N=%d routes to the scalar fallback (need T-2 >= %d and N >= %d)",
			T, N, minCombBlock, minCombBlock)
	}
	gen := combValues(999)
	buf, xb := combBuf(gen, T, N)
	g10, g11, g12 := int16(26208), int16(3280), int16(0)

	sep := make([]int32, xb+N)
	combFilterConst(sep, xb, buf, xb, T, N, g10, g11, g12)
	if slices.Equal(sep[xb:], buf[xb:]) {
		t.Fatalf("combFilterConst was a no-op on nonzero input")
	}

	inplace := slices.Clone(buf)
	combFilterConst(inplace, xb, inplace, xb, T, N, g10, g11, g12)
	if slices.Equal(sep[xb:xb+N], inplace[xb:xb+N]) {
		t.Fatalf("in-place and separate-buffer output identical: recurrence not exercised, N=%d T=%d", N, T)
	}
}

// TestCombFilterConstZeroAlloc asserts the SIMD path allocates nothing (the
// stack-scratch design): a representative in-place call above the crossover.
func TestCombFilterConstZeroAlloc(t *testing.T) {
	const T, N = 120, 480
	gen := combValues(7)
	buf, xb := combBuf(gen, T, N)
	work := slices.Clone(buf)
	g10, g11, g12 := int16(10048), int16(7112), int16(4248)
	if n := testing.AllocsPerRun(50, func() {
		copy(work, buf)
		combFilterConst(work, xb, work, xb, T, N, g10, g11, g12)
	}); n != 0 {
		t.Fatalf("combFilterConst allocated %g times/op, want 0", n)
	}
}

func FuzzCombFilterConst(f *testing.F) {
	// Seeds carry RAW T and N that the body remaps: T -> combfilterMinperiod +
	// (T mod span), so raw 0 lands on the clamp minimum T=15, raw 19 on the SIMD
	// gate T=34, raw 105 on T=120, raw 497 on T=512 and raw 1007 on the clamped
	// maximum combMaxT=1022. N is remapped the same way (never skipped), so the
	// second field of each seed is the raw N and 0/840/240/960/120 pass through.
	f.Add(0, 0, int16(26208), int16(3280), int16(0), int64(1))
	f.Add(19, 840, int16(15200), int16(8784), int16(0), int64(3))
	f.Add(105, 240, int16(10048), int16(7112), int16(4248), int64(7))
	f.Add(497, 960, int16(math.MaxInt16), int16(math.MinInt16), int16(1), int64(42))
	f.Add(1007, 120, int16(0), int16(0), int16(0), int64(99))
	f.Fuzz(func(t *testing.T, T, N int, g10, g11, g12 int16, seed int64) {
		// Map T and N into their call domains rather than discarding out-of-range
		// inputs: T into the clamped [combfilterMinperiod, combMaxT], N into
		// [0, combFuzzMaxN]. The interesting behaviour is the arithmetic and the
		// block recurrence, not unbounded size.
		const combFuzzMaxN = 4096
		span := combMaxT - combfilterMinperiod + 1
		T = combfilterMinperiod + ((T%span)+span)%span
		N = ((N % (combFuzzMaxN + 1)) + (combFuzzMaxN + 1)) % (combFuzzMaxN + 1)
		// Fuzz both regimes: full int32 range for the SATURATE corners, and in band
		// (>> 6) so a divergence is not collapsed onto a shared clamp value. The low
		// bit of seed picks which, so both are covered across the corpus.
		var shift uint
		if seed&1 == 0 {
			shift = 6
		}
		r := rand.New(rand.NewSource(seed))
		buf, xb := combBuf(func(int) int32 { return int32(uint32(r.Uint64())) >> shift }, T, N)
		runCombSep(t, buf, xb, T, N, g10, g11, g12)
		runCombInPlace(t, buf, xb, T, N, g10, g11, g12)
	})
}

func BenchmarkCombFilterConst(b *testing.B) {
	// (T, N) grid over the real decode/prefilter shapes. combFilterConst gets
	// N = frame - shortMdctSize with overlap == 0: N=120 (5 ms), 360 (10 ms) and
	// 840 (20 ms), at pitch period T (commonly 60-512, up to the clamped max
	// combMaxT=1022). The decoder makes two calls per channel per frame, one at
	// N=120 (steady state, postfilter unchanged) and one at N=840, so both N are
	// measured on the vector path, not only the long one.
	//
	// Both arches must be measured: a NEON win can be an AVX2 regression. A cfg
	// below the gate (block width T-2 < minCombBlock, or N < minCombBlock) takes
	// the scalar fallback plus a dispatch branch through combFilterConst, so its
	// second row is labelled impl=dispatch_fallback rather than impl=simd.
	type cfg struct{ T, N int }
	cfgs := []cfg{
		{15, 120}, {16, 360}, {17, 360}, {24, 360}, {32, 360}, {48, 360},
		{64, 360}, {120, 360},
		{64, 840}, {120, 840}, {256, 840}, {512, 840}, {1022, 840},
		// The band [34, 47] that minCombBlock=32 opens but the rows above skip.
		// T=40 is the worst shape in it: a block width of 38 leaves 6 outputs on
		// the kernel's own scalar tail on AVX2.
		{34, 840}, {40, 840},
		// The shorter of the two real decoder shapes on the vector path.
		{64, 120}, {120, 120},
	}
	g10, g11, g12 := int16(10048), int16(7112), int16(4248)
	for _, c := range cfgs {
		gen := combValues(uint32(c.T))
		buf, xb := combBuf(gen, c.T, c.N)
		// Separate read-only x and a distinct y: the block-by-(T-2) work is
		// identical to the in-place path, so this measures the pure kernel cost
		// without a per-iteration input restore muddying the scalar/simd ratio.
		y := make([]int32, xb+c.N)
		simdLabel := "impl=simd"
		if c.T-2 < minCombBlock || c.N < minCombBlock {
			simdLabel = "impl=dispatch_fallback"
		}
		b.Run(fmt.Sprintf("T%d_N%d/impl=scalar", c.T, c.N), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				combFilterConstGeneric(y, xb, buf, xb, c.T, c.N, g10, g11, g12)
			}
		})
		b.Run(fmt.Sprintf("T%d_N%d/%s", c.T, c.N, simdLabel), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				combFilterConst(y, xb, buf, xb, c.T, c.N, g10, g11, g12)
			}
		})
	}
}
