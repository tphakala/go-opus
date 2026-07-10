//go:build refc

/*
 * silkcore_shim.h - C-callable driver over the pinned libopus SILK encoder
 * (silk/enc_API.c silk_Encode) and the SILK per-frame decode
 * (silk/decode_frame.c silk_decode_frame, and the header parse silk/dec_API.c
 * silk_Decode performs) for the internal/silk decode_core / decode_frame
 * differential test.
 *
 * Strategy (hard-parts.md 5, sequence-based cross-frame state testing): the Go SILK
 * port implements only the DECODE side, and SILK is a cross-frame state machine, so a
 * single-frame test cannot validate it. This shim therefore drives the REAL C SILK
 * ENCODER over caller-generated PCM to PRODUCE genuine SILK bitstream packets, then
 * feeds the same range-decoder buffer to the REAL C silk_decode_frame. silkcore_test.go
 * feeds the identical bytes to the pure-Go DecodeFrame IN LOCKSTEP and, after every
 * frame, asserts the decoded int16 output is bit-identical and a hash of the persistent
 * SILK decoder state matches, so a divergence is caught on the frame it first appears.
 *
 * Only the NORMAL (good-frame) decode path is exercised: no packet loss is simulated,
 * so lossCnt stays 0 for the whole sequence. On that path the C silk_decode_frame runs
 * silk_PLC / silk_CNG / silk_PLC_glue_frames, but all three touch only the sPLC/sCNG
 * sub-state (which this phase does not model) and leave both the returned frame and the
 * persistent synthesis state hashed below unchanged (verified by reading PLC.c/CNG.c:
 * PLC_update writes sPLC + prevSignalType, CNG with lossCnt==0 writes only sCNG, and
 * PLC_glue_frames modifies the frame only when the previous frame was lost). So the full
 * C silk_decode_frame is a valid bit-exact reference for the reduced Go DecodeFrame that
 * omits the not-yet-ported PLC/CNG.
 *
 * The encoder internal sampling rate is pinned by forcing min==max==desired==API rate,
 * so no resampling happens and the decoder fs_kHz is known up front. Everything is
 * static so the header pulls straight into silkcore_cgo.go's preamble; silk_Encode /
 * silk_decode_frame (and their whole dependency tree: NSQ, VAD, resampler, PLC, CNG,
 * ...) plus the ec_* symbols and SILK tables are linked in via the existing
 * w_silk_*.c / w_celt_ent*.c wrappers. This file never edits the shared oracle surface
 * (shim.h/shim.c/oracle_cgo.go). Build flags (FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64) and include paths come from oracle_cgo.go.
 */
#ifndef GOOPUS_SILKCORE_SHIM_H
#define GOOPUS_SILKCORE_SHIM_H

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "libopus/silk/API.h"  /* silk_Encode / silk_InitEncoder / silk_Get_Encoder_Size */
#include "libopus/silk/main.h" /* silk_decode_frame / silk_decode_indices / silk_decode_pulses + tables + ent* */

/* Per-packet range-coder buffer. A 60 ms WB packet at the encoder's internal rate
 * cap is well under this; 1275 is the Opus single-frame maximum, so it is ample. */
#define SILKCORE_BUF_BYTES 1275
/* Max SILK frames in one packet (60 ms => 3). */
#define SILKCORE_MAX_FRAMES 3

/* oracle_silkcore_ctx bundles the persistent C encoder + decoder that carry
 * cross-frame/cross-packet state over the whole sequence. */
typedef struct {
    void                  *enc;          /* silk_encoder super struct (heap) */
    silk_EncControlStruct  encControl;   /* encoder control (min==max==desired forces fs) */
    silk_decoder_state    *psDec;        /* single-channel decoder state (heap) */
    int fs_kHz;
    int payloadSize_ms;
    int nFramesPerPacket;
    int nb_subfr;
    int api_rate;
} oracle_silkcore_ctx;

/* oracle_silkcore_frame_out is the C decode result for one SILK frame: the decoded
 * samples, the persistent-state hash, the range-decoder end state and a handful of
 * scalar state fields for pinpointing a divergence. */
typedef struct {
    int16_t out[ MAX_FRAME_LENGTH ]; /* decoded frame ([0:frameLength] valid) */
    int      frameLength;
    int      signalType;             /* psDec->indices.signalType */
    int      quantOffsetType;
    uint64_t stateHash;              /* FNV-1a over the persistent decoder state */
    uint32_t rng;                    /* range decoder rng after this frame */
    int      tell;                   /* ec_tell after this frame */
    int32_t  prevGainQ16;
    int      lagPrev;
    int      lastGainIndex;
    int      prevSignalType;
    int      firstFrameAfterReset;
    int      ecPrevSignalType;
    int      ecPrevLagIndex;
} oracle_silkcore_frame_out;

/* silkcore_hash_i64 folds one int64 (little-endian, byte at a time) into an FNV-1a
 * 64-bit accumulator. silkcore_test.go replicates this exactly on the Go state so the
 * two hashes are directly comparable. */
static void silkcore_hash_i64(uint64_t *h, int64_t v)
{
    uint64_t x = (uint64_t)v;
    int b;
    for (b = 0; b < 8; b++) {
        *h ^= (uint64_t)((x >> (8 * b)) & 0xff);
        *h *= 1099511628211ULL;
    }
}

/* oracle_silkcore_state_hash hashes the persistent SILK decoder synthesis state in a
 * fixed canonical order: sLPC_Q14_buf, outBuf, exc_Q14, prevNLSF_Q15, then the scalar
 * cross-frame fields and the decoded indices. sPLC/sCNG are excluded (not modeled by
 * the Go port; unaffected on the good-frame path). */
static uint64_t oracle_silkcore_state_hash(const silk_decoder_state *d)
{
    uint64_t h = 14695981039346656037ULL;
    const SideInfoIndices *ix = &d->indices;
    int i;

    for (i = 0; i < MAX_LPC_ORDER; i++)                       silkcore_hash_i64(&h, (int64_t)d->sLPC_Q14_buf[i]);
    for (i = 0; i < MAX_FRAME_LENGTH + 2 * MAX_SUB_FRAME_LENGTH; i++) silkcore_hash_i64(&h, (int64_t)d->outBuf[i]);
    for (i = 0; i < MAX_FRAME_LENGTH; i++)                    silkcore_hash_i64(&h, (int64_t)d->exc_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)                       silkcore_hash_i64(&h, (int64_t)d->prevNLSF_Q15[i]);

    silkcore_hash_i64(&h, (int64_t)d->lagPrev);
    silkcore_hash_i64(&h, (int64_t)d->LastGainIndex);
    silkcore_hash_i64(&h, (int64_t)d->prev_gain_Q16);
    silkcore_hash_i64(&h, (int64_t)d->prevSignalType);
    silkcore_hash_i64(&h, (int64_t)d->lossCnt);
    silkcore_hash_i64(&h, (int64_t)d->first_frame_after_reset);
    silkcore_hash_i64(&h, (int64_t)d->ec_prevSignalType);
    silkcore_hash_i64(&h, (int64_t)d->ec_prevLagIndex);

    silkcore_hash_i64(&h, (int64_t)ix->signalType);
    silkcore_hash_i64(&h, (int64_t)ix->quantOffsetType);
    for (i = 0; i < MAX_NB_SUBFR; i++)     silkcore_hash_i64(&h, (int64_t)ix->GainsIndices[i]);
    for (i = 0; i < MAX_NB_SUBFR; i++)     silkcore_hash_i64(&h, (int64_t)ix->LTPIndex[i]);
    for (i = 0; i < MAX_LPC_ORDER + 1; i++) silkcore_hash_i64(&h, (int64_t)ix->NLSFIndices[i]);
    silkcore_hash_i64(&h, (int64_t)ix->lagIndex);
    silkcore_hash_i64(&h, (int64_t)ix->contourIndex);
    silkcore_hash_i64(&h, (int64_t)ix->NLSFInterpCoef_Q2);
    silkcore_hash_i64(&h, (int64_t)ix->PERIndex);
    silkcore_hash_i64(&h, (int64_t)ix->LTP_scaleIndex);
    silkcore_hash_i64(&h, (int64_t)ix->Seed);

    return h;
}

/* oracle_silkcore_create sets up the persistent encoder + decoder for a sequence.
 * The internal rate is forced to fs_kHz (min==max==desired==API rate) so no resampling
 * happens and the decoder configuration is known up front. Returns NULL on failure. */
static oracle_silkcore_ctx *oracle_silkcore_create(int fs_kHz, int payloadSize_ms,
    int bitrate, int complexity, int useDTX)
{
    oracle_silkcore_ctx *ctx;
    int encSz = 0;
    opus_int32 rate = (opus_int32)fs_kHz * 1000;

    ctx = (oracle_silkcore_ctx *)calloc(1, sizeof(*ctx));
    if (ctx == NULL) return NULL;

    silk_Get_Encoder_Size(&encSz, 1);
    ctx->enc = calloc(1, (size_t)encSz);
    ctx->psDec = (silk_decoder_state *)calloc(1, sizeof(silk_decoder_state));
    if (ctx->enc == NULL || ctx->psDec == NULL) {
        free(ctx->enc);
        free(ctx->psDec);
        free(ctx);
        return NULL;
    }

    /* ---- Encoder ---- */
    silk_InitEncoder(ctx->enc, 1, 0, &ctx->encControl);
    ctx->encControl.nChannelsAPI              = 1;
    ctx->encControl.nChannelsInternal         = 1;
    ctx->encControl.API_sampleRate            = rate;
    ctx->encControl.maxInternalSampleRate     = rate;
    ctx->encControl.minInternalSampleRate     = rate;
    ctx->encControl.desiredInternalSampleRate = rate;
    ctx->encControl.payloadSize_ms            = payloadSize_ms;
    ctx->encControl.bitRate                   = bitrate;
    ctx->encControl.packetLossPercentage      = 0;
    ctx->encControl.complexity                = complexity;
    ctx->encControl.useInBandFEC              = 0;
    ctx->encControl.useDRED                   = 0;
    ctx->encControl.LBRR_coded                = 0;
    ctx->encControl.useDTX                    = useDTX;
    ctx->encControl.useCBR                    = 0;
    ctx->encControl.maxBits                   = 8 * (SILKCORE_BUF_BYTES - 1);
    ctx->encControl.toMono                    = 0;
    ctx->encControl.opusCanSwitch             = 0;
    ctx->encControl.reducedDependency         = 0;

    /* ---- Decoder (mirrors silk_Decode's payloadSize_ms mapping + set_fs) ---- */
    ctx->fs_kHz         = fs_kHz;
    ctx->payloadSize_ms = payloadSize_ms;
    ctx->api_rate       = rate;
    switch (payloadSize_ms) {
        case 10: ctx->nFramesPerPacket = 1; ctx->nb_subfr = 2; break;
        case 20: ctx->nFramesPerPacket = 1; ctx->nb_subfr = 4; break;
        case 40: ctx->nFramesPerPacket = 2; ctx->nb_subfr = 4; break;
        default: ctx->nFramesPerPacket = 3; ctx->nb_subfr = 4; break; /* 60 */
    }
    silk_init_decoder(ctx->psDec);
    ctx->psDec->nFramesPerPacket = ctx->nFramesPerPacket;
    ctx->psDec->nb_subfr         = ctx->nb_subfr;
    silk_decoder_set_fs(ctx->psDec, fs_kHz, ctx->api_rate);

    return ctx;
}

static void oracle_silkcore_destroy(oracle_silkcore_ctx *ctx)
{
    if (ctx == NULL) return;
    free(ctx->enc);
    free(ctx->psDec);
    free(ctx);
}

/* oracle_silkcore_encode encodes one packet (payloadSize_ms of PCM) with silk_Encode
 * and finalizes the range coder. Returns the payload byte count, 0 for DTX (no data),
 * or a negative value on error. buf is zeroed then filled; bufcap must be
 * SILKCORE_BUF_BYTES. */
static int oracle_silkcore_encode(oracle_silkcore_ctx *ctx, const int16_t *pcm,
    int nSamples, unsigned char *buf, int bufcap)
{
    ec_enc enc;
    opus_int32 nBytes = bufcap;
    int ret;

    memset(buf, 0, (size_t)bufcap);
    ec_enc_init(&enc, buf, bufcap);
    ctx->encControl.maxBits = 8 * (bufcap - 1);

    ret = silk_Encode(ctx->enc, &ctx->encControl, (const opus_res *)pcm, nSamples,
        &enc, &nBytes, 0, 1 /* activity */);
    if (ret != 0) {
        return -1000 + ret;
    }
    if (nBytes <= 0) {
        return 0; /* DTX: no SILK data for this packet */
    }
    ec_enc_done(&enc);
    return (int)nBytes;
}

/* oracle_silkcore_decode_packet replays silk_Decode's mono, no-loss driver over the
 * packet: reset nFramesDecoded, parse the VAD + LBRR header, then call the real
 * silk_decode_frame for each frame, filling outs[0..nFramesPerPacket). Returns the
 * number of frames decoded. */
static int oracle_silkcore_decode_packet(oracle_silkcore_ctx *ctx,
    const unsigned char *buf, int bufcap, oracle_silkcore_frame_out *outs)
{
    silk_decoder_state *psDec = ctx->psDec;
    ec_dec dec;
    opus_int16 pOut[ MAX_FRAME_LENGTH + 2 ];
    opus_int32 nSamplesOutDec = 0;
    int i, n;

    psDec->nFramesDecoded = 0; /* newPacketFlag */
    ec_dec_init(&dec, (unsigned char *)buf, bufcap);

    /* Decode VAD flags and LBRR flag (mono, single internal channel). */
    for (i = 0; i < psDec->nFramesPerPacket; i++) {
        psDec->VAD_flags[i] = ec_dec_bit_logp(&dec, 1);
    }
    psDec->LBRR_flag = ec_dec_bit_logp(&dec, 1);

    /* Decode LBRR flags + skip LBRR data (FEC is disabled, so this stays inactive;
       kept for faithfulness to silk_Decode's FLAG_DECODE_NORMAL path). */
    memset(psDec->LBRR_flags, 0, sizeof(psDec->LBRR_flags));
    if (psDec->LBRR_flag) {
        if (psDec->nFramesPerPacket == 1) {
            psDec->LBRR_flags[0] = 1;
        } else {
            opus_int32 LBRR_symbol = ec_dec_icdf(&dec,
                silk_LBRR_flags_iCDF_ptr[psDec->nFramesPerPacket - 2], 8) + 1;
            for (i = 0; i < psDec->nFramesPerPacket; i++) {
                psDec->LBRR_flags[i] = silk_RSHIFT(LBRR_symbol, i) & 1;
            }
        }
        for (i = 0; i < psDec->nFramesPerPacket; i++) {
            if (psDec->LBRR_flags[i]) {
                opus_int16 lpulses[ MAX_FRAME_LENGTH ];
                opus_int cc = (i > 0 && psDec->LBRR_flags[i - 1]) ? CODE_CONDITIONALLY : CODE_INDEPENDENTLY;
                silk_decode_indices(psDec, &dec, i, 1, cc);
                silk_decode_pulses(&dec, lpulses, psDec->indices.signalType,
                    psDec->indices.quantOffsetType, psDec->frame_length);
            }
        }
    }

    for (n = 0; n < psDec->nFramesPerPacket; n++) {
        int FrameIndex = psDec->nFramesDecoded;
        int condCoding = (FrameIndex <= 0) ? CODE_INDEPENDENTLY : CODE_CONDITIONALLY;
        oracle_silkcore_frame_out *o = &outs[n];

        silk_decode_frame(psDec, &dec, pOut, &nSamplesOutDec, FLAG_DECODE_NORMAL, condCoding, psDec->arch);
        psDec->nFramesDecoded++;

        o->frameLength = psDec->frame_length;
        for (i = 0; i < psDec->frame_length; i++) {
            o->out[i] = pOut[i];
        }
        o->signalType           = psDec->indices.signalType;
        o->quantOffsetType      = psDec->indices.quantOffsetType;
        o->stateHash            = oracle_silkcore_state_hash(psDec);
        o->rng                  = dec.rng;
        o->tell                 = ec_tell(&dec);
        o->prevGainQ16          = psDec->prev_gain_Q16;
        o->lagPrev              = psDec->lagPrev;
        o->lastGainIndex        = psDec->LastGainIndex;
        o->prevSignalType       = psDec->prevSignalType;
        o->firstFrameAfterReset = psDec->first_frame_after_reset;
        o->ecPrevSignalType     = psDec->ec_prevSignalType;
        o->ecPrevLagIndex       = psDec->ec_prevLagIndex;
    }
    return psDec->nFramesPerPacket;
}

#endif /* GOOPUS_SILKCORE_SHIM_H */
