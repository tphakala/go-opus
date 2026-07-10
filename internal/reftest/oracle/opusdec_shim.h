//go:build refc

/*
 * opusdec_shim.h - a C-callable driver over the pinned libopus TOP-LEVEL Opus
 * ENCODER (src/opus_encoder.c opus_encode) with per-frame FORCE_MODE / bandwidth
 * control, for the internal/opusdec (src/opus_decoder.c) mode-transition,
 * redundancy and FEC/LBRR differential test.
 *
 * Strategy (docs/decoder-architecture.md 1, docs/plan.md phase-3 gate): the pure-Go
 * top-level decoder (opus.Decoder over internal/opusdec) is the CELT/SILK/hybrid
 * mode-transition machine, driven only by the RFC vectors at the gate. To exercise
 * the transitions, the CELT redundancy frames at SILK<->CELT boundaries, and the
 * FEC/LBRR paths on demand, this shim drives the REAL C Opus encoder to synthesize
 * packet SEQUENCES that force a scripted mode per frame (SILK<->hybrid<->CELT, which
 * makes opus_encode emit the redundancy frames automatically) and optionally carry
 * in-band FEC / LBRR. opusdec_test.go then decodes each packet through BOTH the C
 * opus_decode (via the shared oracle Decoder) and the Go opus.Decoder IN LOCKSTEP,
 * asserting bit-identical int16 PCM and equal per-packet final range.
 *
 * The C DECODE side reuses the shared oracle surface (oracle_decoder_create /
 * oracle_decode / oracle_decoder_final_range in shim.c/oracle_cgo.go); this file
 * only adds the encoder. Build flags (FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64) and include paths come from oracle_cgo.go; this header is pulled
 * into opusdec_cgo.go's preamble and never edits the shared oracle files.
 */
#ifndef GOOPUS_OPUSDEC_SHIM_H
#define GOOPUS_OPUSDEC_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "opus.h"
#include "opus_private.h" /* MODE_SILK_ONLY / MODE_HYBRID / MODE_CELT_ONLY, OPUS_SET_FORCE_MODE */

/* oracle_opusenc_ctx bundles a persistent top-level Opus encoder that carries the
 * cross-frame transition/redundancy state over a whole sequence. */
typedef struct {
    OpusEncoder *enc;
    int channels;
    int Fs;
} oracle_opusenc_ctx;

/* oracle_opusenc_create builds an OPUS_APPLICATION_AUDIO encoder at Fs / channels,
 * VBR off (so forced bitrate is honored frame to frame), with in-band FEC and a
 * packet-loss percentage enabled when useFEC != 0 (which makes SILK/hybrid frames
 * carry LBRR). Returns NULL on failure. */
static oracle_opusenc_ctx *oracle_opusenc_create(int Fs, int channels, int bitrate,
    int complexity, int useFEC)
{
    oracle_opusenc_ctx *ctx;
    int err = OPUS_OK;

    ctx = (oracle_opusenc_ctx *)calloc(1, sizeof(*ctx));
    if (ctx == NULL) return NULL;
    ctx->channels = channels;
    ctx->Fs = Fs;

    ctx->enc = opus_encoder_create((opus_int32)Fs, channels, OPUS_APPLICATION_AUDIO, &err);
    if (ctx->enc == NULL || err != OPUS_OK) {
        free(ctx);
        return NULL;
    }
    opus_encoder_ctl(ctx->enc, OPUS_SET_BITRATE(bitrate));
    opus_encoder_ctl(ctx->enc, OPUS_SET_VBR(0));
    opus_encoder_ctl(ctx->enc, OPUS_SET_COMPLEXITY(complexity));
    if (useFEC) {
        opus_encoder_ctl(ctx->enc, OPUS_SET_INBAND_FEC(1));
        opus_encoder_ctl(ctx->enc, OPUS_SET_PACKET_LOSS_PERC(20));
    }
    return ctx;
}

static void oracle_opusenc_destroy(oracle_opusenc_ctx *ctx)
{
    if (ctx == NULL) return;
    if (ctx->enc) opus_encoder_destroy(ctx->enc);
    free(ctx);
}

/* oracle_opusenc_encode encodes one frame (frameSize samples per channel) of
 * interleaved int16 PCM. forceMode is a MODE_* value (1000/1001/1002) or 0 for
 * OPUS_AUTO; forceBandwidth is an OPUS_BANDWIDTH_* value or 0 for OPUS_AUTO. Both
 * are applied for this frame so a scripted mode sequence produces the desired
 * SILK<->hybrid<->CELT transitions (and thus the redundancy frames). Returns the
 * packet byte count (>0), 0 for a DTX/empty packet, or a negative Opus error. */
static int oracle_opusenc_encode(oracle_opusenc_ctx *ctx, const int16_t *pcm,
    int frameSize, int forceMode, int forceBandwidth, unsigned char *buf, int maxBytes)
{
    opus_int32 fm = (forceMode != 0) ? (opus_int32)forceMode : OPUS_AUTO;
    opus_int32 bw = (forceBandwidth != 0) ? (opus_int32)forceBandwidth : OPUS_AUTO;
    int n;

    opus_encoder_ctl(ctx->enc, OPUS_SET_FORCE_MODE(fm));
    opus_encoder_ctl(ctx->enc, OPUS_SET_BANDWIDTH(bw));

    n = opus_encode(ctx->enc, pcm, frameSize, buf, maxBytes);
    return n;
}

/* oracle_opusenc_final_range returns the encoder's range coder state after the
 * last frame (OPUS_GET_FINAL_RANGE), which a matching decoder must reproduce. */
static uint32_t oracle_opusenc_final_range(oracle_opusenc_ctx *ctx)
{
    opus_uint32 range = 0;
    opus_encoder_ctl(ctx->enc, OPUS_GET_FINAL_RANGE(&range));
    return (uint32_t)range;
}

#endif /* GOOPUS_OPUSDEC_SHIM_H */
