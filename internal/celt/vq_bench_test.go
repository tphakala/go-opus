package celt

import (
	"fmt"
	"testing"
)

// vqBenchLCG fills dst with deterministic pseudo-random norm-domain samples. The
// values only need to exercise the pulse search and rotation arithmetic, not be
// meaningful audio; the same seed feeds both sides of any A/B comparison.
func vqBenchLCG(dst []int32, seed uint32) {
	s := seed
	for i := range dst {
		s = s*1664525 + 1013904223
		// int16-range magnitude shifted up into the celt_norm (int32) domain.
		dst[i] = int32(int16(s>>17)) << 8
	}
}

// BenchmarkOpPvqSearch pins the greedy PVQ pulse search (opPvqSearch, the top
// scalar flat consumer of the encode path) over the (N,K) shapes the 48 kHz
// encoder drives it at. Most shapes have K <= N>>1 and exercise the greedy loop;
// the final N16/K12 shape has K > N>>1 and additionally exercises the pre-search
// projection branch. opPvqSearch mutates X in place, so a fresh copy of the
// seeded source is made inside the loop; that copy is identical on both sides of
// an A/B run.
func BenchmarkOpPvqSearch(b *testing.B) {
	shapes := []struct{ N, K int }{
		{8, 4}, {16, 4}, {24, 8}, {32, 10}, {88, 4}, {176, 8},
		{16, 12}, // K > N>>1: exercises the pre-search projection branch
	}
	for _, sh := range shapes {
		src := make([]int32, sh.N)
		vqBenchLCG(src, 0x9E3779B9^uint32(sh.N<<8|sh.K))
		X := make([]int32, sh.N)
		iy := make([]int32, sh.N)
		sc := &scratch{}
		b.Run(fmt.Sprintf("N%d/K%d", sh.N, sh.K), func(b *testing.B) {
			// Pre-grow the pooled scratch (pvqY/pvqSignx/pvqIy) outside the timed
			// loop so its one-time first-call allocation does not show up in B/op.
			copy(X, src)
			opPvqSearch(X, iy, sh.K, sh.N, sc)
			b.ReportAllocs()
			var sink int16
			for b.Loop() {
				copy(X, src)
				sink = opPvqSearch(X, iy, sh.K, sh.N, sc)
			}
			_ = sink
		})
	}
}

// BenchmarkExpRotation pins the spread rotation (expRotation, whose expRotation1
// inner passes are the second scalar flat consumer of the encode path) in both
// the forward (dir<0) and inverse (dir>0) directions. expRotation mutates X, so
// it runs on a fresh copy of the seeded source each iteration.
//
// The shapes cover both dispatch paths that reach the specialized stride-1 pass:
//   - Single-pass (length < 8*stride, so stride2 == 0): only expRotation1Stride1
//     runs, isolating the kernel this benchmark exists to measure. The B4 shapes
//     land here; N8/B4 gives a per-block length of 2, which exercises the
//     backward-skip / boundary-flush tail of expRotation1Stride1.
//   - Two-pass (length >= 8*stride): the wide stride2 expRotation1 pass runs
//     first, then expRotation1Stride1.
func BenchmarkExpRotation(b *testing.B) {
	shapes := []struct {
		N, B, K, spread, dir int
	}{
		{8, 4, 2, spreadNormal, -1},   // single-pass, per-block length 2 (tail)
		{16, 4, 2, spreadNormal, -1},  // single-pass, per-block length 4
		{16, 1, 2, spreadNormal, -1},  // two-pass
		{24, 1, 3, spreadNormal, -1},  // two-pass
		{48, 1, 6, spreadNormal, -1},  // two-pass
		{88, 2, 10, spreadNormal, -1}, // two-pass
		{176, 1, 20, spreadNormal, -1},
		{48, 1, 6, spreadNormal, 1}, // two-pass, inverse direction
	}
	for _, sh := range shapes {
		src := make([]int32, sh.N)
		vqBenchLCG(src, 0x85EBCA6B^uint32(sh.N<<8|sh.B))
		X := make([]int32, sh.N)
		dir := "fwd"
		if sh.dir > 0 {
			dir = "inv"
		}
		b.Run(fmt.Sprintf("N%d/B%d/%s", sh.N, sh.B, dir), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				copy(X, src)
				expRotation(X, sh.N, sh.dir, sh.B, sh.K, sh.spread)
			}
		})
	}
}
