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
		return int32(u) //nolint:gosec // deliberate wrap over the full int32 range
	}
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
// mismatch.
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
	combFilterConstGeneric(a, xb, buf, xb, T, N, g10, g11, g12)
	combFilterConst(b, xb, buf, xb, T, N, g10, g11, g12)
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

// combTestPeriods spans the min pitch (15), block-boundary and prime-ish widths,
// and large periods where a block covers many vector registers, up to the
// clamped max (combfilterMaxperiod-2 = 1022).
var combTestPeriods = []int{15, 16, 17, 31, 32, 33, 63, 120, 255, 512, 1022}

// combTestLengths span below, at and above minCombBlock, non-multiples of the
// block width, and full frame sizes that cross several blocks.
var combTestLengths = []int{0, 1, 2, 3, 7, 8, 9, 13, 15, 16, 17, 31, 33, 63, 120, 240, 480, 960}

func TestCombFilterConstMatchesScalar(t *testing.T) {
	for _, T := range combTestPeriods {
		for ni, N := range combTestLengths {
			gen := combValues(uint32(T*131 + ni))
			buf, xb := combBuf(gen, T, N)
			for _, g := range combGainTriples {
				runCombSep(t, buf, xb, T, N, g[0], g[1], g[2])
				runCombInPlace(t, buf, xb, T, N, g[0], g[1], g[2])
			}
		}
	}
}

// TestCombFilterConstExtremes fills the history and input with int32 wrapping
// edges so the pair-adds overflow, the Q15 truncations hit their corners, and
// the SATURATE clamp at +-sigSat and the -1 bias boundary are exercised.
func TestCombFilterConstExtremes(t *testing.T) {
	edges := []int32{math.MinInt32, math.MinInt32 + 1, -1, 0, 1, sigSat - 1, sigSat, sigSat + 1, math.MaxInt32}
	for _, T := range []int{15, 16, 33, 120, 512} {
		for _, N := range []int{1, 2, 8, 16, 17, 33, 240} {
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
func TestCombFilterConstNotVacuous(t *testing.T) {
	const T, N = 15, 64
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
	f.Add(15, 64, int16(26208), int16(3280), int16(0), int64(1))
	f.Add(120, 240, int16(10048), int16(7112), int16(4248), int64(7))
	f.Add(512, 960, int16(math.MaxInt16), int16(math.MinInt16), int16(1), int64(42))
	f.Add(16, 1, int16(0), int16(0), int16(0), int64(99))
	f.Fuzz(func(t *testing.T, T, N int, g10, g11, g12 int16, seed int64) {
		// Map T into the clamped call domain [combfilterMinperiod,
		// combfilterMaxperiod-2] and bound N; the interesting behaviour is the
		// arithmetic and the block recurrence, not unbounded size.
		if N < 0 || N > 4096 {
			t.Skip()
		}
		T = combfilterMinperiod + ((T%1008)+1008)%1008 // [15, 1022]
		r := rand.New(rand.NewSource(seed))
		buf, xb := combBuf(func(int) int32 { return int32(uint32(r.Uint64())) }, T, N)
		runCombSep(t, buf, xb, T, N, g10, g11, g12)
		runCombInPlace(t, buf, xb, T, N, g10, g11, g12)
	})
}

func BenchmarkCombFilterConst(b *testing.B) {
	// Sweep narrow / medium / wide pitch periods against representative frame
	// output lengths. Narrow periods (small block width) are expected to favour
	// the scalar loop; wide periods let a block span many vector registers. Both
	// arches must be measured: a NEON win can be an AVX2 regression.
	// (T, N) grid over the real decode/prefilter shapes: combFilterConst gets
	// N = frame - shortMdctSize with overlap == 0, i.e. 120 (5 ms), 360 (10 ms)
	// and 840 (20 ms), at the pitch period T (common 60-512, up to the clamped
	// max 1022). The narrow-T rows confirm the scalar fallback is chosen, the
	// wide-T long-N rows the SIMD win.
	type cfg struct{ T, N int }
	cfgs := []cfg{
		{15, 120}, {16, 360}, {17, 360}, {24, 360}, {32, 360}, {48, 360},
		{64, 360}, {120, 360},
		{64, 840}, {120, 840}, {256, 840}, {512, 840}, {1022, 840},
	}
	g10, g11, g12 := int16(10048), int16(7112), int16(4248)
	for _, c := range cfgs {
		gen := combValues(uint32(c.T))
		buf, xb := combBuf(gen, c.T, c.N)
		// Separate read-only x and a distinct y: the block-by-(T-2) work is
		// identical to the in-place path, so this measures the pure kernel cost
		// without a per-iteration input restore muddying the scalar/simd ratio.
		y := make([]int32, xb+c.N)
		b.Run(fmt.Sprintf("T%d_N%d/impl=scalar", c.T, c.N), func(b *testing.B) {
			for range b.N {
				combFilterConstGeneric(y, xb, buf, xb, c.T, c.N, g10, g11, g12)
			}
		})
		b.Run(fmt.Sprintf("T%d_N%d/impl=simd", c.T, c.N), func(b *testing.B) {
			for range b.N {
				combFilterConst(y, xb, buf, xb, c.T, c.N, g10, g11, g12)
			}
		})
	}
}
