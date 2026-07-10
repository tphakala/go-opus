package silkmath

import (
	"math"
	"testing"
)

// These pure-Go sanity checks give `go test ./internal/silkmath/...` (and
// -race) real coverage without cgo. The authoritative bit-exactness proof is the
// differential test against the C oracle in internal/reftest/oracle
// (silkmath_test.go, build tag refc).

func TestSMULBasics(t *testing.T) {
	// SMULWB: (a * int16(b)) >> 16.
	if got := Silk_SMULWB(1<<16, 3); got != 3 {
		t.Errorf("Silk_SMULWB(1<<16,3)=%d want 3", got)
	}
	if got := Silk_SMULWB(-(1 << 16), 3); got != -3 {
		t.Errorf("Silk_SMULWB(-1<<16,3)=%d want -3", got)
	}
	// The low 16 bits of b are taken as a signed int16: 0x18000 -> int16 0x8000 = -32768.
	if got, want := Silk_SMULWB(1<<16, 0x18000), int32(-32768); got != want {
		t.Errorf("Silk_SMULWB(1<<16,0x18000)=%d want %d", got, want)
	}
	// SMULWW: (a * b) >> 16.
	if got := Silk_SMULWW(1<<16, 1<<16); got != 1<<16 {
		t.Errorf("Silk_SMULWW(1<<16,1<<16)=%d want %d", got, 1<<16)
	}
	// SMULBB: int16(a) * int16(b).
	if got := Silk_SMULBB(-3, 7); got != -21 {
		t.Errorf("Silk_SMULBB(-3,7)=%d want -21", got)
	}
	// SMULTT: (a>>16)*(b>>16).
	if got := Silk_SMULTT(0x00030000, 0x00050000); got != 15 {
		t.Errorf("Silk_SMULTT(3<<16,5<<16)=%d want 15", got)
	}
	// SMMUL: (int64(a)*int64(b))>>32.
	if got := Silk_SMMUL(1<<30, 1<<30); got != 1<<28 {
		t.Errorf("Silk_SMMUL(2^30,2^30)=%d want %d", got, 1<<28)
	}
	// SMLAWB accumulates onto a.
	if got := Silk_SMLAWB(100, 1<<16, 5); got != 105 {
		t.Errorf("Silk_SMLAWB(100,1<<16,5)=%d want 105", got)
	}
}

func TestShiftAndRound(t *testing.T) {
	// LSHIFT32 wraps through uint32: 0x40000000 << 1 == INT32_MIN.
	if got := Silk_LSHIFT32(0x40000000, 1); got != math.MinInt32 {
		t.Errorf("Silk_LSHIFT32(0x40000000,1)=%d want %d", got, math.MinInt32)
	}
	// RSHIFT32 is arithmetic.
	if got := Silk_RSHIFT32(-256, 4); got != -16 {
		t.Errorf("Silk_RSHIFT32(-256,4)=%d want -16", got)
	}
	// RSHIFT_ROUND: shift==1 special case and general case.
	if got := Silk_RSHIFT_ROUND(5, 1); got != 3 {
		t.Errorf("Silk_RSHIFT_ROUND(5,1)=%d want 3", got)
	}
	if got := Silk_RSHIFT_ROUND(5, 2); got != 1 {
		t.Errorf("Silk_RSHIFT_ROUND(5,2)=%d want 1", got)
	}
	// RSHIFT_ROUND64 mirrors the 32-bit form.
	if got := Silk_RSHIFT_ROUND64(5, 1); got != 3 {
		t.Errorf("Silk_RSHIFT_ROUND64(5,1)=%d want 3", got)
	}
	// LSHIFT_SAT32 clamps before shifting so it never overflows. The positive
	// clamp is (INT32_MAX>>3)<<3 = 2147483640, not INT32_MAX itself (the low 3
	// bits are lost to the round trip).
	if got := Silk_LSHIFT_SAT32(1<<30, 3); got != 2147483640 {
		t.Errorf("Silk_LSHIFT_SAT32(2^30,3)=%d want 2147483640", got)
	}
	if got := Silk_LSHIFT_SAT32(-(1 << 30), 3); got != silk_int32_MIN {
		t.Errorf("Silk_LSHIFT_SAT32(-2^30,3)=%d want %d", got, silk_int32_MIN)
	}
}

func TestSaturatingAddSub(t *testing.T) {
	if got := Silk_ADD_SAT32(silk_int32_MAX, 1); got != silk_int32_MAX {
		t.Errorf("Silk_ADD_SAT32(MAX,1)=%d want %d", got, silk_int32_MAX)
	}
	if got := Silk_ADD_SAT32(silk_int32_MIN, -1); got != silk_int32_MIN {
		t.Errorf("Silk_ADD_SAT32(MIN,-1)=%d want %d", got, silk_int32_MIN)
	}
	if got := Silk_ADD_SAT32(100, -50); got != 50 {
		t.Errorf("Silk_ADD_SAT32(100,-50)=%d want 50", got)
	}
	if got := Silk_SUB_SAT32(silk_int32_MIN, 1); got != silk_int32_MIN {
		t.Errorf("Silk_SUB_SAT32(MIN,1)=%d want %d", got, silk_int32_MIN)
	}
	if got := Silk_SUB_SAT32(silk_int32_MAX, -1); got != silk_int32_MAX {
		t.Errorf("Silk_SUB_SAT32(MAX,-1)=%d want %d", got, silk_int32_MAX)
	}
	// ADD_SAT16 / SUB_SAT16 clamp to int16.
	if got := Silk_ADD_SAT16(30000, 30000); got != silk_int16_MAX {
		t.Errorf("Silk_ADD_SAT16(30000,30000)=%d want %d", got, silk_int16_MAX)
	}
	if got := Silk_SUB_SAT16(-30000, 30000); got != silk_int16_MIN {
		t.Errorf("Silk_SUB_SAT16(-30000,30000)=%d want %d", got, silk_int16_MIN)
	}
}

func TestCLZAndROR(t *testing.T) {
	cases32 := []struct {
		x   int32
		clz int32
	}{{0, 32}, {1, 31}, {2, 30}, {0x40000000, 1}, {-1, 0}, {math.MinInt32, 0}, {0x7fffffff, 1}}
	for _, c := range cases32 {
		if got := Silk_CLZ32(c.x); got != c.clz {
			t.Errorf("Silk_CLZ32(%d)=%d want %d", c.x, got, c.clz)
		}
	}
	cases16 := []struct {
		x   int16
		clz int32
	}{{0, 16}, {1, 15}, {2, 14}, {0x4000, 1}, {-1, 0}, {math.MinInt16, 0}, {0x7fff, 1}}
	for _, c := range cases16 {
		if got := Silk_CLZ16(c.x); got != c.clz {
			t.Errorf("Silk_CLZ16(%d)=%d want %d", c.x, got, c.clz)
		}
	}
	// ROR32 rotates right; rot==0 is identity, negative rotates left.
	if got := Silk_ROR32(0x12345678, 8); got != 0x78123456 {
		t.Errorf("Silk_ROR32(0x12345678,8)=%#x want 0x78123456", uint32(got))
	}
	if got := Silk_ROR32(0x12345678, -8); got != 0x34567812 {
		t.Errorf("Silk_ROR32(0x12345678,-8)=%#x want 0x34567812", uint32(got))
	}
	if got := Silk_ROR32(0x12345678, 0); got != 0x12345678 {
		t.Errorf("Silk_ROR32(x,0)=%#x want identity", uint32(got))
	}
}

func TestMinMaxAbsSign(t *testing.T) {
	if got := Silk_min_32(-5, 3); got != -5 {
		t.Errorf("Silk_min_32(-5,3)=%d want -5", got)
	}
	if got := Silk_max_32(-5, 3); got != 3 {
		t.Errorf("Silk_max_32(-5,3)=%d want 3", got)
	}
	if got := Silk_abs(-7); got != 7 {
		t.Errorf("Silk_abs(-7)=%d want 7", got)
	}
	// abs wraps at INT32_MIN (documented quirk of the C macro).
	if got := Silk_abs(math.MinInt32); got != math.MinInt32 {
		t.Errorf("Silk_abs(MIN)=%d want %d", got, math.MinInt32)
	}
	if got := Silk_abs_int32(-7); got != 7 {
		t.Errorf("Silk_abs_int32(-7)=%d want 7", got)
	}
	for x, want := range map[int32]int32{-9: -1, 0: 0, 42: 1} {
		if got := Silk_sign(x); got != want {
			t.Errorf("Silk_sign(%d)=%d want %d", x, got, want)
		}
	}
	// LIMIT clamps regardless of the order of the two bounds.
	if got := Silk_LIMIT_32(50, 0, 10); got != 10 {
		t.Errorf("Silk_LIMIT_32(50,0,10)=%d want 10", got)
	}
	if got := Silk_LIMIT_32(-5, 10, 0); got != 0 {
		t.Errorf("Silk_LIMIT_32(-5,10,0)=%d want 0", got)
	}
}

func TestRAND(t *testing.T) {
	// silk_RAND is a deterministic LCG; verify the first step from seed 0 and
	// that the sequence advances.
	if got := Silk_RAND(0); got != RAND_INCREMENT {
		t.Errorf("Silk_RAND(0)=%d want %d", got, RAND_INCREMENT)
	}
	seed := int32(12345)
	next := Silk_RAND(seed)
	if next == seed {
		t.Errorf("Silk_RAND did not advance the seed")
	}
	// Matches the reference LCG: increment + seed*multiplier (mod 2^32, wrapped).
	want := int32(uint32(RAND_INCREMENT) + uint32(seed)*uint32(RAND_MULTIPLIER))
	if next != want {
		t.Errorf("Silk_RAND(%d)=%d want %d", seed, next, want)
	}
}

func TestSqrtApproxBounds(t *testing.T) {
	if got := Silk_SQRT_APPROX(0); got != 0 {
		t.Errorf("Silk_SQRT_APPROX(0)=%d want 0", got)
	}
	if got := Silk_SQRT_APPROX(-100); got != 0 {
		t.Errorf("Silk_SQRT_APPROX(-100)=%d want 0", got)
	}
	// Documented accuracy: within +/-2.5% for outputs > 120. sqrt(1e8)=10000.
	got := float64(Silk_SQRT_APPROX(100000000))
	if math.Abs(got-10000)/10000 > 0.025 {
		t.Errorf("Silk_SQRT_APPROX(1e8)=%g want ~10000 (+/-2.5%%)", got)
	}
}

func TestLog2LinBoundaries(t *testing.T) {
	if got := Silk_log2lin(-1); got != 0 {
		t.Errorf("Silk_log2lin(-1)=%d want 0", got)
	}
	if got := Silk_log2lin(3967); got != silk_int32_MAX {
		t.Errorf("Silk_log2lin(3967)=%d want %d", got, silk_int32_MAX)
	}
	if got := Silk_log2lin(0); got != 1 {
		t.Errorf("Silk_log2lin(0)=%d want 1", got)
	}
	// 2^(128/128) = 2 in the linear domain; log2lin(128) should be near 2.
	if got := Silk_log2lin(128); got < 1 || got > 3 {
		t.Errorf("Silk_log2lin(128)=%d want ~2", got)
	}
	// lin2log is the near-inverse: lin2log(1)=0, lin2log(2)=128.
	if got := Silk_lin2log(1); got != 0 {
		t.Errorf("Silk_lin2log(1)=%d want 0", got)
	}
	if got := Silk_lin2log(2); got != 128 {
		t.Errorf("Silk_lin2log(2)=%d want 128", got)
	}
}

func TestInverseAndDiv(t *testing.T) {
	// INVERSE32_varQ(x, Qres) ~ (1<<Qres)/x. Check a value where it is exact-ish.
	got := Silk_INVERSE32_varQ(1<<16, 30)
	want := int32(1 << 14) // (1<<30)/(1<<16)
	if diff := got - want; diff < -2 || diff > 2 {
		t.Errorf("Silk_INVERSE32_varQ(2^16,30)=%d want ~%d", got, want)
	}
	// DIV32_varQ(a,b,Qres) ~ (a<<Qres)/b.
	gotD := Silk_DIV32_varQ(1000, 10, 8)
	wantD := int32((1000 << 8) / 10)
	if diff := gotD - wantD; diff < -256 || diff > 256 {
		t.Errorf("Silk_DIV32_varQ(1000,10,8)=%d want ~%d", gotD, wantD)
	}
	// Plain truncating divides.
	if got := Silk_DIV32_16(-100, 7); got != -14 {
		t.Errorf("Silk_DIV32_16(-100,7)=%d want -14", got)
	}
	if got := Silk_DIV32(100, 7); got != 14 {
		t.Errorf("Silk_DIV32(100,7)=%d want 14", got)
	}
}
