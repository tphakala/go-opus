package celt

import "math/rand"

// Shared int32 fixtures for the SIMD differential suites, so the sentinel fill,
// the edge-cycling buffer, the alternating min/max buffer and the random fills are
// defined once rather than re-spelled across the comb, haar1 and band-gain suites
// (issue #72). The pitch suite is int16-based and keeps its own fixtures in
// pitch_simd_test.go.

// simdSentinel marks the untouched slack of an output buffer. A kernel that writes
// past its logical output length, or into the sentinel tail, changes one of these
// and is caught by the whole-buffer slices.Equal the differential runners use.
const simdSentinel = int32(0x0BADF00D)

// sentinelBuf returns an n-element buffer filled with simdSentinel.
func sentinelBuf(n int) []int32 {
	b := make([]int32, n)
	for i := range b {
		b[i] = simdSentinel
	}
	return b
}

// edgeFill returns an n-element buffer that cycles through edges, the every-edge
// input the Extremes tests drive so the pair-adds overflow, the Q-format
// truncations hit their corners and the saturate/round boundaries are exercised.
func edgeFill(edges []int32, n int) []int32 {
	b := make([]int32, n)
	for i := range b {
		b[i] = edges[i%len(edges)]
	}
	return b
}

// altMinMax returns an n-element buffer alternating math.MinInt32 and
// math.MaxInt32, the pattern the Extremes tests use to stress sum/difference and
// product overflow at adjacent samples.
func altMinMax(n int) []int32 {
	b := make([]int32, n)
	for i := range b {
		if i%2 == 0 {
			b[i] = -1 << 31 // math.MinInt32
		} else {
			b[i] = 1<<31 - 1 // math.MaxInt32
		}
	}
	return b
}

// seqFillI32 returns the deterministic Knuth-multiplicative sequence the SIMD
// benchmarks seed their buffers with, so a benchmark's input is reproducible and
// independent of the differential fixtures.
func seqFillI32(n int) []int32 {
	b := make([]int32, n)
	for i := range b {
		b[i] = int32(uint32(i)*2654435761 + 1)
	}
	return b
}

// fillRandI32 returns n pseudo-random int32 drawn from r, the source of random
// buffers for the haar1 and band-gain differential sweeps.
func fillRandI32(r *rand.Rand, n int) []int32 {
	x := make([]int32, n)
	for i := range x {
		x[i] = int32(r.Uint32())
	}
	return x
}

// randI32 returns n pseudo-random int32 from a fresh source seeded with seed, for
// callers that want a self-contained buffer without threading a *rand.Rand.
func randI32(seed int64, n int) []int32 {
	return fillRandI32(rand.New(rand.NewSource(seed)), n)
}
