//go:build refc

/*
 * bands_shim.h - C-callable wrappers over the pinned libopus CELT residual band
 * quantizer (celt/bands.c: quant_all_bands, denormalise_bands, anti_collapse)
 * for the internal/celt differential test.
 *
 * quant_all_bands interleaves the range coder, so to exercise the Go DECODE
 * port against C over the SAME bitstream, oracle_bands_pipeline runs the mini
 * decoder pipeline (init_caps -> clt_compute_allocation -> quant_all_bands) in
 * one direction over a caller-supplied buffer, mirroring how celt_decoder.c
 * drives it: the bits / anti_collapse_rsv / total_bits arithmetic is copied
 * verbatim from celt_decoder.c:1409-1491. encode=1 quantizes a random
 * normalized X[] into buf (producing the ground-truth bytes); encode=0 decodes
 * buf back. The test C-encodes once, then decodes buf with both C and Go and
 * asserts bit-identical X[]/collapse_masks/seed/tell/rng.
 *
 * The dynalloc boosts (offsets[]) and alloc_trim are passed as fixed inputs and
 * are NOT range-coded here, so the only bitstream symbols are the allocation
 * skip/intensity/dual flags followed by the PVQ band symbols; encode and decode
 * therefore observe an identical coder state at every step (the range coder
 * guarantees ec_tell_frac agreement between the two directions).
 *
 * Everything is static so the header pulls straight into bands_cgo.go; the
 * non-static quant_all_bands / denormalise_bands / anti_collapse /
 * clt_compute_allocation / init_caps / opus_custom_mode_create / ec_* symbols
 * link via the existing w_celt_bands.c / w_celt_rate.c / w_celt_celt.c /
 * w_celt_modes.c / w_celt_entenc.c / w_celt_entdec.c wrappers. This file never
 * edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64,
 * non-CUSTOM_MODES, non-QEXT) and include paths come from oracle_cgo.go. In this
 * build celt_norm / celt_ener / celt_glog are all opus_val32 (arch.h:145-147).
 */
#ifndef GOOPUS_BANDS_SHIM_H
#define GOOPUS_BANDS_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "arch.h"    /* celt_norm, celt_glog, opus_int32 */
#include "modes.h"   /* CELTMode */
#include "bands.h"   /* quant_all_bands, denormalise_bands, anti_collapse */
#include "rate.h"    /* clt_compute_allocation */
#include "celt.h"    /* init_caps */
#include "entenc.h"  /* ec_enc, ec_enc_init, ec_enc_done */
#include "entdec.h"  /* ec_dec, ec_dec_init */
#include "entcode.h" /* ec_ctx, ec_tell, ec_tell_frac */

/* opus_custom_mode_create is defined even in the non-CUSTOM_MODES build
 * (celt/modes.c); forward-declare it to avoid the public custom header. */
extern CELTMode *opus_custom_mode_create(opus_int32 Fs, int frame_size, int *error);

/* oracle_bands_mode() returns the pinned 48 kHz / 960 mode (or NULL on failure). */
static const CELTMode *oracle_bands_mode(void)
{
   static const CELTMode *m = NULL;
   if (m == NULL)
      m = opus_custom_mode_create(48000, 960, NULL);
   return m;
}

/* oracle_bands_mode_info reports the mode dimensions the Go test needs. */
static void oracle_bands_mode_info(int *nbEBands, int *effEBands, int *shortMdctSize)
{
   const CELTMode *m = oracle_bands_mode();
   *nbEBands = m->nbEBands;
   *effEBands = m->effEBands;
   *shortMdctSize = m->shortMdctSize;
}

/* oracle_bands_eband returns eBands[i] (band boundary in shortMdctSize units). */
static int oracle_bands_eband(int i)
{
   return oracle_bands_mode()->eBands[i];
}

/*
 * oracle_bands_pipeline runs init_caps -> clt_compute_allocation ->
 * quant_all_bands once over buf, mirroring celt_decoder.c. encode!=0 quantizes
 * Xin (channels*N celt_norm, and bandE_in = 2*nbEBands celt_ener) into buf;
 * encode==0 decodes buf into Xout (bandE is NULL on decode, as in the real
 * decoder). Fills collapse_out (channels*nbEBands) and reports seed_out (the
 * threaded LCG state), tell_out (ec_tell), rng_out, codedBands_out,
 * intensity_out, dual_stereo_out. Returns codedBands.
 */
static int oracle_bands_pipeline(
    int encode, int start, int end, int LM, int channels,
    int isTransient, int spread, int intensity_in, int dual_stereo_in,
    int disable_inv, int alloc_trim, int len,
    const int *offsets, const int *tf_res,
    const int32_t *bandE_in, const int32_t *Xin, uint32_t seed_in,
    unsigned char *buf,
    int32_t *Xout, unsigned char *collapse_out,
    uint32_t *seed_out, int *tell_out, uint32_t *rng_out,
    int *codedBands_out, int *intensity_out, int *dual_stereo_out)
{
   const CELTMode *m = oracle_bands_mode();
   int nbEBands = m->nbEBands;
   int M = 1 << LM;
   int N = M * m->shortMdctSize;
   int C = channels;
   int shortBlocks = isTransient ? M : 0;
   int i;
   int intensity = intensity_in;
   int dual_stereo = dual_stereo_in;
   opus_int32 balance = 0;
   int codedBands;
   uint32_t rng = seed_in;
   ec_ctx ec;
   int *cap = (int *)malloc(sizeof(int) * nbEBands);
   int *pulses = (int *)malloc(sizeof(int) * nbEBands);
   int *fine_quant = (int *)malloc(sizeof(int) * nbEBands);
   int *fine_priority = (int *)malloc(sizeof(int) * nbEBands);
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * C * N);
   unsigned char *collapse_masks = (unsigned char *)malloc(C * nbEBands);
   opus_int32 total_bits_frac, bits, anti_collapse_rsv;

   memset(collapse_masks, 0, C * nbEBands);
   init_caps(m, cap, LM, C);

   if (encode) {
      for (i = 0; i < C * N; i++) X[i] = (celt_norm)Xin[i];
      memset(buf, 0, len);
      ec_enc_init(&ec, buf, len);
   } else {
      memset(X, 0, sizeof(celt_norm) * C * N);
      ec_dec_init(&ec, buf, len);
   }

   /* celt_decoder.c:1410-1446 bit accounting (dynalloc symbols are not coded
    * here; offsets[] are passed as fixed boosts). */
   total_bits_frac = ((opus_int32)len * 8) << BITRES;
   bits = total_bits_frac - (opus_int32)ec_tell_frac(&ec) - 1;
   anti_collapse_rsv = (isTransient && LM >= 2 && bits >= ((LM + 2) << BITRES)) ? (1 << BITRES) : 0;
   bits -= anti_collapse_rsv;

   codedBands = clt_compute_allocation(m, start, end, offsets, cap, alloc_trim,
       &intensity, &dual_stereo, bits, &balance, pulses, fine_quant, fine_priority,
       C, LM, &ec, encode, 0, 0);

   quant_all_bands(encode, m, start, end, X, C == 2 ? X + N : NULL, collapse_masks,
       encode ? bandE_in : NULL, pulses, shortBlocks, spread, dual_stereo, intensity,
       (int *)tf_res, ((opus_int32)len * (8 << BITRES)) - anti_collapse_rsv, balance, &ec,
       LM, codedBands, &rng, /*complexity=*/encode ? 5 : 0, /*arch=*/0, disable_inv);

   *tell_out = ec_tell(&ec);
   *rng_out = ec.rng;
   if (encode)
      ec_enc_done(&ec);

   for (i = 0; i < C * N; i++) Xout[i] = X[i];
   memcpy(collapse_out, collapse_masks, C * nbEBands);
   *seed_out = rng;
   *codedBands_out = codedBands;
   *intensity_out = intensity;
   *dual_stereo_out = dual_stereo;

   free(cap);
   free(pulses);
   free(fine_quant);
   free(fine_priority);
   free(X);
   free(collapse_masks);
   return codedBands;
}

/*
 * oracle_denormalise_bands wraps denormalise_bands over Xin (N=M*shortMdctSize
 * celt_norm) and bandLogE (nbEBands celt_glog), writing the synthesis spectrum
 * into freq_out (N celt_sig). M = 1<<LM.
 */
static void oracle_denormalise_bands(const int32_t *Xin, const int32_t *bandLogE,
    int start, int end, int LM, int downsample, int silence, int32_t *freq_out)
{
   const CELTMode *m = oracle_bands_mode();
   int M = 1 << LM;
   int N = M * m->shortMdctSize;
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * N);
   celt_sig *freq = (celt_sig *)malloc(sizeof(celt_sig) * N);
   int i;
   for (i = 0; i < N; i++) X[i] = (celt_norm)Xin[i];
   denormalise_bands(m, X, freq, (const celt_glog *)bandLogE, start, end, M, downsample, silence);
   for (i = 0; i < N; i++) freq_out[i] = (int32_t)freq[i];
   free(X);
   free(freq);
}

/*
 * oracle_anti_collapse wraps anti_collapse over Xin (channels*size celt_norm)
 * with the given collapse masks (channels*nbEBands) and the logE/prev energies
 * (2*nbEBands celt_glog each), writing the result into Xout. encode selects the
 * C==1 mono prev-energy MAX path.
 */
static void oracle_anti_collapse(const int32_t *Xin, const unsigned char *collapse_masks_in,
    int LM, int C, int size, int start, int end,
    const int32_t *logE, const int32_t *prev1logE, const int32_t *prev2logE,
    const int *pulses, uint32_t seed, int encode, int32_t *Xout)
{
   const CELTMode *m = oracle_bands_mode();
   int nbEBands = m->nbEBands;
   int total = C * size;
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * total);
   unsigned char *cm = (unsigned char *)malloc(C * nbEBands);
   int i;
   for (i = 0; i < total; i++) X[i] = (celt_norm)Xin[i];
   memcpy(cm, collapse_masks_in, C * nbEBands);
   anti_collapse(m, X, cm, LM, C, size, start, end,
       (const celt_glog *)logE, (const celt_glog *)prev1logE, (const celt_glog *)prev2logE,
       pulses, seed, encode, /*arch=*/0);
   for (i = 0; i < total; i++) Xout[i] = X[i];
   free(X);
   free(cm);
}

#endif /* GOOPUS_BANDS_SHIM_H */
