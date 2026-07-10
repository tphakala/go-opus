//go:build refc

/*
 * energy_shim.h - C-callable wrappers over the pinned libopus CELT band-energy
 * quantizer (celt/quant_bands.c) for the internal/celt differential test.
 *
 * The Go CELT port only implements the DECODE side (unquant_coarse_energy /
 * unquant_fine_energy / unquant_energy_finalise). To exercise it we need a coded
 * bitstream, so this shim runs the C ENCODE helpers (quant_coarse_energy and
 * friends) over caller-supplied random energies to PRODUCE the bitstream and the
 * encoder's reconstructed oldEBands, and also runs the C DECODE helpers over the
 * same bitstream so the test can cross-check Go against both. Everything is
 * static so the header can be pulled straight into the energy_cgo.go preamble
 * with no extra .c translation unit; the non-static quant_bands.c / laplace.c /
 * entenc.c / entdec.c / modes.c symbols are linked in via the existing w_celt_*.c
 * wrappers (see gen_wrappers.sh). This file never edits the shared oracle
 * surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from the package-level cgo directives in
 * oracle_cgo.go. celt_glog is opus_val32 (int32) in this build, so all energy
 * arrays are int32_t.
 *
 * The C mode is opus_custom_mode_create(48000, 960): under non-CUSTOM_MODES it
 * returns the static mode48000_960_120 (nbEBands=21) that internal/celt mirrors.
 */
#ifndef GOOPUS_ENERGY_SHIM_H
#define GOOPUS_ENERGY_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "modes.h"       /* CELTMode, opus_custom_mode_create */
#include "quant_bands.h" /* quant_/unquant_ coarse/fine/finalise */
#include "entenc.h"      /* ec_enc, ec_enc_init, ec_enc_done */
#include "entdec.h"      /* ec_dec, ec_dec_init, ec_dec_bit_logp */
#include "entcode.h"     /* ec_tell */

/* oracle_energy_mode returns the frozen static CELT mode (48 kHz, 960). */
static const CELTMode *oracle_energy_mode(void)
{
   int err = 0;
   return opus_custom_mode_create(48000, 960, &err);
}

/* oracle_energy_nbebands returns the mode's band count (21). */
static int oracle_energy_nbebands(void)
{
   return oracle_energy_mode()->nbEBands;
}

/*
 * oracle_energy_roundtrip encodes a coarse+fine+finalise energy frame with the C
 * encoder, then decodes it back with the C decoder, and reports both the
 * encoder's reconstructed oldEBands and the decoder's oldEBands so the Go test
 * can assert Go == C-decode == C-encode.
 *
 * Inputs:
 *   lm            frame-size log (0..3)
 *   channels      C (1 or 2)
 *   intra         forced intra-energy flag (0/1); pinned via force_intra with
 *                 two_pass=0 and delayedIntra=0, but the encoder still clamps it
 *                 to 0 when the budget is too tight (quant_bands.c:281), exactly
 *                 as the decoder-side intra flag would then read back as 0.
 *   nb_bytes      packet size in bytes; total bit budget is nb_bytes*8 for both
 *                 sides (matches dec->storage*8).
 *   e_bands       true band log-energies, length channels*nbEBands (Q24).
 *   old_init      initial oldEBands (prior cross-frame state), length
 *                 channels*nbEBands (Q24).
 *   fine_quant    per-band fine bit count, length nbEBands (0..MAX_FINE_BITS).
 *   fine_priority per-band finalise priority (0/1), length nbEBands.
 *
 * Outputs:
 *   buf_out       the coded packet, length nb_bytes (zero-filled first).
 *   old_enc_out   encoder's reconstructed oldEBands, length channels*nbEBands.
 *   old_dec_out   decoder's reconstructed oldEBands, length channels*nbEBands.
 *
 * Returns the intra flag actually decoded from the bitstream (0/1).
 */
static int oracle_energy_roundtrip(
    int lm, int channels, int intra, int nb_bytes,
    const int32_t *e_bands, const int32_t *old_init,
    const int32_t *fine_quant, const int32_t *fine_priority,
    unsigned char *buf_out, int32_t *old_enc_out, int32_t *old_dec_out)
{
   const CELTMode *mode = oracle_energy_mode();
   int nb = mode->nbEBands;
   int start = 0;
   int end = nb;
   int effEnd = end;
   int n = channels * nb;
   int i;
   opus_int32 budget = nb_bytes * 8;
   opus_int32 bits_left;
   opus_int32 tell;
   int intra_dec;

   /* quant_* take int* for the fine tables; copy in from int32_t to avoid any
      int/int32_t aliasing. */
   int fq[64];
   int fp[64];
   for (i = 0; i < nb; i++)
   {
      fq[i] = (int)fine_quant[i];
      fp[i] = (int)fine_priority[i];
   }

   /* Encode side: eBands (true) is const input; old_enc is the prediction base
      and gets overwritten with the reconstruction. */
   celt_glog *e_in = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *old_enc = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *err = (celt_glog *)malloc(sizeof(celt_glog) * n);
   for (i = 0; i < n; i++)
   {
      e_in[i] = e_bands[i];
      old_enc[i] = old_init[i];
      err[i] = 0;
   }

   memset(buf_out, 0, nb_bytes);

   {
      ec_enc enc;
      opus_val32 delayedIntra = 0;
      ec_enc_init(&enc, buf_out, nb_bytes);
      quant_coarse_energy(mode, start, end, effEnd, e_in, old_enc, (opus_uint32)budget,
                          err, &enc, channels, lm, nb_bytes, intra, &delayedIntra,
                          /*two_pass=*/0, /*loss_rate=*/0, /*lfe=*/0);
      quant_fine_energy(mode, start, end, old_enc, err, /*prev_quant=*/NULL, fq, &enc, channels);
      bits_left = budget - ec_tell(&enc);
      quant_energy_finalise(mode, start, end, old_enc, err, fq, fp, bits_left, &enc, channels);
      ec_enc_done(&enc);
   }

   for (i = 0; i < n; i++)
      old_enc_out[i] = old_enc[i];

   /* Decode side: seed old_dec with the SAME initial state, read the intra flag
      the way the decoder pipeline does (celt_decoder.c:1359), then run the three
      unquant_ steps over the just-produced bitstream. */
   celt_glog *old_dec = (celt_glog *)malloc(sizeof(celt_glog) * n);
   for (i = 0; i < n; i++)
      old_dec[i] = old_init[i];

   {
      ec_dec dec;
      ec_dec_init(&dec, buf_out, nb_bytes);
      tell = ec_tell(&dec);
      intra_dec = (tell + 3 <= budget) ? ec_dec_bit_logp(&dec, 3) : 0;
      unquant_coarse_energy(mode, start, end, old_dec, intra_dec, &dec, channels, lm);
      unquant_fine_energy(mode, start, end, old_dec, /*prev_quant=*/NULL, fq, &dec, channels);
      bits_left = budget - ec_tell(&dec);
      unquant_energy_finalise(mode, start, end, old_dec, fq, fp, bits_left, &dec, channels);
   }

   for (i = 0; i < n; i++)
      old_dec_out[i] = old_dec[i];

   free(e_in);
   free(old_enc);
   free(err);
   free(old_dec);
   return intra_dec;
}

#endif /* GOOPUS_ENERGY_SHIM_H */
