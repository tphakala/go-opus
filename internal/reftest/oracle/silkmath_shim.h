//go:build refc

/*
 * silkmath_shim.h - C-callable wrappers over the pinned libopus SILK
 * fixed-point macros/functions (silk/macros.h, silk/SigProc_FIX.h,
 * silk/Inlines.h, silk/log2lin.c, silk/lin2log.c) for the internal/silkmath
 * differential test.
 *
 * Every wrapper is a `static inline` function so this header can be pulled into
 * the silkmath_cgo.go preamble on its own, with no extra .c translation unit
 * (cgo can call statics defined in the preamble). The package-level cgo CFLAGS
 * in oracle_cgo.go already define FIXED_POINT + DISABLE_FLOAT_API and set the
 * include paths; SigProc_FIX.h pulls in macros.h + Inlines.h, and the two
 * non-inline symbols silk_log2lin / silk_lin2log are linked in via
 * w_silk_log2lin.c / w_silk_lin2log.c. OPUS_FAST_INT64 is asserted by shim.c,
 * so the SMUL / SMLA macros here take their int64 branch (matching the Go port).
 *
 * This file is separate from shim.h/shim.c and the other *_shim.h by design; it
 * never edits the shared oracle surface. 16-bit-valued inputs are passed as
 * int32_t and narrowed to (opus_int16)/(opus_int8) where the Go signature uses
 * int16/int8, so both sides see the same domain.
 */
#ifndef GOOPUS_SILKMATH_SHIM_H
#define GOOPUS_SILKMATH_SHIM_H

#include <stdint.h>
#include "libopus/silk/SigProc_FIX.h" /* macros.h + Inlines.h + typedef.h */

/* ------------------------------------------------------------------ */
/* macros.h: OPUS_FAST_INT64 SMUL / SMLA forms.                        */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_SMULWB(int32_t a, int32_t b) { return silk_SMULWB(a, b); }
static inline int32_t oracle_sm_SMLAWB(int32_t a, int32_t b, int32_t c) { return silk_SMLAWB(a, b, c); }
static inline int32_t oracle_sm_SMULWT(int32_t a, int32_t b) { return silk_SMULWT(a, b); }
static inline int32_t oracle_sm_SMLAWT(int32_t a, int32_t b, int32_t c) { return silk_SMLAWT(a, b, c); }
static inline int32_t oracle_sm_SMULBB(int32_t a, int32_t b) { return silk_SMULBB(a, b); }
static inline int32_t oracle_sm_SMLABB(int32_t a, int32_t b, int32_t c) { return silk_SMLABB(a, b, c); }
static inline int32_t oracle_sm_SMULBT(int32_t a, int32_t b) { return silk_SMULBT(a, b); }
static inline int32_t oracle_sm_SMLABT(int32_t a, int32_t b, int32_t c) { return silk_SMLABT(a, b, c); }
static inline int32_t oracle_sm_SMULWW(int32_t a, int32_t b) { return silk_SMULWW(a, b); }
static inline int32_t oracle_sm_SMLAWW(int32_t a, int32_t b, int32_t c) { return silk_SMLAWW(a, b, c); }
static inline int32_t oracle_sm_SMULTT(int32_t a, int32_t b) { return silk_SMULTT(a, b); }
static inline int32_t oracle_sm_SMLATT(int32_t a, int32_t b, int32_t c) { return silk_SMLATT(a, b, c); }
static inline int64_t oracle_sm_SMLAL(int64_t a, int32_t b, int32_t c) { return silk_SMLAL(a, b, c); }
static inline int64_t oracle_sm_SMLALBB(int64_t a, int32_t b, int32_t c) { return silk_SMLALBB(a, (opus_int16)b, (opus_int16)c); }
static inline int64_t oracle_sm_SMULL(int32_t a, int32_t b) { return silk_SMULL(a, b); }
static inline int32_t oracle_sm_SMMUL(int32_t a, int32_t b) { return silk_SMMUL(a, b); }

/* ------------------------------------------------------------------ */
/* macros.h: plain multiply / multiply-accumulate.                     */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_MUL(int32_t a, int32_t b) { return silk_MUL(a, b); }
static inline uint32_t oracle_sm_MUL_uint(uint32_t a, uint32_t b) { return silk_MUL_uint(a, b); }
static inline int32_t oracle_sm_MLA(int32_t a, int32_t b, int32_t c) { return silk_MLA(a, b, c); }
static inline uint32_t oracle_sm_MLA_uint(uint32_t a, uint32_t b, uint32_t c) { return silk_MLA_uint(a, b, c); }
static inline int32_t oracle_sm_MLA_ovflw(int32_t a, int32_t b, int32_t c) { return silk_MLA_ovflw(a, b, c); }
static inline int32_t oracle_sm_SMLABB_ovflw(int32_t a, int32_t b, int32_t c) { return silk_SMLABB_ovflw(a, b, c); }

/* ------------------------------------------------------------------ */
/* macros.h: add/subtract.                                             */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_ADD16(int32_t a, int32_t b) { return silk_ADD16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_ADD32(int32_t a, int32_t b) { return silk_ADD32(a, b); }
static inline int64_t oracle_sm_ADD64(int64_t a, int64_t b) { return silk_ADD64(a, b); }
static inline int32_t oracle_sm_SUB16(int32_t a, int32_t b) { return silk_SUB16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_SUB32(int32_t a, int32_t b) { return silk_SUB32(a, b); }
static inline int64_t oracle_sm_SUB64(int64_t a, int64_t b) { return silk_SUB64(a, b); }
static inline int32_t oracle_sm_ADD32_ovflw(int32_t a, int32_t b) { return silk_ADD32_ovflw(a, b); }
static inline int32_t oracle_sm_SUB32_ovflw(int32_t a, int32_t b) { return silk_SUB32_ovflw(a, b); }

/* ------------------------------------------------------------------ */
/* macros.h: saturating add/subtract.                                  */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_ADD_SAT16(int32_t a, int32_t b) { return silk_ADD_SAT16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_SUB_SAT16(int32_t a, int32_t b) { return silk_SUB_SAT16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_ADD_SAT32(int32_t a, int32_t b) { return silk_ADD_SAT32(a, b); }
static inline int32_t oracle_sm_SUB_SAT32(int32_t a, int32_t b) { return silk_SUB_SAT32(a, b); }
static inline int64_t oracle_sm_ADD_SAT64(int64_t a, int64_t b) { return silk_ADD_SAT64(a, b); }
static inline int64_t oracle_sm_SUB_SAT64(int64_t a, int64_t b) { return silk_SUB_SAT64(a, b); }

/* ------------------------------------------------------------------ */
/* macros.h: shifts.                                                   */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_LSHIFT8(int32_t a, int s) { return silk_LSHIFT8((opus_int8)a, s); }
static inline int32_t oracle_sm_LSHIFT16(int32_t a, int s) { return silk_LSHIFT16((opus_int16)a, s); }
static inline int32_t oracle_sm_LSHIFT32(int32_t a, int s) { return silk_LSHIFT32(a, s); }
static inline int64_t oracle_sm_LSHIFT64(int64_t a, int s) { return silk_LSHIFT64(a, s); }
static inline int32_t oracle_sm_LSHIFT(int32_t a, int s) { return silk_LSHIFT(a, s); }
static inline int32_t oracle_sm_RSHIFT8(int32_t a, int s) { return silk_RSHIFT8((opus_int8)a, s); }
static inline int32_t oracle_sm_RSHIFT16(int32_t a, int s) { return silk_RSHIFT16((opus_int16)a, s); }
static inline int32_t oracle_sm_RSHIFT32(int32_t a, int s) { return silk_RSHIFT32(a, s); }
static inline int64_t oracle_sm_RSHIFT64(int64_t a, int s) { return silk_RSHIFT64(a, s); }
static inline int32_t oracle_sm_RSHIFT(int32_t a, int s) { return silk_RSHIFT(a, s); }
static inline int32_t oracle_sm_LSHIFT_SAT32(int32_t a, int s) { return silk_LSHIFT_SAT32(a, s); }
static inline int32_t oracle_sm_LSHIFT_ovflw(int32_t a, int s) { return silk_LSHIFT_ovflw(a, s); }
static inline uint32_t oracle_sm_LSHIFT_uint(uint32_t a, int s) { return silk_LSHIFT_uint(a, s); }
static inline uint32_t oracle_sm_RSHIFT_uint(uint32_t a, int s) { return silk_RSHIFT_uint(a, s); }
static inline int32_t oracle_sm_ADD_LSHIFT(int32_t a, int32_t b, int s) { return silk_ADD_LSHIFT(a, b, s); }
static inline int32_t oracle_sm_ADD_LSHIFT32(int32_t a, int32_t b, int s) { return silk_ADD_LSHIFT32(a, b, s); }
static inline uint32_t oracle_sm_ADD_LSHIFT_uint(uint32_t a, uint32_t b, int s) { return silk_ADD_LSHIFT_uint(a, b, s); }
static inline int32_t oracle_sm_ADD_RSHIFT(int32_t a, int32_t b, int s) { return silk_ADD_RSHIFT(a, b, s); }
static inline int32_t oracle_sm_ADD_RSHIFT32(int32_t a, int32_t b, int s) { return silk_ADD_RSHIFT32(a, b, s); }
static inline uint32_t oracle_sm_ADD_RSHIFT_uint(uint32_t a, uint32_t b, int s) { return silk_ADD_RSHIFT_uint(a, b, s); }
static inline int32_t oracle_sm_SUB_LSHIFT32(int32_t a, int32_t b, int s) { return silk_SUB_LSHIFT32(a, b, s); }
static inline int32_t oracle_sm_SUB_RSHIFT32(int32_t a, int32_t b, int s) { return silk_SUB_RSHIFT32(a, b, s); }
static inline int32_t oracle_sm_RSHIFT_ROUND(int32_t a, int s) { return silk_RSHIFT_ROUND(a, s); }
static inline int64_t oracle_sm_RSHIFT_ROUND64(int64_t a, int s) { return silk_RSHIFT_ROUND64(a, s); }

/* ------------------------------------------------------------------ */
/* macros.h: divide, saturation.                                       */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_DIV32_16(int32_t a, int32_t b) { return silk_DIV32_16(a, b); }
static inline int32_t oracle_sm_DIV32(int32_t a, int32_t b) { return silk_DIV32(a, b); }
static inline int32_t oracle_sm_SAT8(int32_t a) { return silk_SAT8(a); }
static inline int32_t oracle_sm_SAT16(int32_t a) { return silk_SAT16(a); }
static inline int32_t oracle_sm_SAT32(int32_t a) { return silk_SAT32(a); }

/* ------------------------------------------------------------------ */
/* macros.h / SigProc_FIX.h: CLZ, rotate.                              */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_CLZ16(int32_t a) { return silk_CLZ16((opus_int16)a); }
static inline int32_t oracle_sm_CLZ32(int32_t a) { return silk_CLZ32(a); }
static inline int32_t oracle_sm_ROR32(int32_t a, int rot) { return silk_ROR32(a, rot); }

/* ------------------------------------------------------------------ */
/* macros.h: min/max, abs, sign, limit.                                */
/* ------------------------------------------------------------------ */

static inline int oracle_sm_min_int(int a, int b) { return silk_min_int(a, b); }
static inline int32_t oracle_sm_min_16(int32_t a, int32_t b) { return silk_min_16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_min_32(int32_t a, int32_t b) { return silk_min_32(a, b); }
static inline int64_t oracle_sm_min_64(int64_t a, int64_t b) { return silk_min_64(a, b); }
static inline int oracle_sm_max_int(int a, int b) { return silk_max_int(a, b); }
static inline int32_t oracle_sm_max_16(int32_t a, int32_t b) { return silk_max_16((opus_int16)a, (opus_int16)b); }
static inline int32_t oracle_sm_max_32(int32_t a, int32_t b) { return silk_max_32(a, b); }
static inline int64_t oracle_sm_max_64(int64_t a, int64_t b) { return silk_max_64(a, b); }
static inline int32_t oracle_sm_LIMIT_32(int32_t a, int32_t l1, int32_t l2) { return silk_LIMIT_32(a, l1, l2); }
static inline int oracle_sm_LIMIT_int(int a, int l1, int l2) { return silk_LIMIT_int(a, l1, l2); }
static inline int32_t oracle_sm_LIMIT_16(int32_t a, int32_t l1, int32_t l2) { return silk_LIMIT_16((opus_int16)a, (opus_int16)l1, (opus_int16)l2); }
static inline int32_t oracle_sm_abs(int32_t a) { return silk_abs(a); }
static inline int32_t oracle_sm_abs_int32(int32_t a) { return silk_abs_int32(a); }
static inline int64_t oracle_sm_abs_int64(int64_t a) { return silk_abs_int64(a); }
static inline int32_t oracle_sm_sign(int32_t a) { return silk_sign(a); }

/* ------------------------------------------------------------------ */
/* macros.h: pseudo-random generator.                                  */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_RAND(int32_t seed) { return silk_RAND(seed); }

/* ------------------------------------------------------------------ */
/* Inlines.h: CLZ64, CLZ_FRAC, SQRT_APPROX, DIV32_varQ, INVERSE32_varQ */
/* log2lin.c / lin2log.c.                                              */
/* silk_CLZ_FRAC returns two values via pointers; split into two       */
/* wrappers (lz and frac_Q7) so the Go side can compare each.          */
/* ------------------------------------------------------------------ */

static inline int32_t oracle_sm_CLZ64(int64_t a) { return silk_CLZ64(a); }
static inline int32_t oracle_sm_CLZ_FRAC_lz(int32_t x) {
    opus_int32 lz, frac;
    silk_CLZ_FRAC(x, &lz, &frac);
    return lz;
}
static inline int32_t oracle_sm_CLZ_FRAC_frac(int32_t x) {
    opus_int32 lz, frac;
    silk_CLZ_FRAC(x, &lz, &frac);
    return frac;
}
static inline int32_t oracle_sm_SQRT_APPROX(int32_t x) { return silk_SQRT_APPROX(x); }
static inline int32_t oracle_sm_DIV32_varQ(int32_t a, int32_t b, int Qres) { return silk_DIV32_varQ(a, b, Qres); }
static inline int32_t oracle_sm_INVERSE32_varQ(int32_t b, int Qres) { return silk_INVERSE32_varQ(b, Qres); }
static inline int32_t oracle_sm_log2lin(int32_t x) { return silk_log2lin(x); }
static inline int32_t oracle_sm_lin2log(int32_t x) { return silk_lin2log(x); }

/* ------------------------------------------------------------------ */
/* Row batch for the exhaustive int16 x int16 grid of silk_SMULBB:     */
/* fills out[0..65535] with SMULBB(a, b) for b = -32768..32767         */
/* (out index = b + 32768). Keeps the 2^32-pair sweep to 65536 cgo     */
/* calls instead of 2^32.                                              */
/* ------------------------------------------------------------------ */

static inline void oracle_sm_row_SMULBB(int32_t a, int32_t *out) {
    int i;
    for (i = 0; i < 65536; i++) {
        out[i] = silk_SMULBB(a, i - 32768);
    }
}

#endif /* GOOPUS_SILKMATH_SHIM_H */
