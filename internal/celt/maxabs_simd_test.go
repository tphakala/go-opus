package celt

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// Differential and fuzz tests for the SIMD max-abs reductions. celtMaxabs32,
// celtMaxabs16 and celtMaxabsRes are backed by github.com/tphakala/simd
// (i32.MaxAbs / i16.MaxAbs) on every architecture (maxabs_simd.go), while
// celtMaxabs32Generic / celtMaxabs16Generic / celtMaxabsResGeneric in maxabs_ref.go
// are the scalar reference. These tests compare the live library-backed reductions
// against that reference on identical inputs, so a mapping bug (wrong slice, wrong
// offset, wrong fold) or a library regression surfaces here.
//
// The hazards being hunted, the ones a random-input test sails past:
//
//   - The type-minimum divergence. The int32 reference celtMaxabs32Generic seeds its
//     scan at 0 and floors the result at 0: an all-negative window whose most-negative
//     element is MinInt32 yields 0. The raw library i32.MaxAbs omits that floor and
//     returns the negative array max there, so celtMaxabs32 must clamp with
//     max(0, ...) to match. These tests drive exactly that input (all MinInt32,
//     MinInt32 beside negatives, single MinInt32 at end), so dropping the clamp turns
//     them red. The int16 forms widen to int32 before negating, so |MinInt16| = 32768
//     is exact and no clamp is needed; both are pinned anyway.
//   - Every tail remainder. The library's vector loops step several lanes at a time
//     and hand the rest to a scalar epilogue, so every length mod the vector width
//     has to run, hence every length 0..600 rather than a handful of sizes.
//   - Over-read past the window. The operands are embedded in a poisoned buffer with
//     an offset, so a kernel that reads before the window or past its end lands on a
//     poison value and diverges from the reference, which reads only the window.

// maxLenSweep is the length sweep for the differential suites: every length from 0
// to simdMaxLen (600, defined in pitch_simd_test.go) so every vector-width tail
// remainder of every backend is exercised. 0 pins the empty-window zero result.
const maxLenSweep = simdMaxLen

// maxabsPad is the poison margin placed before and after every test window, wider
// than one whole block of the widest backend kernel, so a block-aligned over-read
// still lands on poison rather than sailing off the allocation.
const maxabsPad = 32

// poison32 / poison16 mark the buffer slack outside the logical window. They are
// deliberately large magnitudes so that if a kernel over-reads them, the resulting
// max-abs differs from the reference's window-only result. poison32 is MaxInt32 so
// an over-read past the window drives the result to MaxInt32 whenever the true
// window peak is smaller.
const (
	poison32 = int32(math.MaxInt32)
	poison16 = int16(math.MaxInt16)
)

// --- int32 (celtMaxabs32) -------------------------------------------------------

type maxabs32Pattern struct {
	name string
	f    func(i, n int) int32
}

var maxabs32Patterns = []maxabs32Pattern{
	{"all zero", func(_, _ int) int32 { return 0 }},
	{"all MinInt32", func(_, _ int) int32 { return math.MinInt32 }},
	{"all MaxInt32", func(_, _ int) int32 { return math.MaxInt32 }},
	// The documented wrap regime: a MinInt32 sitting beside plain negatives. The
	// fold returns the plain negative (max(maxval, -minval) with -MinInt32 wrapping
	// back to MinInt32), NOT +2^31. An abs-then-max kernel diverges here.
	{"MinInt32 beside negatives", func(i, _ int) int32 {
		if i%2 == 0 {
			return math.MinInt32
		}
		return -3
	}},
	{"alternating MinInt32/MaxInt32", func(i, _ int) int32 {
		if i%2 == 0 {
			return math.MinInt32
		}
		return math.MaxInt32
	}},
	{"all negative ramp", func(i, _ int) int32 { return -int32(i%65536) - 1 }},
	{"all positive ramp", func(i, _ int) int32 { return int32(i%65536) + 1 }},
	{"single MinInt32 at end", func(i, n int) int32 {
		if i == n-1 {
			return math.MinInt32
		}
		return -1
	}},
	{"single MaxInt32 at end", func(i, n int) int32 {
		if i == n-1 {
			return math.MaxInt32
		}
		return 1
	}},
	{"one nonzero at end", func(i, n int) int32 {
		if i == n-1 {
			return math.MinInt32
		}
		return 0
	}},
	{"lcg pseudorandom", func(i, _ int) int32 {
		s := uint32(i)*2654435761 ^ 0x9E3779B9
		s = s*1664525 + 1013904223
		return int32(s)
	}},
	{"full-scale ramp", func(i, _ int) int32 {
		return int32(math.MinInt32 + int64(i)*int64(math.MaxUint32)/int64(maxLenSweep+1))
	}},
}

// build32 embeds n samples of a pattern at offset maxabsPad in a buffer whose slack
// is filled with poison32, and returns (buf, off, n). A kernel that reads outside
// [off, off+n) lands on poison and diverges from the window-only reference.
func build32(p maxabs32Pattern, n int) (buf []int32, off, length int) {
	buf = make([]int32, maxabsPad+n+maxabsPad)
	for i := range buf {
		buf[i] = poison32
	}
	for i := 0; i < n; i++ {
		buf[maxabsPad+i] = p.f(i, n)
	}
	return buf, maxabsPad, n
}

func TestCeltMaxabs32SIMDMatchesScalar(t *testing.T) {
	for _, p := range maxabs32Patterns {
		t.Run(p.name, func(t *testing.T) {
			for n := 0; n <= maxLenSweep; n++ {
				buf, off, length := build32(p, n)
				got := celtMaxabs32(buf, off, length)
				want := celtMaxabs32Generic(buf, off, length)
				if got != want {
					t.Fatalf("n=%d off=%d: celtMaxabs32=%d, want %d", n, off, got, want)
				}
			}
		})
	}
}

// --- int16 (celtMaxabs16 / celtMaxabsRes) ---------------------------------------

type maxabs16Pattern struct {
	name string
	f    func(i, n int) int16
}

var maxabs16Patterns = []maxabs16Pattern{
	{"all zero", func(_, _ int) int16 { return 0 }},
	{"all MinInt16", func(_, _ int) int16 { return math.MinInt16 }},
	{"all MaxInt16", func(_, _ int) int16 { return math.MaxInt16 }},
	{"MinInt16 beside negatives", func(i, _ int) int16 {
		if i%2 == 0 {
			return math.MinInt16
		}
		return -3
	}},
	{"alternating MinInt16/MaxInt16", func(i, _ int) int16 {
		if i%2 == 0 {
			return math.MinInt16
		}
		return math.MaxInt16
	}},
	{"all negative ramp", func(i, _ int) int16 { return int16(-(i % 32768) - 1) }},
	{"all positive ramp", func(i, _ int) int16 { return int16(i%32767 + 1) }},
	{"single MinInt16 at end", func(i, n int) int16 {
		if i == n-1 {
			return math.MinInt16
		}
		return -1
	}},
	{"single MaxInt16 at end", func(i, n int) int16 {
		if i == n-1 {
			return math.MaxInt16
		}
		return 1
	}},
	{"one nonzero at end", func(i, n int) int16 {
		if i == n-1 {
			return math.MinInt16
		}
		return 0
	}},
	{"lcg pseudorandom", func(i, _ int) int16 {
		s := uint32(i)*2654435761 ^ 0x9E3779B9
		s = s*1664525 + 1013904223
		return int16(s >> 16)
	}},
}

func build16(p maxabs16Pattern, n int) (buf []int16, off, length int) {
	buf = make([]int16, maxabsPad+n+maxabsPad)
	for i := range buf {
		buf[i] = poison16
	}
	for i := 0; i < n; i++ {
		buf[maxabsPad+i] = p.f(i, n)
	}
	return buf, maxabsPad, n
}

func TestCeltMaxabs16SIMDMatchesScalar(t *testing.T) {
	for _, p := range maxabs16Patterns {
		t.Run(p.name, func(t *testing.T) {
			for n := 0; n <= maxLenSweep; n++ {
				buf, off, length := build16(p, n)
				// celtMaxabs16 takes no offset, so slice the window out first. win's
				// cap runs to the buffer end, so the trailing poison guards a
				// past-the-end over-read; the celtMaxabsRes call below reads with the
				// offset and is guarded on both ends by the buffer's leading and
				// trailing poison.
				win := buf[off : off+length]
				got := celtMaxabs16(win, length)
				want := celtMaxabs16Generic(win, length)
				if got != want {
					t.Fatalf("n=%d: celtMaxabs16=%d, want %d", n, got, want)
				}
				// celtMaxabsRes over the same window, exercising the offset form.
				gotRes := celtMaxabsRes(buf, off, length)
				wantRes := celtMaxabsResGeneric(buf, off, length)
				if gotRes != wantRes {
					t.Fatalf("n=%d off=%d: celtMaxabsRes=%d, want %d", n, off, gotRes, wantRes)
				}
			}
		})
	}
}

// --- panic guards --------------------------------------------------------------

// TestCeltMaxabsGuards pins the window-bounds panic guards, the load-bearing
// contract documented in maxabs_simd.go: a mis-sized caller must panic here rather
// than reach i32.MaxAbs / i16.MaxAbs, which clamp to the slice they are handed and
// would return a silent wrong answer. Each case names the guard clause it trips. The
// empty window (len_==0, including the xOff==len(x) boundary) is in range and returns
// 0, not a panic. The *Generic oracles carry byte-identical guards (verified in the
// diff), so pinning the production wrappers covers both.
func TestCeltMaxabsGuards(t *testing.T) {
	// cap > len on purpose: x[xOff:xOff+len_] bounds against cap, not len, so a
	// window that runs past len(x) but stays within cap would silently reslice into
	// the spare capacity if the guard were missing (a wrong answer, no native slice
	// panic). That len-vs-cap gap is exactly what the guards close, so the guard
	// cases below use windows in (len, cap]; with cap==len they would trip Go's own
	// bounds check and the test could not tell the guard apart from it.
	x32 := make([]int32, 10, 20)
	x16 := make([]int16, 10, 20)

	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic, got none", name)
			}
		}()
		f()
	}
	mustReturn := func(name string, got, want int32) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		}
	}
	// mustPanicMsg additionally checks the panic came from the named window guard,
	// not from a downstream slice-bounds panic. It pins the overflow-safe guard: a
	// naive xOff+len_ > len(x) check wraps for a window whose xOff+len_ overflows int
	// and slips through, deferring the panic to the slice expression with a different
	// message; the guard here must catch it.
	mustPanicMsg := func(name, want string, f func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("%s: expected panic, got none", name)
				return
			}
			if s, ok := r.(string); !ok || !strings.Contains(s, want) {
				t.Errorf("%s: panic %v does not contain %q (guard bypassed?)", name, r, want)
			}
		}()
		f()
	}

	mustPanic("maxabs32 len_<0", func() { celtMaxabs32(x32, 0, -1) })
	mustPanic("maxabs32 xOff<0", func() { celtMaxabs32(x32, -1, 1) })
	mustPanic("maxabs32 xOff+len_>len", func() { celtMaxabs32(x32, 5, 6) })
	mustPanic("maxabs16 len_<0", func() { celtMaxabs16(x16, -1) })
	mustPanic("maxabs16 len_>len", func() { celtMaxabs16(x16, 11) })
	mustPanic("maxabsRes xOff<0", func() { celtMaxabsRes(x16, -1, 1) })
	mustPanic("maxabsRes len_<0", func() { celtMaxabsRes(x16, 0, -1) })
	mustPanic("maxabsRes xOff+len_>len", func() { celtMaxabsRes(x16, 5, 6) })

	// A window whose xOff+len_ overflows int must still hit the named guard.
	mustPanicMsg("maxabs32 len_ overflow", "celtMaxabs32", func() { celtMaxabs32(x32, 5, math.MaxInt) })
	mustPanicMsg("maxabsRes len_ overflow", "celtMaxabsRes", func() { celtMaxabsRes(x16, 5, math.MaxInt) })

	mustReturn("maxabs32 empty", celtMaxabs32(x32, 0, 0), 0)
	mustReturn("maxabs32 boundary empty", celtMaxabs32(x32, len(x32), 0), 0)
	mustReturn("maxabs16 empty", celtMaxabs16(x16, 0), 0)
	mustReturn("maxabsRes boundary empty", celtMaxabsRes(x16, len(x16), 0), 0)
}

// --- fuzz -----------------------------------------------------------------------

func FuzzCeltMaxabs32(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0x80}) // one MinInt32 (LE)
	f.Add([]byte{0xff, 0xff, 0xff, 0x7f, 0, 0, 0, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		n := len(b) / 4
		x := make([]int32, n)
		for i := 0; i < n; i++ {
			x[i] = int32(uint32(b[4*i]) | uint32(b[4*i+1])<<8 | uint32(b[4*i+2])<<16 | uint32(b[4*i+3])<<24)
		}
		if got, want := celtMaxabs32(x, 0, n), celtMaxabs32Generic(x, 0, n); got != want {
			t.Fatalf("n=%d: celtMaxabs32=%d, want %d", n, got, want)
		}
		// Also exercise a non-zero offset when the buffer is long enough.
		if n >= 2 {
			off := n / 3
			if got, want := celtMaxabs32(x, off, n-off), celtMaxabs32Generic(x, off, n-off); got != want {
				t.Fatalf("n=%d off=%d: celtMaxabs32=%d, want %d", n, off, got, want)
			}
		}
	})
}

func FuzzCeltMaxabs16(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0x80}) // MinInt16 (LE)
	f.Add([]byte{0xff, 0x7f, 0, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		n := len(b) / 2
		x := make([]int16, n)
		for i := 0; i < n; i++ {
			x[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
		}
		if got, want := celtMaxabs16(x, n), celtMaxabs16Generic(x, n); got != want {
			t.Fatalf("n=%d: celtMaxabs16=%d, want %d", n, got, want)
		}
		if n >= 2 {
			off := n / 3
			if got, want := celtMaxabsRes(x, off, n-off), celtMaxabsResGeneric(x, off, n-off); got != want {
				t.Fatalf("n=%d off=%d: celtMaxabsRes=%d, want %d", n, off, got, want)
			}
		}
	})
}

// --- benchmarks -----------------------------------------------------------------

// maxabsBenchSizes spans the band windows compute_band_energies scans and the
// frame/xcorr windows transient_analysis and pitch_downsample scan. It starts at
// N=1 because at LM=0 the first eight eBands have width 1, so computeBandEnergies
// calls celtMaxabs32 at N=1/2/4/6. Those small sizes straddle the library's own
// small-N gate (i32.MaxAbs falls to a scalar reduction below 4 elements on arm64
// NEON, below 8 on amd64 AVX2), so the rows confirm the non-inlined wrapper does not
// regress the tiny bands on either the scalar or the vector path; the large sizes
// show the SIMD win.
var maxabsBenchSizes = []int{1, 2, 4, 8, 16, 32, 64, 128, 176, 240, 480, 960, 1920}

func BenchmarkCeltMaxabs32(b *testing.B) {
	for _, n := range maxabsBenchSizes {
		x := seqFillI32(n)
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var s int32
			for i := 0; i < b.N; i++ {
				s ^= celtMaxabs32(x, 0, n)
			}
			runtimeSink32 = s
		})
	}
}

func BenchmarkCeltMaxabs16(b *testing.B) {
	for _, n := range maxabsBenchSizes {
		x := make([]int16, n)
		for i := range x {
			x[i] = int16(uint16(i)*40503 + 1)
		}
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var s int32
			for i := 0; i < b.N; i++ {
				s ^= celtMaxabs16(x, n)
			}
			runtimeSink32 = s
		})
	}
}

var runtimeSink32 int32
