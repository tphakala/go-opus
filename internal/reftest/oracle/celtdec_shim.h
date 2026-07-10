//go:build refc

/*
 * celtdec_shim.h - C-callable wrappers over the pinned libopus RAW CELT encoder
 * and decoder (celt/celt_encoder.c, celt/celt_decoder.c) for the sequence-based
 * differential test of the pure-Go internal/celt decoder pipeline.
 *
 * WHY A RAW CELT ENCODER/DECODER (not opus_encode/opus_decode): the Go port
 * implements celt_decode_with_ec directly, so the oracle must drive the exact
 * same C entry point. Using the raw CELT API (celt_encoder_init /
 * celt_encode_with_ec and celt_decoder_init / celt_decode_with_ec) gives full
 * control over channels, frame size (LM 0..3), and CBR byte budget, and produces
 * pure CELT payloads (no Opus TOC), so both decoders read byte-identical input
 * with start=0, end=effEBands. No packet parsing, no bandwidth ambiguity.
 *
 * Build config (from oracle_cgo.go CFLAGS): FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64, non-CUSTOM_MODES, non-QEXT, non-ENABLE_RES24, non-DEEP_PLC.
 * In this config opus_res is opus_int16 (PCM in/out are int16), celt_sig /
 * celt_norm / celt_glog are opus_int32, celt_coef / opus_val16 are opus_int16.
 * The non-static celt_encoder.c / celt_decoder.c / bands.c / rate.c / ... symbols
 * are linked via the existing w_celt_*.c wrappers (see gen_wrappers.sh); this
 * header only adds static glue and never edits the shared oracle surface
 * (shim.h/shim.c/oracle_cgo.go) or any other existing oracle file.
 *
 * PERSISTENT-STATE DUMP: the CELT decoder is a cross-frame state machine
 * (docs/hard-parts.md section 5). struct OpusCustomDecoder is private to
 * celt_decoder.c, so oracle_celtdec_dump reads it through a byte-identical
 * replica struct (oracle_dec_layout) valid for the frozen config, and asserts
 * the replica's _decode_mem address matches the address derived from
 * celt_decoder_get_size(), so a struct drift on a libopus re-pin fails loudly
 * instead of hashing garbage.
 */
#ifndef GOOPUS_CELTDEC_SHIM_H
#define GOOPUS_CELTDEC_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>
#include "celt.h"       /* celt_encode_with_ec / celt_decode_with_ec / *_init / *_get_size,
                           CELTEncoder/CELTDecoder/CELTMode, arch.h types, opus_defines */
#include "modes.h"      /* OpusCustomMode fields (overlap, nbEBands) */
#include "celt_lpc.h"   /* CELT_LPC_ORDER */

/* DECODE_BUFFER_SIZE is #defined inside celt_decoder.c (== DEC_PITCH_BUF_SIZE);
   modes.h exposes DEC_PITCH_BUF_SIZE. */
#define ORACLE_DEC_BUF DEC_PITCH_BUF_SIZE

/* opus_custom_encoder_ctl is gated out of this TU by opus_custom.h (needs
   CELT_ENCODER_C), so declare the one ctl we use (encoder complexity). The
   signature matches include/opus_custom.h exactly; no conflicting declaration is
   in scope here. */
extern int opus_custom_encoder_ctl(OpusCustomEncoder *st, int request, ...);

/* Fail the build loudly if the type config drifts from what the dump assumes. */
typedef char oracle_assert_res_is_int16[(sizeof(opus_res) == 2) ? 1 : -1];
typedef char oracle_assert_sig_is_int32[(sizeof(celt_sig) == 4) ? 1 : -1];
typedef char oracle_assert_glog_is_int32[(sizeof(celt_glog) == 4) ? 1 : -1];

/*
 * Byte-identical replica of struct OpusCustomDecoder (celt_decoder.c:87) for the
 * frozen config (no ENABLE_QEXT, no ENABLE_DEEP_PLC members). Field order/types
 * mirror the source so member offsets match the real struct's ABI; only used to
 * read persistent state, never to allocate.
 */
struct oracle_dec_layout {
   const void *mode;
   int overlap;
   int channels;
   int stream_channels;
   int downsample;
   int start, end;
   int signalling;
   int disable_inv;
   int complexity;
   int arch;
   /* DECODER_RESET_START rng */
   opus_uint32 rng;
   int error;
   int last_pitch_index;
   int loss_duration;
   int plc_duration;
   int last_frame_type;
   int skip_plc;
   int postfilter_period;
   int postfilter_period_old;
   opus_val16 postfilter_gain;
   opus_val16 postfilter_gain_old;
   int postfilter_tapset;
   int postfilter_tapset_old;
   int prefilter_and_fold;
   celt_sig preemph_memD[2];
   celt_sig _decode_mem[1];
};

/* --- raw CELT encoder ---------------------------------------------------- */

static void *oracle_celtenc_create(int channels, int complexity)
{
   int size = celt_encoder_get_size(channels);
   CELTEncoder *st = (CELTEncoder *)malloc((size_t)size);
   if (!st)
      return NULL;
   if (celt_encoder_init(st, 48000, channels, 0) != OPUS_OK) {
      free(st);
      return NULL;
   }
   /* Default state after init is CBR (st->vbr==0). Only set complexity. */
   opus_custom_encoder_ctl(st, OPUS_SET_COMPLEXITY_REQUEST, complexity);
   return st;
}

/* oracle_celtenc_encode CBR-encodes one int16 frame into nb_bytes bytes and
   returns the actual packet length (celt_encode_with_ec return value). */
static int oracle_celtenc_encode(void *enc, const int16_t *pcm, int frame_size,
      int nb_bytes, unsigned char *buf)
{
   return celt_encode_with_ec((CELTEncoder *)enc, (const opus_res *)pcm,
         frame_size, buf, nb_bytes, NULL);
}

static void oracle_celtenc_destroy(void *enc) { free(enc); }

/* --- raw CELT decoder ---------------------------------------------------- */

static void *oracle_celtdec_create(int channels)
{
   int size = celt_decoder_get_size(channels);
   CELTDecoder *st = (CELTDecoder *)malloc((size_t)size);
   if (!st)
      return NULL;
   if (celt_decoder_init(st, 48000, channels) != OPUS_OK) {
      free(st);
      return NULL;
   }
   return st;
}

/* oracle_celtdec_decode decodes one packet into interleaved int16 pcm and
   returns samples per channel (celt_decode_with_ec return value). */
static int oracle_celtdec_decode(void *dec, const unsigned char *data, int len,
      int frame_size, int16_t *pcm)
{
   return celt_decode_with_ec((CELTDecoder *)dec, data, len, (opus_res *)pcm,
         frame_size, NULL, 0);
}

static void oracle_celtdec_destroy(void *dec) { free(dec); }

static int oracle_celtdec_nbebands(void)
{
   int e = 0;
   const OpusCustomMode *m = opus_custom_mode_create(48000, 960, &e);
   return m->nbEBands;
}

static int oracle_celtdec_decodemem_len(int channels)
{
   int e = 0;
   const OpusCustomMode *m = opus_custom_mode_create(48000, 960, &e);
   return channels * (ORACLE_DEC_BUF + m->overlap);
}

static int oracle_celtdec_lpc_len(int channels) { return channels * CELT_LPC_ORDER; }

/*
 * oracle_celtdec_dump copies the decoder's persistent cross-frame state into
 * caller-provided buffers.
 *   scalars     len 16: [0]=rng, [1]=postfilter_period, [2]=..._period_old,
 *               [3]=postfilter_gain, [4]=..._gain_old, [5]=postfilter_tapset,
 *               [6]=..._tapset_old, [7]=prefilter_and_fold, [8]=loss_duration,
 *               [9]=plc_duration, [10]=skip_plc, [11]=last_frame_type,
 *               [12]=last_pitch_index, [13]=preemph_memD[0], [14]=preemph_memD[1],
 *               [15]=reserved.
 *   decode_mem  len channels*(ORACLE_DEC_BUF+overlap)
 *   old_ebands / old_loge / old_loge2 / background  len 2*nbEBands each
 *   lpc         len channels*CELT_LPC_ORDER (int16)
 */
static void oracle_celtdec_dump(void *decoder, int channels,
      int32_t *scalars, int32_t *decode_mem,
      int32_t *old_ebands, int32_t *old_loge, int32_t *old_loge2,
      int32_t *background, int16_t *lpc)
{
   struct oracle_dec_layout *st = (struct oracle_dec_layout *)decoder;
   const OpusCustomMode *mode = (const OpusCustomMode *)st->mode;
   int overlap = mode->overlap;
   int nbEBands = mode->nbEBands;
   int per = ORACLE_DEC_BUF + overlap;
   int dm_len = channels * per;
   int i;

   celt_sig *dm = st->_decode_mem;
   celt_glog *oldBandE = (celt_glog *)(dm + (size_t)per * channels);
   celt_glog *oldLogE = oldBandE + 2 * nbEBands;
   celt_glog *oldLogE2 = oldLogE + 2 * nbEBands;
   celt_glog *backgroundLogE = oldLogE2 + 2 * nbEBands;
   opus_val16 *lpcp = (opus_val16 *)(backgroundLogE + 2 * nbEBands);

   /* Cross-check the replica against celt_decoder_get_size so a re-pin that
      changes the struct fails the assert instead of dumping garbage. */
   {
      size_t total = (size_t)celt_decoder_get_size(channels);
      size_t tail = (size_t)(channels * per - 1) * sizeof(celt_sig)
                  + (size_t)(4 * 2 * nbEBands) * sizeof(celt_glog)
                  + (size_t)(channels * CELT_LPC_ORDER) * sizeof(opus_val16);
      size_t struct_size = total - tail;
      celt_sig *dm_formula = (celt_sig *)((char *)decoder + struct_size - sizeof(celt_sig));
      assert(dm_formula == dm);
   }

   scalars[0] = (int32_t)st->rng;
   scalars[1] = st->postfilter_period;
   scalars[2] = st->postfilter_period_old;
   scalars[3] = st->postfilter_gain;
   scalars[4] = st->postfilter_gain_old;
   scalars[5] = st->postfilter_tapset;
   scalars[6] = st->postfilter_tapset_old;
   scalars[7] = st->prefilter_and_fold;
   scalars[8] = st->loss_duration;
   scalars[9] = st->plc_duration;
   scalars[10] = st->skip_plc;
   scalars[11] = st->last_frame_type;
   scalars[12] = st->last_pitch_index;
   scalars[13] = st->preemph_memD[0];
   scalars[14] = st->preemph_memD[1];
   scalars[15] = 0;

   for (i = 0; i < dm_len; i++)
      decode_mem[i] = dm[i];
   for (i = 0; i < 2 * nbEBands; i++) {
      old_ebands[i] = oldBandE[i];
      old_loge[i] = oldLogE[i];
      old_loge2[i] = oldLogE2[i];
      background[i] = backgroundLogE[i];
   }
   for (i = 0; i < channels * CELT_LPC_ORDER; i++)
      lpc[i] = lpcp[i];
}

#endif /* GOOPUS_CELTDEC_SHIM_H */
