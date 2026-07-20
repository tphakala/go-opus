package celt

import (
	"math"
	"math/rand"
	"runtime"
	"testing"
)

// spreadingHistNs covers every tail remainder mod 4 plus band-size boundaries and
// the N<=8 corners (spreadingDecision only calls with N>8, but the kernel must be
// correct for any N, and the test drives the kernel directly).
var spreadingHistNs = []int{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18,
	19, 20, 31, 32, 33, 47, 48, 49, 63, 64, 65, 88, 96, 127, 128, 129,
	175, 176, 240, 255, 256, 511, 512, 599, 600,
}

// spreadingHistPatterns returns adversarial length-max(n,1) bands. Each stresses
// a specific bit-exactness hazard of the SIMD kernels: the int16 truncation of
// x[j]>>10 (which SATURATING packs would get wrong, so values whose >>10 exceeds
// int16 range are included), the int16 truncation of (xj*xj)>>15 at its 2^15
// boundary, the signed threshold compares at exactly 128/512/2048, sign handling,
// and a single non-zero at the tail (catches a dropped scalar remainder).
func spreadingHistPatterns(r *rand.Rand, n int) [][]int32 {
	ln := n
	if ln == 0 {
		ln = 1
	}
	mk := func(f func(i int) int32) []int32 {
		s := make([]int32, ln)
		for i := range s {
			s[i] = f(i)
		}
		return s
	}
	pats := [][]int32{
		mk(func(i int) int32 { return 0 }),
		mk(func(i int) int32 { return math.MaxInt32 }), // >>10 overflows int16
		mk(func(i int) int32 { return math.MinInt32 }), // >>10 overflows int16
		mk(func(i int) int32 { return 1 << 25 }),       // >>10 = 2^15, int16 wraps to -2^15
		mk(func(i int) int32 { return -(1 << 25) }),
		mk(func(i int) int32 { return (1 << 25) - 1024 }), // >>10 = 2^15-1 = int16 max
		mk(func(i int) int32 {
			if i%2 == 0 {
				return 1 << 21
			}
			return -(1 << 21)
		}),
		mk(func(i int) int32 { return int32(i%512) << 10 }), // ramp of xj = 0..511
		mk(func(i int) int32 { return int32(i%7-3) << 15 }),
	}
	if n >= 1 {
		for _, pos := range []int{0, n - 1, n / 2} {
			s := make([]int32, ln)
			s[pos] = 1 << 24
			pats = append(pats, s)
			s2 := make([]int32, ln)
			s2[pos] = math.MinInt32
			pats = append(pats, s2)
		}
		// Values engineered so |x|^2*N lands right at each threshold, plus a few
		// steps either side, to pin the strict-less-than boundary per lane.
		for _, thr := range []int32{spreadThr2, spreadThr1, spreadThr0} {
			sq := float64(thr) / float64(n)
			xj := int32(math.Sqrt(sq * 32768))
			for _, d := range []int32{-3, -2, -1, 0, 1, 2, 3} {
				v := (xj + d) << 10
				pats = append(pats, mk(func(i int) int32 { return v }))
			}
		}
	}
	for seed := 0; seed < 6; seed++ {
		// Threshold-crossing magnitude range.
		s := make([]int32, ln)
		for i := range s {
			s[i] = int32(r.Intn(1<<22) - (1 << 21))
		}
		pats = append(pats, s)
		// Full int32 range (stresses both truncations at the extremes).
		s2 := make([]int32, ln)
		for i := range s2 {
			s2[i] = int32(r.Uint32())
		}
		pats = append(pats, s2)
	}
	return pats
}

// TestSpreadingHistMatchesGeneric pins the dispatched spreadingHist (the NEON or
// SSE2 kernel on arm64/amd64) to the pure-Go spreadingHistGeneric across every N
// tail remainder and the adversarial patterns above. On architectures without a
// kernel the two are the same function, so this is a no-op there; the value is on
// arm64/amd64. spreadingHistGeneric itself is pinned to libopus by
// TestSpreadingDecisionMatchesC, so a green kernel here is transitively bit-exact
// against C.
func TestSpreadingHistMatchesGeneric(t *testing.T) {
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("no SIMD kernel on this GOARCH: spreadingHist == spreadingHistGeneric, so the comparison is a tautology")
	}
	r := rand.New(rand.NewSource(0x533D))
	for _, n := range spreadingHistNs {
		for pi, x := range spreadingHistPatterns(r, n) {
			// Back the input with a poisoned tail (cap > n) so a kernel over-read
			// past N fails deterministically instead of relying on the allocator
			// zero-filling the slack. The sentinel is SMALL on purpose: for this
			// count-below-threshold reduction, x2N(1) = 0 is counted in all three
			// bins, so an over-read is visible, whereas a LARGE poison would give
			// x2N >= 2048, be counted by nothing, and hide the over-read.
			padded := make([]int32, n+4)
			copy(padded, x[:n])
			for k := n; k < len(padded); k++ {
				padded[k] = 1
			}
			xk := padded[:n]
			g0, g1, g2 := spreadingHistGeneric(xk, n)
			a0, a1, a2 := spreadingHist(xk, n)
			if g0 != a0 || g1 != a1 || g2 != a2 {
				t.Fatalf("N=%d pattern=%d: generic=(%d,%d,%d) kernel=(%d,%d,%d)\n x[:min]=%v",
					n, pi, g0, g1, g2, a0, a1, a2, xk[:min(n, 8)])
			}
		}
	}
}

// FuzzSpreadingHist pins the dispatched kernel to the pure-Go reference over
// coverage-guided int32 inputs, the convention pitch_simd_test.go establishes for
// every SIMD kernel in this package (FuzzCeltInnerProd / FuzzXcorrKernel). It is a
// no-op self-comparison on architectures without a kernel; the value is on
// arm64/amd64, where it explores the int32^N domain far past the fixed patterns.
func FuzzSpreadingHist(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}, 1)
	f.Add([]byte{0xff, 0xff, 0xff, 0x7f}, 1) // MaxInt32: x>>10 overflows int16
	f.Add([]byte{0x00, 0x00, 0x00, 0x80}, 1) // MinInt32
	f.Add(make([]byte, 4*12), 12)
	f.Add(make([]byte, 4*17), 17) // odd tail remainder

	f.Fuzz(func(t *testing.T, xb []byte, nRaw int) {
		n := len(xb) / 4
		if n == 0 {
			return
		}
		// Derive N in [1, n] so both full-width and short reads over the same
		// backing are exercised; the kernel must never read past N.
		nn := nRaw
		if nn < 0 {
			nn = -nn
		}
		nn = nn%n + 1
		x := make([]int32, n)
		for i := 0; i < n; i++ {
			x[i] = int32(uint32(xb[4*i]) | uint32(xb[4*i+1])<<8 | uint32(xb[4*i+2])<<16 | uint32(xb[4*i+3])<<24)
		}
		g0, g1, g2 := spreadingHistGeneric(x, nn)
		a0, a1, a2 := spreadingHist(x, nn)
		if g0 != a0 || g1 != a1 || g2 != a2 {
			t.Fatalf("n=%d nn=%d: generic=(%d,%d,%d) kernel=(%d,%d,%d)", n, nn, g0, g1, g2, a0, a1, a2)
		}
	})
}
