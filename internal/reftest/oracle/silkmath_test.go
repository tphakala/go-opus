//go:build refc

package oracle

// Differential tests: the pure-Go internal/silkmath port versus the pinned
// libopus FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64 oracle (wrappers in
// silkmath_shim.h / silkmath_cgo.go). Every ported macro/function is asserted
// bit-exact against the C, over exhaustive, dense-structured, boundary, and
// random inputs.
//
// The oracle build is proven OPUS_FAST_INT64 by TestBuildConfig (oracle_test.go)
// and the compile-time #error in shim.c, so the 64-bit SMUL*/SMLA* forms
// compared here are the same ones the Go port uses (docs/hard-parts.md 4).
//
// The int16/int32/uint32/int64 boundary generators b16set/b32set/bu32set/newRand
// are shared with fixedmath_test.go (same package); only the int64 set is local.

import (
	"math"
	"testing"

	sm "github.com/tphakala/go-opus/internal/silkmath"
)

// b64set is the set of "interesting" int64 values.
func b64set() []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(v int64) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range []int64{
		math.MinInt64, math.MaxInt64, 0, 1, -1, 2, -2,
		math.MinInt32, math.MaxInt32, 1 << 31, -(1 << 31), 1 << 32, -(1 << 32),
		1 << 62, -(1 << 62), (1 << 62) - 1, math.MaxInt64 - 1, math.MinInt64 + 1,
	} {
		add(v)
	}
	for k := 0; k <= 63; k++ {
		p := int64(1) << uint(k)
		add(p)
		add(-p)
		add(p - 1)
		add(-(p - 1))
	}
	return out
}

// ---- 32x32 int64-form multiplies (the OPUS_FAST_INT64 forms) ----

func TestSilkWideMul2(t *testing.T) {
	r := newRand()
	type op struct {
		name string
		gfn  func(a, b int32) int32
		cfn  func(a, b int32) int32
	}
	ops := []op{
		{"SMULWB", sm.Silk_SMULWB, csmSMULWB},
		{"SMULWT", sm.Silk_SMULWT, csmSMULWT},
		{"SMULWW", sm.Silk_SMULWW, csmSMULWW},
		{"SMULTT", sm.Silk_SMULTT, csmSMULTT},
		{"SMMUL", sm.Silk_SMMUL, csmSMMUL},
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
	// SMULL returns int64.
	for _, a := range b32 {
		for _, b := range b32 {
			if got, want := sm.Silk_SMULL(a, b), csmSMULL(a, b); got != want {
				t.Fatalf("SMULL(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for i := 0; i < 500000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if got, want := sm.Silk_SMULL(a, b), csmSMULL(a, b); got != want {
			t.Fatalf("SMULL(%d,%d)=%d want %d", a, b, got, want)
		}
	}
}

func TestSilkWideMul3(t *testing.T) {
	r := newRand()
	type op struct {
		name string
		gfn  func(a, b, c int32) int32
		cfn  func(a, b, c int32) int32
	}
	ops := []op{
		{"SMLAWB", sm.Silk_SMLAWB, csmSMLAWB},
		{"SMLAWT", sm.Silk_SMLAWT, csmSMLAWT},
		{"SMLAWW", sm.Silk_SMLAWW, csmSMLAWW},
		{"SMLATT", sm.Silk_SMLATT, csmSMLATT},
	}
	b32 := b32set()
	for _, o := range ops {
		// Boundary cross-product with c walked over the boundary set for each pair.
		for _, a := range b32 {
			for _, b := range b32 {
				for _, c := range []int32{math.MinInt32, math.MaxInt32, 0, 1, -1, 65535, -65536, 32768} {
					if got, want := o.gfn(a, b, c), o.cfn(a, b, c); got != want {
						t.Fatalf("%s(%d,%d,%d)=%d want %d", o.name, a, b, c, got, want)
					}
				}
			}
		}
		for i := 0; i < 700000; i++ {
			a := int32(r.Uint32())
			b := int32(r.Uint32())
			c := int32(r.Uint32())
			if got, want := o.gfn(a, b, c), o.cfn(a, b, c); got != want {
				t.Fatalf("%s(%d,%d,%d)=%d want %d", o.name, a, b, c, got, want)
			}
		}
	}
}

// ---- 16-bit multiplies / MAC ----

func TestSilk16Mul(t *testing.T) {
	r := newRand()
	b16 := b16set()
	b32 := b32set()
	type op2 struct {
		name string
		gfn  func(a, b int32) int32
		cfn  func(a, b int32) int32
	}
	ops2 := []op2{
		{"SMULBB", sm.Silk_SMULBB, csmSMULBB},
		{"SMULBT", sm.Silk_SMULBT, csmSMULBT},
		{"MUL", sm.Silk_MUL, csmMUL},
	}
	for _, o := range ops2 {
		for _, a := range b32 {
			for _, b := range b32 {
				if got, want := o.gfn(a, b), o.cfn(a, b); got != want {
					t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
				}
			}
		}
		for i := 0; i < 400000; i++ {
			a := int32(r.Uint32())
			b := int32(r.Uint32())
			if got, want := o.gfn(a, b), o.cfn(a, b); got != want {
				t.Fatalf("%s(%d,%d)=%d want %d", o.name, a, b, got, want)
			}
		}
	}
	type op3 struct {
		name string
		gfn  func(a, b, c int32) int32
		cfn  func(a, b, c int32) int32
	}
	ops3 := []op3{
		{"SMLABB", sm.Silk_SMLABB, csmSMLABB},
		{"SMLABT", sm.Silk_SMLABT, csmSMLABT},
		{"MLA", sm.Silk_MLA, csmMLA},
		{"MLA_ovflw", sm.Silk_MLA_ovflw, csmMLA_ovflw},
		{"SMLABB_ovflw", sm.Silk_SMLABB_ovflw, csmSMLABB_ovflw},
	}
	for _, o := range ops3 {
		for _, c := range b32 {
			for _, a := range b16 {
				for _, b := range b16 {
					if got, want := o.gfn(c, int32(a), int32(b)), o.cfn(c, int32(a), int32(b)); got != want {
						t.Fatalf("%s(%d,%d,%d)=%d want %d", o.name, c, a, b, got, want)
					}
				}
			}
		}
		for i := 0; i < 500000; i++ {
			a := int32(r.Uint32())
			b := int32(r.Uint32())
			c := int32(r.Uint32())
			if got, want := o.gfn(a, b, c), o.cfn(a, b, c); got != want {
				t.Fatalf("%s(%d,%d,%d)=%d want %d", o.name, a, b, c, got, want)
			}
		}
	}
	// MUL_uint / MLA_uint (unsigned).
	for i := 0; i < 400000; i++ {
		a := r.Uint32()
		b := r.Uint32()
		c := r.Uint32()
		if got, want := sm.Silk_MUL_uint(a, b), csmMUL_uint(a, b); got != want {
			t.Fatalf("MUL_uint(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := sm.Silk_MLA_uint(a, b, c), csmMLA_uint(a, b, c); got != want {
			t.Fatalf("MLA_uint(%d,%d,%d)=%d want %d", a, b, c, got, want)
		}
	}
}

// TestSilkSMULBBExhaustive covers the full 2^32-pair int16 x int16 grid of
// silk_SMULBB, one cgo call per row. Skipped under -short.
func TestSilkSMULBBExhaustive(t *testing.T) {
	if testing.Short() {
		t.Skip("full int16 x int16 grid (2^32 pairs); run without -short")
	}
	buf := make([]int32, 65536)
	for ai := 0; ai < 65536; ai++ {
		a := int16(ai - 32768)
		csmRowSMULBB(int32(a), buf)
		for bi := 0; bi < 65536; bi++ {
			b := int16(bi - 32768)
			if got := sm.Silk_SMULBB(int32(a), int32(b)); got != buf[bi] {
				t.Fatalf("SMULBB(%d,%d)=%d want %d", a, b, got, buf[bi])
			}
		}
	}
}

// ---- SMLAL / SMLALBB (int64 accumulate) ----

func TestSilkSMLAL(t *testing.T) {
	r := newRand()
	b16 := b16set()
	// Keep |a64| and the product well inside int64 so the sum does not overflow
	// (silk_ADD64 is a plain add; the overflow corners are covered by ADD64).
	a64s := []int64{0, 1, -1, 1 << 40, -(1 << 40), 1 << 60, -(1 << 60), math.MaxInt32, math.MinInt32}
	for _, a := range a64s {
		for _, b := range []int32{0, 1, -1, 32767, -32768, 1 << 15, 1 << 20, -(1 << 20)} {
			for _, c := range []int32{0, 1, -1, 32767, -32768, 1 << 15, 1 << 20, -(1 << 20)} {
				if got, want := sm.Silk_SMLAL(a, b, c), csmSMLAL(a, b, c); got != want {
					t.Fatalf("SMLAL(%d,%d,%d)=%d want %d", a, b, c, got, want)
				}
			}
		}
	}
	for i := 0; i < 300000; i++ {
		a := int64(int32(r.Uint32())) << 20
		b := int32(r.Uint32())
		c := int32(r.Uint32())
		if got, want := sm.Silk_SMLAL(a, b, c), csmSMLAL(a, b, c); got != want {
			t.Fatalf("SMLAL(%d,%d,%d)=%d want %d", a, b, c, got, want)
		}
	}
	// SMLALBB: 16x16 accumulate onto int64.
	for _, a := range []int64{0, 1, -1, 1 << 40, -(1 << 40), math.MaxInt32} {
		for _, b := range b16 {
			for _, c := range b16 {
				if got, want := sm.Silk_SMLALBB(a, b, c), csmSMLALBB(a, b, c); got != want {
					t.Fatalf("SMLALBB(%d,%d,%d)=%d want %d", a, b, c, got, want)
				}
			}
		}
	}
}

// ---- add / subtract ----

func TestSilkAddSub(t *testing.T) {
	r := newRand()
	b16 := b16set()
	b32 := b32set()
	b64 := b64set()
	for _, a := range b16 {
		for _, b := range b16 {
			if got, want := sm.Silk_ADD16(a, b), csmADD16(int32(a), int32(b)); got != want {
				t.Fatalf("ADD16(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB16(a, b), csmSUB16(int32(a), int32(b)); got != want {
				t.Fatalf("SUB16(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for _, a := range b32 {
		for _, b := range b32 {
			if got, want := sm.Silk_ADD32(a, b), csmADD32(a, b); got != want {
				t.Fatalf("ADD32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB32(a, b), csmSUB32(a, b); got != want {
				t.Fatalf("SUB32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_ADD32_ovflw(a, b), csmADD32_ovflw(a, b); got != want {
				t.Fatalf("ADD32_ovflw(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB32_ovflw(a, b), csmSUB32_ovflw(a, b); got != want {
				t.Fatalf("SUB32_ovflw(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for _, a := range b64 {
		for _, b := range b64 {
			if got, want := sm.Silk_ADD64(a, b), csmADD64(a, b); got != want {
				t.Fatalf("ADD64(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB64(a, b), csmSUB64(a, b); got != want {
				t.Fatalf("SUB64(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for i := 0; i < 400000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if got, want := sm.Silk_ADD32(a, b), csmADD32(a, b); got != want {
			t.Fatalf("ADD32(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := sm.Silk_SUB32_ovflw(a, b), csmSUB32_ovflw(a, b); got != want {
			t.Fatalf("SUB32_ovflw(%d,%d)=%d want %d", a, b, got, want)
		}
	}
}

// ---- saturating add / subtract ----

func TestSilkSaturating(t *testing.T) {
	r := newRand()
	b16 := b16set()
	b32 := b32set()
	b64 := b64set()
	for _, a := range b16 {
		for _, b := range b16 {
			if got, want := int32(sm.Silk_ADD_SAT16(a, b)), csmADD_SAT16(int32(a), int32(b)); got != want {
				t.Fatalf("ADD_SAT16(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := int32(sm.Silk_SUB_SAT16(a, b)), csmSUB_SAT16(int32(a), int32(b)); got != want {
				t.Fatalf("SUB_SAT16(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for _, a := range b32 {
		for _, b := range b32 {
			if got, want := sm.Silk_ADD_SAT32(a, b), csmADD_SAT32(a, b); got != want {
				t.Fatalf("ADD_SAT32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB_SAT32(a, b), csmSUB_SAT32(a, b); got != want {
				t.Fatalf("SUB_SAT32(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for _, a := range b64 {
		for _, b := range b64 {
			if got, want := sm.Silk_ADD_SAT64(a, b), csmADD_SAT64(a, b); got != want {
				t.Fatalf("ADD_SAT64(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_SUB_SAT64(a, b), csmSUB_SAT64(a, b); got != want {
				t.Fatalf("SUB_SAT64(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for i := 0; i < 500000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if got, want := sm.Silk_ADD_SAT32(a, b), csmADD_SAT32(a, b); got != want {
			t.Fatalf("ADD_SAT32(%d,%d)=%d want %d", a, b, got, want)
		}
		if got, want := sm.Silk_SUB_SAT32(a, b), csmSUB_SAT32(a, b); got != want {
			t.Fatalf("SUB_SAT32(%d,%d)=%d want %d", a, b, got, want)
		}
	}
}

// ---- shifts (all C-legal shift amounts) ----

func TestSilkShifts(t *testing.T) {
	r := newRand()
	b16 := b16set()
	b32 := b32set()
	b64 := b64set()
	// 8-bit and 16-bit shifts (exhaustive over the int8/int16 domains).
	for v := -128; v <= 127; v++ {
		a := int32(int8(v))
		for s := 0; s <= 7; s++ {
			if got, want := sm.Silk_LSHIFT8(int8(v), s), int8(csmLSHIFT8(a, s)); got != want {
				t.Fatalf("LSHIFT8(%d,%d)=%d want %d", v, s, got, want)
			}
			if got, want := sm.Silk_RSHIFT8(int8(v), s), int8(csmRSHIFT8(a, s)); got != want {
				t.Fatalf("RSHIFT8(%d,%d)=%d want %d", v, s, got, want)
			}
		}
	}
	for _, a := range b16 {
		for s := 0; s <= 15; s++ {
			if got, want := int32(sm.Silk_LSHIFT16(a, s)), csmLSHIFT16(int32(a), s); got != want {
				t.Fatalf("LSHIFT16(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := int32(sm.Silk_RSHIFT16(a, s)), csmRSHIFT16(int32(a), s); got != want {
				t.Fatalf("RSHIFT16(%d,%d)=%d want %d", a, s, got, want)
			}
		}
	}
	// 32-bit shifts, all amounts 0..31.
	for _, a := range b32 {
		for s := 0; s <= 31; s++ {
			if got, want := sm.Silk_LSHIFT32(a, s), csmLSHIFT32(a, s); got != want {
				t.Fatalf("LSHIFT32(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_RSHIFT32(a, s), csmRSHIFT32(a, s); got != want {
				t.Fatalf("RSHIFT32(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_LSHIFT(a, s), csmLSHIFT(a, s); got != want {
				t.Fatalf("LSHIFT(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_RSHIFT(a, s), csmRSHIFT(a, s); got != want {
				t.Fatalf("RSHIFT(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_LSHIFT_ovflw(a, s), csmLSHIFT_ovflw(a, s); got != want {
				t.Fatalf("LSHIFT_ovflw(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_LSHIFT_SAT32(a, s), csmLSHIFT_SAT32(a, s); got != want {
				t.Fatalf("LSHIFT_SAT32(%d,%d)=%d want %d", a, s, got, want)
			}
			ua := uint32(a)
			if got, want := sm.Silk_LSHIFT_uint(ua, s), csmLSHIFT_uint(ua, s); got != want {
				t.Fatalf("LSHIFT_uint(%d,%d)=%d want %d", ua, s, got, want)
			}
			if got, want := sm.Silk_RSHIFT_uint(ua, s), csmRSHIFT_uint(ua, s); got != want {
				t.Fatalf("RSHIFT_uint(%d,%d)=%d want %d", ua, s, got, want)
			}
		}
		// RSHIFT_ROUND requires shift > 0.
		for s := 1; s <= 31; s++ {
			if got, want := sm.Silk_RSHIFT_ROUND(a, s), csmRSHIFT_ROUND(a, s); got != want {
				t.Fatalf("RSHIFT_ROUND(%d,%d)=%d want %d", a, s, got, want)
			}
		}
	}
	// 64-bit shifts, all amounts 0..63, and RSHIFT_ROUND64 for 1..63.
	for _, a := range b64 {
		for s := 0; s <= 63; s++ {
			if got, want := sm.Silk_LSHIFT64(a, s), csmLSHIFT64(a, s); got != want {
				t.Fatalf("LSHIFT64(%d,%d)=%d want %d", a, s, got, want)
			}
			if got, want := sm.Silk_RSHIFT64(a, s), csmRSHIFT64(a, s); got != want {
				t.Fatalf("RSHIFT64(%d,%d)=%d want %d", a, s, got, want)
			}
		}
		for s := 1; s <= 63; s++ {
			if got, want := sm.Silk_RSHIFT_ROUND64(a, s), csmRSHIFT_ROUND64(a, s); got != want {
				t.Fatalf("RSHIFT_ROUND64(%d,%d)=%d want %d", a, s, got, want)
			}
		}
	}
	// ADD/SUB LSHIFT/RSHIFT variants over random pairs x all shift amounts.
	for i := 0; i < 200000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		s := r.IntN(32)
		if got, want := sm.Silk_ADD_LSHIFT(a, b, s), csmADD_LSHIFT(a, b, s); got != want {
			t.Fatalf("ADD_LSHIFT(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		if got, want := sm.Silk_ADD_LSHIFT32(a, b, s), csmADD_LSHIFT32(a, b, s); got != want {
			t.Fatalf("ADD_LSHIFT32(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		if got, want := sm.Silk_ADD_RSHIFT(a, b, s), csmADD_RSHIFT(a, b, s); got != want {
			t.Fatalf("ADD_RSHIFT(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		if got, want := sm.Silk_ADD_RSHIFT32(a, b, s), csmADD_RSHIFT32(a, b, s); got != want {
			t.Fatalf("ADD_RSHIFT32(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		if got, want := sm.Silk_SUB_LSHIFT32(a, b, s), csmSUB_LSHIFT32(a, b, s); got != want {
			t.Fatalf("SUB_LSHIFT32(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		if got, want := sm.Silk_SUB_RSHIFT32(a, b, s), csmSUB_RSHIFT32(a, b, s); got != want {
			t.Fatalf("SUB_RSHIFT32(%d,%d,%d)=%d want %d", a, b, s, got, want)
		}
		ua, ub := uint32(a), uint32(b)
		if got, want := sm.Silk_ADD_LSHIFT_uint(ua, ub, s), csmADD_LSHIFT_uint(ua, ub, s); got != want {
			t.Fatalf("ADD_LSHIFT_uint(%d,%d,%d)=%d want %d", ua, ub, s, got, want)
		}
		if got, want := sm.Silk_ADD_RSHIFT_uint(ua, ub, s), csmADD_RSHIFT_uint(ua, ub, s); got != want {
			t.Fatalf("ADD_RSHIFT_uint(%d,%d,%d)=%d want %d", ua, ub, s, got, want)
		}
	}
}

// ---- divide, saturation-to-width ----

func TestSilkDivSat(t *testing.T) {
	r := newRand()
	b32 := b32set()
	// DIV32 / DIV32_16: avoid divisor 0 (div-by-zero) and the INT32_MIN/-1
	// overflow corner (UB in C; Go defines it, so skip to stay on defined ground).
	safeDiv := func(a, b int32) bool { return b != 0 && !(a == math.MinInt32 && b == -1) }
	for _, a := range b32 {
		for _, b := range b32 {
			if !safeDiv(a, b) {
				continue
			}
			if got, want := sm.Silk_DIV32(a, b), csmDIV32(a, b); got != want {
				t.Fatalf("DIV32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_DIV32_16(a, b), csmDIV32_16(a, b); got != want {
				t.Fatalf("DIV32_16(%d,%d)=%d want %d", a, b, got, want)
			}
		}
		// SAT8/16/32 over the boundary set.
		if got, want := sm.Silk_SAT8(a), csmSAT8(a); got != want {
			t.Fatalf("SAT8(%d)=%d want %d", a, got, want)
		}
		if got, want := sm.Silk_SAT16(a), csmSAT16(a); got != want {
			t.Fatalf("SAT16(%d)=%d want %d", a, got, want)
		}
		if got, want := sm.Silk_SAT32(a), csmSAT32(a); got != want {
			t.Fatalf("SAT32(%d)=%d want %d", a, got, want)
		}
	}
	for i := 0; i < 400000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if safeDiv(a, b) {
			if got, want := sm.Silk_DIV32(a, b), csmDIV32(a, b); got != want {
				t.Fatalf("DIV32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_DIV32_16(a, b), csmDIV32_16(a, b); got != want {
				t.Fatalf("DIV32_16(%d,%d)=%d want %d", a, b, got, want)
			}
		}
		if got, want := sm.Silk_SAT8(a), csmSAT8(a); got != want {
			t.Fatalf("SAT8(%d)=%d want %d", a, got, want)
		}
		if got, want := sm.Silk_SAT16(a), csmSAT16(a); got != want {
			t.Fatalf("SAT16(%d)=%d want %d", a, got, want)
		}
	}
}

// ---- CLZ (exhaustive int16), CLZ32/64, ROR32 ----

func TestSilkCLZ(t *testing.T) {
	r := newRand()
	// CLZ16 exhaustive over all int16.
	for v := -32768; v <= 32767; v++ {
		if got, want := sm.Silk_CLZ16(int16(v)), csmCLZ16(int32(int16(v))); got != want {
			t.Fatalf("CLZ16(%d)=%d want %d", v, got, want)
		}
	}
	// CLZ32: boundary + all powers of two + zero + random.
	for _, x := range b32set() {
		if got, want := sm.Silk_CLZ32(x), csmCLZ32(x); got != want {
			t.Fatalf("CLZ32(%d)=%d want %d", x, got, want)
		}
	}
	if got, want := sm.Silk_CLZ32(0), csmCLZ32(0); got != want {
		t.Fatalf("CLZ32(0)=%d want %d", got, want)
	}
	for i := 0; i < 500000; i++ {
		x := int32(r.Uint32())
		if got, want := sm.Silk_CLZ32(x), csmCLZ32(x); got != want {
			t.Fatalf("CLZ32(%d)=%d want %d", x, got, want)
		}
	}
	// CLZ64: boundary + random.
	for _, x := range b64set() {
		if got, want := sm.Silk_CLZ64(x), csmCLZ64(x); got != want {
			t.Fatalf("CLZ64(%d)=%d want %d", x, got, want)
		}
	}
	for i := 0; i < 300000; i++ {
		x := int64(r.Uint64())
		if got, want := sm.Silk_CLZ64(x), csmCLZ64(x); got != want {
			t.Fatalf("CLZ64(%d)=%d want %d", x, got, want)
		}
	}
	// ROR32 over the boundary set x rot in [-31,31] (the C-legal range) + random.
	for _, x := range b32set() {
		for rot := -31; rot <= 31; rot++ {
			if got, want := sm.Silk_ROR32(x, rot), csmROR32(x, rot); got != want {
				t.Fatalf("ROR32(%d,%d)=%d want %d", x, rot, got, want)
			}
		}
	}
	for i := 0; i < 300000; i++ {
		x := int32(r.Uint32())
		rot := r.IntN(63) - 31
		if got, want := sm.Silk_ROR32(x, rot), csmROR32(x, rot); got != want {
			t.Fatalf("ROR32(%d,%d)=%d want %d", x, rot, got, want)
		}
	}
}

// ---- min / max / abs / sign / limit ----

func TestSilkMinMaxAbsSign(t *testing.T) {
	r := newRand()
	b16 := b16set()
	b32 := b32set()
	b64 := b64set()
	for _, a := range b32 {
		for _, b := range b32 {
			if got, want := sm.Silk_min_32(a, b), csmMin32(a, b); got != want {
				t.Fatalf("min_32(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_max_32(a, b), csmMax32(a, b); got != want {
				t.Fatalf("max_32(%d,%d)=%d want %d", a, b, got, want)
			}
			// int form; values stay in int32 range so the C int (32-bit) agrees.
			if got, want := sm.Silk_min_int(int(a), int(b)), csmMinInt(int(a), int(b)); got != want {
				t.Fatalf("min_int(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_max_int(int(a), int(b)), csmMaxInt(int(a), int(b)); got != want {
				t.Fatalf("max_int(%d,%d)=%d want %d", a, b, got, want)
			}
		}
		if got, want := sm.Silk_abs(a), csmAbs(a); got != want {
			t.Fatalf("abs(%d)=%d want %d", a, got, want)
		}
		if got, want := sm.Silk_abs_int32(a), csmAbsInt32(a); got != want {
			t.Fatalf("abs_int32(%d)=%d want %d", a, got, want)
		}
		if got, want := sm.Silk_sign(a), csmSign(a); got != want {
			t.Fatalf("sign(%d)=%d want %d", a, got, want)
		}
	}
	for _, a := range b16 {
		for _, b := range b16 {
			if got, want := int32(sm.Silk_min_16(a, b)), csmMin16(int32(a), int32(b)); got != want {
				t.Fatalf("min_16(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := int32(sm.Silk_max_16(a, b)), csmMax16(int32(a), int32(b)); got != want {
				t.Fatalf("max_16(%d,%d)=%d want %d", a, b, got, want)
			}
		}
	}
	for _, a := range b64 {
		for _, b := range b64 {
			if got, want := sm.Silk_min_64(a, b), csmMin64(a, b); got != want {
				t.Fatalf("min_64(%d,%d)=%d want %d", a, b, got, want)
			}
			if got, want := sm.Silk_max_64(a, b), csmMax64(a, b); got != want {
				t.Fatalf("max_64(%d,%d)=%d want %d", a, b, got, want)
			}
		}
		if got, want := sm.Silk_abs_int64(a), csmAbsInt64(a); got != want {
			t.Fatalf("abs_int64(%d)=%d want %d", a, got, want)
		}
	}
	// LIMIT_32 / LIMIT_int / LIMIT_16 over boundary triples and random.
	for _, a := range b32 {
		for _, l1 := range []int32{-1000, 0, 1000, math.MinInt32, math.MaxInt32} {
			for _, l2 := range []int32{-1000, 0, 1000, math.MinInt32, math.MaxInt32} {
				if got, want := sm.Silk_LIMIT_32(a, l1, l2), csmLIMIT_32(a, l1, l2); got != want {
					t.Fatalf("LIMIT_32(%d,%d,%d)=%d want %d", a, l1, l2, got, want)
				}
				if got, want := sm.Silk_LIMIT_int(int(a), int(l1), int(l2)), csmLIMIT_int(int(a), int(l1), int(l2)); got != want {
					t.Fatalf("LIMIT_int(%d,%d,%d)=%d want %d", a, l1, l2, got, want)
				}
			}
		}
	}
	for _, a := range b16 {
		for _, l1 := range []int16{-1000, 0, 1000, math.MinInt16, math.MaxInt16} {
			for _, l2 := range []int16{-1000, 0, 1000, math.MinInt16, math.MaxInt16} {
				if got, want := int32(sm.Silk_LIMIT_16(a, l1, l2)), csmLIMIT_16(int32(a), int32(l1), int32(l2)); got != want {
					t.Fatalf("LIMIT_16(%d,%d,%d)=%d want %d", a, l1, l2, got, want)
				}
			}
		}
	}
	for i := 0; i < 300000; i++ {
		a := int32(r.Uint32())
		l1 := int32(r.Uint32())
		l2 := int32(r.Uint32())
		if got, want := sm.Silk_LIMIT_32(a, l1, l2), csmLIMIT_32(a, l1, l2); got != want {
			t.Fatalf("LIMIT_32(%d,%d,%d)=%d want %d", a, l1, l2, got, want)
		}
	}
}

// ---- silk_RAND (LCG chain + boundary seeds) ----

func TestSilkRAND(t *testing.T) {
	// Chain the generator for many steps, matching C at every step.
	seed := int32(0)
	for i := 0; i < 1000000; i++ {
		g := sm.Silk_RAND(seed)
		c := csmRAND(seed)
		if g != c {
			t.Fatalf("RAND step %d seed=%d: got %d want %d", i, seed, g, c)
		}
		seed = g
	}
	for _, s := range b32set() {
		if got, want := sm.Silk_RAND(s), csmRAND(s); got != want {
			t.Fatalf("RAND(%d)=%d want %d", s, got, want)
		}
	}
}

// ---- Inlines.h + log2lin.c + lin2log.c ----

func TestSilkSqrtApprox(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := sm.Silk_SQRT_APPROX(x), csmSQRTApprox(x); got != want {
			t.Fatalf("SQRT_APPROX(%d)=%d want %d", x, got, want)
		}
	}
	limit := int32(1 << 20)
	if testing.Short() {
		limit = 1 << 16
	}
	for x := int32(0); x <= limit; x++ {
		check(x)
	}
	// Perfect squares and their neighbours across the range.
	for k := int64(1); k <= 46340; k++ { // 46340^2 < 2^31
		sq := k * k
		check(int32(sq))
		check(int32(sq - 1))
		check(int32(sq + 1))
	}
	for _, x := range b32set() {
		check(x)
	}
	n := 1000000
	if testing.Short() {
		n = 100000
	}
	for i := 0; i < n; i++ {
		check(int32(r.Uint32()))
	}
}

func TestSilkLog2Lin(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := sm.Silk_log2lin(x), csmLog2lin(x); got != want {
			t.Fatalf("log2lin(%d)=%d want %d", x, got, want)
		}
	}
	// Exhaustive over [-300, 4200]: covers the <0, [0,2047], [2048,3966] and
	// >=3967 branches, every value.
	for x := int32(-300); x <= 4200; x++ {
		check(x)
	}
	for _, x := range b32set() {
		check(x)
	}
	for i := 0; i < 500000; i++ {
		check(int32(r.Uint32()))
	}
}

func TestSilkLin2Log(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := sm.Silk_lin2log(x), csmLin2log(x); got != want {
			t.Fatalf("lin2log(%d)=%d want %d", x, got, want)
		}
	}
	// Dense low range plus zero, boundary, and full-range random (incl. negatives:
	// CLZ_FRAC is defined for all int32).
	for x := int32(0); x <= 1<<16; x++ {
		check(x)
	}
	for _, x := range b32set() {
		check(x)
	}
	for i := 0; i < 700000; i++ {
		check(int32(r.Uint32()))
	}
}

func TestSilkCLZFrac(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		glz, gfrac := sm.Silk_CLZ_FRAC(x)
		if glz != csmCLZFracLz(x) || gfrac != csmCLZFracFrac(x) {
			t.Fatalf("CLZ_FRAC(%d)=(%d,%d) want (%d,%d)", x, glz, gfrac, csmCLZFracLz(x), csmCLZFracFrac(x))
		}
	}
	for _, x := range b32set() {
		check(x)
	}
	check(0)
	for i := 0; i < 500000; i++ {
		check(int32(r.Uint32()))
	}
}

func TestSilkVarQDivInverse(t *testing.T) {
	r := newRand()
	// silk_abs(INT32_MIN) wraps negative, making CLZ32-1 == -1, which the varQ
	// functions feed to silk_LSHIFT as a negative shift (UB in C, panic in Go).
	// silk never hits that; exclude INT32_MIN, and keep Qres <= 30 so the
	// internal shift amounts stay in [0,31] (see the derivation in the test that
	// the LSHIFT_SAT32/RSHIFT arms never shift by >= 32 there).
	notMin := func(x int32) bool { return x != math.MinInt32 }
	var aset, bset []int32
	for _, v := range b32set() {
		if notMin(v) {
			aset = append(aset, v)
			if v != 0 {
				bset = append(bset, v)
			}
		}
	}
	qresDiv := []int{0, 1, 2, 5, 8, 10, 14, 16, 20, 24, 29, 30}
	qresInv := []int{1, 2, 5, 8, 10, 14, 16, 20, 24, 29, 30}
	for _, Qres := range qresDiv {
		for _, a := range aset {
			for _, b := range bset {
				if got, want := sm.Silk_DIV32_varQ(a, b, Qres), csmDIV32varQ(a, b, Qres); got != want {
					t.Fatalf("DIV32_varQ(%d,%d,%d)=%d want %d", a, b, Qres, got, want)
				}
			}
		}
	}
	for _, Qres := range qresInv {
		for _, b := range bset {
			if got, want := sm.Silk_INVERSE32_varQ(b, Qres), csmINVERSE32varQ(b, Qres); got != want {
				t.Fatalf("INVERSE32_varQ(%d,%d)=%d want %d", b, Qres, got, want)
			}
		}
	}
	for i := 0; i < 400000; i++ {
		a := int32(r.Uint32())
		b := int32(r.Uint32())
		if !notMin(a) || !notMin(b) || b == 0 {
			continue
		}
		Qres := qresDiv[r.IntN(len(qresDiv))]
		if got, want := sm.Silk_DIV32_varQ(a, b, Qres), csmDIV32varQ(a, b, Qres); got != want {
			t.Fatalf("DIV32_varQ(%d,%d,%d)=%d want %d", a, b, Qres, got, want)
		}
		Qi := qresInv[r.IntN(len(qresInv))]
		if got, want := sm.Silk_INVERSE32_varQ(b, Qi), csmINVERSE32varQ(b, Qi); got != want {
			t.Fatalf("INVERSE32_varQ(%d,%d)=%d want %d", b, Qi, got, want)
		}
	}
}

// TestSilkMathMutation is the non-vacuity check: the differential comparison must
// have teeth. For a sample of ops the correct Go value matches the oracle, while
// a deliberately mutated Go value (result+1) must differ for at least one input,
// proving the wrappers exercise real, varying values rather than constants.
func TestSilkMathMutation(t *testing.T) {
	b32 := b32set()
	mutantCaught := 0
	correctOK := 0
	for _, a := range b32 {
		for _, b := range b32 {
			want := csmSMULWW(a, b)
			if sm.Silk_SMULWW(a, b) == want {
				correctOK++
			}
			if sm.Silk_SMULWW(a, b)+1 != want {
				mutantCaught++
			}
		}
	}
	if correctOK == 0 {
		t.Fatal("mutation check vacuous: SMULWW never matched the oracle at all")
	}
	if mutantCaught == 0 {
		t.Fatal("mutation check vacuous: mutated SMULWW never differed from the oracle")
	}
	// Same for an inline approximation.
	caughtInline := false
	for _, x := range b32 {
		if sm.Silk_lin2log(x)+1 != csmLin2log(x) {
			caughtInline = true
			break
		}
	}
	if !caughtInline {
		t.Fatal("mutation check vacuous: mutated lin2log never differed from the oracle")
	}
}
