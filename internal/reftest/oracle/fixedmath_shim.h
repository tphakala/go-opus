//go:build refc

/*
 * fixedmath_shim.h - C-callable wrappers over the pinned libopus fixed-point
 * math macros/functions (celt/fixed_generic.h, celt/mathops.h/.c) for the
 * internal/fixedmath differential test.
 *
 * Every wrapper is a `static inline` function so this header can be pulled into
 * the fixedmath_cgo.go preamble on its own, with no extra .c translation unit
 * (cgo can call statics defined in the preamble). The package-level cgo CFLAGS
 * in oracle_cgo.go already define FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64 and set the include paths, and the non-inline mathops.c
 * symbols (isqrt32, celt_rsqrt_norm, celt_sqrt, celt_cos_norm, celt_rcp,
 * frac_div32[_q29]) are linked in via w_celt_mathops.c.
 *
 * This file is separate from shim.h/shim.c by design; it never edits the shared
 * oracle surface.
 */
#ifndef GOOPUS_FIXEDMATH_SHIM_H
#define GOOPUS_FIXEDMATH_SHIM_H

#include <stdint.h>
#include "arch.h"     /* fixed_generic.h macros, MIN16/MAX16, ABS*, typedefs */
#include "mathops.h"  /* celt_ilog2/zlog2/log2/exp2*, celt_sqrt, celt_rcp, ... */
#include "entcode.h"  /* celt_udiv, celt_sudiv */

/* ------------------------------------------------------------------ */
/* Scalar wrappers: one per fixed_generic.h macro.                     */
/* 16-bit-valued inputs are passed as int32_t; the macros that expect  */
/* a val16 cast it themselves (MULT16_16, ADD16, ...). SHR16/SHL16/    */
/* NEG16 do NOT cast, so those wrappers narrow to opus_val16 first to  */
/* match the Go int16 domain.                                          */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_fm_SHR16(int32_t a, int s) { return SHR16((opus_val16)a, s); }
static inline int32_t oracle_fm_SHL16(int32_t a, int s) { return SHL16((opus_val16)a, s); }
static inline int32_t oracle_fm_SHR32(int32_t a, int s) { return SHR32(a, s); }
static inline int32_t oracle_fm_SHL32(int32_t a, int s) { return SHL32(a, s); }
static inline int32_t oracle_fm_PSHR32(int32_t a, int s) { return PSHR32(a, s); }
static inline int32_t oracle_fm_VSHR32(int32_t a, int s) { return VSHR32(a, s); }
static inline int32_t oracle_fm_SHL32_ovflw(int32_t a, int s) { return SHL32_ovflw(a, s); }
static inline int32_t oracle_fm_PSHR32_ovflw(int32_t a, int s) { return PSHR32_ovflw(a, s); }
static inline int32_t oracle_fm_ROUND16(int32_t x, int a) { return ROUND16(x, a); }

static inline int32_t oracle_fm_SATURATE16(int32_t x) { return SATURATE16(x); }
static inline int32_t oracle_fm_SATURATE(int32_t x, int32_t a) { return SATURATE(x, a); }

static inline int32_t oracle_fm_ADD16(int32_t a, int32_t b) { return ADD16(a, b); }
static inline int32_t oracle_fm_SUB16(int32_t a, int32_t b) { return SUB16(a, b); }
static inline int32_t oracle_fm_ADD32(int32_t a, int32_t b) { return ADD32(a, b); }
static inline int32_t oracle_fm_SUB32(int32_t a, int32_t b) { return SUB32(a, b); }
static inline int32_t oracle_fm_ADD32_ovflw(int32_t a, int32_t b) { return ADD32_ovflw(a, b); }
static inline int32_t oracle_fm_SUB32_ovflw(int32_t a, int32_t b) { return SUB32_ovflw(a, b); }
static inline int32_t oracle_fm_NEG16(int32_t a) { return NEG16((opus_val16)a); }
static inline int32_t oracle_fm_NEG32(int32_t a) { return NEG32(a); }

static inline int32_t oracle_fm_MULT16_16(int32_t a, int32_t b) { return MULT16_16(a, b); }
static inline int32_t oracle_fm_MULT16_16SU(int32_t a, int32_t b) { return MULT16_16SU(a, b); }
static inline int32_t oracle_fm_MAC16_16(int32_t c, int32_t a, int32_t b) { return MAC16_16(c, a, b); }
static inline int32_t oracle_fm_MULT16_16_Q11(int32_t a, int32_t b) { return MULT16_16_Q11(a, b); }
static inline int32_t oracle_fm_MULT16_16_Q13(int32_t a, int32_t b) { return MULT16_16_Q13(a, b); }
static inline int32_t oracle_fm_MULT16_16_Q14(int32_t a, int32_t b) { return MULT16_16_Q14(a, b); }
static inline int32_t oracle_fm_MULT16_16_Q15(int32_t a, int32_t b) { return MULT16_16_Q15(a, b); }
static inline int32_t oracle_fm_MULT16_16_P13(int32_t a, int32_t b) { return MULT16_16_P13(a, b); }
static inline int32_t oracle_fm_MULT16_16_P14(int32_t a, int32_t b) { return MULT16_16_P14(a, b); }
static inline int32_t oracle_fm_MULT16_16_P15(int32_t a, int32_t b) { return MULT16_16_P15(a, b); }

static inline int32_t oracle_fm_MULT16_32_Q15(int32_t a, int32_t b) { return MULT16_32_Q15(a, b); }
static inline int32_t oracle_fm_MULT16_32_Q16(int32_t a, int32_t b) { return MULT16_32_Q16(a, b); }
static inline int32_t oracle_fm_MULT16_32_P16(int32_t a, int32_t b) { return MULT16_32_P16(a, b); }
static inline int32_t oracle_fm_MULT32_32_Q31(int32_t a, int32_t b) { return MULT32_32_Q31(a, b); }
static inline int32_t oracle_fm_MULT32_32_P31(int32_t a, int32_t b) { return MULT32_32_P31(a, b); }
static inline int32_t oracle_fm_MAC16_32_Q15(int32_t c, int32_t a, int32_t b) { return MAC16_32_Q15(c, a, b); }
static inline int32_t oracle_fm_MAC16_32_Q16(int32_t c, int32_t a, int32_t b) { return MAC16_32_Q16(c, a, b); }

static inline int32_t oracle_fm_QCONST16(double x, int bits) { return QCONST16(x, bits); }
static inline int32_t oracle_fm_QCONST32(double x, int bits) { return QCONST32(x, bits); }

static inline uint32_t oracle_fm_celt_udiv(uint32_t n, uint32_t d) { return celt_udiv(n, d); }
static inline int32_t oracle_fm_celt_sudiv(int32_t n, int32_t d) { return celt_sudiv(n, d); }

/* ------------------------------------------------------------------ */
/* mathops.h / mathops.c wrappers.                                     */
/* EC_ILOG resolves to __builtin_clz on this build (UB for 0); callers */
/* pass x>0 only. ec_ilog() is not compiled under EC_CLZ, so there is  */
/* no wrapper for it (Go's Ec_ilog == EC_ILOG == bits.Len32).          */
/* ------------------------------------------------------------------ */

static inline int oracle_fm_EC_ILOG(uint32_t x) { return EC_ILOG(x); }
static inline int oracle_fm_celt_ilog2(int32_t x) { return celt_ilog2(x); }
static inline int oracle_fm_celt_zlog2(int32_t x) { return celt_zlog2(x); }
static inline uint32_t oracle_fm_isqrt32(uint32_t x) { return isqrt32(x); }
static inline int32_t oracle_fm_celt_rsqrt_norm(int32_t x) { return celt_rsqrt_norm(x); }
static inline int32_t oracle_fm_celt_sqrt(int32_t x) { return celt_sqrt(x); }
static inline int32_t oracle_fm_celt_cos_norm(int32_t x) { return celt_cos_norm(x); }
static inline int32_t oracle_fm_celt_rcp(int32_t x) { return celt_rcp(x); }
static inline int32_t oracle_fm_celt_exp2_frac(int32_t x) { return celt_exp2_frac((opus_val16)x); }
static inline int32_t oracle_fm_celt_exp2(int32_t x) { return celt_exp2((opus_val16)x); }
static inline int32_t oracle_fm_celt_log2(int32_t x) { return celt_log2(x); }
static inline int32_t oracle_fm_frac_div32(int32_t a, int32_t b) { return frac_div32(a, b); }
static inline int32_t oracle_fm_frac_div32_q29(int32_t a, int32_t b) { return frac_div32_q29(a, b); }

/* ------------------------------------------------------------------ */
/* Row batch for the exhaustive int16 x int16 grid: fills out[0..65535] */
/* with op(a, b) for b = -32768..32767 (out index = b + 32768). Keeps   */
/* the 4.3e9-pair sweep to 65536 cgo calls instead of 4.3e9.            */
/* ------------------------------------------------------------------ */

enum {
    FM_ROW_MULT16_16 = 0,
    FM_ROW_MULT16_16_Q11,
    FM_ROW_MULT16_16_Q13,
    FM_ROW_MULT16_16_Q14,
    FM_ROW_MULT16_16_Q15,
    FM_ROW_MULT16_16_P13,
    FM_ROW_MULT16_16_P14,
    FM_ROW_MULT16_16_P15
};

static inline void oracle_fm_row16(int op, int32_t a, int32_t *out) {
    int i;
    for (i = 0; i < 65536; i++) {
        int32_t b = i - 32768;
        int32_t r;
        switch (op) {
        case FM_ROW_MULT16_16:     r = MULT16_16(a, b); break;
        case FM_ROW_MULT16_16_Q11: r = MULT16_16_Q11(a, b); break;
        case FM_ROW_MULT16_16_Q13: r = MULT16_16_Q13(a, b); break;
        case FM_ROW_MULT16_16_Q14: r = MULT16_16_Q14(a, b); break;
        case FM_ROW_MULT16_16_Q15: r = MULT16_16_Q15(a, b); break;
        case FM_ROW_MULT16_16_P13: r = MULT16_16_P13(a, b); break;
        case FM_ROW_MULT16_16_P14: r = MULT16_16_P14(a, b); break;
        case FM_ROW_MULT16_16_P15: r = MULT16_16_P15(a, b); break;
        default: r = 0; break;
        }
        out[i] = r;
    }
}

#endif /* GOOPUS_FIXEDMATH_SHIM_H */
