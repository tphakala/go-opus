//go:build refc

package oracle

/*
#include "fixedmath_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus fixed-point macros/functions (via the
// static-inline wrappers in fixedmath_shim.h) as plain Go functions so
// fixedmath_test.go can compare the pure-Go internal/fixedmath port against the
// C oracle without importing "C" itself. All names mirror the C macro/function
// they wrap, prefixed with a lowercase c.

// ---- fixed_generic.h scalar wrappers ----

func cSHR16(a int32, s int) int32  { return int32(C.oracle_fm_SHR16(C.int32_t(a), C.int(s))) }
func cSHL16(a int32, s int) int32  { return int32(C.oracle_fm_SHL16(C.int32_t(a), C.int(s))) }
func cSHR32(a int32, s int) int32  { return int32(C.oracle_fm_SHR32(C.int32_t(a), C.int(s))) }
func cSHL32(a int32, s int) int32  { return int32(C.oracle_fm_SHL32(C.int32_t(a), C.int(s))) }
func cPSHR32(a int32, s int) int32 { return int32(C.oracle_fm_PSHR32(C.int32_t(a), C.int(s))) }
func cVSHR32(a int32, s int) int32 { return int32(C.oracle_fm_VSHR32(C.int32_t(a), C.int(s))) }
func cSHL32_ovflw(a int32, s int) int32 {
	return int32(C.oracle_fm_SHL32_ovflw(C.int32_t(a), C.int(s)))
}
func cPSHR32_ovflw(a int32, s int) int32 {
	return int32(C.oracle_fm_PSHR32_ovflw(C.int32_t(a), C.int(s)))
}
func cROUND16(x int32, a int) int32 { return int32(C.oracle_fm_ROUND16(C.int32_t(x), C.int(a))) }

func cSATURATE16(x int32) int32  { return int32(C.oracle_fm_SATURATE16(C.int32_t(x))) }
func cSATURATE(x, a int32) int32 { return int32(C.oracle_fm_SATURATE(C.int32_t(x), C.int32_t(a))) }

func cADD16(a, b int32) int32 { return int32(C.oracle_fm_ADD16(C.int32_t(a), C.int32_t(b))) }
func cSUB16(a, b int32) int32 { return int32(C.oracle_fm_SUB16(C.int32_t(a), C.int32_t(b))) }
func cADD32(a, b int32) int32 { return int32(C.oracle_fm_ADD32(C.int32_t(a), C.int32_t(b))) }
func cSUB32(a, b int32) int32 { return int32(C.oracle_fm_SUB32(C.int32_t(a), C.int32_t(b))) }
func cADD32_ovflw(a, b int32) int32 {
	return int32(C.oracle_fm_ADD32_ovflw(C.int32_t(a), C.int32_t(b)))
}
func cSUB32_ovflw(a, b int32) int32 {
	return int32(C.oracle_fm_SUB32_ovflw(C.int32_t(a), C.int32_t(b)))
}
func cNEG16(a int32) int32 { return int32(C.oracle_fm_NEG16(C.int32_t(a))) }
func cNEG32(a int32) int32 { return int32(C.oracle_fm_NEG32(C.int32_t(a))) }

func cMULT16_16(a, b int32) int32 { return int32(C.oracle_fm_MULT16_16(C.int32_t(a), C.int32_t(b))) }
func cMULT16_16SU(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16SU(C.int32_t(a), C.int32_t(b)))
}
func cMAC16_16(c, a, b int32) int32 {
	return int32(C.oracle_fm_MAC16_16(C.int32_t(c), C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_Q11(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_Q11(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_Q13(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_Q13(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_Q14(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_Q14(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_Q15(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_Q15(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_P13(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_P13(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_P14(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_P14(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_16_P15(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_16_P15(C.int32_t(a), C.int32_t(b)))
}

func cMULT16_32_Q15(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_32_Q15(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_32_Q16(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_32_Q16(C.int32_t(a), C.int32_t(b)))
}
func cMULT16_32_P16(a, b int32) int32 {
	return int32(C.oracle_fm_MULT16_32_P16(C.int32_t(a), C.int32_t(b)))
}
func cMULT32_32_Q31(a, b int32) int32 {
	return int32(C.oracle_fm_MULT32_32_Q31(C.int32_t(a), C.int32_t(b)))
}
func cMULT32_32_P31(a, b int32) int32 {
	return int32(C.oracle_fm_MULT32_32_P31(C.int32_t(a), C.int32_t(b)))
}
func cMAC16_32_Q15(c, a, b int32) int32 {
	return int32(C.oracle_fm_MAC16_32_Q15(C.int32_t(c), C.int32_t(a), C.int32_t(b)))
}
func cMAC16_32_Q16(c, a, b int32) int32 {
	return int32(C.oracle_fm_MAC16_32_Q16(C.int32_t(c), C.int32_t(a), C.int32_t(b)))
}

func cQCONST16(x float64, bits int) int32 {
	return int32(C.oracle_fm_QCONST16(C.double(x), C.int(bits)))
}
func cQCONST32(x float64, bits int) int32 {
	return int32(C.oracle_fm_QCONST32(C.double(x), C.int(bits)))
}

func cCeltUdiv(n, d uint32) uint32 {
	return uint32(C.oracle_fm_celt_udiv(C.uint32_t(n), C.uint32_t(d)))
}
func cCeltSudiv(n, d int32) int32 {
	return int32(C.oracle_fm_celt_sudiv(C.int32_t(n), C.int32_t(d)))
}

// ---- mathops.h / mathops.c wrappers ----

func cEC_ILOG(x uint32) int        { return int(C.oracle_fm_EC_ILOG(C.uint32_t(x))) }
func cCeltIlog2(x int32) int       { return int(C.oracle_fm_celt_ilog2(C.int32_t(x))) }
func cCeltZlog2(x int32) int       { return int(C.oracle_fm_celt_zlog2(C.int32_t(x))) }
func cIsqrt32(x uint32) uint32     { return uint32(C.oracle_fm_isqrt32(C.uint32_t(x))) }
func cCeltRsqrtNorm(x int32) int32 { return int32(C.oracle_fm_celt_rsqrt_norm(C.int32_t(x))) }
func cCeltSqrt(x int32) int32      { return int32(C.oracle_fm_celt_sqrt(C.int32_t(x))) }
func cCeltCosNorm(x int32) int32   { return int32(C.oracle_fm_celt_cos_norm(C.int32_t(x))) }
func cCeltRcp(x int32) int32       { return int32(C.oracle_fm_celt_rcp(C.int32_t(x))) }
func cCeltExp2Frac(x int32) int32  { return int32(C.oracle_fm_celt_exp2_frac(C.int32_t(x))) }
func cCeltExp2(x int32) int32      { return int32(C.oracle_fm_celt_exp2(C.int32_t(x))) }
func cCeltLog2(x int32) int32      { return int32(C.oracle_fm_celt_log2(C.int32_t(x))) }
func cFracDiv32(a, b int32) int32  { return int32(C.oracle_fm_frac_div32(C.int32_t(a), C.int32_t(b))) }
func cFracDiv32Q29(a, b int32) int32 {
	return int32(C.oracle_fm_frac_div32_q29(C.int32_t(a), C.int32_t(b)))
}

// cRow16 fills out (which must have length 65536) with op(a, b) for
// b = -32768..32767, out[b+32768]. op is one of the FM_ROW_* constants.
func cRow16(op int, a int32, out []int32) {
	C.oracle_fm_row16(C.int(op), C.int32_t(a), (*C.int32_t)(unsafe.Pointer(&out[0])))
}

// Row op codes mirroring the enum in fixedmath_shim.h.
const (
	rowMULT16_16 = iota
	rowMULT16_16_Q11
	rowMULT16_16_Q13
	rowMULT16_16_Q14
	rowMULT16_16_Q15
	rowMULT16_16_P13
	rowMULT16_16_P14
	rowMULT16_16_P15
)
