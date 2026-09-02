package celt

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// runBandGainRequantPair runs the scalar reference and the library-backed
// bandGainRequant on the same src into two fresh dst buffers and reports the
// first divergence. dst is pre-filled with a sentinel and may be longer than
// src: n = min(len(dst), len(src)) elements are written and the sentinel tail
// must survive on both paths, so slices.Equal over the whole buffer catches a
// stray write past n as well as any value mismatch.
func runBandGainRequantPair(t *testing.T, src []int32, dstLen int, g int32, preShift, postShift int) {
	t.Helper()
	a := sentinelBuf(dstLen)
	b := sentinelBuf(dstLen)
	bandGainRequantGeneric(a, src, g, preShift, postShift)
	bandGainRequant(b, src, g, preShift, postShift)
	if !slices.Equal(a, b) {
		t.Fatalf("bandGainRequant mismatch len(src)=%d dstLen=%d g=%d preShift=%d postShift=%d:\n scalar=%v\n simd  =%v",
			len(src), dstLen, g, preShift, postShift, a, b)
	}
}

// runBandGainRequantInPlace exercises the exact-alias path GainQ31 permits: dst
// and src are one slice, requanted in place, diffed against the reference
// computed into a separate buffer. Real callers pass OPUS_RESTRICT freq/X so
// they never alias, but the library allows the exact overlap and this proves it.
func runBandGainRequantInPlace(t *testing.T, src []int32, g int32, preShift, postShift int) {
	t.Helper()
	want := make([]int32, len(src))
	bandGainRequantGeneric(want, src, g, preShift, postShift)
	got := append([]int32(nil), src...)
	bandGainRequant(got, got, g, preShift, postShift)
	if !slices.Equal(want, got) {
		t.Fatalf("bandGainRequant in-place mismatch len=%d g=%d preShift=%d postShift=%d:\n want=%v\n got =%v",
			len(src), g, preShift, postShift, want, got)
	}
}

// bandGainGains covers the Q31 gain edges the two callers actually produce:
// denormaliseBands emits 0 (shift>=31 branch) and q31one = MaxInt32 (shift<0
// branch); normaliseBands emits Celt_rcp_norm32(E), a positive normalized
// reciprocal. The signed wrapping edges (MinInt32, -1, ...) exercise the
// truncating Q31 multiply across its whole range.
var bandGainGains = []int32{0, 1, -1, 2, 255, 32767, 1 << 20, math.MaxInt32, math.MinInt32, math.MinInt32 + 1}

// bandGainShifts stays inside GainQ31's [0,31] domain and includes the
// production constant 6 (30-normShift, the pre-shift for decode and the
// post-shift for encode), the per-band ceiling 30 that celt_zlog2 can drive
// either shift to, the no-op 0 and the domain boundary 31.
var bandGainShifts = []int{0, 1, 6, 14, 30, 31}

func TestBandGainRequantMatchesScalar(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	lengths := []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 48, 96, 120, 176, 240}
	for _, n := range lengths {
		for _, g := range bandGainGains {
			for _, preShift := range bandGainShifts {
				for _, postShift := range bandGainShifts {
					src := fillRandI32(r, n)
					// dstLen == n, then n+8 slack to prove the untouched tail,
					// then n-1 (a short dst) to prove the min(len(dst),len(src))
					// write bound: only dstLen elements are written and src past
					// dstLen is not read into dst.
					runBandGainRequantPair(t, src, n, g, preShift, postShift)
					runBandGainRequantPair(t, src, n+8, g, preShift, postShift)
					if n > 1 {
						runBandGainRequantPair(t, src, n-1, g, preShift, postShift)
					}
				}
			}
		}
	}
}

// TestBandGainRequantProductionShifts sweeps the exact per-direction shift
// families the codec reaches: the decoder pins preShift=6 and varies postShift
// over [0,30] (= shift), the encoder pins postShift=6 and varies preShift over
// [0,30] (= 30-celt_zlog2(E)). It adds a gain shaped like a Celt_rcp_norm32
// output (a positive high-bit Q31 reciprocal), which the wrapping-edge
// bandGainGains set does not otherwise represent, since the encoder's gain is
// always such a value.
func TestBandGainRequantProductionShifts(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	gains := append(append([]int32(nil), bandGainGains...), 0x60000000, 0x3B000000)
	lengths := []int{1, 8, 16, 96, 240}
	for _, n := range lengths {
		for _, g := range gains {
			src := fillRandI32(r, n)
			for s := 0; s <= 30; s++ {
				// Encoder shape: variable preShift, constant postShift 6.
				runBandGainRequantPair(t, src, n, g, s, 6)
				// Decoder shape: constant preShift 6, variable postShift.
				runBandGainRequantPair(t, src, n, g, 6, s)
			}
		}
	}
}

// TestBandGainRequantExtremes drives the int32 wrapping edges through the whole
// (g, preShift, postShift) grid: the pre-shift, the Q31 product and the rounding
// requant can each wrap, and MinInt32 has no positive counterpart.
func TestBandGainRequantExtremes(t *testing.T) {
	edges := []int32{math.MinInt32, math.MinInt32 + 1, -1, 0, 1, 255, math.MaxInt32 - 1, math.MaxInt32}
	lengths := []int{1, 2, 8, 16, 17}
	for _, n := range lengths {
		// Every-edge buffer.
		allEdge := edgeFill(edges, n)
		// Alternating min/max to stress the product and rounding wraps.
		alt := altMinMax(n)
		for _, g := range bandGainGains {
			for _, preShift := range bandGainShifts {
				for _, postShift := range bandGainShifts {
					runBandGainRequantPair(t, allEdge, n, g, preShift, postShift)
					runBandGainRequantPair(t, alt, n, g, preShift, postShift)
					runBandGainRequantInPlace(t, allEdge, g, preShift, postShift)
				}
			}
		}
	}
}

// TestBandGainRequantInPlace checks the exact-alias path on random input across
// the production-representative shift pairs for both directions: (6,14) is the
// decoder shape (const preShift, variable postShift), (14,6) the encoder shape.
func TestBandGainRequantInPlace(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, n := range []int{1, 3, 8, 16, 33, 96, 240} {
		for _, g := range bandGainGains {
			for _, sp := range [][2]int{{6, 14}, {14, 6}} {
				runBandGainRequantInPlace(t, fillRandI32(r, n), g, sp[0], sp[1])
			}
		}
	}
}

// TestBandGainRequantNotVacuous is the negative control: it proves the
// differential tests above are not comparing two no-ops. For a nonzero gain and
// nonzero input the kernel must actually change the buffer, and both shifts must
// actually matter (a wrong requant shift must produce a different result), so a
// SIMD path that silently ignored either would be caught rather than pass.
func TestBandGainRequantNotVacuous(t *testing.T) {
	src := []int32{1 << 24, -(1 << 24), 1 << 20, math.MaxInt32, math.MinInt32, 7, -7, 1 << 28}
	// Real work: gain q31one with preShift 6, postShift 0 is not the identity on
	// this input (each sample is left-shifted by 6, then scaled by the Q31 gain).
	out := make([]int32, len(src))
	bandGainRequant(out, src, math.MaxInt32, 6, 0)
	if slices.Equal(out, src) {
		t.Fatalf("bandGainRequant was a no-op on nonzero input: %v", out)
	}
	// postShift matters: two different postShifts must diverge on this input.
	a := make([]int32, len(src))
	b := make([]int32, len(src))
	bandGainRequant(a, src, math.MaxInt32, 6, 6)
	bandGainRequant(b, src, math.MaxInt32, 6, 14)
	if slices.Equal(a, b) {
		t.Fatalf("bandGainRequant ignored postShift: 6 and 14 gave identical output %v", a)
	}
	// preShift matters too, so the negative control is symmetric: a kernel that
	// silently dropped preShift would still pass the postShift check above.
	c := make([]int32, len(src))
	d := make([]int32, len(src))
	bandGainRequant(c, src, math.MaxInt32, 0, 6)
	bandGainRequant(d, src, math.MaxInt32, 6, 6)
	if slices.Equal(c, d) {
		t.Fatalf("bandGainRequant ignored preShift: 0 and 6 gave identical output %v", c)
	}
}

func FuzzBandGainRequant(f *testing.F) {
	f.Add(8, int32(1<<20), 6, 14, int64(1))
	f.Add(1, int32(math.MaxInt32), 6, 0, int64(7))
	f.Add(176, int32(0), 6, 30, int64(42))
	f.Add(17, int32(math.MinInt32), 31, 31, int64(99))
	// Encoder-shaped seeds: variable preShift, constant postShift 6, positive
	// reciprocal-shaped gains.
	f.Add(96, int32(0x3B000000), 14, 6, int64(5))
	f.Add(240, int32(math.MaxInt32), 30, 6, int64(11))
	f.Fuzz(func(t *testing.T, n int, g int32, preShiftRaw, postShiftRaw int, seed int64) {
		// Bound the buffer; the interesting behaviour is arithmetic, not size.
		if n < 0 || n > 4096 {
			t.Skip()
		}
		// Map the shifts into GainQ31's [0,31] domain (outside it GainQ31 panics
		// by contract, which is not what this differential test is checking).
		preShift := ((preShiftRaw % 32) + 32) % 32
		postShift := ((postShiftRaw % 32) + 32) % 32
		r := rand.New(rand.NewSource(seed))
		src := fillRandI32(r, n)
		runBandGainRequantPair(t, src, n, g, preShift, postShift)
		if n > 0 {
			runBandGainRequantPair(t, src, n+4, g, preShift, postShift)
			runBandGainRequantInPlace(t, src, g, preShift, postShift)
		}
	})
}

func BenchmarkBandGainRequant(b *testing.B) {
	// Representative per-band run lengths from normaliseBands / denormaliseBands
	// over a 48 kHz 960-sample frame: narrow low bands up to the wide fullband
	// tail. The shift counts are runtime operands to the same vector
	// instructions, so the encoder-shaped (14,6) pair times identically to the
	// decoder-shaped (6,14) benchmarked here; it is not benchmarked separately.
	lengths := []int{4, 8, 16, 24, 48, 96, 176, 240}
	for _, n := range lengths {
		seed := seqFillI32(n)
		const g = int32(1 << 24)
		b.Run(fmt.Sprintf("n%d/impl=scalar", n), func(b *testing.B) {
			dst := make([]int32, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bandGainRequantGeneric(dst, seed, g, 6, 14)
			}
		})
		b.Run(fmt.Sprintf("n%d/impl=simd", n), func(b *testing.B) {
			dst := make([]int32, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bandGainRequant(dst, seed, g, 6, 14)
			}
		})
	}
}
