//go:build refc

/*
 * bandsenc_shim.h - C-callable wrapper over the pinned libopus CELT residual
 * band quantizer (celt/bands.c: quant_all_bands) driven in the ENCODE direction,
 * for the internal/celt phase-4 encoder differential test (CP7).
 *
 * oracle_bandsenc_pipeline runs the full encode mini-pipeline (init_caps ->
 * clt_compute_allocation(encode=1) -> quant_all_bands(encode=1) -> ec_enc_done)
 * once over buf, mirroring celt_encoder.c's bit accounting exactly (the
 * total_bits / anti_collapse_rsv arithmetic is the encoder twin of the
 * celt_decoder.c:1409-1491 sequence the decode oracle uses). complexity is a
 * parameter so the theta-RDO path (complexity>=8, stereo, !dual_stereo) can be
 * exercised. It returns the FINALIZED bitstream (buf, len bytes, after
 * ec_enc_done, so both the range-coder head and the raw-bit tail are captured),
 * the resynth spectrum X (C*N, meaningful only for the resynth cases), the
 * collapse_masks (C*nbEBands), the threaded LCG seed, and the pre-done range
 * state (tell + rng) plus codedBands / intensity / dual_stereo.
 *
 * This file #includes bands_shim.h to reuse the pinned mode helpers
 * (oracle_bands_mode / oracle_bands_mode_info / oracle_bands_eband) and never
 * edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go) or the decode
 * bands_shim.h. Build flags come from oracle_cgo.go (FIXED_POINT +
 * DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES, non-QEXT); celt_norm /
 * celt_ener / celt_glog are all opus_val32 in this build.
 */
#ifndef GOOPUS_BANDSENC_SHIM_H
#define GOOPUS_BANDSENC_SHIM_H

#include "bands_shim.h"

/*
 * oracle_bandsenc_pipeline: encode the random normalized spectrum Xin (C*N
 * celt_norm) with band energies bandE_in (2*nbEBands celt_ener) into buf,
 * threading the LCG seed. Returns codedBands and fills the out-parameters.
 */
static int oracle_bandsenc_pipeline(
    int start, int end, int LM, int channels,
    int isTransient, int spread, int intensity_in, int dual_stereo_in,
    int disable_inv, int alloc_trim, int complexity, int len,
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

   for (i = 0; i < C * N; i++) X[i] = (celt_norm)Xin[i];
   memset(buf, 0, len);
   ec_enc_init(&ec, buf, len);

   /* Encoder bit accounting (celt_encoder.c), the twin of the decode oracle. */
   total_bits_frac = ((opus_int32)len * 8) << BITRES;
   bits = total_bits_frac - (opus_int32)ec_tell_frac(&ec) - 1;
   anti_collapse_rsv = (isTransient && LM >= 2 && bits >= ((LM + 2) << BITRES)) ? (1 << BITRES) : 0;
   bits -= anti_collapse_rsv;

   codedBands = clt_compute_allocation(m, start, end, offsets, cap, alloc_trim,
       &intensity, &dual_stereo, bits, &balance, pulses, fine_quant, fine_priority,
       C, LM, &ec, /*encode=*/1, /*prev=*/0, /*signalBandwidth=*/0);

   quant_all_bands(/*encode=*/1, m, start, end, X, C == 2 ? X + N : NULL, collapse_masks,
       bandE_in, pulses, shortBlocks, spread, dual_stereo, intensity,
       (int *)tf_res, ((opus_int32)len * (8 << BITRES)) - anti_collapse_rsv, balance, &ec,
       LM, codedBands, &rng, complexity, /*arch=*/0, disable_inv);

   *tell_out = ec_tell(&ec);
   *rng_out = ec.rng;
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
 * oracle_spreading_decision wraps spreading_decision (bands.c:470), the
 * encoder-only spread choice. Xin is channels*(M*shortMdctSize) celt_norm;
 * average / hf_average / tapset_decision are read-modify-write (passed by
 * pointer). Returns the SPREAD_* decision.
 */
static int oracle_spreading_decision(
    const int32_t *Xin, int *average, int last_decision, int *hf_average,
    int *tapset_decision, int update_hf, int end, int channels, int M,
    const int *spread_weight, int totalN)
{
   const CELTMode *m = oracle_bands_mode();
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * totalN);
   int i, decision;
   for (i = 0; i < totalN; i++) X[i] = (celt_norm)Xin[i];
   decision = spreading_decision(m, X, average, last_decision, hf_average,
       tapset_decision, update_hf, end, channels, M, spread_weight);
   free(X);
   return decision;
}

/*
 * oracle_bands_logN returns m->logN[i], and oracle_bands_cache_max returns
 * cache[cache[0]] (the max PVQ bits for band i at LM), i.e. the exact values
 * compute_theta's pulse_cap and quant_partition's split condition use. These let
 * the encode test independently evaluate the avoid_split_noise snap predicate
 * without instrumenting the verbatim production code.
 */
static int oracle_bands_logN(int i)
{
   return (int)oracle_bands_mode()->logN[i];
}

static int oracle_bands_cache_max(int LM, int i)
{
   const CELTMode *m = oracle_bands_mode();
   const unsigned char *cache = m->cache.bits + m->cache.index[(LM + 1) * m->nbEBands + i];
   return (int)cache[cache[0]];
}

#endif /* GOOPUS_BANDSENC_SHIM_H */
