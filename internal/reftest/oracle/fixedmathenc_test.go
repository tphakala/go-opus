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
