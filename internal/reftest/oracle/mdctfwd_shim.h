//go:build refc

/*
 * mdctfwd_shim.h - C-callable wrappers over the pinned libopus forward FFT
 * (celt/kiss_fft.c opus_fft / opus_fft_impl) and forward MDCT (celt/mdct.c
 * clt_mdct_forward) for the internal/celt Checkpoint 3 differential test.
 *
 * Same design as mdct_shim.h: each wrapper is a `static` function, so this
 * header is pulled into the mdctfwd_cgo.go preamble as its own translation unit
 * with no extra .c translation unit. The non-static opus_fft_c / opus_fft_impl /
 * clt_mdct_forward_c / opus_custom_mode_create symbols are linked in via
 * w_celt_kiss_fft.c, w_celt_mdct.c and w_celt_modes.c. The package-level cgo
 * CFLAGS in oracle_cgo.go already define FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64 (non-CUSTOM_MODES, non-QEXT) and set the include paths.
 *
 * This file is separate from shim.h/shim.c and from the existing mdct_shim.h by
 * design; it never edits the shared oracle surface or the existing mdct/kiss
 * oracle files. Its statics have distinct names (mdctfwd_*), and even if they
 * did not, internal linkage keeps them from colliding across translation units.
 *
 * The C mode pointer is obtained via opus_custom_mode_create(48000, 960): under
 * non-CUSTOM_MODES that returns the static mode48000_960_120 from
 * static_mode_list (modes.c), exactly the mode internal/celt mirrors.
 */
#ifndef GOOPUS_MDCTFWD_SHIM_H
#define GOOPUS_MDCTFWD_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include "modes.h" /* CELTMode, mdct_lookup, kiss_fft_state/cpx, opus_fft,
                      opus_fft_impl & clt_mdct_forward macros,
                      opus_custom_mode_create decl */

/* mdctfwd_mode returns the frozen static CELT mode (48 kHz, 960). */
static const CELTMode *mdctfwd_mode(void)
{
   int err = 0;
   return opus_custom_mode_create(48000, 960, &err);
}

/* mdctfwd_fft_nfft returns the FFT length of the mode's kfft[idx] (480/240/120/
   60 for idx 0..3). */
static int mdctfwd_fft_nfft(int idx)
{
   return mdctfwd_mode()->mdct.kfft[idx]->nfft;
}

/* mdctfwd_ffft runs the forward opus_fft on the mode's kfft[idx] over nfft
   complex inputs (inR/inI), writing the complex output (outR/outI). All four
   arrays must have length >= nfft. Returns nfft. */
static int mdctfwd_ffft(int idx, const int32_t *inR, const int32_t *inI,
                        int32_t *outR, int32_t *outI)
{
   const kiss_fft_state *st = mdctfwd_mode()->mdct.kfft[idx];
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

/* mdctfwd_fft_impl runs opus_fft_impl directly on the mode's kfft[idx] with an
   explicit downshift over the caller-provided complex buffer (not bit-reversed).
   For per-stage downshift localization only. Returns nfft. */
static int mdctfwd_fft_impl(int idx, const int32_t *inR, const int32_t *inI,
                            int downshift, int32_t *outR, int32_t *outI)
{
   const kiss_fft_state *st = mdctfwd_mode()->mdct.kfft[idx];
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

/* mdctfwd_forward runs clt_mdct_forward for LM (0..3) with the given output
   stride (1 or 2), non-transient. `in` holds N2+overlap = (120<<lm)+overlap
   time-domain samples; `out` must have length >= stride*N2 and is zero-filled
   first. Returns the output length (stride*N2). */
static int mdctfwd_forward(int lm, int stride, const int32_t *in, int32_t *out)
{
   const CELTMode *mode = mdctfwd_mode();
   int shift = mode->maxLM - lm;
   int overlap = mode->overlap;
   int N = mode->mdct.n >> shift;
   int N2 = N >> 1;
   int inlen = N2 + overlap;
   int outlen = stride * N2;
   int i;
   kiss_fft_scalar *incopy = (kiss_fft_scalar *)malloc(sizeof(kiss_fft_scalar) * inlen);
   for (i = 0; i < inlen; i++)
      incopy[i] = in[i];
   for (i = 0; i < outlen; i++)
      out[i] = 0;
   clt_mdct_forward(&mode->mdct, incopy, (kiss_fft_scalar *)out, mode->window,
                    overlap, shift, stride, 0);
   free(incopy);
   return outlen;
}

#endif /* GOOPUS_MDCTFWD_SHIM_H */
