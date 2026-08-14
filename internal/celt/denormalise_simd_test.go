package celt

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// runDenormaliseGainPair runs the scalar reference and the library-backed
// denormaliseGain on the same src into two fresh dst buffers and reports the
// first divergence. dst is pre-filled with a sentinel and may be longer than
// src: n = min(len(dst), len(src)) elements are written and the sentinel tail
// must survive on both paths, so slices.Equal over the whole buffer catches a
// stray write past n as well as any value mismatch.
func runDenormaliseGainPair(t *testing.T, src []int32, dstLen int, g int32, preShift, postShift int) {
	t.Helper()
	const sentinel = int32(0x0BADF00D)
	a := make([]int32, dstLen)
	b := make([]int32, dstLen)
	for i := range a {
		a[i] = sentinel
		b[i] = sentinel
	}
	denormaliseGainGeneric(a, src, g, preShift, postShift)
	denormaliseGain(b, src, g, preShift, postShift)
	if !slices.Equal(a, b) {
		t.Fatalf("denormaliseGain mismatch len(src)=%d dstLen=%d g=%d preShift=%d postShift=%d:\n scalar=%v\n simd  =%v",
			len(src), dstLen, g, preShift, postShift, a, b)
	}
}

// runDenormaliseGainInPlace exercises the exact-alias path GainQ31 permits and
// denormaliseBands can hit when a caller passes the same buffer for freq and X:
// dst and src are one slice, requanted in place, diffed against the reference
// computed into a separate buffer.
func runDenormaliseGainInPlace(t *testing.T, src []int32, g int32, preShift, postShift int) {
	t.Helper()
	want := make([]int32, len(src))
	denormaliseGainGeneric(want, src, g, preShift, postShift)
	got := append([]int32(nil), src...)
	denormaliseGain(got, got, g, preShift, postShift)
	if !slices.Equal(want, got) {
		t.Fatalf("denormaliseGain in-place mismatch len=%d g=%d preShift=%d postShift=%d:\n want=%v\n got =%v",
			len(src), g, preShift, postShift, want, got)
	}
}

// gains covers the Q31 gain edges denormaliseBands actually produces (0 in the
// shift>=31 branch, q31one = MaxInt32 in the shift<0 branch) plus signed wrapping
// edges so the truncating Q31 multiply is exercised across its range.
var denormGains = []int32{0, 1, -1, 2, 255, 32767, 1 << 20, math.MaxInt32, math.MinInt32, math.MinInt32 + 1}

// shifts stays inside GainQ31's [0,31] domain and includes the production
// constant preShift (6), the postShift ceiling denormaliseBands can reach (30),
// the no-op 0 and the domain boundary 31.
var denormShifts = []int{0, 1, 6, 14, 30, 31}

func TestDenormaliseGainMatchesScalar(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	lengths := []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 48, 96, 120, 176, 240}
	for _, n := range lengths {
		for _, g := range denormGains {
			for _, preShift := range denormShifts {
				for _, postShift := range denormShifts {
					src := fillRandI32(r, n)
					// dstLen == n, then n+8 slack to prove the untouched tail.
					runDenormaliseGainPair(t, src, n, g, preShift, postShift)
					runDenormaliseGainPair(t, src, n+8, g, preShift, postShift)
				}
			}
		}
	}
}

// TestDenormaliseGainExtremes drives the int32 wrapping edges through the whole
// (g, preShift, postShift) grid: the pre-shift, the Q31 product and the rounding
// requant can each wrap, and MinInt32 has no positive counterpart.
func TestDenormaliseGainExtremes(t *testing.T) {
	edges := []int32{math.MinInt32, math.MinInt32 + 1, -1, 0, 1, 255, math.MaxInt32 - 1, math.MaxInt32}
	lengths := []int{1, 2, 8, 16, 17}
	for _, n := range lengths {
		// Every-edge buffer.
		allEdge := make([]int32, n)
		for i := range allEdge {
			allEdge[i] = edges[i%len(edges)]
		}
		// Alternating min/max to stress the product and rounding wraps.
		alt := make([]int32, n)
		for i := range alt {
			if i%2 == 0 {
				alt[i] = math.MinInt32
			} else {
				alt[i] = math.MaxInt32
			}
		}
		for _, g := range denormGains {
			for _, preShift := range denormShifts {
				for _, postShift := range denormShifts {
					runDenormaliseGainPair(t, allEdge, n, g, preShift, postShift)
					runDenormaliseGainPair(t, alt, n, g, preShift, postShift)
					runDenormaliseGainInPlace(t, allEdge, g, preShift, postShift)
				}
			}
		}
	}
}

// TestDenormaliseGainInPlace checks the exact-alias path on random input across
// the production-representative shift pairs.
func TestDenormaliseGainInPlace(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, n := range []int{1, 3, 8, 16, 33, 96, 240} {
		for _, g := range denormGains {
			runDenormaliseGainInPlace(t, fillRandI32(r, n), g, 6, 14)
		}
	}
}

// TestDenormaliseGainNotVacuous is the negative control: it proves the
// differential tests above are not comparing two no-ops. For a nonzero gain and
// nonzero input the kernel must actually change the buffer, and postShift must
// actually matter (a wrong requant shift must produce a different result), so a
// SIMD path that silently ignored either would be caught rather than pass.
func TestDenormaliseGainNotVacuous(t *testing.T) {
	src := []int32{1 << 24, -(1 << 24), 1 << 20, math.MaxInt32, math.MinInt32, 7, -7, 1 << 28}
	// Real work: gain q31one with preShift 6, postShift 0 is not the identity on
	// this input (each sample is left-shifted by 6, then scaled by the Q31 gain).
	out := make([]int32, len(src))
	denormaliseGain(out, src, math.MaxInt32, 6, 0)
	if slices.Equal(out, src) {
		t.Fatalf("denormaliseGain was a no-op on nonzero input: %v", out)
	}
	// postShift matters: two different postShifts must diverge on this input.
	a := make([]int32, len(src))
	b := make([]int32, len(src))
	denormaliseGain(a, src, math.MaxInt32, 6, 6)
	denormaliseGain(b, src, math.MaxInt32, 6, 14)
	if slices.Equal(a, b) {
		t.Fatalf("denormaliseGain ignored postShift: 6 and 14 gave identical output %v", a)
	}
	// preShift matters too, so the negative control is symmetric: a kernel that
	// silently dropped preShift would still pass the postShift check above.
	c := make([]int32, len(src))
	d := make([]int32, len(src))
	denormaliseGain(c, src, math.MaxInt32, 0, 6)
	denormaliseGain(d, src, math.MaxInt32, 6, 6)
	if slices.Equal(c, d) {
		t.Fatalf("denormaliseGain ignored preShift: 0 and 6 gave identical output %v", c)
	}
}

func FuzzDenormaliseGain(f *testing.F) {
	f.Add(8, int32(1<<20), 6, 14, int64(1))
	f.Add(1, int32(math.MaxInt32), 6, 0, int64(7))
	f.Add(176, int32(0), 6, 30, int64(42))
	f.Add(17, int32(math.MinInt32), 31, 31, int64(99))
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
		runDenormaliseGainPair(t, src, n, g, preShift, postShift)
		if n > 0 {
			runDenormaliseGainPair(t, src, n+4, g, preShift, postShift)
			runDenormaliseGainInPlace(t, src, g, preShift, postShift)
		}
	})
}

func BenchmarkDenormaliseGain(b *testing.B) {
	// Representative per-band run lengths from denormaliseBands over a 48 kHz
	// 960-sample frame: narrow low bands up to the wide fullband tail.
	lengths := []int{4, 8, 16, 24, 48, 96, 176, 240}
	for _, n := range lengths {
		seed := make([]int32, n)
		for i := range seed {
			seed[i] = int32(i*2654435761 + 1)
		}
		const g = int32(1 << 24)
		b.Run(fmt.Sprintf("scalar/n%d", n), func(b *testing.B) {
			dst := make([]int32, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				denormaliseGainGeneric(dst, seed, g, 6, 14)
			}
		})
		b.Run(fmt.Sprintf("simd/n%d", n), func(b *testing.B) {
			dst := make([]int32, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				denormaliseGain(dst, seed, g, 6, 14)
			}
		})
	}
}
