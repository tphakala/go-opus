//go:build refc

package oracle

// Differential tests for the encoder-only additions to internal/fixedmath: the
// scalar CELT square-root and reciprocal approximations celt_sqrt32 and
// celt_rcp_norm32 (celt/mathops.c), asserted bit-exact against the pinned
// FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64 libopus oracle. Companion to
// fixedmath_test.go (shares its input generators and PRNG); wrappers live in
// fixedmathenc_shim.h / fixedmathenc_cgo.go.

import (
	"math"
	"testing"

	fm "github.com/tphakala/go-opus/internal/fixedmath"
)

// ---- mathops: sqrt32 ----

func TestFMSqrt32(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := fm.Celt_sqrt32(x), cCeltSqrt32(x); got != want {
			t.Fatalf("celt_sqrt32(%d)=%d want %d", x, got, want)
		}
	}
	// Special cases: 0 -> 0, >=2^30 -> 2^31-1. The k<12 vs k>=12 split inside the
	// [1,2^30) range is exercised by the dense/random sweeps below.
	check(0)
	check(1073741824) // 2^30, smallest saturating input
	check(1073741823) // 2^30-1, largest non-saturating input
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

// ---- mathops: rcp_norm32 ----

func TestFMRcpNorm32(t *testing.T) {
	r := newRand()
	check := func(x int32) {
		if got, want := fm.Celt_rcp_norm32(x), cCeltRcpNorm32(x); got != want {
			t.Fatalf("celt_rcp_norm32(%d)=%d want %d", x, got, want)
		}
	}
	// Documented domain: Q31 input in [0.5, 1.0) == [2^30, 2^31). Below 2^30 the
	// C celt_sig_assert would fire, so the test stays inside the valid range.
	const lo = int32(1073741824) // 2^30, smallest valid input (0.5)
	check(lo)
	check(math.MaxInt32) // ~1.0, largest valid input
	for _, x := range b32set() {
		if x >= lo {
			check(x)
		}
	}
	// Sweep the 2^15 distinct SHR32(x,15) reciprocal classes at their low edge,
	// low edge + 1, and high edge, so every celt_rcp_norm16 input is exercised.
	for k := int32(0); k < (1 << 15); k++ {
		base := lo + (k << 15)
		check(base)
		check(base + 1)
		check(base + (1<<15 - 1))
	}
	// Dense random across the whole domain [2^30, 2^31).
	for i := 0; i < 2000000; i++ {
		check(lo + int32(r.Uint32()&0x3fffffff))
	}
}

// ---- fixed_generic.h scalar macros the CELT encoder front half added ----
// These are exercised only transitively by transient_analysis / patch_transient,
// so pin them directly against the C macros (fixed_generic.h:141/200/203). The
// internal/celt package keeps its own local sround16/div32/div32_16 copies that
// must match these; a divergence here means one of the two families drifted.

func TestFMSround16(t *testing.T) {
	r := newRand()
	sawHi, sawLo, sawMid := false, false, false
	check := func(x int32, a int) {
		got, want := fm.SROUND16(x, a), cSround16(x, a)
		if got != want {
			t.Fatalf("SROUND16(%d,%d)=%d want %d", x, a, got, want)
		}
		switch got {
		case 32767:
			sawHi = true
		case -32767:
			sawLo = true
		default:
			sawMid = true
		}
	}
	for a := 0; a <= 16; a++ {
		// PSHR32 rounding bias, then the asymmetric SATURATE(.,32767) edges.
		for _, base := range []int32{0, 1, -1, 32766, 32767, 32768, -32766, -32767, -32768, 65535, -65535} {
			check(base<<a, a)
			check((base<<a)+(int32(1)<<a)>>1, a)
			check((base<<a)-1, a)
		}
		check(math.MaxInt32, a)
		check(math.MinInt32, a)
		for i := 0; i < 50000; i++ {
			check(int32(r.Uint32()), a)
		}
	}
	if !sawHi || !sawLo || !sawMid {
		t.Fatalf("coverage: sawHi=%v sawLo=%v sawMid=%v (want the +/-32767 saturation edges and the pass-through all exercised)", sawHi, sawLo, sawMid)
	}
}

func TestFMDiv32(t *testing.T) {
	r := newRand()
	sawNegTrunc := false
	check := func(a, b int32) {
		if b == 0 || (a == math.MinInt32 && b == -1) { // b==0 undefined; INT_MIN/-1 is C UB
			return
		}
		got, want := fm.DIV32(a, b), cDiv32(a, b)
		if got != want {
			t.Fatalf("DIV32(%d,%d)=%d want %d", a, b, got, want)
		}
		if a < 0 && b > 0 && a%b != 0 {
			sawNegTrunc = true
		}
	}
	// Truncation toward zero in both signs (e.g. -7/2 == -3, not -4).
	for _, p := range [][2]int32{{7, 2}, {-7, 2}, {7, -2}, {-7, -2}, {1, 1}, {-1, 1}, {math.MaxInt32, 1}, {math.MinInt32, 1}, {math.MaxInt32, -1}, {5, 3}, {-5, 3}} {
		check(p[0], p[1])
	}
	if fm.DIV32(-7, 2) != -3 {
		t.Fatalf("DIV32(-7,2)=%d want -3 (truncate toward zero)", fm.DIV32(-7, 2))
	}
	for i := 0; i < 1000000; i++ {
		check(int32(r.Uint32()), int32(r.Uint32()))
	}
	if !sawNegTrunc {
		t.Fatal("coverage: no negative-operand truncating division exercised")
	}
}

func TestFMDiv3216(t *testing.T) {
	r := newRand()
	sawNarrow := false
	check := func(a int32, b int16) {
		if b == 0 || (a == math.MinInt32 && b == -1) {
			return
		}
		got, want := fm.DIV32_16(a, b), cDiv3216(a, b)
		if got != want {
			t.Fatalf("DIV32_16(%d,%d)=%d want %d", a, b, got, want)
		}
		if q := a / int32(b); q > 32767 || q < -32768 {
			sawNarrow = true // the int16 narrowing of the quotient is exercised
		}
	}
	for _, p := range []struct {
		a int32
		b int16
	}{{7, 2}, {-7, 2}, {7, -2}, {-7, -2}, {math.MaxInt32, 1}, {math.MinInt32, 1}, {math.MaxInt32, 2}, {1 << 20, 3}, {-(1 << 20), 3}} {
		check(p.a, p.b)
	}
	if fm.DIV32_16(-7, 2) != -3 {
		t.Fatalf("DIV32_16(-7,2)=%d want -3", fm.DIV32_16(-7, 2))
	}
	for i := 0; i < 1000000; i++ {
		check(int32(r.Uint32()), int16(r.Uint32()))
	}
	if !sawNarrow {
		t.Fatal("coverage: no quotient exceeding int16 range (narrowing not exercised)")
	}
}
