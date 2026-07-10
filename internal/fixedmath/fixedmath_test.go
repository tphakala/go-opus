package fixedmath

import "testing"

// These pure-Go sanity checks give `go test ./internal/fixedmath/...` (and
// -race) real coverage without cgo. The authoritative bit-exactness proof is the
// differential test against the C oracle in internal/reftest/oracle
// (fixedmath_test.go, build tag refc).

func TestShiftBasics(t *testing.T) {
	// SHL32 wraps through uint32 (documented trap): 0x40000000 << 1 == INT32_MIN.
	if got := SHL32(0x40000000, 1); got != -2147483648 {
		t.Errorf("SHL32(0x40000000,1)=%d want %d", got, -2147483648)
	}
	// Signed >> is arithmetic.
	if got := SHR32(-256, 4); got != -16 {
		t.Errorf("SHR32(-256,4)=%d want -16", got)
	}
	// PSHR32 rounds to nearest.
	if got := PSHR32(5, 1); got != 3 { // (5+1)>>1
		t.Errorf("PSHR32(5,1)=%d want 3", got)
	}
	// PSHR32 addend overflow: (INT32_MAX + 1) wraps to INT32_MIN, >>1.
	if got := PSHR32(2147483647, 1); got != -1073741824 {
		t.Errorf("PSHR32(MaxInt32,1)=%d want -1073741824", got)
	}
	// VSHR32 with negative shift shifts left.
	if got := VSHR32(3, -2); got != 12 {
		t.Errorf("VSHR32(3,-2)=%d want 12", got)
	}
}

func TestMultBasics(t *testing.T) {
	if got := MULT16_16(-3, 7); got != -21 {
		t.Errorf("MULT16_16(-3,7)=%d want -21", got)
	}
	if got := MULT16_16_Q15(16384, 16384); got != 8192 { // 0.5*0.5 = 0.25 in Q15
		t.Errorf("MULT16_16_Q15(16384,16384)=%d want 8192", got)
	}
	// MULT16_32_Q15 uses the int64 form: 16384 (0.5 Q15) * 2^30 >> 15 = 2^29.
	if got := MULT16_32_Q15(16384, 1<<30); got != 1<<29 {
		t.Errorf("MULT16_32_Q15(16384,2^30)=%d want %d", got, 1<<29)
	}
	if got := MULT32_32_Q31(1<<30, 1<<30); got != 1<<29 {
		t.Errorf("MULT32_32_Q31(2^30,2^30)=%d want %d", got, 1<<29)
	}
}

func TestIlogAndSqrt(t *testing.T) {
	cases := []struct {
		x    int32
		ilog int
	}{{1, 0}, {2, 1}, {3, 1}, {4, 2}, {255, 7}, {256, 8}, {0x7fffffff, 30}}
	for _, c := range cases {
		if got := Celt_ilog2(c.x); got != c.ilog {
			t.Errorf("Celt_ilog2(%d)=%d want %d", c.x, got, c.ilog)
		}
	}
	if got := Celt_zlog2(0); got != 0 {
		t.Errorf("Celt_zlog2(0)=%d want 0", got)
	}
	// isqrt32 exact floor sqrt.
	for _, x := range []uint32{1, 2, 3, 4, 8, 9, 15, 16, 1000000, 0xfffffffe} {
		want := uint32(0)
		for (want+1)*(want+1) <= x && (want+1) <= 65535 {
			want++
		}
		if got := Isqrt32(x); got != want {
			t.Errorf("Isqrt32(%d)=%d want %d", x, got, want)
		}
	}
}

func TestUdiv(t *testing.T) {
	if got := Celt_udiv(100, 7); got != 14 {
		t.Errorf("Celt_udiv(100,7)=%d want 14", got)
	}
	if got := Celt_sudiv(-100, 7); got != -14 { // truncation toward zero
		t.Errorf("Celt_sudiv(-100,7)=%d want -14", got)
	}
}
