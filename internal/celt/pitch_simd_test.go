package celt

import (
	"math"
	"testing"
)

// Differential tests for the SIMD pitch kernels. celtInnerProd and xcorrKernel
// are assembly on arm64 and amd64 (pitch_arm64.s, pitch_amd64.s) and the scalar
// reference in pitch_ref.go on everything else; celtInnerProdGeneric and
// xcorrKernelGeneric are compiled into *every* build, so these tests always
// compare the live implementation against the reference on identical inputs. On
// the generic build the comparison is a tautology, which is fine -- the point is
// that on arm64/amd64 it is not.
//
// The hazard being hunted here is a vector kernel that agrees with the scalar
// path on well-behaved audio-shaped input but diverges on the corners:
//
//   - -32768 (INT16_MIN) operands. This is the one input where the products can
//     reach 2^30 and a PMADDWD lane can land exactly on 2^31, i.e. wrap to
//     INT32_MIN. A kernel that saturates instead of wrapping passes random tests
//     and fails here.
//   - int32 accumulator overflow. MAC16_16 accumulates into a *wrapping* int32;
//     any implementation that widens to int64 and saturates, or that reduces
//     lanes through a wider type without truncating back, diverges once the sum
//     exceeds 2^31.
//   - every tail remainder. The vector loops step 16/8/4 samples at a time and
//     hand the rest to a scalar epilogue, so every length mod the vector width
//     has to be exercised -- hence every length from 0 to 600 rather than a
//     handful of sizes.
//
// A codec that is bit-exact for random input and wrong on INT16_MIN would ship
// broken packets on real clipped audio, so these cases are tested deliberately
// rather than hoped for.

const simdMaxLen = 600

// fillPattern is one adversarial input generator: given the sample index it
// returns the x and y sample at that index.
type fillPattern struct {
	name string
	f    func(i int) (x, y int16)
}

// simdPatterns are the input patterns both kernels are tested against. The
// wraparound cases are engineered, not incidental: "all INT16_MIN" reaches the
// 2^31 PMADDWD corner on every single lane, and the saturating patterns push the
// int32 accumulator through several full wraps well before length 600.
var simdPatterns = []fillPattern{
	{"all zero", func(int) (int16, int16) { return 0, 0 }},
	{"all INT16_MIN", func(int) (int16, int16) { return math.MinInt16, math.MinInt16 }},
	{"all INT16_MAX", func(int) (int16, int16) { return math.MaxInt16, math.MaxInt16 }},
	{"INT16_MIN x INT16_MAX", func(int) (int16, int16) { return math.MinInt16, math.MaxInt16 }},
	{"INT16_MIN x 1", func(int) (int16, int16) { return math.MinInt16, 1 }},
	{"alternating signs", func(i int) (int16, int16) {
		if i%2 == 0 {
			return math.MinInt16, math.MaxInt16
		}
		return math.MaxInt16, math.MinInt16
	}},
	{"alternating INT16_MIN/0", func(i int) (int16, int16) {
		if i%2 == 0 {
			return math.MinInt16, math.MinInt16
		}
		return 0, 0
	}},
	// Every product is +2^30, so the accumulator wraps every 2 samples: this
	// walks the full int32 range over and over.
	{"forced accumulator wrap", func(int) (int16, int16) { return math.MinInt16, math.MinInt16 }},
	// Products alternate around the top of the int32 range to straddle the wrap
	// boundary rather than sail through it.
	{"straddle INT32_MAX", func(i int) (int16, int16) {
		if i%3 == 0 {
			return math.MaxInt16, math.MaxInt16
		}
		return math.MinInt16, math.MaxInt16
	}},
	{"one nonzero at end", func(i int) (int16, int16) {
		if i == simdMaxLen-1 {
			return math.MinInt16, math.MinInt16
		}
		return 0, 0
	}},
	{"lcg pseudorandom", func(i int) (int16, int16) {
		s := uint32(i)*2654435761 ^ 0x9E3779B9
		s = s*1664525 + 1013904223
		t := s*1664525 + 1013904223
		return int16(s >> 16), int16(t >> 16)
	}},
	{"full-scale ramp", func(i int) (int16, int16) {
		return int16(math.MinInt16 + i%65536), int16(math.MaxInt16 - i%65536)
	}},
}

// makeVecs builds x and y for a pattern. y is given `slack` extra samples beyond
// n, which xcorrKernel needs (it reads three past the end of x).
func makeVecs(p fillPattern, n, slack int) (x, y []int16) {
	x = make([]int16, n)
	y = make([]int16, n+slack)
	for i := range y {
		xv, yv := p.f(i)
		if i < n {
			x[i] = xv
		}
		y[i] = yv
	}
	return x, y
}

func TestCeltInnerProdSIMDMatchesScalar(t *testing.T) {
	for _, p := range simdPatterns {
		t.Run(p.name, func(t *testing.T) {
			for n := 0; n <= simdMaxLen; n++ {
				x, y := makeVecs(p, n, 0)
				want := celtInnerProdGeneric(x, 0, y, 0, n)
				got := celtInnerProd(x, 0, y, 0, n)
				if got != want {
					t.Fatalf("n=%d: celtInnerProd = %d, scalar reference = %d", n, got, want)
				}
			}
		})
	}
}

// TestCeltInnerProdSIMDOffsets exercises non-zero xOff/yOff, which is how every
// real caller uses it (removeDoubling correlates x against x at a lag) and which
// is where the assembly's base-pointer arithmetic could go wrong. Offsets are
// deliberately not equal to each other and not multiples of the vector width, so
// the two operands sit at different misalignments.
func TestCeltInnerProdSIMDOffsets(t *testing.T) {
	for _, p := range simdPatterns {
		t.Run(p.name, func(t *testing.T) {
			for _, xOff := range []int{0, 1, 3, 7, 8, 15} {
				for _, yOff := range []int{0, 1, 2, 5, 9, 16} {
					for _, n := range []int{0, 1, 3, 4, 5, 7, 8, 9, 15, 16, 17, 23, 31, 33, 100, 240, 481} {
						x, y := makeVecs(p, n+xOff, 0)
						_, y2 := makeVecs(p, n+yOff, 0)
						y = append(y, y2...) //nolint:gocritic // building a longer y, not reassigning a param alias
						want := celtInnerProdGeneric(x, xOff, y, yOff, n)
						got := celtInnerProd(x, xOff, y, yOff, n)
						if got != want {
							t.Fatalf("xOff=%d yOff=%d n=%d: celtInnerProd = %d, scalar reference = %d",
								xOff, yOff, n, got, want)
						}
					}
				}
			}
		})
	}
}

func TestXcorrKernelSIMDMatchesScalar(t *testing.T) {
	// Non-zero seeds prove the kernel *accumulates* into sum rather than
	// overwriting it, and that the seed is carried through the wrap correctly.
	seeds := [][4]int32{
		{0, 0, 0, 0},
		{1, -1, 2, -2},
		{math.MaxInt32, math.MinInt32, math.MaxInt32 - 1, math.MinInt32 + 1},
		{-1, -1, -1, -1},
	}
	for _, p := range simdPatterns {
		t.Run(p.name, func(t *testing.T) {
			for n := 0; n <= simdMaxLen; n++ {
				// xcorrKernel reads three samples of y past the end of x.
				x, y := makeVecs(p, n, 3)
				for si, seed := range seeds {
					want := seed
					xcorrKernelGeneric(x, y, &want, n)
					got := seed
					xcorrKernel(x, y, &got, n)
					if got != want {
						t.Fatalf("n=%d seed=%d: xcorrKernel = %v, scalar reference = %v",
							n, si, got, want)
					}
				}
			}
		})
	}
}

// TestCeltPitchXcorrSIMDMatchesScalar checks the whole driver loop, not just the
// kernel: celtPitchXcorr mixes the vectorized xcorrKernel (four lags at a time)
// with a celtInnerProd tail when maxPitch is not a multiple of 4, so both paths
// have to agree with a fully scalar recomputation, including the returned
// running maximum.
func TestCeltPitchXcorrSIMDMatchesScalar(t *testing.T) {
	for _, p := range simdPatterns {
		t.Run(p.name, func(t *testing.T) {
			for _, len_ := range []int{3, 4, 5, 7, 8, 9, 16, 17, 60, 240} {
				// Cover every maxPitch mod 4 so the scalar tail is exercised.
				for _, maxPitch := range []int{1, 2, 3, 4, 5, 6, 7, 8, 17, 244} {
					x, y := makeVecs(p, len_+maxPitch+8, 0)
					x = x[:len_]

					xcorr := make([]int32, maxPitch)
					maxcorr := celtPitchXcorr(x, y, xcorr, len_, maxPitch)

					// Independent scalar recomputation from the definition.
					wantXcorr := make([]int32, maxPitch)
					wantMax := int32(1)
					for i := 0; i < maxPitch; i++ {
						s := celtInnerProdGeneric(x, 0, y, i, len_)
						wantXcorr[i] = s
						if s > wantMax {
							wantMax = s
						}
					}
					for i := range wantXcorr {
						if xcorr[i] != wantXcorr[i] {
							t.Fatalf("len_=%d maxPitch=%d: xcorr[%d] = %d, want %d",
								len_, maxPitch, i, xcorr[i], wantXcorr[i])
						}
					}
					if maxcorr != wantMax {
						t.Fatalf("len_=%d maxPitch=%d: maxcorr = %d, want %d",
							len_, maxPitch, maxcorr, wantMax)
					}
				}
			}
		})
	}
}

func FuzzCeltInnerProd(f *testing.F) {
	f.Add([]byte{}, []byte{})
	f.Add([]byte{0x00, 0x80}, []byte{0x00, 0x80})                         // INT16_MIN * INT16_MIN
	f.Add([]byte{0xff, 0x7f, 0xff, 0x7f}, []byte{0x00, 0x80, 0x00, 0x80}) // MAX * MIN
	f.Add(make([]byte, 64), make([]byte, 64))

	f.Fuzz(func(t *testing.T, xb, yb []byte) {
		// Interpret the corpus bytes as int16 samples; n is whatever both cover.
		n := min(len(xb)/2, len(yb)/2)
		if n > 4096 {
			n = 4096
		}
		x := make([]int16, n)
		y := make([]int16, n)
		for i := 0; i < n; i++ {
			x[i] = int16(uint16(xb[2*i]) | uint16(xb[2*i+1])<<8)
			y[i] = int16(uint16(yb[2*i]) | uint16(yb[2*i+1])<<8)
		}
		want := celtInnerProdGeneric(x, 0, y, 0, n)
		got := celtInnerProd(x, 0, y, 0, n)
		if got != want {
			t.Fatalf("n=%d: celtInnerProd = %d, scalar reference = %d\nx=%v\ny=%v", n, got, want, x, y)
		}
	})
}

func FuzzXcorrKernel(f *testing.F) {
	f.Add([]byte{}, []byte{}, int32(0))
	f.Add(make([]byte, 32), make([]byte, 38), int32(0))
	f.Add([]byte{0x00, 0x80, 0x00, 0x80, 0x00, 0x80},
		[]byte{0x00, 0x80, 0x00, 0x80, 0x00, 0x80, 0x00, 0x80, 0x00, 0x80, 0x00, 0x80}, int32(math.MaxInt32))

	f.Fuzz(func(t *testing.T, xb, yb []byte, seed int32) {
		// y must carry three samples more than x. Clamping n at 0 can leave the
		// corpus with fewer than the 2*(n+3) bytes y wants, so both buffers are
		// filled defensively and any shortfall stays zero -- the kernels must be
		// exact for zero samples too.
		n := min(len(xb)/2, len(yb)/2-3)
		if n < 0 {
			n = 0
		}
		if n > 4096 {
			n = 4096
		}
		sampleAt := func(b []byte, i int) int16 {
			if 2*i+1 >= len(b) {
				return 0
			}
			return int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
		}
		x := make([]int16, n)
		y := make([]int16, n+3)
		for i := 0; i < n; i++ {
			x[i] = sampleAt(xb, i)
		}
		for i := 0; i < n+3; i++ {
			y[i] = sampleAt(yb, i)
		}
		want := [4]int32{seed, seed - 1, seed + 1, -seed}
		got := want
		xcorrKernelGeneric(x, y, &want, n)
		xcorrKernel(x, y, &got, n)
		if got != want {
			t.Fatalf("n=%d seed=%d: xcorrKernel = %v, scalar reference = %v\nx=%v\ny=%v",
				n, seed, got, want, x, y)
		}
	})
}

// BenchmarkCeltInnerProd pins the inner product at two shapes that matter: N=480
// is the length remove_doubling correlates at, and N=8 is a short CELT band --
// the case where Go's un-inlined assembly call overhead could plausibly make the
// vector path a loss rather than a win.
func BenchmarkCeltInnerProd(b *testing.B) {
	for _, n := range []int{8, 480} {
		x := make([]int16, n)
		y := make([]int16, n)
		s := uint32(0x9E3779B9)
		for i := range x {
			s = s*1664525 + 1013904223
			x[i] = int16(s >> 17)
			s = s*1664525 + 1013904223
			y[i] = int16(s >> 17)
		}
		b.Run("N="+itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int32
			for b.Loop() {
				sink = celtInnerProd(x, 0, y, 0, n)
			}
			_ = sink
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
