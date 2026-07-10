//go:build refc

/*
 * rateenc_shim.h - C-callable ENCODE-direction wrapper over the pinned libopus
 * CELT bit-allocation (celt/rate.c: clt_compute_allocation, interp_bits2pulses)
 * for Checkpoint 6 of the phase-4 encoder differential harness.
 *
 * The decode-direction oracle (rate_shim.h) already drives clt_compute_allocation
 * both ways over a shared buffer. This file is the dedicated encode-direction
 * driver: it runs clt_compute_allocation with encode=1 so the range coder EMITS
 * the skip/intensity/dual-stereo flags (rate.c:347-370, 396-418) into a
 * caller-supplied buffer via an ec_enc, then the Go test asserts the pure-Go
 * interpBits2pulses/cltComputeAllocation writes byte-identical output and the
 * same pulses/ebits/fine_priority/codedBands/balance/intensity/dual_stereo plus
 * range-coder end-state.
 *
 * All wrappers are static with unique oracle_rateenc_* names so this translation
 * unit never collides with rate_shim.h's static oracle_rate_* helpers; the two
 * headers are pulled into separate cgo files. This file never edits the shared
 * oracle surface (shim.h/shim.c/oracle_cgo.go). clt_compute_allocation /
 * init_caps / opus_custom_mode_create / ec_* are linked via the existing
 * w_celt_rate.c / w_celt_celt.c / w_celt_modes.c / w_celt_entenc.c wrappers.
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from oracle_cgo.go.
 */
#ifndef GOOPUS_RATEENC_SHIM_H
#define GOOPUS_RATEENC_SHIM_H

#include <stdint.h>
#include <string.h>
#include "arch.h"    /* opus_int32, CELTMode via modes.h */
#include "modes.h"   /* CELTMode struct */
#include "rate.h"    /* clt_compute_allocation */
#include "celt.h"    /* init_caps */
#include "entenc.h"  /* ec_enc, ec_enc_init, ec_enc_done */
#include "entcode.h" /* ec_ctx, ec_tell */

/* opus_custom_mode_create is declared in opus_custom.h (public custom API); the
 * non-CUSTOM_MODES build still defines it (celt/modes.c). Forward-declare it here
 * to avoid pulling the public custom header; CELTMode == OpusCustomMode. */
extern CELTMode *opus_custom_mode_create(opus_int32 Fs, int frame_size, int *error);

/* oracle_rateenc_mode() returns the pinned 48 kHz / 960 mode (or NULL on failure). */
static const CELTMode *oracle_rateenc_mode(void)
{
   static const CELTMode *m = NULL;
   if (m == NULL)
      m = opus_custom_mode_create(48000, 960, NULL);
   return m;
}

/* oracle_rateenc_mode_info reports the mode dimensions the Go test needs. */
static void oracle_rateenc_mode_info(int *nbEBands, int *nbAllocVectors, int *effEBands)
{
   const CELTMode *m = oracle_rateenc_mode();
   *nbEBands = m->nbEBands;
   *nbAllocVectors = m->nbAllocVectors;
   *effEBands = m->effEBands;
}

/* oracle_rateenc_init_caps fills cap[0..nbEBands) via init_caps. */
static void oracle_rateenc_init_caps(int LM, int C, int *cap)
{
   init_caps(oracle_rateenc_mode(), cap, LM, C);
}

/*
 * oracle_rateenc_alloc runs clt_compute_allocation once with encode=1 over buf.
 * It memsets buf, initializes an ec_enc, runs the allocation (emitting the
 * skip/intensity/dual flags into buf), captures ec_tell and rng BEFORE
 * ec_enc_done (so the end-state lines up with the Go Encoder.Tell()/Rng()
 * captured before EncDone), then finalizes with ec_enc_done so the full buffer is
 * byte-comparable. intensity_in / dual_stereo_in seed *intensity / *dual_stereo
 * (read on the encode path). Outputs pulses/ebits/fine_priority (length
 * nbEBands), the resolved intensity/dual_stereo, balance, tell, rng, and returns
 * codedBands.
 */
static int oracle_rateenc_alloc(int start, int end, const int *offsets, const int *cap,
    int alloc_trim, int intensity_in, int dual_stereo_in, int total, int C, int LM,
    int prev, int signalBandwidth, unsigned char *buf, int nbytes,
    int *pulses, int *ebits, int *fine_priority, int *intensity_out,
    int *dual_stereo_out, int *balance_out, int *tell_out, uint32_t *rng_out)
{
   const CELTMode *m = oracle_rateenc_mode();
   int intensity = intensity_in;
   int dual_stereo = dual_stereo_in;
   opus_int32 balance = 0;
   int codedBands;
   ec_enc ec;

   memset(buf, 0, nbytes);
   ec_enc_init(&ec, buf, nbytes);
   codedBands = clt_compute_allocation(m, start, end, offsets, cap, alloc_trim,
       &intensity, &dual_stereo, (opus_int32)total, &balance, pulses, ebits,
       fine_priority, C, LM, &ec, 1, prev, signalBandwidth);
   *tell_out = ec_tell(&ec);
   *rng_out = ec.rng;
   ec_enc_done(&ec);
   *intensity_out = intensity;
   *dual_stereo_out = dual_stereo;
   *balance_out = (int)balance;
   return codedBands;
}

#endif /* GOOPUS_RATEENC_SHIM_H */
