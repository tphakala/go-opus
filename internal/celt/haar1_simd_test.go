package celt

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// haar1TouchLen is the number of X elements haar1 reads and writes for a given
// (N0, stride): m combs, each 2*stride wide, m = N0>>1.
func haar1TouchLen(N0, stride int) int {
	m := N0 >> 1
	if stride <= 0 || m <= 0 {
		return 0
	}
	return m * stride * 2
}

// runHaar1Pair runs the scalar reference and the library-backed haar1 on two
// copies of the same buffer and reports the first divergence. The buffer carries
// slack past the touched region so a stray write past m*stride*2 is caught too:
// slices.Equal compares the whole buffer, slack included.
func runHaar1Pair(t *testing.T, src []int32, N0, stride int) {
	t.Helper()
	a := append([]int32(nil), src...)
	b := append([]int32(nil), src...)
	haar1Generic(a, N0, stride)
	haar1(b, N0, stride)
	if !slices.Equal(a, b) {
		t.Fatalf("haar1 mismatch N0=%d stride=%d len=%d:\n scalar=%v\n simd  =%v", N0, stride, len(src), a, b)
	}
}

func TestHaar1SIMDMatchesScalar(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	strides := []int{-1, 0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 30}
	// Both sides of the butterfly dispatch boundary must stay in the matrix, or a
	// future threshold change would silently leave one branch untested.
	if !slices.Contains(strides, minHaar1ButterflyStride) || !slices.Contains(strides, minHaar1ButterflyStride-1) {
		t.Fatalf("stride matrix must straddle minHaar1ButterflyStride=%d", minHaar1ButterflyStride)
	}
	n0s := []int{0, 1, 2, 3, 4, 5, 6, 8, 10, 12, 16, 30, 32, 64, 120, 176, 240}
	for _, stride := range strides {
		for _, N0 := range n0s {
			total := haar1TouchLen(N0, stride) + 8 // + slack to catch overwrites
			for trial := 0; trial < 4; trial++ {
				runHaar1Pair(t, fillRandI32(r, total), N0, stride)
			}
		}
	}
}

// TestHaar1SIMDExtremes exercises the int32 wrapping edges: the scale product and
// the add/sub can both wrap, and MinInt32 has no positive counterpart.
func TestHaar1SIMDExtremes(t *testing.T) {
	edges := []int32{math.MinInt32, math.MinInt32 + 1, -1, 0, 1, math.MaxInt32 - 1, math.MaxInt32}
	cases := []struct{ N0, stride int }{
		{2, 1}, {4, 1}, {4, 2}, {8, 2}, {16, 4}, {64, 8}, {32, 16},
	}
	for _, c := range cases {
		n := haar1TouchLen(c.N0, c.stride) + 4
		// Every-edge buffer.
		runHaar1Pair(t, edgeFill(edges, n), c.N0, c.stride)
		// Alternating min/max to stress sum/difference overflow.
		runHaar1Pair(t, altMinMax(n), c.N0, c.stride)
	}
}

func FuzzHaar1(f *testing.F) {
	f.Add(4, 2, int64(1))
	f.Add(2, 1, int64(7))
	f.Add(176, 8, int64(42))
	f.Fuzz(func(t *testing.T, N0, stride int, seed int64) {
		// Keep the allocation bounded; the interesting behaviour is in the
		// arithmetic, not in huge buffers.
		if stride < -4 || stride > 128 || N0 < -4 || N0 > 1024 {
			t.Skip()
		}
		total := haar1TouchLen(N0, stride) + 4
		r := rand.New(rand.NewSource(seed))
		runHaar1Pair(t, fillRandI32(r, total), N0, stride)
	})
}

func BenchmarkHaar1(b *testing.B) {
	// Representative (N0, stride) pairs from quant_all_bands haar1(X, N>>k, 1<<k)
	// and the TF analysis: stride grows and comb count shrinks as k rises.
	cases := []struct{ N0, stride int }{
		{960, 1}, {480, 2}, {240, 4}, {120, 8}, {64, 16}, {32, 32},
		{176, 1}, {176, 2}, {88, 4},
		// touch = 100 is not a multiple of the 8-wide SIMD width, so this shape
		// times the ScaleQ31 scalar remainder that the divisible-by-8 shapes skip.
		{100, 1},
	}
	for _, c := range cases {
		n := haar1TouchLen(c.N0, c.stride)
		if n == 0 {
			continue
		}
		seed := seqFillI32(n)
		b.Run(fmt.Sprintf("N%d_s%d/impl=scalar", c.N0, c.stride), func(b *testing.B) {
			buf := append([]int32(nil), seed...)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				haar1Generic(buf, c.N0, c.stride)
			}
		})
		b.Run(fmt.Sprintf("N%d_s%d/impl=simd", c.N0, c.stride), func(b *testing.B) {
			buf := append([]int32(nil), seed...)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				haar1(buf, c.N0, c.stride)
			}
		})
	}
}
