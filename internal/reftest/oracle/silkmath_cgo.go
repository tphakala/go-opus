//go:build refc

package oracle

/*
#include "silkmath_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK fixed-point macros/functions (via
// the static-inline wrappers in silkmath_shim.h) as plain Go functions so
// silkmath_test.go can compare the pure-Go internal/silkmath port against the C
// oracle without importing "C" itself. All names mirror the C macro/function
// they wrap, prefixed with csm (matching the oracle_sm_ C wrappers and avoiding
// collisions with the fixedmath cWrappers in this same package).

// ---- macros.h: SMUL / SMLA (OPUS_FAST_INT64 forms) ----

func csmSMULWB(a, b int32) int32 { return int32(C.oracle_sm_SMULWB(C.int32_t(a), C.int32_t(b))) }
func csmSMLAWB(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLAWB(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULWT(a, b int32) int32 { return int32(C.oracle_sm_SMULWT(C.int32_t(a), C.int32_t(b))) }
func csmSMLAWT(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLAWT(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULBB(a, b int32) int32 { return int32(C.oracle_sm_SMULBB(C.int32_t(a), C.int32_t(b))) }
func csmSMLABB(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLABB(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULBT(a, b int32) int32 { return int32(C.oracle_sm_SMULBT(C.int32_t(a), C.int32_t(b))) }
func csmSMLABT(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLABT(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULWW(a, b int32) int32 { return int32(C.oracle_sm_SMULWW(C.int32_t(a), C.int32_t(b))) }
func csmSMLAWW(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLAWW(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULTT(a, b int32) int32 { return int32(C.oracle_sm_SMULTT(C.int32_t(a), C.int32_t(b))) }
func csmSMLATT(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLATT(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMLAL(a int64, b, c int32) int64 {
	return int64(C.oracle_sm_SMLAL(C.int64_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMLALBB(a int64, b, c int16) int64 {
	return int64(C.oracle_sm_SMLALBB(C.int64_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMULL(a, b int32) int64 { return int64(C.oracle_sm_SMULL(C.int32_t(a), C.int32_t(b))) }
func csmSMMUL(a, b int32) int32 { return int32(C.oracle_sm_SMMUL(C.int32_t(a), C.int32_t(b))) }

// ---- macros.h: plain multiply / MLA ----

func csmMUL(a, b int32) int32 { return int32(C.oracle_sm_MUL(C.int32_t(a), C.int32_t(b))) }
func csmMUL_uint(a, b uint32) uint32 {
	return uint32(C.oracle_sm_MUL_uint(C.uint32_t(a), C.uint32_t(b)))
}
func csmMLA(a, b, c int32) int32 {
	return int32(C.oracle_sm_MLA(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmMLA_uint(a, b, c uint32) uint32 {
	return uint32(C.oracle_sm_MLA_uint(C.uint32_t(a), C.uint32_t(b), C.uint32_t(c)))
}
func csmMLA_ovflw(a, b, c int32) int32 {
	return int32(C.oracle_sm_MLA_ovflw(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}
func csmSMLABB_ovflw(a, b, c int32) int32 {
	return int32(C.oracle_sm_SMLABB_ovflw(C.int32_t(a), C.int32_t(b), C.int32_t(c)))
}

// ---- macros.h: add/subtract ----

func csmADD16(a, b int32) int32 { return int32(C.oracle_sm_ADD16(C.int32_t(a), C.int32_t(b))) }
func csmADD32(a, b int32) int32 { return int32(C.oracle_sm_ADD32(C.int32_t(a), C.int32_t(b))) }
func csmADD64(a, b int64) int64 { return int64(C.oracle_sm_ADD64(C.int64_t(a), C.int64_t(b))) }
func csmSUB16(a, b int32) int32 { return int32(C.oracle_sm_SUB16(C.int32_t(a), C.int32_t(b))) }
func csmSUB32(a, b int32) int32 { return int32(C.oracle_sm_SUB32(C.int32_t(a), C.int32_t(b))) }
func csmSUB64(a, b int64) int64 { return int64(C.oracle_sm_SUB64(C.int64_t(a), C.int64_t(b))) }
func csmADD32_ovflw(a, b int32) int32 {
	return int32(C.oracle_sm_ADD32_ovflw(C.int32_t(a), C.int32_t(b)))
}
func csmSUB32_ovflw(a, b int32) int32 {
	return int32(C.oracle_sm_SUB32_ovflw(C.int32_t(a), C.int32_t(b)))
}

// ---- macros.h: saturating add/subtract ----

func csmADD_SAT16(a, b int32) int32 { return int32(C.oracle_sm_ADD_SAT16(C.int32_t(a), C.int32_t(b))) }
func csmSUB_SAT16(a, b int32) int32 { return int32(C.oracle_sm_SUB_SAT16(C.int32_t(a), C.int32_t(b))) }
func csmADD_SAT32(a, b int32) int32 { return int32(C.oracle_sm_ADD_SAT32(C.int32_t(a), C.int32_t(b))) }
func csmSUB_SAT32(a, b int32) int32 { return int32(C.oracle_sm_SUB_SAT32(C.int32_t(a), C.int32_t(b))) }
func csmADD_SAT64(a, b int64) int64 { return int64(C.oracle_sm_ADD_SAT64(C.int64_t(a), C.int64_t(b))) }
func csmSUB_SAT64(a, b int64) int64 { return int64(C.oracle_sm_SUB_SAT64(C.int64_t(a), C.int64_t(b))) }

// ---- macros.h: shifts ----

func csmLSHIFT8(a int32, s int) int32  { return int32(C.oracle_sm_LSHIFT8(C.int32_t(a), C.int(s))) }
func csmLSHIFT16(a int32, s int) int32 { return int32(C.oracle_sm_LSHIFT16(C.int32_t(a), C.int(s))) }
func csmLSHIFT32(a int32, s int) int32 { return int32(C.oracle_sm_LSHIFT32(C.int32_t(a), C.int(s))) }
func csmLSHIFT64(a int64, s int) int64 { return int64(C.oracle_sm_LSHIFT64(C.int64_t(a), C.int(s))) }
func csmLSHIFT(a int32, s int) int32   { return int32(C.oracle_sm_LSHIFT(C.int32_t(a), C.int(s))) }
func csmRSHIFT8(a int32, s int) int32  { return int32(C.oracle_sm_RSHIFT8(C.int32_t(a), C.int(s))) }
func csmRSHIFT16(a int32, s int) int32 { return int32(C.oracle_sm_RSHIFT16(C.int32_t(a), C.int(s))) }
func csmRSHIFT32(a int32, s int) int32 { return int32(C.oracle_sm_RSHIFT32(C.int32_t(a), C.int(s))) }
func csmRSHIFT64(a int64, s int) int64 { return int64(C.oracle_sm_RSHIFT64(C.int64_t(a), C.int(s))) }
func csmRSHIFT(a int32, s int) int32   { return int32(C.oracle_sm_RSHIFT(C.int32_t(a), C.int(s))) }
func csmLSHIFT_SAT32(a int32, s int) int32 {
	return int32(C.oracle_sm_LSHIFT_SAT32(C.int32_t(a), C.int(s)))
}
func csmLSHIFT_ovflw(a int32, s int) int32 {
	return int32(C.oracle_sm_LSHIFT_ovflw(C.int32_t(a), C.int(s)))
}
func csmLSHIFT_uint(a uint32, s int) uint32 {
	return uint32(C.oracle_sm_LSHIFT_uint(C.uint32_t(a), C.int(s)))
}
func csmRSHIFT_uint(a uint32, s int) uint32 {
	return uint32(C.oracle_sm_RSHIFT_uint(C.uint32_t(a), C.int(s)))
}
func csmADD_LSHIFT(a, b int32, s int) int32 {
	return int32(C.oracle_sm_ADD_LSHIFT(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmADD_LSHIFT32(a, b int32, s int) int32 {
	return int32(C.oracle_sm_ADD_LSHIFT32(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmADD_LSHIFT_uint(a, b uint32, s int) uint32 {
	return uint32(C.oracle_sm_ADD_LSHIFT_uint(C.uint32_t(a), C.uint32_t(b), C.int(s)))
}
func csmADD_RSHIFT(a, b int32, s int) int32 {
	return int32(C.oracle_sm_ADD_RSHIFT(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmADD_RSHIFT32(a, b int32, s int) int32 {
	return int32(C.oracle_sm_ADD_RSHIFT32(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmADD_RSHIFT_uint(a, b uint32, s int) uint32 {
	return uint32(C.oracle_sm_ADD_RSHIFT_uint(C.uint32_t(a), C.uint32_t(b), C.int(s)))
}
func csmSUB_LSHIFT32(a, b int32, s int) int32 {
	return int32(C.oracle_sm_SUB_LSHIFT32(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmSUB_RSHIFT32(a, b int32, s int) int32 {
	return int32(C.oracle_sm_SUB_RSHIFT32(C.int32_t(a), C.int32_t(b), C.int(s)))
}
func csmRSHIFT_ROUND(a int32, s int) int32 {
	return int32(C.oracle_sm_RSHIFT_ROUND(C.int32_t(a), C.int(s)))
}
func csmRSHIFT_ROUND64(a int64, s int) int64 {
	return int64(C.oracle_sm_RSHIFT_ROUND64(C.int64_t(a), C.int(s)))
}

// ---- macros.h: divide, saturation ----

func csmDIV32_16(a, b int32) int32 { return int32(C.oracle_sm_DIV32_16(C.int32_t(a), C.int32_t(b))) }
func csmDIV32(a, b int32) int32    { return int32(C.oracle_sm_DIV32(C.int32_t(a), C.int32_t(b))) }
func csmSAT8(a int32) int32        { return int32(C.oracle_sm_SAT8(C.int32_t(a))) }
func csmSAT16(a int32) int32       { return int32(C.oracle_sm_SAT16(C.int32_t(a))) }
func csmSAT32(a int32) int32       { return int32(C.oracle_sm_SAT32(C.int32_t(a))) }

// ---- macros.h / SigProc_FIX.h: CLZ, rotate ----

func csmCLZ16(a int32) int32 { return int32(C.oracle_sm_CLZ16(C.int32_t(a))) }
func csmCLZ32(a int32) int32 { return int32(C.oracle_sm_CLZ32(C.int32_t(a))) }
func csmROR32(a int32, rot int) int32 {
	return int32(C.oracle_sm_ROR32(C.int32_t(a), C.int(rot)))
}

// ---- macros.h: min/max, abs, sign, limit ----

func csmMinInt(a, b int) int    { return int(C.oracle_sm_min_int(C.int(a), C.int(b))) }
func csmMin16(a, b int32) int32 { return int32(C.oracle_sm_min_16(C.int32_t(a), C.int32_t(b))) }
func csmMin32(a, b int32) int32 { return int32(C.oracle_sm_min_32(C.int32_t(a), C.int32_t(b))) }
func csmMin64(a, b int64) int64 { return int64(C.oracle_sm_min_64(C.int64_t(a), C.int64_t(b))) }
func csmMaxInt(a, b int) int    { return int(C.oracle_sm_max_int(C.int(a), C.int(b))) }
func csmMax16(a, b int32) int32 { return int32(C.oracle_sm_max_16(C.int32_t(a), C.int32_t(b))) }
func csmMax32(a, b int32) int32 { return int32(C.oracle_sm_max_32(C.int32_t(a), C.int32_t(b))) }
func csmMax64(a, b int64) int64 { return int64(C.oracle_sm_max_64(C.int64_t(a), C.int64_t(b))) }
func csmLIMIT_32(a, l1, l2 int32) int32 {
	return int32(C.oracle_sm_LIMIT_32(C.int32_t(a), C.int32_t(l1), C.int32_t(l2)))
}
func csmLIMIT_int(a, l1, l2 int) int {
	return int(C.oracle_sm_LIMIT_int(C.int(a), C.int(l1), C.int(l2)))
}
func csmLIMIT_16(a, l1, l2 int32) int32 {
	return int32(C.oracle_sm_LIMIT_16(C.int32_t(a), C.int32_t(l1), C.int32_t(l2)))
}
func csmAbs(a int32) int32      { return int32(C.oracle_sm_abs(C.int32_t(a))) }
func csmAbsInt32(a int32) int32 { return int32(C.oracle_sm_abs_int32(C.int32_t(a))) }
func csmAbsInt64(a int64) int64 { return int64(C.oracle_sm_abs_int64(C.int64_t(a))) }
func csmSign(a int32) int32     { return int32(C.oracle_sm_sign(C.int32_t(a))) }

// ---- macros.h: pseudo-random generator ----

func csmRAND(seed int32) int32 { return int32(C.oracle_sm_RAND(C.int32_t(seed))) }

// ---- Inlines.h / log2lin.c / lin2log.c ----

func csmCLZ64(a int64) int32     { return int32(C.oracle_sm_CLZ64(C.int64_t(a))) }
func csmCLZFracLz(x int32) int32 { return int32(C.oracle_sm_CLZ_FRAC_lz(C.int32_t(x))) }
func csmCLZFracFrac(x int32) int32 {
	return int32(C.oracle_sm_CLZ_FRAC_frac(C.int32_t(x)))
}
func csmSQRTApprox(x int32) int32 { return int32(C.oracle_sm_SQRT_APPROX(C.int32_t(x))) }
func csmDIV32varQ(a, b int32, Qres int) int32 {
	return int32(C.oracle_sm_DIV32_varQ(C.int32_t(a), C.int32_t(b), C.int(Qres)))
}
func csmINVERSE32varQ(b int32, Qres int) int32 {
	return int32(C.oracle_sm_INVERSE32_varQ(C.int32_t(b), C.int(Qres)))
}
func csmLog2lin(x int32) int32 { return int32(C.oracle_sm_log2lin(C.int32_t(x))) }
func csmLin2log(x int32) int32 { return int32(C.oracle_sm_lin2log(C.int32_t(x))) }

// cRowSMULBB fills out (which must have length 65536) with SMULBB(a, b) for
// b = -32768..32767, out[b+32768]. One cgo call per row for the full grid.
func csmRowSMULBB(a int32, out []int32) {
	C.oracle_sm_row_SMULBB(C.int32_t(a), (*C.int32_t)(unsafe.Pointer(&out[0])))
}
