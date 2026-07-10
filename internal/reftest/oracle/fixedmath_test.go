//go:build refc

package oracle

// Differential tests: the pure-Go internal/fixedmath port versus the pinned
// libopus FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64 oracle (wrappers in
// fixedmath_shim.h / fixedmath_cgo.go). Every ported macro/function is asserted
// bit-exact against the C, over exhaustive, dense-structured, boundary, and
// random inputs.
//
// The oracle build is proven to be OPUS_FAST_INT64 by TestBuildConfig
// (oracle_test.go); these tests rely on that rather than re-asserting it, so the
// 64-bit MULT16_32/MULT32_32 forms compared here are the same ones the Go port
// uses (docs/hard-parts.md section 4).

import (
	"math"
	"math/rand/v2"
	"testing"

	fm "github.com/tphakala/go-opus/internal/fixedmath"
)

// ---- input generators ----

func newRand() *rand.Rand { return rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0xbf58476d1ce4e5b9)) }

// b16set is the set of "interesting" int16 values: extremes, zero, small
// neighbourhoods, byte edges, and powers of two with +-1 neighbours.
func b16set() []int16 {
	seen := map[int16]bool{}
	var out []int16
	add := func(v int) {
		if v >= math.MinInt16 && v <= math.MaxInt16 {
			x := int16(v)
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	for _, v := range []int{
		-32768, -32767, -32766, 0, 1, 2, 3, -1, -2, -3,
		127, 128, 129, 255, 256, 257, -127, -128, -129, -255, -256, -257,
		181, -181, 16384, -16384, 32767, 32766,
	} {
		add(v)
	}
	for k := 0; k <= 15; k++ {
		p := 1 << k
		add(p)
		add(-p)
		add(p - 1)
		add(-(p - 1))
		add(p + 1)
		add(-(p + 1))
	}
	return out
}

// b32set is the set of "interesting" int32 values.
func b32set() []int32 {
	seen := map[int32]bool{}
	var out []int32
	add := func(v int64) {
		if v >= math.MinInt32 && v <= math.MaxInt32 {
			x := int32(v)
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	for _, v := range []int64{
		math.MinInt32, math.MaxInt32, 0, 1, 2, 3, -1, -2, -3,
		32767, 32768, -32768, 65535, 65536, -65536,
		0x40000000, -0x40000000, 536870911, 536870912, -536870912,
		1073741824, -1073741824, 2147483646,
	} {
		add(v)
	}
	for k := 0; k <= 31; k++ {
		p := int64(1) << k
		add(p)
		add(-p)
		add(p - 1)
		add(-(p - 1))
		add(p + 1)
		add(-(p + 1))
	}
	return out
}

// bu32set is the set of "interesting" positive uint32 values (EC_ILOG/isqrt32
// domain: greater than zero).
func bu32set() []uint32 {
	seen := map[uint32]bool{}
	var out []uint32
	add := func(v uint64) {
		if v >= 1 && v <= math.MaxUint32 {
			x := uint32(v)
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	for _, v := range []uint64{
		1, 2, 3, 255, 256, 257, 65535, 65536, 0x7fffffff, 0x80000000, 0xfffffffe, 0xffffffff,
	} {
		add(v)
	}
	for k := 0; k <= 31; k++ {
		p := uint64(1) << k
		add(p)
		add(p - 1)
		add(p + 1)
	}
	return out
}

// ---- 16x16 macros ----

type row16Op struct {
	name string
	code int
	gfn  func(a, b int16) int32
	cfn  func(a, b int32) int32
}

func row16Ops() []row16Op {
	return []row16Op{
		{"MULT16_16", rowMULT16_16, fm.MULT16_16, cMULT16_16},
		{"MULT16_16_Q11", rowMULT16_16_Q11, fm.MULT16_16_Q11, cMULT16_16_Q11},
		{"MULT16_16_Q13", rowMULT16_16_Q13, fm.MULT16_16_Q13, cMULT16_16_Q13},
		{"MULT16_16_Q14", rowMULT16_16_Q14, fm.MULT16_16_Q14, cMULT16_16_Q14},
		{"MULT16_16_Q15", rowMULT16_16_Q15, fm.MULT16_16_Q15, cMULT16_16_Q15},
		{"MULT16_16_P13", rowMULT16_16_P13, fm.MULT16_16_P13, cMULT16_16_P13},
		{"MULT16_16_P14", rowMULT16_16_P14, fm.MULT16_16_P14, cMULT16_16_P14},
		{"MULT16_16_P15", rowMULT16_16_P15, fm.MULT16_16_P15, cMULT16_16_P15},
	}
}

// TestFM16x16Dense sweeps every 16x16 macro over: all int16 b for every boundary
// a (exhaustive in b for a dense set of a), plus random pairs. The whole-row
// compare uses cRow16 so it is one cgo call per row.
func TestFM16x16Dense(t *testing.T) {
	buf := make([]int32, 65536)
	r := newRand()
	for _, op := range row16Ops() {
		for _, a := range b16set() {
			cRow16(op.code, int32(a), buf)
			for bi := 0; bi < 65536; bi++ {
				b := int16(bi - 32768)
				if got := op.gfn(a, b); got != buf[bi] {
					t.Fatalf("%s(%d,%d)=%d want %d", op.name, a, b, got, buf[bi])
				}
			}
		}
		for i := 0; i < 300000; i++ {
			a := int16(r.Uint32())
			b := int16(r.Uint32())
			if got, want := op.gfn(a, b), op.cfn(int32(a), int32(b)); got != want {
				t.Fatalf("%s(%d,%d)=%d want %d", op.name, a, b, got, want)
			}
		}
	}
}

// TestFM16x16Exhaustive covers the full 2^32-pair int16 x int16 grid for the
// representative truncating (Q15) and rounding (P15) multiplies, plus the plain
// 32-bit-result MULT16_16. Skipped under -short.
func TestFM16x16Exhaustive(t *testing.T) {
	if testing.Short() {
		t.Skip("full int16 x int16 grid (2^32 pairs); run without -short")
	}
	buf := make([]int32, 65536)
	fullGrid := func(name string, code int, gfn func(a, b int16) int32) {
		for ai := 0; ai < 65536; ai++ {
			a := int16(ai - 32768)
			cRow16(code, int32(a), buf)
			for bi := 0; bi < 65536; bi++ {
				b := int16(bi - 32768)
				if got := gfn(a, b); got != buf[bi] {
					t.Fatalf("%s(%d,%d)=%d want %d", name, a, b, got, buf[bi])
				}
			}
		}
	}
	fullGrid("MULT16_16", rowMULT16_16, fm.MULT16_16)
	fullGrid("MULT16_16_Q15", rowMULT16_16_Q15, fm.MULT16_16_Q15)
	fullGrid("MULT16_16_P15", rowMULT16_16_P15, fm.MULT16_16_P15)
}

func TestFM16x16Misc(t *testing.T) {
	r := newRand()
	// ADD16 / SUB16 over the boundary cross-product and random.
	for _, a := range b16set() {
		for _, b := range b16set() {
			if got, want := int32(fm.ADD16(a, b)), cADD16(int32(a), int32(b)); got != want {
				t.Fatalf("ADD16(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := fm.SUB16(a, b), cSUB16(int32(a), int32(b)); got != want {
				t.Fatalf("SUB16(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := fm.MULT16_16SU(a, uint16(b)), cMULT16_16SU(int32(a), int32(uint16(b))); got != want {
				t.Fatalf("MULT16_16SU(%d,%d)=%d want %d", a, uint16(b), got, want)
			}
		}
	}
	for i := 0; i < 300000; i++ {
		a := int16(r.Uint32())
		b := int16(r.Uint32())
		if got, want := int32(fm.ADD16(a, b)), cADD16(int32(a), int32(b)); got != want {
			t.Fatalf("ADD16(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := fm.SUB16(a, b), cSUB16(int32(a), int32(b)); got != want {
			t.Fatalf("SUB16(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := fm.MULT16_16SU(a, uint16(b)), cMULT16_16SU(int32(a), int32(uint16(b))); got != want {
			t.Fatalf("MULT16_16SU(%d,%d)=%d want %d", a, uint16(b), got, want)
		}
	}
	// NEG16 over all int16.
	for v := -32768; v <= 32767; v++ {
		if got, want := fm.NEG16(int16(v)), cNEG16(int32(int16(v))); got != want {
			t.Fatalf("NEG16(%d)=%d want %d", v, got, want)
		}
	}
}

// ---- 16x32 macros ----

func TestFM16x32(t *testing.T) {
	r := newRand()
	type op struct {
		name string
		gfn  func(a int16, b int32) int32
		cfn  func(a, b int32) int32
	}
	ops := []op{
		{"MULT16_32_Q15", fm.MULT16_32_Q15, cMULT16_32_Q15},
		{"MULT16_32_Q16", fm.MULT16_32_Q16, cMULT16_32_Q16},
		{"MULT16_32_P16", fm.MULT16_32_P16, cMULT16_32_P16},
	}
	a16 := b16set()
	b32 := b32set()
	for _, o := range ops {
		for _, a := range a16 {
			for _, b := range b32 {
				if got, want := o.gfn(a, b), o.cfn(int32(a), b); got != want {
					t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
				}
			}
		}
		for i := 0; i < 500000; i++ {
			a := int16(r.Uint32())
			b := int32(r.Uint32())
			if got, want := o.gfn(a, b), o.cfn(int32(a), b); got != want {
				t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
			}
		}
	}
}

// ---- 32x32 macros and add/sub ----

func TestFM32x32(t *testing.T) {
	r := newRand()
	type op struct {
		name string
		gfn  func(a, b int32) int32
		cfn  func(a, b int32) int32
	}
	ops := []op{
		{"MULT32_32_Q31", fm.MULT32_32_Q31, cMULT32_32_Q31},
		{"MULT32_32_P31", fm.MULT32_32_P31, cMULT32_32_P31},
		{"ADD32", fm.ADD32, cADD32},
		{"SUB32", fm.SUB32, cSUB32},
		{"ADD32_ovflw", fm.ADD32_ovflw, cADD32_ovflw},
		{"SUB32_ovflw", fm.SUB32_ovflw, cSUB32_ovflw},
	}
	b32 := b32set()
	for _, o := range ops {
		for _, a := range b32 {
			for _, b := range b32 {
				if got, want := o.gfn(a, b), o.cfn(a, b); got != want {
					t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
				}
			}
		}
		for i := 0; i < 500000; i++ {
			a := int32(r.Uint32())
			b := int32(r.Uint32())
			if got, want := o.gfn(a, b), o.cfn(a, b); got != want {
				t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
			}
		}
	}
	// MULT32_32_P31_ovflw is identical to MULT32_32_P31 under OPUS_FAST_INT64.
	for i := 0; i < 200000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if got, want := fm.MULT32_32_P31_ovflw(a, b), cMULT32_32_P31(a, b); got != want {
			t.Fatalf("MULT32_32_P31_ovflw(%d,%d)=%d want %d", a, b, got, want)
		}
	}
}

// ---- MAC macros ----

func TestFMMac(t *testing.T) {
	r := newRand()
	a16 := b16set()
	b32 := b32set()
	// MAC16_16(c,a,b): c 32-bit, a,b 16-bit.
	for _, c := range b32 {
		for _, a := range a16 {
			for _, b := range a16 {
				if got, want := fm.MAC16_16(c, a, b), cMAC16_16(c, int32(a), int32(b)); got != want {
					t.Fatalf("MAC16_16(%d,%d,%d)=%d want %d", c, a, b, got, want)
				}
			}
		}
	}
	// MAC16_32_Q15 / Q16 (two-part decompositions, never the int64 form).
	for i := 0; i < 500000; i++ {
		c := int32(r.Uint32())
		a := int16(r.Uint32())
		b := int32(r.Uint32())
		if got, want := fm.MAC16_32_Q15(c, a, b), cMAC16_32_Q15(c, int32(a), b); got != want {
			t.Fatalf("MAC16_32_Q15(%d,%d,%d)=%d want %d", c, a, b, got, want)
		}
		if got, want := fm.MAC16_32_Q16(c, a, b), cMAC16_32_Q16(c, int32(a), b); got != want {
			t.Fatalf("MAC16_32_Q16(%d,%d,%d)=%d want %d", c, a, b, got, want)
		}
	}
	for _, c := range b32 {
		for _, a := range a16 {
			for _, b := range b32 {
				if got, want := fm.MAC16_32_Q15(c, a, b), cMAC16_32_Q15(c, int32(a), b); got != want {
					t.Fatalf("MAC16_32_Q15(%d,%d,%d)=%d want %d", c, a, b, got, want)
				}
				if got, want := fm.MAC16_32_Q16(c, a, b), cMAC16_32_Q16(c, int32(a), b); got != want {
					t.Fatalf("MAC16_32_Q16(%d,%d,%d)=%d want %d", c, a, b, got, want)
				}
			}
		}
	}
}

// ---- shifts ----

func TestFMShifts32(t *testing.T) {
	r := newRand()
	b32 := b32set()
	for _, a := range b32 {
		for s := 0; s <= 31; s++ {
			if got, want := fm.SHR32(a, s), cSHR32(a, s); got != want {
				t.Fatalf("SHR32(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := fm.SHL32(a, s), cSHL32(a, s); got != want {
				t.Fatalf("SHL32(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := fm.SHL32_ovflw(a, s), cSHL32_ovflw(a, s); got != want {
				t.Fatalf("SHL32_ovflw(%d,%d)=%d want %d", a, s, got, want)
			}
		}
		// PSHR32 covers the round addend and the a+addend overflow trap.
		for s := 0; s <= 31; s++ {
			if got, want := fm.PSHR32(a, s), cPSHR32(a, s); got != want {
				t.Fatalf("PSHR32(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := fm.PSHR32_ovflw(a, s), cPSHR32_ovflw(a, s); got != want {
				t.Fatalf("PSHR32_ovflw(%d,%d)=%d want %d", a, s, got, want)
			}
		}
		// VSHR32 shifts either direction.
		for s := -31; s <= 31; s++ {
			if got, want := fm.VSHR32(a, s), cVSHR32(a, s); got != want {
				t.Fatalf("VSHR32(%d,%d)=%d want %d", a, s, got, want)
			}
		}
		// ROUND16 = EXTRACT16(PSHR32(x,a)).
		for s := 0; s <= 31; s++ {
			if got, want := int32(fm.ROUND16(a, s)), cROUND16(a, s); got != want {
				t.Fatalf("ROUND16(%d,%d)=%d want %d", a, s, got, want)
			}
		}
	}
	for i := 0; i < 200000; i++ {
		a := int32(r.Uint32())
		s := r.IntN(32)
		if got, want := fm.SHR32(a, s), cSHR32(a, s); got != want {
			t.Fatalf("SHR32(%d,%d)=%d want %d", a, s, got, want)
		}
		if got, want := fm.SHL32(a, s), cSHL32(a, s); got != want {
			t.Fatalf("SHL32(%d,%d)=%d want %d", a, s, got, want)
		}
		if got, want := fm.PSHR32(a, s), cPSHR32(a, s); got != want {
			t.Fatalf("PSHR32(%d,%d)=%d want %d", a, s, got, want)
		}
		vs := r.IntN(63) - 31
		if got, want := fm.VSHR32(a, vs), cVSHR32(a, vs); got != want {
			t.Fatalf("VSHR32(%d,%d)=%d want %d", a, vs, got, want)
		}
	}
}

func TestFMShifts16(t *testing.T) {
	for v := -32768; v <= 32767; v++ {
		a := int16(v)
		for s := 0; s <= 15; s++ {
			if got, want := int32(fm.SHR16(a, s)), cSHR16(int32(a), s); got != want {
				t.Fatalf("SHR16(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := int32(fm.SHL16(a, s)), cSHL16(int32(a), s); got != want {
				t.Fatalf("SHL16(%d,%d)=%d want %d", a, s, got, want)
			}
		}
	}
}

// ---- saturate ----

func TestFMSaturate(t *testing.T) {
	r := newRand()
	b32 := b32set()
	for _, x := range b32 {
		if got, want := int32(fm.SATURATE16(x)), cSATURATE16(x); got != want {
			t.Fatalf("SATURATE16(%d)=%d want %d", x, got, want)
		}
		for _, a := range []int32{1, 2, 32767, 32768, 536870911, 1073741823, 2147483647} {
			if got, want := fm.SATURATE(x, a), cSATURATE(x, a); got != want {
				t.Fatalf("SATURATE(%d,%d)=%d want %d", x, a, got, want)
			}
		}
	}
	for i := 0; i < 400000; i++ {
		x := int32(r.Uint32())
		if got, want := int32(fm.SATURATE16(x)), cSATURATE16(x); got != want {
			t.Fatalf("SATURATE16(%d)=%d want %d", x, got, want)
		}
		a := int32(r.Uint32() >> 1) // positive clamp bound
		if a == 0 {
			a = 1
		}
		if got, want := fm.SATURATE(x, a), cSATURATE(x, a); got != want {
			t.Fatalf("SATURATE(%d,%d)=%d want %d", x, a, got, want)
		}
	}
}

// ---- QCONST ----

func TestFMQConst(t *testing.T) {
	r := newRand()
	type qc struct {
		x    float64
		bits int
	}
	cases := []qc{
		{0.5, 15}, {1.0, 14}, {-0.5, 15}, {0.8, 15}, {-1.0, 14},
		{0.0009765625, 15}, {0.85, 15}, {0.0, 15}, {0.25, 15}, {-0.25, 15},
		{1.0, 30}, {0.001, 30}, {-0.001, 30}, {0.5, 30}, {0.99, 30}, {-0.99, 30},
	}
	for _, c := range cases {
		if got, want := int32(fm.QCONST16(c.x, c.bits)), cQCONST16(c.x, c.bits); got != want {
			t.Fatalf("QCONST16(%g,%d)=%d want %d", c.x, c.bits, got, want)
		}
		if got, want := fm.QCONST32(c.x, c.bits), cQCONST32(c.x, c.bits); got != want {
			t.Fatalf("QCONST32(%g,%d)=%d want %d", c.x, c.bits, got, want)
		}
	}
	// Random x constrained so the result stays in range (float->int out of range
	// is implementation-defined; in range Go and C both truncate toward zero).
	for i := 0; i < 100000; i++ {
		x := r.Float64()*2 - 1 // [-1,1)
		bits := r.IntN(15)     // 0..14 keeps |x|*2^bits < 32767
		if got, want := int32(fm.QCONST16(x, bits)), cQCONST16(x, bits); got != want {
			t.Fatalf("QCONST16(%g,%d)=%d want %d", x, bits, got, want)
		}
		bits32 := r.IntN(30) // 0..29
		if got, want := fm.QCONST32(x, bits32), cQCONST32(x, bits32); got != want {
			t.Fatalf("QCONST32(%g,%d)=%d want %d", x, bits32, got, want)
		}
	}
}

// ---- celt_udiv / celt_sudiv ----

func TestFMUdiv(t *testing.T) {
	r := newRand()
	nsU := bu32set()
	nsU = append(nsU, 0) // udiv numerator may be zero
	// d in 1..256 exhaustive (the range celt_udiv is documented for), plus wider.
	dset := make([]uint32, 0, 512)
	for d := uint32(1); d <= 256; d++ {
		dset = append(dset, d)
	}
	dset = append(dset, 257, 1000, 65535, 65536, 0x7fffffff, 0xffffffff)
	for _, d := range dset {
		for _, n := range nsU {
			if got, want := fm.Celt_udiv(n, d), cCeltUdiv(n, d); got != want {
				t.Fatalf("celt_udiv(%d,%d)=%d want %d", n, d, got, want)
			}
		}
		for i := 0; i < 2000; i++ {
			n := r.Uint32()
			if got, want := fm.Celt_udiv(n, d), cCeltUdiv(n, d); got != want {
				t.Fatalf("celt_udiv(%d,%d)=%d want %d", n, d, got, want)
			}
		}
	}
	// celt_sudiv: signed numerator (incl. negative), positive divisor.
	nsS := b32set()
	dsetS := make([]int32, 0, 512)
	for d := int32(1); d <= 256; d++ {
		dsetS = append(dsetS, d)
	}
	dsetS = append(dsetS, 257, 1000, 65535, 65536, 0x7fffffff)
	for _, d := range dsetS {
		for _, n := range nsS {
			if got, want := fm.Celt_sudiv(n, d), cCeltSudiv(n, d); got != want {
				t.Fatalf("celt_sudiv(%d,%d)=%d want %d", n, d, got, want)
			}
		}
		for i := 0; i < 2000; i++ {
			n := int32(r.Uint32())
			if got, want := fm.Celt_sudiv(n, d), cCeltSudiv(n, d); got != want {
				t.Fatalf("celt_sudiv(%d,%d)=%d want %d", n, d, got, want)
			}
		}
	}
}

// ---- mathops: integer logs ----

func TestFMILog(t *testing.T) {
	r := newRand()
	// EC_ILOG is UB in C for 0; assert the Go definition-for-zero directly.
	if fm.EC_ILOG(0) != 0 || fm.Ec_ilog(0) != 0 {
		t.Fatalf("EC_ILOG(0)=%d Ec_ilog(0)=%d want 0", fm.EC_ILOG(0), fm.Ec_ilog(0))
	}
	for _, x := range bu32set() {
		if got, want := fm.EC_ILOG(x), cEC_ILOG(x); got != want {
			t.Fatalf("EC_ILOG(%d)=%d want %d", x, got, want)
		}
		if fm.Ec_ilog(x) != fm.EC_ILOG(x) {
			t.Fatalf("Ec_ilog(%d)=%d != EC_ILOG=%d", x, fm.Ec_ilog(x), fm.EC_ILOG(x))
		}
		// celt_ilog2 is defined for x>0 (as an int32).
		if x <= math.MaxInt32 {
			xi := int32(x)
			if got, want := fm.Celt_ilog2(xi), cCeltIlog2(xi); got != want {
				t.Fatalf("celt_ilog2(%d)=%d want %d", xi, got, want)
			}
		}
	}
	for i := 0; i < 1000000; i++ {
		x := r.Uint32()
		if x == 0 {
			continue
		}
		if got, want := fm.EC_ILOG(x), cEC_ILOG(x); got != want {
			t.Fatalf("EC_ILOG(%d)=%d want %d", x, got, want)
		}
	}
	// celt_zlog2 is defined for all int32 (0 and negatives -> 0).
	for _, x := range b32set() {
		if got, want := fm.Celt_zlog2(x), cCeltZlog2(x); got != want {
			t.Fatalf("celt_zlog2(%d)=%d want %d", x, got, want)
		}
	}
	for i := 0; i < 500000; i++ {
		x := int32(r.Uint32())
		if got, want := fm.Celt_zlog2(x), cCeltZlog2(x); got != want {
			t.Fatalf("celt_zlog2(%d)=%d want %d", x, got, want)
		}
		if x > 0 {
			if got, want := fm.Celt_ilog2(x), cCeltIlog2(x); got != want {
				t.Fatalf("celt_ilog2(%d)=%d want %d", x, got, want)
			}
		}
	}
}

// ---- mathops: isqrt32 ----

func TestFMIsqrt32(t *testing.T) {
	r := newRand()
	check := func(x uint32) {
		if got, want := fm.Isqrt32(x), cIsqrt32(x); got != want {
			t.Fatalf("isqrt32(%d)=%d want %d", x, got, want)
		}
	}
	limit := uint32(1 << 20)
	if testing.Short() {
		limit = 1 << 16
	}
	for x := uint32(1); x <= limit; x++ {
		check(x)
	}
	// Perfect squares and their neighbours across the full range.
	for k := uint64(1); k <= 65535; k++ {
		sq := k * k
		check(uint32(sq))
		if sq > 1 {
			check(uint32(sq - 1))
		}
		if sq+1 <= math.MaxUint32 {
			check(uint32(sq + 1))
		}
	}
	for _, x := range bu32set() {
		check(x)
	}
	n := 2000000
	if testing.Short() {
		n = 100000
	}
	for i := 0; i < n; i++ {
		x := r.Uint32()
		if x == 0 {
			x = 1
		}
		check(x)
	}
}

// ---- mathops: exp2 / exp2_frac (exhaustive over all int16 inputs) ----

func TestFMExp2(t *testing.T) {
	for v := -32768; v <= 32767; v++ {
		x := int32(int16(v))
		if got, want := fm.Celt_exp2_frac(int16(v)), cCeltExp2Frac(x); got != want {
			t.Fatalf("celt_exp2_frac(%d)=%d want %d", v, got, want)
		}
		if got, want := fm.Celt_exp2(int16(v)), cCeltExp2(x); got != want {
			t.Fatalf("celt_exp2(%d)=%d want %d", v, got, want)
		}
	}
}

// ---- mathops: cos_norm (exhaustive over the 17-bit domain) ----

func TestFMCosNorm(t *testing.T) {
	r := newRand()
	for x := int32(0); x <= 0x1ffff; x++ {
		if got, want := int32(fm.Celt_cos_norm(x)), cCeltCosNorm(x); got != want {
			t.Fatalf("celt_cos_norm(%d)=%d want %d", x, got, want)
		}
	}
	// Full-range inputs to confirm the x&0x1ffff masking matches.
	for i := 0; i < 500000; i++ {
		x := int32(r.Uint32())
		if got, want := int32(fm.Celt_cos_norm(x)), cCeltCosNorm(x); got != want {
			t.Fatalf("celt_cos_norm(%d)=%d want %d", x, got, want)
		}
	}
}

// ---- mathops: rsqrt_norm ----

func TestFMRsqrtNorm(t *testing.T) {
	r := newRand()
	// Documented input range [0.25,1) in Q16 == [16384,65536): exhaustive.
	for x := int32(16384); x < 65536; x++ {
		if got, want := int32(fm.Celt_rsqrt_norm(x)), cCeltRsqrtNorm(x); got != want {
			t.Fatalf("celt_rsqrt_norm(%d)=%d want %d", x, got, want)
		}
	}
	for _, x := range b32set() {
		if got, want := int32(fm.Celt_rsqrt_norm(x)), cCeltRsqrtNorm(x); got != want {
			t.Fatalf("celt_rsqrt_norm(%d)=%d want %d", x, got, want)
		}
	}
	for i := 0; i < 500000; i++ {
		x := int32(r.Uint32())
		if got, want := int32(fm.Celt_rsqrt_norm(x)), cCeltRsqrtNorm(x); got != want {
			t.Fatalf("celt_rsqrt_norm(%d)=%d want %d", x, got, want)
		}
	}
}

// ---- mathops: sqrt ----

func TestFMSqrt(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := fm.Celt_sqrt(x), cCeltSqrt(x); got != want {
			t.Fatalf("celt_sqrt(%d)=%d want %d", x, got, want)
		}
	}
	// Special cases: 0 -> 0, >=2^30 -> 32767.
	check(0)
	check(1073741824)
	check(1073741823)
	check(math.MaxInt32)
	for _, x := range b32set() {
		if x >= 0 {
			check(x)
		}
	}
	// Dense low range plus random over the full non-negative range.
	for x := int32(1); x <= 1<<16; x++ {
		check(x)
	}
	for i := 0; i < 1000000; i++ {
		x := int32(r.Uint32() >> 1) // [0, 2^31)
		check(x)
	}
}

// ---- mathops: rcp ----

func TestFMRcp(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := fm.Celt_rcp(x), cCeltRcp(x); got != want {
			t.Fatalf("celt_rcp(%d)=%d want %d", x, got, want)
		}
	}
	for _, x := range b32set() {
		if x > 0 { // domain: x>0
			check(x)
		}
	}
	for x := int32(1); x <= 1<<16; x++ {
		check(x)
	}
	for i := 0; i < 1000000; i++ {
		x := int32(r.Uint32() >> 1)
		if x <= 0 {
			x = 1
		}
		check(x)
	}
}

// ---- mathops: log2 ----

func TestFMLog2(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := int32(fm.Celt_log2(x)), cCeltLog2(x); got != want {
			t.Fatalf("celt_log2(%d)=%d want %d", x, got, want)
		}
	}
	check(0) // special case -> -32767
	for _, x := range b32set() {
		check(x)
	}
	for x := int32(1); x <= 1<<16; x++ {
		check(x)
	}
	for i := 0; i < 1000000; i++ {
		check(int32(r.Uint32()))
	}
}

// ---- mathops: frac_div32 / frac_div32_q29 ----

func TestFMFracDiv32(t *testing.T) {
	r := newRand()
	// Divisor must be > 0: celt_ilog2(b) needs b>0, and b=0 makes EC_ILOG(0) UB
	// in C. Any b>0 is safe because frac_div32_q29 first normalizes b to ~2^29
	// (VSHR32 by celt_ilog2(b)-29) before taking the reciprocal, so ROUND16(b,16)
	// is always positive and celt_rcp never sees 0.
	var bset []int32
	for _, b := range b32set() {
		if b > 0 {
			bset = append(bset, b)
		}
	}
	aset := b32set()
	for _, a := range aset {
		for _, b := range bset {
			if got, want := fm.Frac_div32_q29(a, b), cFracDiv32Q29(a, b); got != want {
				t.Fatalf("frac_div32_q29(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := fm.Frac_div32(a, b), cFracDiv32(a, b); got != want {
				t.Fatalf("frac_div32(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for i := 0; i < 500000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32()>>1) | 0x8000 // >= 32768, positive
		if got, want := fm.Frac_div32_q29(a, b), cFracDiv32Q29(a, b); got != want {
			t.Fatalf("frac_div32_q29(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := fm.Frac_div32(a, b), cFracDiv32(a, b); got != want {
			t.Fatalf("frac_div32(%d,%d)=%d want %d", a, b, got, want)
		}
	}
}
