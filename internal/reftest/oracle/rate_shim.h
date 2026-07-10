//go:build refc

/*
 * rate_shim.h - C-callable wrappers over the pinned libopus CELT bit-allocation
 * (celt/rate.c: clt_compute_allocation, interp_bits2pulses; celt/rate.h:
 * get_pulses/bits2pulses/pulses2bits; celt/celt.c: init_caps) for the
 * internal/celt differential test.
 *
 * clt_compute_allocation interleaves the range coder: it encodes (encode=1) or
 * decodes (encode=0) the skip/intensity/dual-stereo flags WHILE computing the
 * budget. To exercise the Go port both ways against C over the SAME bitstream,
 * oracle_rate_alloc runs one direction over a caller-supplied buffer: encode=1
 * emits the flags into buf via an ec_enc; encode=0 reads them back via an ec_dec.
 * The test runs C-encode to produce the ground-truth bytes, then decodes them
 * with both C and Go, and also runs Go-encode to confirm byte-identical output.
 *
 * The CELTMode comes from opus_custom_mode_create(48000, 960, NULL), which the
 * pinned libopus exposes even in the non-CUSTOM_MODES build (it returns a
 * static_mode_list entry; celt/modes.c:227, and celt_decoder.c:186 relies on it).
 *
 * get_pulses/bits2pulses/pulses2bits are static OPUS_INLINE in rate.h, so they
 * compile straight into this TU. clt_compute_allocation / init_caps /
 * opus_custom_mode_create / ec_* are linked via the existing w_celt_rate.c /
 * w_celt_celt.c / w_celt_modes.c / w_celt_entenc.c / w_celt_entdec.c wrappers.
 * This file never edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from oracle_cgo.go.
 */
#ifndef GOOPUS_RATE_SHIM_H
#define GOOPUS_RATE_SHIM_H

#include <stdint.h>
#include <string.h>
#include "arch.h"    /* opus_int32, CELTMode via modes.h */
#include "modes.h"   /* CELTMode struct */
#include "rate.h"    /* clt_compute_allocation, get_pulses, bits2pulses, pulses2bits */
#include "celt.h"    /* init_caps */
#include "entenc.h"  /* ec_enc, ec_enc_init, ec_enc_done */
#include "entdec.h"  /* ec_dec, ec_dec_init */
#include "entcode.h" /* ec_ctx, ec_tell */

/* opus_custom_mode_create is declared in opus_custom.h (public custom API); the
 * non-CUSTOM_MODES build still defines it (celt/modes.c). Forward-declare it here
 * to avoid pulling the public custom header; CELTMode == OpusCustomMode. */
extern CELTMode *opus_custom_mode_create(opus_int32 Fs, int frame_size, int *error);

/* oracle_rate_mode() returns the pinned 48 kHz / 960 mode (or NULL on failure). */
static const CELTMode *oracle_rate_mode(void)
{
   static const CELTMode *m = NULL;
   if (m == NULL)
      m = opus_custom_mode_create(48000, 960, NULL);
   return m;
}

/* oracle_rate_mode_info reports the mode dimensions the Go test needs. */
static void oracle_rate_mode_info(int *nbEBands, int *nbAllocVectors, int *effEBands)
{
   const CELTMode *m = oracle_rate_mode();
   *nbEBands = m->nbEBands;
   *nbAllocVectors = m->nbAllocVectors;
   *effEBands = m->effEBands;
}

/* oracle_rate_init_caps fills cap[0..nbEBands) via init_caps. */
static void oracle_rate_init_caps(int LM, int C, int *cap)
{
   init_caps(oracle_rate_mode(), cap, LM, C);
}

/*
 * oracle_rate_alloc runs clt_compute_allocation once over buf. encode!=0 emits
 * the skip/intensity/dual flags into buf via an ec_enc (finalized with
 * ec_enc_done); encode==0 reads them back via an ec_dec. intensity_in /
 * dual_stereo_in seed *intensity / *dual_stereo (read on encode, overwritten on
 * decode). Outputs pulses/ebits/fine_priority (length nbEBands), the resolved
 * intensity/dual_stereo, balance, the coder's ec_tell and rng captured BEFORE
 * ec_enc_done (so encode and decode end-state are comparable), and returns
 * codedBands.
 */
static int oracle_rate_alloc(int start, int end, const int *offsets, const int *cap,
    int alloc_trim, int intensity_in, int dual_stereo_in, int total, int C, int LM,
    int encode, int prev, int signalBandwidth, unsigned char *buf, int nbytes,
    int *pulses, int *ebits, int *fine_priority, int *intensity_out,
    int *dual_stereo_out, int *balance_out, int *tell_out, uint32_t *rng_out)
{
   const CELTMode *m = oracle_rate_mode();
   int intensity = intensity_in;
   int dual_stereo = dual_stereo_in;
   opus_int32 balance = 0;
   int codedBands;
   ec_ctx ec;

   if (encode) {
      memset(buf, 0, nbytes);
      ec_enc_init(&ec, buf, nbytes);
      codedBands = clt_compute_allocation(m, start, end, offsets, cap, alloc_trim,
          &intensity, &dual_stereo, (opus_int32)total, &balance, pulses, ebits,
          fine_priority, C, LM, &ec, 1, prev, signalBandwidth);
      *tell_out = ec_tell(&ec);
      *rng_out = ec.rng;
      ec_enc_done(&ec);
   } else {
      ec_dec_init(&ec, buf, nbytes);
      codedBands = clt_compute_allocation(m, start, end, offsets, cap, alloc_trim,
          &intensity, &dual_stereo, (opus_int32)total, &balance, pulses, ebits,
          fine_priority, C, LM, &ec, 0, prev, signalBandwidth);
      *tell_out = ec_tell(&ec);
      *rng_out = ec.rng;
   }
   *intensity_out = intensity;
   *dual_stereo_out = dual_stereo;
   *balance_out = (int)balance;
   return codedBands;
}

/* Direct cache-helper wrappers (rate.h static inlines). */
static int oracle_get_pulses(int i) { return get_pulses(i); }
static int oracle_bits2pulses(int band, int LM, int bits) { return bits2pulses(oracle_rate_mode(), band, LM, bits); }
static int oracle_pulses2bits(int band, int LM, int pulses) { return pulses2bits(oracle_rate_mode(), band, LM, pulses); }

#endif /* GOOPUS_RATE_SHIM_H */
