//go:build refc

/*
 * vq_shim.h - C-callable wrappers over the pinned libopus CELT PVQ codec
 * (celt/vq.c, celt/cwrs.c) for the internal/celt differential test.
 *
 * The Go CELT port only implements the DECODE side (AlgUnquant / DecodePulses /
 * ExpRotation / RenormaliseVector). To exercise it against C we need coded PVQ
 * codewords, so this shim runs the C ENCODE helper (alg_quant, resynth=0) over a
 * caller-supplied random input vector to PRODUCE a bitstream, then runs the C
 * DECODE helpers (alg_unquant / decode_pulses) over the same bitstream so the
 * test can cross-check Go against both. It also wraps exp_rotation and
 * renormalise_vector directly, encodes a raw PVQ index for the direct cwrsi test,
 * and computes V(N,K) by an independent O(NK) recurrence plus the table-valid
 * per-N pulse cap.
 *
 * Everything is static so the header pulls straight into vq_cgo.go's preamble;
 * the non-static alg_quant / alg_unquant / exp_rotation / renormalise_vector /
 * decode_pulses / encode_pulses / ec_* symbols are linked in via the existing
 * w_celt_vq.c / w_celt_cwrs.c / w_celt_entenc.c / w_celt_entdec.c wrappers. This
 * file never edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64, non-CUSTOM_MODES,
 * non-QEXT) and include paths come from oracle_cgo.go. In this build celt_norm is
 * opus_val32 (arch.h:145), so all norm buffers are int32_t, and the non-QEXT
 * alg_quant/alg_unquant signatures have no ARG_QEXT parameters (celt.h:49).
 */
#ifndef GOOPUS_VQ_SHIM_H
#define GOOPUS_VQ_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "arch.h"    /* celt_norm, opus_val32 */
#include "vq.h"      /* alg_quant, alg_unquant, exp_rotation, renormalise_vector */
#include "cwrs.h"    /* decode_pulses, encode_pulses */
#include "entenc.h"  /* ec_enc, ec_enc_init, ec_enc_uint, ec_enc_done */
#include "entdec.h"  /* ec_dec, ec_dec_init */
#include "entcode.h" /* ec_tell */

/*
 * oracle_pvq_alg_quant runs the C encoder alg_quant over Xin (length N,
 * interpreted as celt_norm) to PRODUCE a coded PVQ codeword in buf (nbytes,
 * zero-filled first). resynth=0 (so gain is unused on the encode side) and
 * arch=0. Returns the collapse mask.
 */
static unsigned oracle_pvq_alg_quant(const int32_t *Xin, int N, int K, int spread,
    int B, unsigned char *buf, int nbytes)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * (N + 3));
   int i;
   ec_enc enc;
   unsigned cm;
   for (i = 0; i < N; i++) X[i] = (celt_norm)Xin[i];
   memset(buf, 0, nbytes);
   ec_enc_init(&enc, buf, nbytes);
   cm = alg_quant(X, N, K, spread, B, &enc, (opus_val32)Q31ONE, /*resynth=*/0, /*arch=*/0);
   ec_enc_done(&enc);
   free(X);
   return cm;
}

/*
 * oracle_pvq_alg_unquant decodes buf (nbytes) with the C alg_unquant into Xout
 * (length N), reporting the decoder tell and rng after. Returns the collapse
 * mask. gain is the band gain (Q31).
 */
static unsigned oracle_pvq_alg_unquant(const unsigned char *buf, int nbytes,
    int N, int K, int spread, int B, int32_t gain,
    int32_t *Xout, int *tell_out, uint32_t *rng_out)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * (N + 3));
   int i;
   ec_dec dec;
   unsigned cm;
   ec_dec_init(&dec, (unsigned char *)buf, nbytes);
   cm = alg_unquant(X, N, K, spread, B, &dec, (opus_val32)gain);
   *tell_out = ec_tell(&dec);
   *rng_out = dec.rng;
   for (i = 0; i < N; i++) Xout[i] = X[i];
   free(X);
   return cm;
}

/*
 * oracle_pvq_decode_pulses decodes buf (nbytes) with the C decode_pulses into
 * yout (length N), reporting Ryy (returned), tell and rng.
 */
static int32_t oracle_pvq_decode_pulses(const unsigned char *buf, int nbytes,
    int N, int K, int32_t *yout, int *tell_out, uint32_t *rng_out)
{
   int *y = (int *)malloc(sizeof(int) * N);
   int i;
   ec_dec dec;
   opus_val32 ryy;
   ec_dec_init(&dec, (unsigned char *)buf, nbytes);
   ryy = decode_pulses(y, N, K, &dec);
   *tell_out = ec_tell(&dec);
   *rng_out = dec.rng;
   for (i = 0; i < N; i++) yout[i] = y[i];
   free(y);
   return (int32_t)ryy;
}

/*
 * oracle_pvq_encode_index writes a raw PVQ codeword index over the ft alphabet
 * into buf (nbytes, zero-filled first) via ec_enc_uint, for the direct
 * decode_pulses test.
 */
static void oracle_pvq_encode_index(uint32_t index, uint32_t ft,
    unsigned char *buf, int nbytes)
{
   ec_enc enc;
   memset(buf, 0, nbytes);
   ec_enc_init(&enc, buf, nbytes);
   ec_enc_uint(&enc, index, ft);
   ec_enc_done(&enc);
}

/*
 * oracle_pvq_exp_rotation runs exp_rotation over Xin (length len) and copies the
 * rotated buffer to Xout, for the direct rotation test.
 */
static void oracle_pvq_exp_rotation(const int32_t *Xin, int len, int dir,
    int stride, int K, int spread, int32_t *Xout)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * len);
   int i;
   for (i = 0; i < len; i++) X[i] = (celt_norm)Xin[i];
   exp_rotation(X, len, dir, stride, K, spread);
   for (i = 0; i < len; i++) Xout[i] = X[i];
   free(X);
}

/*
 * oracle_pvq_renormalise runs renormalise_vector over Xin (length N) with the
 * given Q31 gain and copies the result to Xout.
 */
static void oracle_pvq_renormalise(const int32_t *Xin, int N, int32_t gain,
    int32_t *Xout)
{
   celt_norm *X = (celt_norm *)malloc(sizeof(celt_norm) * N);
   int i;
   for (i = 0; i < N; i++) X[i] = (celt_norm)Xin[i];
   renormalise_vector(X, N, (opus_val32)gain, /*arch=*/0);
   for (i = 0; i < N; i++) Xout[i] = X[i];
   free(X);
}

/*
 * oracle_trueU computes the true U(N,K) by the O(NK) recurrence
 * U(n,k)=U(n-1,k)+U(n,k-1)+U(n-1,k-1) with U(0,0)=1, U(0,k>0)=0, U(n>0,0)=0,
 * in 64-bit so values that exceed 2^32 (unrepresentable in the table) are
 * detected. Independent of the static PVQ table.
 */
static uint64_t oracle_trueU(int n, int k)
{
   static uint64_t memo[320][320];
   static int done[320][320];
   uint64_t v;
   if (n < 0 || k < 0) return 0;
   if (n == 0) return k == 0 ? 1 : 0;
   if (k == 0) return 0;
   if (n >= 320 || k >= 320) return (uint64_t)1 << 40; /* huge => invalid */
   if (done[n][k]) return memo[n][k];
   done[n][k] = 1;
   v = oracle_trueU(n - 1, k) + oracle_trueU(n, k - 1) + oracle_trueU(n - 1, k - 1);
   memo[n][k] = v;
   return v;
}

/*
 * oracle_true_pvq_v returns V(N,K)=U(N,K)+U(N,K+1) via the independent recurrence,
 * or 0 if it (or either term) is not representable in 32 bits.
 */
static uint32_t oracle_true_pvq_v(int N, int K)
{
   uint64_t uk = oracle_trueU(N, K);
   uint64_t uk1 = oracle_trueU(N, K + 1);
   uint64_t v;
   if (uk >= ((uint64_t)1 << 32) || uk1 >= ((uint64_t)1 << 32)) return 0;
   v = uk + uk1;
   if (v >= ((uint64_t)1 << 32)) return 0;
   return (uint32_t)v;
}

/*
 * oracle_pvq_max_k returns the largest K such that decode_pulses(N,K) is
 * table-valid: V(N,K) representable AND the static table macro CELT_PVQ_U equals
 * the true U for both U(N,K) and U(N,K+1) (i.e. the reads stay inside the stored
 * columns). Precomputed for N=0..176 by comparing the pinned CELT_PVQ_U macro
 * against oracle_trueU (see the report's codegen). N>176 needs the CUSTOM_MODES
 * extra-rows table, absent from this non-CUSTOM_MODES build, so returns 0.
 */
static int oracle_pvq_max_k(int N)
{
   static const int maxk[177] = {
      0, 175, 175, 175, 175, 175, 95, 53, 36, 27, 22, 18, 16, 15, 13, 12,
      12, 11, 11, 10, 9, 9, 9, 9, 9, 8, 8, 8, 8, 7, 7, 7,
      7, 7, 7, 7, 7, 7, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
      6, 6, 6, 6, 6, 6, 6, 5, 5, 5, 5, 5, 5, 5, 5, 5,
      5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
      5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
      5, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
      4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
      4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
      4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
      4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
      4,
   };
   if (N < 0 || N > 176) return 0;
   return maxk[N];
}

#endif /* GOOPUS_VQ_SHIM_H */
