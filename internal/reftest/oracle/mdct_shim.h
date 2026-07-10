//go:build refc

/*
 * mdct_shim.h - C-callable wrappers over the pinned libopus inverse FFT
 * (celt/kiss_fft.c opus_ifft) and inverse MDCT (celt/mdct.c clt_mdct_backward)
 * for the internal/celt differential test.
 *
 * Each wrapper is a `static` function so this header can be pulled into the
 * mdct_cgo.go preamble on its own, with no extra .c translation unit (cgo can
 * call statics defined in the preamble; each TU that includes the header gets
 * its own copy, and the shared opus_ifft_c / clt_mdct_backward_c /
 * opus_custom_mode_create symbols are linked in via w_celt_kiss_fft.c,
 * w_celt_mdct.c and w_celt_modes.c).
 *
 * The package-level cgo CFLAGS in oracle_cgo.go already define FIXED_POINT +
 * DISABLE_FLOAT_API + OPUS_FAST_INT64 (non-CUSTOM_MODES, non-QEXT) and set the
 * include paths. This file is separate from shim.h/shim.c by design; it never
 * edits the shared oracle surface.
 *
 * The C mode pointer is obtained via opus_custom_mode_create(48000, 960): under
 * non-CUSTOM_MODES that function returns the static mode48000_960_120 from
 * static_mode_list (modes.c), which is exactly the mode internal/celt mirrors.
 */
#ifndef GOOPUS_MDCT_SHIM_H
#define GOOPUS_MDCT_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include "modes.h" /* CELTMode, mdct_lookup, kiss_fft_state/cpx, opus_ifft &
                      clt_mdct_backward macros, opus_custom_mode_create decl */

/* oracle_celt_mode returns the frozen static CELT mode (48 kHz, 960). */
static const CELTMode *oracle_celt_mode(void)
{
   int err = 0;
   return opus_custom_mode_create(48000, 960, &err);
}

/* oracle_fft_nfft returns the FFT length of the mode's kfft[idx] (480/240/120/
   60 for idx 0..3). */
static int oracle_fft_nfft(int idx)
{
   const CELTMode *mode = oracle_celt_mode();
   return mode->mdct.kfft[idx]->nfft;
}

/* oracle_ifft runs opus_ifft on the mode's kfft[idx] over nfft complex inputs
   (inR/inI) and writes the complex output (outR/outI). All four arrays must
   have length >= nfft. Returns nfft. */
static int oracle_ifft(int idx, const int32_t *inR, const int32_t *inI,
                       int32_t *outR, int32_t *outI)
{
   const CELTMode *mode = oracle_celt_mode();
   const kiss_fft_state *st = mode->mdct.kfft[idx];
   int nfft = st->nfft;
   int i;
   kiss_fft_cpx *fin = (kiss_fft_cpx *)malloc(sizeof(kiss_fft_cpx) * nfft);
   kiss_fft_cpx *fout = (kiss_fft_cpx *)malloc(sizeof(kiss_fft_cpx) * nfft);
   for (i = 0; i < nfft; i++)
   {
      fin[i].r = inR[i];
      fin[i].i = inI[i];
   }
   opus_ifft(st, fin, fout, 0);
   for (i = 0; i < nfft; i++)
   {
      outR[i] = fout[i].r;
      outI[i] = fout[i].i;
   }
   free(fin);
   free(fout);
   return nfft;
}

/* oracle_ffft runs the forward opus_fft on the mode's kfft[idx] (same layout as
   oracle_ifft). Returns nfft. */
static int oracle_ffft(int idx, const int32_t *inR, const int32_t *inI,
                       int32_t *outR, int32_t *outI)
{
   const CELTMode *mode = oracle_celt_mode();
   const kiss_fft_state *st = mode->mdct.kfft[idx];
   int nfft = st->nfft;
   int i;
   kiss_fft_cpx *fin = (kiss_fft_cpx *)malloc(sizeof(kiss_fft_cpx) * nfft);
   kiss_fft_cpx *fout = (kiss_fft_cpx *)malloc(sizeof(kiss_fft_cpx) * nfft);
   for (i = 0; i < nfft; i++)
   {
      fin[i].r = inR[i];
      fin[i].i = inI[i];
   }
   opus_fft(st, fin, fout, 0);
   for (i = 0; i < nfft; i++)
   {
      outR[i] = fout[i].r;
      outI[i] = fout[i].i;
   }
   free(fin);
   free(fout);
   return nfft;
}

/* oracle_fft_impl runs opus_fft_impl directly on the mode's kfft[idx] with an
   explicit downshift over the caller-provided complex buffer (not bit-reversed).
   Returns nfft. For stage localization only. */
static int oracle_fft_impl(int idx, const int32_t *inR, const int32_t *inI,
                           int downshift, int32_t *outR, int32_t *outI)
{
   const CELTMode *mode = oracle_celt_mode();
   const kiss_fft_state *st = mode->mdct.kfft[idx];
   int nfft = st->nfft;
   int i;
   kiss_fft_cpx *buf = (kiss_fft_cpx *)malloc(sizeof(kiss_fft_cpx) * nfft);
   for (i = 0; i < nfft; i++)
   {
      buf[i].r = inR[i];
      buf[i].i = inI[i];
   }
   opus_fft_impl(st, buf, downshift);
   for (i = 0; i < nfft; i++)
   {
      outR[i] = buf[i].r;
      outI[i] = buf[i].i;
   }
   free(buf);
   return nfft;
}

/* oracle_mdct_backward runs clt_mdct_backward for LM (0..3), mono/non-transient,
   stride 1. `in` holds the N2 = (mode->mdct.n>>shift)>>1 = 120<<lm frequency
   coefficients; `out` must have length >= overlap/2 + N2 and is zero-filled
   first. Returns the written length (overlap/2 + N2). */
static int oracle_mdct_backward(int lm, const int32_t *in, int32_t *out)
{
   const CELTMode *mode = oracle_celt_mode();
   int shift = mode->maxLM - lm;
   int overlap = mode->overlap;
   int N = mode->mdct.n >> shift;
   int N2 = N >> 1;
   int outlen = overlap / 2 + N2;
   int i;
   kiss_fft_scalar *incopy = (kiss_fft_scalar *)malloc(sizeof(kiss_fft_scalar) * N2);
   for (i = 0; i < N2; i++)
      incopy[i] = in[i];
   for (i = 0; i < outlen; i++)
      out[i] = 0;
   clt_mdct_backward(&mode->mdct, incopy, (kiss_fft_scalar *)out, mode->window,
                     overlap, shift, 1, 0);
   free(incopy);
   return outlen;
}

#endif /* GOOPUS_MDCT_SHIM_H */
