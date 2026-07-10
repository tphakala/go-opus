//go:build refc

/*
 * vqenc_shim.h - C-callable wrappers over the pinned libopus CELT PVQ ENCODE
 * side (celt/vq.c alg_quant / op_pvq_search / stereo_itheta, celt/cwrs.c
 * encode_pulses / icwrs) for the internal/celt phase-4 differential test.
 *
 * This drives the C encoder helpers to PRODUCE codewords / index bytes / itheta
 * values so vqenc_test.go can assert the pure-Go internal/celt encode path is
 * bit-exact against C. op_pvq_search_c and icwrs are static in libopus, so they
 * are validated TRANSITIVELY: alg_quant is driven with resynth=1 and its emitted
 * bytes, coded pulse vector (recovered by decoding the codeword), collapse mask,
 * and reconstructed unit-norm output are compared; icwrs is checked through
 * encode_pulses (byte-exact) and through the index recovered by decoding the
 * codeword.
 *
 * It reuses the independent V(N,K) recurrence and the table-valid pulse cap from
 * vq_shim.h (included below; that file is not modified).
 *
 * All drivers are static so the header pulls straight into vqenc_cgo.go's
 * preamble; the non-static alg_quant / stereo_itheta / encode_pulses /
 * decode_pulses / ec_* symbols link in via the existing w_celt_vq.c /
 * w_celt_cwrs.c / w_celt_entenc.c / w_celt_entdec.c wrappers. This file never
 * edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go) or vq_shim.h.
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from oracle_cgo.go. celt_norm is opus_val32
 * (arch.h:145), so norm buffers are int32_t, and the non-QEXT alg_quant signature
 * has no ARG_QEXT parameters.
 */
#ifndef GOOPUS_VQENC_SHIM_H
#define GOOPUS_VQENC_SHIM_H

#include "vq_shim.h" /* oracle_true_pvq_v, oracle_pvq_max_k; arch/vq/cwrs/ec includes */

/*
 * oracle_pvqenc_alg_quant runs the C encoder alg_quant over Xin (length N,
 * celt_norm) to PRODUCE a coded PVQ codeword in buf (nbytes, zero-filled and
 * finalized with ec_enc_done). It then decodes that codeword to recover the
 * coded pulse vector into iyOut, and copies the (resynth) reconstructed band to
 * Xout. gain is the band gain (Q31); resynth selects the reconstruction path.
 * Returns the collapse mask.
 */
static unsigned oracle_pvqenc_alg_quant(const int32_t *Xin, int N, int K, int spread,
    int B, int32_t gain, int resynth, unsigned char *buf, int nbytes,
    int32_t *iyOut, int32_t *Xout)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * (N + 3));
   int *iy = (int *)malloc(sizeof(int) * N);
   int i;
   ec_enc enc;
   ec_dec dec;
   unsigned cm;
   for (i = 0; i < N; i++) X[i] = (celt_norm)Xin[i];
   memset(buf, 0, nbytes);
   ec_enc_init(&enc, buf, nbytes);
   cm = alg_quant(X, N, K, spread, B, &enc, (opus_val32)gain, resynth, /*arch=*/0);
   ec_enc_done(&enc);
   /* Recover the coded pulse vector by decoding the codeword we just wrote. */
   ec_dec_init(&dec, buf, nbytes);
   decode_pulses(iy, N, K, &dec);
   for (i = 0; i < N; i++) {
      iyOut[i] = iy[i];
      Xout[i] = X[i];
   }
   free(iy);
   free(X);
   return cm;
}

/*
 * oracle_pvqenc_encode_pulses runs the C encode_pulses over the pulse vector
 * iyIn (length N, K pulses) into buf (nbytes, zero-filled + finalized).
 */
static void oracle_pvqenc_encode_pulses(const int32_t *iyIn, int N, int K,
    unsigned char *buf, int nbytes)
{
   int *iy = (int *)malloc(sizeof(int) * N);
   int i;
   ec_enc enc;
   for (i = 0; i < N; i++) iy[i] = iyIn[i];
   memset(buf, 0, nbytes);
   ec_enc_init(&enc, buf, nbytes);
   encode_pulses(iy, N, K, &enc);
   ec_enc_done(&enc);
   free(iy);
}

/*
 * oracle_pvqenc_icwrs returns the C icwrs index for iyIn (length N, K pulses),
 * recovered by encoding the codeword and decoding the index back over the ft =
 * V(N,K) alphabet (icwrs itself is static). ft is supplied by the caller.
 */
static uint32_t oracle_pvqenc_icwrs(const int32_t *iyIn, int N, int K, uint32_t ft)
{
   int *iy = (int *)malloc(sizeof(int) * N);
   int i;
   ec_enc enc;
   ec_dec dec;
   uint32_t idx;
   unsigned char buf[128];
   for (i = 0; i < N; i++) iy[i] = iyIn[i];
   memset(buf, 0, sizeof(buf));
   ec_enc_init(&enc, buf, sizeof(buf));
   encode_pulses(iy, N, K, &enc);
   ec_enc_done(&enc);
   ec_dec_init(&dec, buf, sizeof(buf));
   idx = ec_dec_uint(&dec, ft);
   free(iy);
   return idx;
}

/*
 * oracle_pvqenc_stereo_itheta runs the C stereo_itheta over X, Y (length N,
 * celt_norm) in the given stereo mode and returns itheta.
 */
static int32_t oracle_pvqenc_stereo_itheta(const int32_t *Xin, const int32_t *Yin,
    int stereo, int N)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * N);
   celt_norm *Y = (celt_norm *)malloc(sizeof(celt_norm) * N);
   int i;
   opus_int32 itheta;
   for (i = 0; i < N; i++) {
      X[i] = (celt_norm)Xin[i];
      Y[i] = (celt_norm)Yin[i];
   }
   itheta = stereo_itheta(X, Y, stereo, N, /*arch=*/0);
   free(X);
   free(Y);
   return (int32_t)itheta;
}

#endif /* GOOPUS_VQENC_SHIM_H */
