//go:build refc

/*
 * silkdecapi_shim.h - C-callable driver over the pinned libopus top-level SILK
 * decoder (silk/dec_API.c silk_Decode / silk_InitDecoder / silk_Get_Decoder_Size)
 * and the SILK encoder (silk/enc_API.c silk_Encode) for the internal/silk dec_API.go
 * end-to-end differential test.
 *
 * Strategy (hard-parts.md 5, decoder-architecture.md 3): the Go SILK port implements
 * the whole decode chain (per-channel frames, stereo un-mixing, resampler, PLC/CNG and
 * the FEC/LBRR path) tied together by silk_Decode, and SILK is a cross-frame /
 * cross-packet state machine, so a single call cannot validate it. This shim drives the
 * REAL C SILK ENCODER over caller-generated PCM to produce genuine SILK bitstream
 * packets (mono or stereo, optionally with in-band FEC / LBRR enabled), then drives the
 * REAL C silk_Decode exactly the way src/opus_decoder.c opus_decode_frame does (the
 * do/while loop over the payload's SILK frames, with the FLAG_DECODE_NORMAL /
 * FLAG_DECODE_LBRR / FLAG_PACKET_LOST lost-flag protocol). silkdecapi_test.go feeds the
 * identical bytes/flags to the pure-Go silk.Decoder.Decode IN LOCKSTEP and, after every
 * SILK frame, asserts the decoded int16 API-rate PCM is bit-identical and a hash of the
 * FULL persistent decoder state (both channel states incl. resampler + PLC + CNG, the
 * stereo un-mix state, and the super-struct channel counts / prev_decode_only_middle)
 * matches, so a divergence is caught on the frame it first appears.
 *
 * The encoder internal rate is pinned to fs_kHz (min==max==desired==encoder API rate),
 * so the packets are at a known SILK internal rate; the DECODER is then run at a
 * possibly different API_sampleRate so the resampler (internal -> API) is exercised for
 * real. Everything is static so the header pulls straight into silkdecapi_cgo.go's
 * preamble; silk_Encode / silk_Decode (and their whole dependency tree: NSQ, VAD,
 * resampler, PLC, CNG, stereo, ...) plus the ec_* symbols and SILK tables are linked in
 * via the existing w_silk_*.c / w_celt_ent*.c wrappers. This file never edits the shared
 * oracle surface (shim.h/shim.c/oracle_cgo.go). Build flags (FIXED_POINT +
 * DISABLE_FLOAT_API + OPUS_FAST_INT64) and include paths come from oracle_cgo.go.
 */
#ifndef GOOPUS_SILKDECAPI_SHIM_H
#define GOOPUS_SILKDECAPI_SHIM_H

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "libopus/silk/API.h"   /* silk_Encode / silk_InitEncoder / silk_Get_Encoder_Size / silk_Decode / silk_InitDecoder / silk_Get_Decoder_Size */
#include "libopus/silk/main.h"  /* silk_decoder_state / stereo_dec_state / tables + ec_* */

/* Per-packet range-coder buffer. A 60 ms WB stereo packet is well under this. */
#define SILKDECAPI_BUF_BYTES 1275
/* Max SILK frames produced by one decode operation (60 ms => 3). */
#define SILKDECAPI_MAX_FRAMES 3
/* Max interleaved API-rate samples one silk_Decode call can emit: 20 ms * 48 kHz * 2ch. */
#define SILKDECAPI_MAX_FRAME_SAMPLES (2 * 960)

/* resampler_function id for USE_silk_resampler_private_IIR_FIR (a dec_API-local #define
 * not exported in any header); the only path that reads the sFIR union via its i16 view. */
#define ORACLE_USE_silk_resampler_private_IIR_FIR 2

/* oracle_silk_decoder re-declares silk/dec_API.c's file-local silk_decoder super struct
 * so the shim can read the persistent state for hashing. With ENABLE_OSCE off (the frozen
 * build) there is no leading osce_model, so this layout is byte-identical to the real
 * silk_decoder; oracle_silkdecapi_create asserts sizeof matches silk_Get_Decoder_Size. */
typedef struct {
    silk_decoder_state channel_state[2];
    stereo_dec_state   sStereo;
    opus_int           nChannelsAPI;
    opus_int           nChannelsInternal;
    opus_int           prev_decode_only_middle;
} oracle_silk_decoder;

/* oracle_silkdecapi_ctx bundles the persistent C encoder + decoder that carry
 * cross-frame / cross-packet state over a whole sequence, plus the fixed decode config. */
typedef struct {
    void                  *enc;         /* silk_encoder super struct (heap) */
    silk_EncControlStruct  encControl;  /* encoder control (min==max==desired forces fs) */
    oracle_silk_decoder   *psDec;       /* decoder super struct (heap) */
    silk_DecControlStruct  decControl;  /* decoder control, reset per decode op */
    int fs_kHz;                         /* SILK internal rate (kHz) */
    int api_rate;                       /* decoder API output rate (Hz) */
    int payloadSize_ms;
    int nChannelsAPI;
    int nChannelsInternal;
    unsigned char dummy[2];             /* dummy range-coder buffer for the PLC path */
} oracle_silkdecapi_ctx;

/* oracle_silkdecapi_frame_out is the C decode result for one SILK frame within an
 * operation: the interleaved API-rate samples, the full persistent-state hash, and a few
 * scalars for pinpointing a divergence. */
typedef struct {
    int      nSamplesOut;                          /* per-channel API-rate sample count */
    int16_t  pcm[ SILKDECAPI_MAX_FRAME_SAMPLES ];  /* interleaved, [0 : nSamplesOut*nChannelsAPI) */
    uint64_t stateHash;
    uint32_t rng;                                  /* range decoder rng after this frame */
    int      tell;                                 /* ec_tell after this frame */
    int      prevPitchLag;                         /* decControl.prevPitchLag */
} oracle_silkdecapi_frame_out;

/* --- state hashing (FNV-1a, replicated byte-for-byte on the Go side) --------------- */

static void silkdecapi_hash_i64(uint64_t *h, int64_t v)
{
    uint64_t x = (uint64_t)v;
    int b;
    for (b = 0; b < 8; b++) {
        *h ^= (uint64_t)((x >> (8 * b)) & 0xff);
        *h *= 1099511628211ULL;
    }
}

/* silkdecapi_hash_resampler folds one channel's resampler_state into h. sFIR is
 * canonicalized to int32 through whichever union view the selected function uses (i16 for
 * IIR_FIR, i32 otherwise), which is exactly what the pure-Go ResamplerState.SFIR stores.
 * The Coefs pointer is skipped (it is fully determined by the config scalars). */
static void silkdecapi_hash_resampler(uint64_t *h, const silk_resampler_state_struct *S)
{
    int i;
    for (i = 0; i < SILK_RESAMPLER_MAX_IIR_ORDER; i++) silkdecapi_hash_i64(h, (int64_t)S->sIIR[i]);
    if (S->resampler_function == ORACLE_USE_silk_resampler_private_IIR_FIR) {
        for (i = 0; i < SILK_RESAMPLER_MAX_FIR_ORDER; i++) silkdecapi_hash_i64(h, (int64_t)S->sFIR.i16[i]);
    } else {
        for (i = 0; i < SILK_RESAMPLER_MAX_FIR_ORDER; i++) silkdecapi_hash_i64(h, (int64_t)S->sFIR.i32[i]);
    }
    for (i = 0; i < 96; i++) silkdecapi_hash_i64(h, (int64_t)S->delayBuf[i]);
    silkdecapi_hash_i64(h, (int64_t)S->resampler_function);
    silkdecapi_hash_i64(h, (int64_t)S->batchSize);
    silkdecapi_hash_i64(h, (int64_t)S->invRatio_Q16);
    silkdecapi_hash_i64(h, (int64_t)S->FIR_Order);
    silkdecapi_hash_i64(h, (int64_t)S->FIR_Fracs);
    silkdecapi_hash_i64(h, (int64_t)S->Fs_in_kHz);
    silkdecapi_hash_i64(h, (int64_t)S->Fs_out_kHz);
    silkdecapi_hash_i64(h, (int64_t)S->inputDelay);
}

/* silkdecapi_hash_channel folds one silk_decoder_state (synthesis + indices + resampler
 * + CNG + PLC + rate geometry) into h, in a fixed canonical order the Go side mirrors. */
static void silkdecapi_hash_channel(uint64_t *h, const silk_decoder_state *d)
{
    const SideInfoIndices *ix = &d->indices;
    const silk_CNG_struct *cng = &d->sCNG;
    const silk_PLC_struct *plc = &d->sPLC;
    int i;

    silkdecapi_hash_i64(h, (int64_t)d->prev_gain_Q16);
    for (i = 0; i < MAX_FRAME_LENGTH; i++)                              silkdecapi_hash_i64(h, (int64_t)d->exc_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)                                 silkdecapi_hash_i64(h, (int64_t)d->sLPC_Q14_buf[i]);
    for (i = 0; i < MAX_FRAME_LENGTH + 2 * MAX_SUB_FRAME_LENGTH; i++)   silkdecapi_hash_i64(h, (int64_t)d->outBuf[i]);
    silkdecapi_hash_i64(h, (int64_t)d->lagPrev);
    silkdecapi_hash_i64(h, (int64_t)d->LastGainIndex);
    silkdecapi_hash_i64(h, (int64_t)d->fs_kHz);
    silkdecapi_hash_i64(h, (int64_t)d->fs_API_hz);
    silkdecapi_hash_i64(h, (int64_t)d->nb_subfr);
    silkdecapi_hash_i64(h, (int64_t)d->frame_length);
    silkdecapi_hash_i64(h, (int64_t)d->subfr_length);
    silkdecapi_hash_i64(h, (int64_t)d->ltp_mem_length);
    silkdecapi_hash_i64(h, (int64_t)d->LPC_order);
    for (i = 0; i < MAX_LPC_ORDER; i++)                                 silkdecapi_hash_i64(h, (int64_t)d->prevNLSF_Q15[i]);
    silkdecapi_hash_i64(h, (int64_t)d->first_frame_after_reset);
    silkdecapi_hash_i64(h, (int64_t)d->nFramesDecoded);
    silkdecapi_hash_i64(h, (int64_t)d->nFramesPerPacket);
    silkdecapi_hash_i64(h, (int64_t)d->ec_prevSignalType);
    silkdecapi_hash_i64(h, (int64_t)d->ec_prevLagIndex);
    for (i = 0; i < MAX_FRAMES_PER_PACKET; i++)                         silkdecapi_hash_i64(h, (int64_t)d->VAD_flags[i]);
    silkdecapi_hash_i64(h, (int64_t)d->LBRR_flag);
    for (i = 0; i < MAX_FRAMES_PER_PACKET; i++)                         silkdecapi_hash_i64(h, (int64_t)d->LBRR_flags[i]);

    silkdecapi_hash_resampler(h, &d->resampler_state);

    silkdecapi_hash_i64(h, (int64_t)ix->signalType);
    silkdecapi_hash_i64(h, (int64_t)ix->quantOffsetType);
    for (i = 0; i < MAX_NB_SUBFR; i++)      silkdecapi_hash_i64(h, (int64_t)ix->GainsIndices[i]);
    for (i = 0; i < MAX_NB_SUBFR; i++)      silkdecapi_hash_i64(h, (int64_t)ix->LTPIndex[i]);
    for (i = 0; i < MAX_LPC_ORDER + 1; i++) silkdecapi_hash_i64(h, (int64_t)ix->NLSFIndices[i]);
    silkdecapi_hash_i64(h, (int64_t)ix->lagIndex);
    silkdecapi_hash_i64(h, (int64_t)ix->contourIndex);
    silkdecapi_hash_i64(h, (int64_t)ix->NLSFInterpCoef_Q2);
    silkdecapi_hash_i64(h, (int64_t)ix->PERIndex);
    silkdecapi_hash_i64(h, (int64_t)ix->LTP_scaleIndex);
    silkdecapi_hash_i64(h, (int64_t)ix->Seed);

    for (i = 0; i < MAX_FRAME_LENGTH; i++) silkdecapi_hash_i64(h, (int64_t)cng->CNG_exc_buf_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)    silkdecapi_hash_i64(h, (int64_t)cng->CNG_smth_NLSF_Q15[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++)    silkdecapi_hash_i64(h, (int64_t)cng->CNG_synth_state[i]);
    silkdecapi_hash_i64(h, (int64_t)cng->CNG_smth_Gain_Q16);
    silkdecapi_hash_i64(h, (int64_t)cng->rand_seed);
    silkdecapi_hash_i64(h, (int64_t)cng->fs_kHz);

    silkdecapi_hash_i64(h, (int64_t)d->lossCnt);
    silkdecapi_hash_i64(h, (int64_t)d->prevSignalType);

    silkdecapi_hash_i64(h, (int64_t)plc->pitchL_Q8);
    for (i = 0; i < LTP_ORDER; i++)     silkdecapi_hash_i64(h, (int64_t)plc->LTPCoef_Q14[i]);
    for (i = 0; i < MAX_LPC_ORDER; i++) silkdecapi_hash_i64(h, (int64_t)plc->prevLPC_Q12[i]);
    silkdecapi_hash_i64(h, (int64_t)plc->last_frame_lost);
    silkdecapi_hash_i64(h, (int64_t)plc->rand_seed);
    silkdecapi_hash_i64(h, (int64_t)plc->randScale_Q14);
    silkdecapi_hash_i64(h, (int64_t)plc->conc_energy);
    silkdecapi_hash_i64(h, (int64_t)plc->conc_energy_shift);
    silkdecapi_hash_i64(h, (int64_t)plc->prevLTP_scale_Q14);
    silkdecapi_hash_i64(h, (int64_t)plc->prevGain_Q16[0]);
    silkdecapi_hash_i64(h, (int64_t)plc->prevGain_Q16[1]);
    silkdecapi_hash_i64(h, (int64_t)plc->fs_kHz);
    silkdecapi_hash_i64(h, (int64_t)plc->nb_subfr);
    silkdecapi_hash_i64(h, (int64_t)plc->subfr_length);
}

/* oracle_silkdecapi_state_hash hashes the whole silk_decoder super struct: both channel
 * states, the stereo un-mix state and the super-struct scalars. */
static uint64_t oracle_silkdecapi_state_hash(const oracle_silk_decoder *d)
{
    uint64_t h = 14695981039346656037ULL;
    int i;

    silkdecapi_hash_channel(&h, &d->channel_state[0]);
    silkdecapi_hash_channel(&h, &d->channel_state[1]);

    for (i = 0; i < 2; i++) silkdecapi_hash_i64(&h, (int64_t)d->sStereo.pred_prev_Q13[i]);
    for (i = 0; i < 2; i++) silkdecapi_hash_i64(&h, (int64_t)d->sStereo.sMid[i]);
    for (i = 0; i < 2; i++) silkdecapi_hash_i64(&h, (int64_t)d->sStereo.sSide[i]);

    silkdecapi_hash_i64(&h, (int64_t)d->nChannelsAPI);
    silkdecapi_hash_i64(&h, (int64_t)d->nChannelsInternal);
    silkdecapi_hash_i64(&h, (int64_t)d->prev_decode_only_middle);

    return h;
}

/* --- sequence context lifecycle --------------------------------------------------- */

/* oracle_silkdecapi_create sets up the persistent encoder + decoder for a sequence. The
 * encoder internal rate is forced to fs_kHz (min==max==desired == encoder API rate ==
 * fs_kHz*1000) so packets are at a known internal rate; the decoder is later run at
 * api_rate (which may differ, exercising the resampler). useFEC enables in-band FEC /
 * LBRR coding. Returns NULL on failure. */
static oracle_silkdecapi_ctx *oracle_silkdecapi_create(int fs_kHz, int api_rate,
    int payloadSize_ms, int nChannels, int bitrate, int complexity, int useDTX, int useFEC)
{
    oracle_silkdecapi_ctx *ctx;
    int encSz = 0, decSz = 0;
    opus_int32 encRate = (opus_int32)fs_kHz * 1000;

    ctx = (oracle_silkdecapi_ctx *)calloc(1, sizeof(*ctx));
    if (ctx == NULL) return NULL;

    silk_Get_Encoder_Size(&encSz, nChannels);
    silk_Get_Decoder_Size(&decSz);
    ctx->enc   = calloc(1, (size_t)encSz);
    ctx->psDec = (oracle_silk_decoder *)calloc(1, sizeof(oracle_silk_decoder));
    if (ctx->enc == NULL || ctx->psDec == NULL || (size_t)decSz != sizeof(oracle_silk_decoder)) {
        /* A size mismatch means the real silk_decoder layout diverged from the shim's
         * re-declaration (e.g. an unexpected ENABLE_OSCE build); fail loudly. */
        free(ctx->enc);
        free(ctx->psDec);
        free(ctx);
        return NULL;
    }

    ctx->fs_kHz            = fs_kHz;
    ctx->api_rate          = api_rate;
    ctx->payloadSize_ms    = payloadSize_ms;
    ctx->nChannelsAPI      = nChannels;
    ctx->nChannelsInternal = nChannels;

    /* ---- Encoder ---- */
    silk_InitEncoder(ctx->enc, 1, 0, &ctx->encControl);
    ctx->encControl.nChannelsAPI              = nChannels;
    ctx->encControl.nChannelsInternal         = nChannels;
    ctx->encControl.API_sampleRate            = encRate;
    ctx->encControl.maxInternalSampleRate     = encRate;
    ctx->encControl.minInternalSampleRate     = encRate;
    ctx->encControl.desiredInternalSampleRate = encRate;
    ctx->encControl.payloadSize_ms            = payloadSize_ms;
    ctx->encControl.bitRate                   = bitrate;
    ctx->encControl.packetLossPercentage      = useFEC ? 20 : 0;
    ctx->encControl.complexity                = complexity;
    ctx->encControl.useInBandFEC              = useFEC ? 1 : 0;
    ctx->encControl.useDRED                   = 0;
    ctx->encControl.LBRR_coded                = useFEC ? 1 : 0;
    ctx->encControl.useDTX                    = useDTX;
    ctx->encControl.useCBR                    = 0;
    ctx->encControl.maxBits                   = 8 * (SILKDECAPI_BUF_BYTES - 1);
    ctx->encControl.toMono                    = 0;
    ctx->encControl.opusCanSwitch             = 0;
    ctx->encControl.reducedDependency         = 0;

    /* ---- Decoder ---- */
    silk_InitDecoder(ctx->psDec);

    return ctx;
}

static void oracle_silkdecapi_destroy(oracle_silkdecapi_ctx *ctx)
{
    if (ctx == NULL) return;
    free(ctx->enc);
    free(ctx->psDec);
    free(ctx);
}

/* oracle_silkdecapi_encode encodes one packet (payloadSize_ms of interleaved PCM,
 * nSamplesPerChannel per channel) with silk_Encode and finalizes the range coder.
 * activity is the caller's voice-activity decision (0/1); it must be 0 on silent frames
 * for DTX to eventually emit an empty packet. Returns the payload byte count, 0 for DTX
 * (no data), or a negative value on error. */
static int oracle_silkdecapi_encode(oracle_silkdecapi_ctx *ctx, const int16_t *pcm,
    int nSamplesPerChannel, unsigned char *buf, int bufcap, int activity)
{
    ec_enc enc;
    opus_int32 nBytes = bufcap;
    int ret;

    memset(buf, 0, (size_t)bufcap);
    ec_enc_init(&enc, buf, bufcap);
    ctx->encControl.maxBits    = 8 * (bufcap - 1);
    ctx->encControl.LBRR_coded = ctx->encControl.useInBandFEC ? 1 : 0;

    ret = silk_Encode(ctx->enc, &ctx->encControl, (const opus_res *)pcm, nSamplesPerChannel,
        &enc, &nBytes, 0, activity);
    if (ret != 0) {
        return -1000 + ret;
    }
    if (nBytes <= 0) {
        return 0; /* DTX: no SILK data for this packet */
    }
    ec_enc_done(&enc);
    return (int)nBytes;
}

/* oracle_silkdecapi_has_lbrr peeks the VAD/LBRR header of a packet (without consuming
 * the real decode) and returns 1 if any internal channel signals LBRR data, else 0. */
static int oracle_silkdecapi_has_lbrr(oracle_silkdecapi_ctx *ctx, const unsigned char *buf, int len)
{
    ec_dec dec;
    int n, i, any = 0;
    ec_dec_init(&dec, (unsigned char *)buf, len);
    for (n = 0; n < ctx->nChannelsInternal; n++) {
        int nFrames = (ctx->payloadSize_ms <= 20) ? 1 : (ctx->payloadSize_ms == 40 ? 2 : 3);
        for (i = 0; i < nFrames; i++) {
            (void)ec_dec_bit_logp(&dec, 1); /* VAD flag */
        }
        if (ec_dec_bit_logp(&dec, 1)) { /* LBRR flag */
            any = 1;
        }
    }
    return any;
}

/* oracle_silkdecapi_decode replicates opus_decode_frame's SILK driver over one decode
 * operation: it sets up decControl, then loops silk_Decode (first_frame on the first
 * call) until frame_size samples per channel are produced, recording one frame_out per
 * silk_Decode call. lostFlag is FLAG_DECODE_NORMAL / FLAG_DECODE_LBRR / FLAG_PACKET_LOST;
 * buf/len is the packet (ignored, and may be NULL, on the packet-loss path). frameSizeMs
 * is the requested output duration (the concealed/recovered frame length for the loss and
 * FEC paths). Returns the number of frames recorded, or a negative silk_Decode error. */
static int oracle_silkdecapi_decode(oracle_silkdecapi_ctx *ctx, int lostFlag,
    const unsigned char *buf, int len, int frameSizeMs, oracle_silkdecapi_frame_out *outs)
{
    ec_dec dec;
    opus_int32 frame_size, silk_frame_size = 0;
    int decoded_samples, nFrames = 0;
    opus_int16 *pcm_ptr;
    opus_int16 pcm[ SILKDECAPI_MAX_FRAMES * SILKDECAPI_MAX_FRAME_SAMPLES ];

    ctx->decControl.nChannelsAPI       = ctx->nChannelsAPI;
    ctx->decControl.nChannelsInternal  = ctx->nChannelsInternal;
    ctx->decControl.API_sampleRate     = ctx->api_rate;
    ctx->decControl.internalSampleRate = (opus_int32)ctx->fs_kHz * 1000;
    ctx->decControl.payloadSize_ms     = frameSizeMs;
    ctx->decControl.enable_deep_plc    = 0;

    /* frame_size (per channel) at API rate for the requested duration. */
    frame_size = (opus_int32)frameSizeMs * (ctx->api_rate / 1000);

    if (buf != NULL) {
        ec_dec_init(&dec, (unsigned char *)buf, len);
    } else {
        ec_dec_init(&dec, ctx->dummy, (opus_int32)sizeof(ctx->dummy));
    }

    decoded_samples = 0;
    pcm_ptr = pcm;
    do {
        int first_frame = decoded_samples == 0;
        oracle_silkdecapi_frame_out *o = &outs[nFrames];
        int silk_ret = silk_Decode(ctx->psDec, &ctx->decControl, lostFlag, first_frame,
            &dec, pcm_ptr, &silk_frame_size, 0 /* arch */);
        if (silk_ret != 0) {
            return -2000 + silk_ret;
        }

        o->nSamplesOut  = (int)silk_frame_size;
        memcpy(o->pcm, pcm_ptr, (size_t)silk_frame_size * ctx->nChannelsAPI * sizeof(int16_t));
        o->stateHash    = oracle_silkdecapi_state_hash(ctx->psDec);
        o->rng          = dec.rng;
        o->tell         = ec_tell(&dec);
        o->prevPitchLag = ctx->decControl.prevPitchLag;

        pcm_ptr        += silk_frame_size * ctx->nChannelsAPI;
        decoded_samples += silk_frame_size;
        nFrames++;
    } while (decoded_samples < frame_size && nFrames < SILKDECAPI_MAX_FRAMES);

    return nFrames;
}

#endif /* GOOPUS_SILKDECAPI_SHIM_H */
