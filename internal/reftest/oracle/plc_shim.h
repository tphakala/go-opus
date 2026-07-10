//go:build refc

/*
 * plc_shim.h - C-callable wrappers over the pinned libopus CELT packet-loss
 * concealment primitives (celt/pitch.c, celt/celt_lpc.c) and a decoder
 * start-band setter, for the differential test of the pure-Go internal/celt PLC
 * (celt_decode_lost, pitch.go, celt_lpc.go).
 *
 * Two layers of test drive off this header:
 *   1. Function-level differential tests of the pure PLC math (_celt_lpc,
 *      celt_fir, celt_iir, _celt_autocorr, celt_pitch_xcorr_c, pitch_downsample,
 *      pitch_search) on random inputs. These are pure functions
 *      (docs/hard-parts.md section 8), so a function-level differential suffices.
 *   2. A SEQUENCE loss-pattern differential reusing the celtdec_shim.h raw CELT
 *      encoder/decoder: encode a frame sequence, then decode through the C and
 *      Go CELT decoders in lockstep with chosen frames DROPPED (data==NULL /
 *      len 0), asserting bit-exact PCM + rng + persistent-state after every frame
 *      including the concealed and recovery frames.
 *
 * The pitch.c / celt_lpc.c objects are already linked via w_celt_pitch.c and
 * w_celt_celt_lpc.c (see gen_wrappers.sh); this header only adds static glue and
 * never edits any existing oracle file. Build config/type mapping are as in
 * celtdec_shim.h (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64): opus_val16
 * / celt_coef = int16, opus_val32 / celt_sig = int32.
 */
#ifndef GOOPUS_PLC_SHIM_H
#define GOOPUS_PLC_SHIM_H

#include <stdint.h>
#include "celt.h"      /* CELTDecoder, CELT_SET_START_BAND_REQUEST */
#include "celt_lpc.h"  /* CELT_LPC_ORDER, _celt_lpc, celt_fir_c, celt_iir, _celt_autocorr */
#include "pitch.h"     /* pitch_downsample, pitch_search, celt_pitch_xcorr_c */

/* opus_custom_decoder_ctl is gated out of this TU by opus_custom.h (needs
   CELT_DECODER_C); declare the one signature we use. Matches
   include/opus_custom.h exactly. */
extern int opus_custom_decoder_ctl(OpusCustomDecoder *st, int request, ...);

/* --- pure PLC math (function-level differential) ------------------------- */

/* _celt_lpc: Levinson-Durbin + 16-bit fit. ac has p+1 entries; lpc gets p. */
static void oracle_celt_lpc(const int32_t *ac, int p, int16_t *lpc)
{
   _celt_lpc((opus_val16 *)lpc, (const opus_val32 *)ac, p);
}

/* celt_fir_c: xfull holds ord history samples then N samples (length N+ord); the
   filter is applied starting at xfull+ord. y receives N outputs. */
static void oracle_celt_fir(const int16_t *xfull, int N, int ord,
      const int16_t *num, int16_t *y)
{
   celt_fir_c((const opus_val16 *)xfull + ord, (const opus_val16 *)num,
         (opus_val16 *)y, N, ord, 0);
}

/* celt_iir: x,y length N; den,mem length ord (ord must be a multiple of 4). mem
   is updated in place on return. x and y are distinct here. */
static void oracle_celt_iir(const int32_t *x, const int16_t *den, int32_t *y,
      int N, int ord, int16_t *mem)
{
   celt_iir((const opus_val32 *)x, (const opus_val16 *)den, (opus_val32 *)y,
         N, ord, (opus_val16 *)mem, 0);
}

/* _celt_autocorr: returns the scaling shift; ac gets lag+1 entries. window may
   be NULL (with overlap 0). */
static int oracle_celt_autocorr(const int16_t *x, int32_t *ac,
      const int16_t *window, int overlap, int lag, int n)
{
   return _celt_autocorr((const opus_val16 *)x, (opus_val32 *)ac,
         (const celt_coef *)window, overlap, lag, n, 0);
}

/* celt_pitch_xcorr_c: returns maxcorr; xcorr gets max_pitch entries. */
static int32_t oracle_pitch_xcorr(const int16_t *x, const int16_t *y,
      int32_t *xcorr, int len, int max_pitch)
{
   return (int32_t)celt_pitch_xcorr_c((const opus_val16 *)x,
         (const opus_val16 *)y, (opus_val32 *)xcorr, len, max_pitch, 0);
}

/* pitch_downsample: x0/x1 are celt_sig buffers of length len*factor; x1 may be
   NULL for mono. x_lp gets len samples. */
static void oracle_pitch_downsample(const int32_t *x0, const int32_t *x1,
      int16_t *x_lp, int len, int C, int factor)
{
   celt_sig *x[2];
   x[0] = (celt_sig *)x0;
   x[1] = (celt_sig *)x1;
   pitch_downsample(x, (opus_val16 *)x_lp, len, C, factor, 0);
}

/* pitch_search: returns the pitch lag. */
static int oracle_pitch_search(const int16_t *x_lp, const int16_t *y,
      int len, int max_pitch)
{
   int pitch = 0;
   pitch_search((const opus_val16 *)x_lp, (opus_val16 *)y, len, max_pitch,
         &pitch, 0);
   return pitch;
}

/* --- decoder start band (for the start != 0 noise-regime differential) --- */

static int oracle_celtdec_set_start(void *dec, int start)
{
   return opus_custom_decoder_ctl((OpusCustomDecoder *)dec,
         CELT_SET_START_BAND_REQUEST, start);
}

#endif /* GOOPUS_PLC_SHIM_H */
