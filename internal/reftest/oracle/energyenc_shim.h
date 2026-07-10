//go:build refc

/*
 * energyenc_shim.h - C-callable wrappers over the pinned libopus CELT
 * ENCODE-side band-energy path (celt/bands.c compute_band_energies /
 * normalise_bands, celt/quant_bands.c amp2Log2 / quant_coarse_energy /
 * quant_fine_energy / quant_energy_finalise, celt/laplace.c ec_laplace_encode)
 * for the internal/celt CP4 differential test.
 *
 * CP1/CP3 exercised fixedmath and the forward MDCT; CP4 adds the encoder energy
 * quantizer. The existing energy_shim.h drives the C encoder only to feed the Go
 * DECODE port; this file instead exposes the C ENCODE primitives directly so the
 * Go ENCODE port (quantCoarseEnergy etc.) can be asserted bit-for-bit against
 * them: emitted bytes, the intra/inter RDO decision, and the reconstructed
 * oldEBands/error, across multi-frame sequences (the inter path predicts from the
 * previous frame, so single-frame tests are insufficient per hard-parts 5).
 *
 * Everything is static so the header pulls straight into the energyenc_cgo.go
 * preamble with no extra .c translation unit; the non-static quant_bands.c /
 * laplace.c / bands.c / entenc.c / entdec.c / modes.c symbols link via the
 * existing w_celt_*.c wrappers (gen_wrappers.sh). quant_coarse_energy_impl and
 * loss_distortion are static, so they are driven transitively through their
 * non-static caller quant_coarse_energy. This file never edits the shared oracle
 * surface (shim.h/shim.c/oracle_cgo.go) or another checkpoint's oracle files.
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from the package-level cgo directives in
 * oracle_cgo.go. celt_sig/celt_ener/celt_norm/celt_glog are all opus_val32
 * (int32) in this build, so every spectral/energy array is int32_t. The C mode is
 * opus_custom_mode_create(48000, 960): under non-CUSTOM_MODES it returns the
 * static mode48000_960 (nbEBands=21) that internal/celt mirrors.
 */
#ifndef GOOPUS_ENERGYENC_SHIM_H
#define GOOPUS_ENERGYENC_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "modes.h"       /* CELTMode, opus_custom_mode_create */
#include "bands.h"       /* compute_band_energies, normalise_bands */
#include "quant_bands.h" /* amp2Log2, quant_ coarse/fine/finalise */
#include "laplace.h"     /* ec_laplace_encode */
#include "entenc.h"      /* ec_enc, ec_enc_init, ec_enc_done */
#include "entdec.h"      /* ec_dec, ec_dec_init, ec_dec_bit_logp */
#include "entcode.h"     /* ec_tell, ec_tell_frac */

/* oracle_ee_mode returns the frozen static CELT mode (48 kHz, 960). */
static const CELTMode *oracle_ee_mode(void)
{
   int err = 0;
   return opus_custom_mode_create(48000, 960, &err);
}

/* oracle_ee_nbebands returns the mode's band count (21). */
static int oracle_ee_nbebands(void) { return oracle_ee_mode()->nbEBands; }

/* oracle_ee_shortmdctsize returns the mode's short-MDCT size (120). */
static int oracle_ee_shortmdctsize(void) { return oracle_ee_mode()->shortMdctSize; }

/*
 * oracle_ee_laplace_seq encodes a sequence of n Laplace symbols with the C
 * ec_laplace_encode, then flushes. values/fss/decays are the per-symbol inputs
 * (values is copied so the in/out clamp is reported via val_out). It reports the
 * finalized packet (buf_out, nb_bytes), the range register state captured right
 * before ec_enc_done (rng_out/val_out_reg), and the write-back values.
 */
static void oracle_ee_laplace_seq(
    int n, int nb_bytes,
    const int32_t *values, const uint32_t *fss, const int32_t *decays,
    unsigned char *buf_out, int32_t *val_out,
    uint32_t *rng_out, uint32_t *val_reg_out, uint32_t *rangebytes_out)
{
   ec_enc enc;
   int i;
   memset(buf_out, 0, nb_bytes);
   ec_enc_init(&enc, buf_out, nb_bytes);
   for (i = 0; i < n; i++)
   {
      int v = (int)values[i];
      ec_laplace_encode(&enc, &v, (unsigned)fss[i], (int)decays[i]);
      val_out[i] = v;
   }
   *rng_out = enc.rng;
   *val_reg_out = enc.val;
   *rangebytes_out = ec_range_bytes(&enc);
   ec_enc_done(&enc);
}

/*
 * oracle_ee_compute_band_energies runs compute_band_energies over X (celt_sig,
 * length channels*(shortMdctSize<<lm)) into bandE_out (channels*nbEBands).
 */
static void oracle_ee_compute_band_energies(
    int lm, int channels, int end, const int32_t *X, int32_t *bandE_out)
{
   const CELTMode *m = oracle_ee_mode();
   int N = m->shortMdctSize << lm;
   int n = channels * N;
   celt_sig *x = (celt_sig *)malloc(sizeof(celt_sig) * n);
   int i;
   for (i = 0; i < n; i++)
      x[i] = X[i];
   compute_band_energies(m, x, bandE_out, end, channels, lm, 0);
   free(x);
}

/*
 * oracle_ee_normalise_bands runs normalise_bands over freq (celt_sig) using
 * bandE, writing celt_norm into X_out. M = 1<<lm; freq/X_out have length
 * channels*(M*shortMdctSize).
 */
static void oracle_ee_normalise_bands(
    int lm, int channels, int end, const int32_t *freq, const int32_t *bandE,
    int32_t *X_out)
{
   const CELTMode *m = oracle_ee_mode();
   int M = 1 << lm;
   int N = M * m->shortMdctSize;
   int n = channels * N;
   celt_sig *f = (celt_sig *)malloc(sizeof(celt_sig) * n);
   celt_ener *e = (celt_ener *)malloc(sizeof(celt_ener) * channels * m->nbEBands);
   int i;
   for (i = 0; i < n; i++)
      f[i] = freq[i];
   for (i = 0; i < channels * m->nbEBands; i++)
      e[i] = bandE[i];
   normalise_bands(m, f, X_out, e, end, channels, M);
   free(f);
   free(e);
}

/*
 * oracle_ee_amp2log2 runs amp2Log2 over bandE (celt_ener) into bandLogE_out
 * (celt_glog), both length channels*nbEBands.
 */
static void oracle_ee_amp2log2(
    int effEnd, int end, int channels, const int32_t *bandE, int32_t *bandLogE_out)
{
   const CELTMode *m = oracle_ee_mode();
   int n = channels * m->nbEBands;
   celt_ener *e = (celt_ener *)malloc(sizeof(celt_ener) * n);
   int i;
   for (i = 0; i < n; i++)
      e[i] = bandE[i];
   amp2Log2(m, effEnd, end, e, bandLogE_out, channels);
   free(e);
}

/*
 * oracle_ee_coarse runs quant_coarse_energy for ONE frame. old_in is the
 * prediction base; old_out receives the reconstruction; delayed_in/out carry the
 * RDO bias. It reports the packet, the error[] array, and the intra flag decoded
 * back from the just-produced bitstream (how the decoder would read it). The
 * caller carries old_out/delayed_out into the next frame.
 */
static int oracle_ee_coarse(
    int lm, int channels, int nb_bytes, int force_intra, int two_pass,
    int loss_rate, int lfe, int start, int end, int effEnd, int pre_bits,
    const int32_t *e_in, const int32_t *old_in, int32_t *old_out,
    int32_t *err_out, int32_t delayed_in, int32_t *delayed_out,
    unsigned char *buf_out)
{
   const CELTMode *m = oracle_ee_mode();
   int nb = m->nbEBands;
   int n = channels * nb;
   opus_int32 budget = nb_bytes * 8;
   int i, k, intra_dec;
   opus_val32 delayedIntra = delayed_in;

   celt_glog *e = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *old = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *err = (celt_glog *)malloc(sizeof(celt_glog) * n);
   for (i = 0; i < n; i++)
   {
      e[i] = e_in[i];
      old[i] = old_in[i];
      err[i] = 0;
   }
   memset(buf_out, 0, nb_bytes);
   {
      ec_enc enc;
      ec_enc_init(&enc, buf_out, nb_bytes);
      /* Emit pre_bits range-coded bits before coarse energy so ec_tell(enc)>0
         at entry, mirroring the silence/postfilter/transient flags that precede
         coarse energy in the real encoder. */
      for (k = 0; k < pre_bits; k++)
         ec_enc_bit_logp(&enc, 0, 1);
      quant_coarse_energy(m, start, end, effEnd, e, old, (opus_uint32)budget, err, &enc,
                          channels, lm, nb_bytes, force_intra, &delayedIntra,
                          two_pass, loss_rate, lfe);
      ec_enc_done(&enc);
   }
   for (i = 0; i < n; i++)
   {
      old_out[i] = old[i];
      err_out[i] = err[i];
   }
   *delayed_out = delayedIntra;
   {
      ec_dec dec;
      opus_int32 tell;
      ec_dec_init(&dec, buf_out, nb_bytes);
      for (k = 0; k < pre_bits; k++)
         ec_dec_bit_logp(&dec, 1);
      tell = ec_tell(&dec);
      intra_dec = (tell + 3 <= budget) ? ec_dec_bit_logp(&dec, 3) : 0;
   }
   free(e);
   free(old);
   free(err);
   return intra_dec;
}

/*
 * oracle_ee_full runs the full coarse+fine+finalise energy path for ONE frame,
 * matching the real encoder order (celt_encoder.c: quant_coarse_energy,
 * quant_fine_energy, quant_energy_finalise). Same in/out state contract as
 * oracle_ee_coarse, plus fine_quant/fine_priority (per band). Returns the intra
 * flag decoded back from the bitstream.
 */
static int oracle_ee_full(
    int lm, int channels, int nb_bytes, int force_intra, int two_pass,
    int loss_rate, int lfe,
    const int32_t *e_in, const int32_t *old_in, int32_t *old_out,
    int32_t *err_out, int32_t delayed_in, int32_t *delayed_out,
    const int32_t *fine_quant, const int32_t *fine_priority,
    unsigned char *buf_out)
{
   const CELTMode *m = oracle_ee_mode();
   int nb = m->nbEBands;
   int start = 0, end = nb, effEnd = end;
   int n = channels * nb;
   opus_int32 budget = nb_bytes * 8;
   opus_int32 bits_left;
   int i, intra_dec;
   opus_val32 delayedIntra = delayed_in;
   int fq[64], fp[64];

   for (i = 0; i < nb; i++)
   {
      fq[i] = (int)fine_quant[i];
      fp[i] = (int)fine_priority[i];
   }

   celt_glog *e = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *old = (celt_glog *)malloc(sizeof(celt_glog) * n);
   celt_glog *err = (celt_glog *)malloc(sizeof(celt_glog) * n);
   for (i = 0; i < n; i++)
   {
      e[i] = e_in[i];
      old[i] = old_in[i];
      err[i] = 0;
   }
   memset(buf_out, 0, nb_bytes);
   {
      ec_enc enc;
      ec_enc_init(&enc, buf_out, nb_bytes);
      quant_coarse_energy(m, start, end, effEnd, e, old, (opus_uint32)budget, err, &enc,
                          channels, lm, nb_bytes, force_intra, &delayedIntra,
                          two_pass, loss_rate, lfe);
      quant_fine_energy(m, start, end, old, err, NULL, fq, &enc, channels);
      bits_left = budget - ec_tell(&enc);
      quant_energy_finalise(m, start, end, old, err, fq, fp, bits_left, &enc, channels);
      ec_enc_done(&enc);
   }
   for (i = 0; i < n; i++)
   {
      old_out[i] = old[i];
      err_out[i] = err[i];
   }
   *delayed_out = delayedIntra;
   {
      ec_dec dec;
      opus_int32 tell;
      ec_dec_init(&dec, buf_out, nb_bytes);
      tell = ec_tell(&dec);
      intra_dec = (tell + 3 <= budget) ? ec_dec_bit_logp(&dec, 3) : 0;
   }
   free(e);
   free(old);
   free(err);
   return intra_dec;
}

#endif /* GOOPUS_ENERGYENC_SHIM_H */
