//go:build refc

/*
 * silkplc_shim.h - C-callable driver over the pinned libopus SILK encoder
 * (silk/enc_API.c silk_Encode) and the SILK per-frame decode
 * (silk/decode_frame.c silk_decode_frame) exercising the PACKET-LOSS branch that
 * runs silk_PLC (concealment), silk_CNG (comfort noise) and silk_PLC_glue_frames,
 * for the internal/silk plc.go / cng.go differential test.
 *
 * Strategy (hard-parts.md 5, sequence-based cross-frame state testing): SILK is a
 * cross-frame state machine and its packet-loss concealment carries its own sub-state
 * (sPLC, sCNG), so a single-frame test cannot validate it. This shim drives the REAL
 * C SILK ENCODER over caller-generated PCM to PRODUCE genuine SILK bitstream packets,
 * then decodes the sequence with the REAL C silk_decode_frame, with CHOSEN packets
 * marked LOST: a lost packet runs silk_decode_frame with FLAG_PACKET_LOST for each of
 * its frames (the else branch, which never touches the range coder), so PLC/CNG
 * conceal the missing audio; a received packet decodes normally. silkplc_test.go feeds
 * the identical byte stream and the identical loss pattern to the pure-Go DecodeFrame
 * IN LOCKSTEP and, after every frame (concealed AND recovery), asserts the decoded
 * int16 output is bit-identical and a hash of the persistent SILK decoder state
 * (INCLUDING sPLC and sCNG) matches, so a divergence is caught on the frame it first
 * appears.
 *
 * Losing whole packets keeps the per-packet range coder in sync: the first frame of
 * every packet is CODE_INDEPENDENTLY, so a lost packet never breaks the conditional
 * coding of the next packet; only the persistent synthesis state (which concealment
 * updates: outBuf, sLPC_Q14_buf, lagPrev, and the sPLC/sCNG sub-state) carries across,
 * exactly as in real decoding. The recovery frame (first good frame after a loss)
 * exercises silk_PLC_glue_frames.
 *
 * The encoder internal rate is pinned by forcing min==max==desired==API rate, so no
 * resampling happens and the decoder fs_kHz is known up front. This file never edits
 * the shared oracle surface. Build flags (FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64) and include paths come from oracle_cgo.go. ENABLE_DEEP_PLC /
 * ENABLE_OSCE / SMALL_FOOTPRINT are undefined in the oracle build, so this matches the
 * scalar reference paths the Go port targets.
 */
#ifndef GOOPUS_SILKPLC_SHIM_H
#define GOOPUS_SILKPLC_SHIM_H

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "libopus/silk/API.h"  /* silk_Encode / silk_InitEncoder / silk_Get_Encoder_Size */
#include "libopus/silk/main.h" /* silk_decode_frame + tables + ent* */

#define SILKPLC_BUF_BYTES 1275
#define SILKPLC_MAX_FRAMES 3

/* oracle_silkplc_ctx bundles the persistent C encoder + decoder that carry
 * cross-frame/cross-packet state (including sPLC/sCNG) over the whole sequence. */
typedef struct {
    void                  *enc;          /* silk_encoder super struct (heap) */
    silk_EncControlStruct  encControl;   /* encoder control (min==max==desired forces fs) */
    silk_decoder_state    *psDec;        /* single-channel decoder state (heap) */
    int fs_kHz;
    int payloadSize_ms;
    int nFramesPerPacket;
    int nb_subfr;
    int api_rate;
} oracle_silkplc_ctx;

/* oracle_silkplc_frame_out is the C decode result for one SILK frame. */
typedef struct {
    int16_t out[ MAX_FRAME_LENGTH ]; /* decoded frame ([0:frameLength] valid) */
    int      frameLength;
    int      signalType;             /* psDec->indices.signalType */
    int      quantOffsetType;
    uint64_t stateHash;              /* FNV-1a over the persistent decoder state (incl sPLC/sCNG) */
    uint32_t rng;                    /* range decoder rng after this frame (0 for lost frames) */
    int      tell;                   /* ec_tell after this frame (0 for lost frames) */
    int32_t  prevGainQ16;
    int      lagPrev;
    int      lastGainIndex;
    int      prevSignalType;
    int      firstFrameAfterReset;
    int      lossCnt;
    int      plcLastFrameLost;
    int32_t  plcRandSeed;
    int32_t  cngRandSeed;
} oracle_silkplc_frame_out;

/* silkplc_hash_i64 folds one int64 (little-endian) into an FNV-1a 64-bit accumulator.
 * silkplc_test.go replicates this exactly on the Go state. */
static void silkplc_hash_i64(uint64_t *h, int64_t v)
{
    uint64_t x = (uint64_t)v;
    int b;
    for (b = 0; b < 8; b++) {
        *h ^= (uint64_t)((x >> (8 * b)) & 0xff);
        *h *= 1099511628211ULL;
    }
}

/* oracle_silkplc_state_hash hashes the persistent SILK decoder synthesis state in a
 * fixed canonical order: sLPC_Q14_buf, outBuf, exc_Q14, prevNLSF_Q15, the scalar
 * cross-frame fields, the decoded indices, then the full sPLC and sCNG sub-state. The
 * ENABLE_DEEP_PLC-only sPLC.enable_deep_plc member is excluded (not modeled by Go;
 * stays 0). */
static uint64_t oracle_silkplc_state_hash(const silk_decoder_state *d)
{
    uint64_t h = 14695981039346656037ULL;
    const SideInfoIndices *ix = &d->indices;
    const silk_PLC_struct *p = &d->sPLC;
    const silk_CNG_struct *c = &d->sCNG;
    int i;

    for (i = 0; i < MAX_LPC_ORDER; i++)                              silkplc_hash_i64(&h, (int64_t)d->sLPC_Q14_buf[i]);
    for (i = 0; i < MAX_FRAME_LENGTH + 2 * MAX_SUB_FRAME_LENGTH; i++) silkplc_hash_i64(&h, (int64_t)d->outBuf[i]);
    for (i = 0; i < MAX_FRAME_LENGTH; i++)                           silkplc_hash_i64(&h, (int64_t)d->exc_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)                              silkplc_hash_i64(&h, (int64_t)d->prevNLSF_Q15[i]);

    silkplc_hash_i64(&h, (int64_t)d->lagPrev);
    silkplc_hash_i64(&h, (int64_t)d->LastGainIndex);
    silkplc_hash_i64(&h, (int64_t)d->prev_gain_Q16);
    silkplc_hash_i64(&h, (int64_t)d->prevSignalType);
    silkplc_hash_i64(&h, (int64_t)d->lossCnt);
    silkplc_hash_i64(&h, (int64_t)d->first_frame_after_reset);
    silkplc_hash_i64(&h, (int64_t)d->ec_prevSignalType);
    silkplc_hash_i64(&h, (int64_t)d->ec_prevLagIndex);

    silkplc_hash_i64(&h, (int64_t)ix->signalType);
    silkplc_hash_i64(&h, (int64_t)ix->quantOffsetType);
    for (i = 0; i < MAX_NB_SUBFR; i++)      silkplc_hash_i64(&h, (int64_t)ix->GainsIndices[i]);
    for (i = 0; i < MAX_NB_SUBFR; i++)      silkplc_hash_i64(&h, (int64_t)ix->LTPIndex[i]);
    for (i = 0; i < MAX_LPC_ORDER + 1; i++) silkplc_hash_i64(&h, (int64_t)ix->NLSFIndices[i]);
    silkplc_hash_i64(&h, (int64_t)ix->lagIndex);
    silkplc_hash_i64(&h, (int64_t)ix->contourIndex);
    silkplc_hash_i64(&h, (int64_t)ix->NLSFInterpCoef_Q2);
    silkplc_hash_i64(&h, (int64_t)ix->PERIndex);
    silkplc_hash_i64(&h, (int64_t)ix->LTP_scaleIndex);
    silkplc_hash_i64(&h, (int64_t)ix->Seed);

    /* sPLC */
    silkplc_hash_i64(&h, (int64_t)p->pitchL_Q8);
    for (i = 0; i < LTP_ORDER; i++)     silkplc_hash_i64(&h, (int64_t)p->LTPCoef_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++) silkplc_hash_i64(&h, (int64_t)p->prevLPC_Q12[i]);
    silkplc_hash_i64(&h, (int64_t)p->last_frame_lost);
    silkplc_hash_i64(&h, (int64_t)p->rand_seed);
    silkplc_hash_i64(&h, (int64_t)p->randScale_Q14);
    silkplc_hash_i64(&h, (int64_t)p->conc_energy);
    silkplc_hash_i64(&h, (int64_t)p->conc_energy_shift);
    silkplc_hash_i64(&h, (int64_t)p->prevLTP_scale_Q14);
    silkplc_hash_i64(&h, (int64_t)p->prevGain_Q16[0]);
    silkplc_hash_i64(&h, (int64_t)p->prevGain_Q16[1]);
    silkplc_hash_i64(&h, (int64_t)p->fs_kHz);
    silkplc_hash_i64(&h, (int64_t)p->nb_subfr);
    silkplc_hash_i64(&h, (int64_t)p->subfr_length);

    /* sCNG */
    for (i = 0; i < MAX_FRAME_LENGTH; i++) silkplc_hash_i64(&h, (int64_t)c->CNG_exc_buf_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)    silkplc_hash_i64(&h, (int64_t)c->CNG_smth_NLSF_Q15[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)    silkplc_hash_i64(&h, (int64_t)c->CNG_synth_state[i]);
    silkplc_hash_i64(&h, (int64_t)c->CNG_smth_Gain_Q16);
    silkplc_hash_i64(&h, (int64_t)c->rand_seed);
    silkplc_hash_i64(&h, (int64_t)c->fs_kHz);

    return h;
}

/* oracle_silkplc_create sets up the persistent encoder + decoder for a sequence, the
 * internal rate forced to fs_kHz. Returns NULL on failure. */
static oracle_silkplc_ctx *oracle_silkplc_create(int fs_kHz, int payloadSize_ms,
    int bitrate, int complexity, int useDTX)
{
    oracle_silkplc_ctx *ctx;
    int encSz = 0;
    opus_int32 rate = (opus_int32)fs_kHz * 1000;

    ctx = (oracle_silkplc_ctx *)calloc(1, sizeof(*ctx));
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
    ctx->encControl.maxBits                   = 8 * (SILKPLC_BUF_BYTES - 1);
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

static void oracle_silkplc_destroy(oracle_silkplc_ctx *ctx)
{
    if (ctx == NULL) return;
    free(ctx->enc);
    free(ctx->psDec);
    free(ctx);
}

/* oracle_silkplc_encode encodes one packet with silk_Encode. Returns the payload
 * byte count, 0 for DTX (no data), or negative on error. */
static int oracle_silkplc_encode(oracle_silkplc_ctx *ctx, const int16_t *pcm,
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

/* oracle_silkplc_fill_out captures the decode result + persistent-state hash for one
 * frame. rng/tell are zeroed for lost frames (the range coder is not consulted). */
static void oracle_silkplc_fill_out(oracle_silkplc_ctx *ctx, oracle_silkplc_frame_out *o,
    const opus_int16 *pOut, const ec_dec *dec, int lost)
{
    silk_decoder_state *psDec = ctx->psDec;
    int i;

    o->frameLength = psDec->frame_length;
    for (i = 0; i < psDec->frame_length; i++) {
        o->out[i] = pOut[i];
    }
    o->signalType           = psDec->indices.signalType;
    o->quantOffsetType      = psDec->indices.quantOffsetType;
    o->stateHash            = oracle_silkplc_state_hash(psDec);
    o->rng                  = lost ? 0u : dec->rng;
    o->tell                 = lost ? 0  : ec_tell((ec_dec *)dec);
    o->prevGainQ16          = psDec->prev_gain_Q16;
    o->lagPrev              = psDec->lagPrev;
    o->lastGainIndex        = psDec->LastGainIndex;
    o->prevSignalType       = psDec->prevSignalType;
    o->firstFrameAfterReset = psDec->first_frame_after_reset;
    o->lossCnt              = psDec->lossCnt;
    o->plcLastFrameLost     = psDec->sPLC.last_frame_lost;
    o->plcRandSeed          = psDec->sPLC.rand_seed;
    o->cngRandSeed          = psDec->sCNG.rand_seed;
}

/* oracle_silkplc_decode_packet decodes (lost==0) or conceals (lost!=0) one packet's
 * worth of frames, filling outs[0..nFramesPerPacket). A received packet parses the
 * VAD + LBRR header then calls silk_decode_frame FLAG_DECODE_NORMAL per frame; a lost
 * packet skips the header and the range coder entirely and calls silk_decode_frame
 * FLAG_PACKET_LOST per frame. Returns the number of frames decoded. */
static int oracle_silkplc_decode_packet(oracle_silkplc_ctx *ctx,
    const unsigned char *buf, int bufcap, int lost, oracle_silkplc_frame_out *outs)
{
    silk_decoder_state *psDec = ctx->psDec;
    ec_dec dec;
    unsigned char lostbuf[2] = { 0, 0 };
    opus_int16 pOut[ MAX_FRAME_LENGTH + 2 ];
    opus_int32 nSamplesOutDec = 0;
    int n;

    psDec->nFramesDecoded = 0; /* newPacketFlag */

    if (lost) {
        /* Lost packet: no range coder data. The FLAG_PACKET_LOST branch of
         * silk_decode_frame never reads it, but init a valid dummy so the pointer is
         * well-formed. */
        ec_dec_init(&dec, lostbuf, sizeof(lostbuf));
    } else {
        int i;
        ec_dec_init(&dec, (unsigned char *)buf, bufcap);
        /* Decode VAD flags and LBRR flag (mono, single internal channel). */
        for (i = 0; i < psDec->nFramesPerPacket; i++) {
            psDec->VAD_flags[i] = ec_dec_bit_logp(&dec, 1);
        }
        psDec->LBRR_flag = ec_dec_bit_logp(&dec, 1);
        /* FEC disabled: no LBRR data to skip. */
        memset(psDec->LBRR_flags, 0, sizeof(psDec->LBRR_flags));
    }

    for (n = 0; n < psDec->nFramesPerPacket; n++) {
        int FrameIndex = psDec->nFramesDecoded;
        int condCoding = (FrameIndex <= 0) ? CODE_INDEPENDENTLY : CODE_CONDITIONALLY;
        int lostFlag   = lost ? FLAG_PACKET_LOST : FLAG_DECODE_NORMAL;

        silk_decode_frame(psDec, &dec, pOut, &nSamplesOutDec, lostFlag, condCoding, psDec->arch);
        psDec->nFramesDecoded++;

        oracle_silkplc_fill_out(ctx, &outs[n], pOut, &dec, lost);
    }
    return psDec->nFramesPerPacket;
}

#endif /* GOOPUS_SILKPLC_SHIM_H */
