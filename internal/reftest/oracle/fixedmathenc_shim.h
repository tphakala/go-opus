//go:build refc

/*
 * fixedmathenc_shim.h - C-callable wrappers for the encoder-only additions to
 * the CELT fixed-point math layer, for the internal/fixedmath differential
 * test. Covers celt_sqrt32 and celt_rcp_norm32 (celt/mathops.c), the two scalar
 * approximations the frozen-config (no ENABLE_QEXT) CELT encoder reaches through
 * compute_band_energies, normalise_bands and stereo_itheta but the decoder never
 * needed.
 *
 * Mirrors fixedmath_shim.h and is kept as a parallel file so the shared oracle
 * surface (shim.h/shim.c) and the existing fixedmath oracle files stay untouched.
 * Both wrapped symbols are non-inline mathops.c functions compiled under
 * FIXED_POINT and linked via w_celt_mathops.c, exactly like celt_sqrt/celt_rcp
 * already are, so this header includes the declaring headers only; no .c include
 * is needed.
 */
#ifndef GOOPUS_FIXEDMATHENC_SHIM_H
#define GOOPUS_FIXEDMATHENC_SHIM_H

#include <stdint.h>
#include "arch.h"     /* fixed_generic.h macros, typedefs */
#include "mathops.h"  /* celt_sqrt32, celt_rcp_norm32 declarations */

static inline int32_t oracle_fme_celt_sqrt32(int32_t x) { return celt_sqrt32(x); }
static inline int32_t oracle_fme_celt_rcp_norm32(int32_t x) { return celt_rcp_norm32(x); }

#endif /* GOOPUS_FIXEDMATHENC_SHIM_H */
