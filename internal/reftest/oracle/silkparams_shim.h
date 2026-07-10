//go:build refc

/*
 * silkparams_shim.h - C-callable wrapper over the pinned libopus SILK frame
 * index/parameter decode (silk/decode_indices.c, silk/decode_parameters.c,
 * silk/gain_quant.c silk_gains_dequant, silk/decode_pitch.c) for the internal/silk
 * differential test.
 *
 * The Go SILK port implements only the DECODE side. To exercise it against C we
 * need a valid side-information bitstream, so this shim runs the C ENCODE helper
 * silk_encode_indices over a caller-supplied, fully specified SideInfoIndices to
 * PRODUCE a bitstream, then runs the C DECODE helpers silk_decode_indices +
 * silk_decode_parameters over the same bitstream. silkparams_test.go feeds the SAME
 * byte buffer to the pure-Go DecodeIndices / DecodeParameters and asserts every
 * decoded index, every decoded parameter (Gains_Q16, PredCoef_Q12 for both
 * subframes, pitchL, LTPCoef_Q14, LTP_scale_Q14), the updated cross-frame state
 * (LastGainIndex, ec_prevSignalType, ec_prevLagIndex, prevNLSF_Q15) and the range
 * decoder end state (tell + rng) are bit-identical.
 *
 * The encoder and decoder states are configured with the same fs_kHz / nb_subfr
 * table selection silk/decoder_set_fs.c performs (NLSF codebook, pitch-lag low-bits
 * iCDF, pitch-contour iCDF), so the encode side and both decode sides agree.
 *
 * Everything is static so the header pulls straight into silkparams_cgo.go's
 * preamble; silk_encode_indices / silk_decode_indices / silk_decode_parameters
 * (and the silk_gains_dequant / silk_decode_pitch / silk_NLSF_decode / silk_NLSF2A
 * / silk_bwexpander helpers they call) plus the ec_* symbols and the SILK tables
 * are linked in via the existing w_silk_*.c / w_celt_ent*.c wrappers. This file
 * never edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64) and include paths
 * come from oracle_cgo.go; silk/main.h declares the encode/decode functions and
 * pulls in define.h (MAX_NB_SUBFR, MAX_LPC_ORDER, LTP_ORDER), structs.h
 * (silk_decoder_state / silk_encoder_state / silk_decoder_control / SideInfoIndices),
 * tables.h (the codebooks + iCDF tables) and entenc.h / entdec.h.
 */
#ifndef GOOPUS_SILKPARAMS_SHIM_H
#define GOOPUS_SILKPARAMS_SHIM_H

#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "libopus/silk/main.h" /* silk_encode/decode_indices + decode_parameters + tables + ent* */

/*
 * oracle_silkparams_in fully specifies one frame's SideInfoIndices plus the decoder
 * configuration and cross-frame state the encode/decode path reads. SideInfoIndices
 * fields are widened to int for a clean cgo mapping; the shim narrows them back to
 * the opus_int8 / opus_int16 SideInfoIndices members.
 */
typedef struct {
    int fs_kHz;                 /* 8 / 12 / 16 */
    int nb_subfr;               /* MAX_NB_SUBFR (20 ms) or MAX_NB_SUBFR/2 (10 ms) */
    int condCoding;             /* CODE_INDEPENDENTLY / _NO_LTP_SCALING / CODE_CONDITIONALLY */
    int frameIndex;             /* frame number (selects VAD flag) */
    int decodeLBRR;             /* LBRR (redundant) frame flag */
    int vadFlag;                /* VAD_flags[frameIndex] on the decoder */
    int ec_prevSignalType;      /* previous-frame entropy signal type */
    int ec_prevLagIndex;        /* previous-frame entropy lag index */
    int first_frame_after_reset;/* deactivates NLSF interpolation */
    int lossCnt;                /* nonzero -> BWE of LPC coefs */
    int lastGainIndex;          /* prev gain index (LastGainIndex), enc + dec */

    int signalType;
    int quantOffsetType;
    int gainsIndices[MAX_NB_SUBFR];
    int ltpIndex[MAX_NB_SUBFR];
    int nlsfIndices[MAX_LPC_ORDER + 1];
    int lagIndex;
    int contourIndex;
    int nlsfInterpCoef_Q2;
    int perIndex;
    int ltpScaleIndex;
    int seed;

    int16_t prevNLSF_Q15[MAX_LPC_ORDER];
} oracle_silkparams_in;

/*
 * oracle_silkparams_out returns the C-decoded SideInfoIndices, the decoded
 * prediction parameters, the updated cross-frame state and the range-decoder end
 * state, all as plain ints / fixed-width types for the Go comparison.
 */
typedef struct {
    int signalType;
    int quantOffsetType;
    int gainsIndices[MAX_NB_SUBFR];
    int ltpIndex[MAX_NB_SUBFR];
    int nlsfIndices[MAX_LPC_ORDER + 1];
    int lagIndex;
    int contourIndex;
    int nlsfInterpCoef_Q2;
    int perIndex;
    int ltpScaleIndex;
    int seed;

    int pitchL[MAX_NB_SUBFR];
    int32_t gains_Q16[MAX_NB_SUBFR];
    int16_t predCoef_Q12[2][MAX_LPC_ORDER];
    int16_t ltpCoef_Q14[LTP_ORDER * MAX_NB_SUBFR];
    int ltpScale_Q14;

    int lastGainIndex;          /* psDec->LastGainIndex after decode_parameters */
    int ec_prevSignalType;      /* psDec->ec_prevSignalType after decode_indices */
    int ec_prevLagIndex;        /* psDec->ec_prevLagIndex after decode_indices */
    int16_t prevNLSF_Q15[MAX_LPC_ORDER]; /* psDec->prevNLSF_Q15 after decode_parameters */

    int tell;                   /* ec_tell after decode_indices */
    uint32_t rng;               /* range-decoder rng after decode_indices */
    int nBytes;                 /* encoded byte count */
} oracle_silkparams_out;

/* oracle_silkparams_select_cb mirrors silk_decoder_set_fs codebook selection. */
static const silk_NLSF_CB_struct *oracle_silkparams_select_cb(int fs_kHz)
{
    return (fs_kHz == 16) ? &silk_NLSF_CB_WB : &silk_NLSF_CB_NB_MB;
}

/* oracle_silkparams_pitch_low mirrors silk_decoder_set_fs pitch_lag_low_bits_iCDF. */
static const opus_uint8 *oracle_silkparams_pitch_low(int fs_kHz)
{
    if (fs_kHz == 16) return silk_uniform8_iCDF;
    if (fs_kHz == 12) return silk_uniform6_iCDF;
    return silk_uniform4_iCDF; /* fs_kHz == 8 */
}

/* oracle_silkparams_pitch_contour mirrors silk_decoder_set_fs pitch_contour_iCDF. */
static const opus_uint8 *oracle_silkparams_pitch_contour(int fs_kHz, int nb_subfr)
{
    if (fs_kHz == 8) {
        return (nb_subfr == MAX_NB_SUBFR) ? silk_pitch_contour_NB_iCDF : silk_pitch_contour_10_ms_NB_iCDF;
    }
    return (nb_subfr == MAX_NB_SUBFR) ? silk_pitch_contour_iCDF : silk_pitch_contour_10_ms_iCDF;
}

/* oracle_silkparams_fill fills a SideInfoIndices from the widened input struct. */
static void oracle_silkparams_fill(SideInfoIndices *idx, const oracle_silkparams_in *in)
{
    int i;
    memset(idx, 0, sizeof(*idx));
    idx->signalType       = (opus_int8)in->signalType;
    idx->quantOffsetType  = (opus_int8)in->quantOffsetType;
    for (i = 0; i < MAX_NB_SUBFR; i++) {
        idx->GainsIndices[i] = (opus_int8)in->gainsIndices[i];
        idx->LTPIndex[i]     = (opus_int8)in->ltpIndex[i];
    }
    for (i = 0; i < MAX_LPC_ORDER + 1; i++) {
        idx->NLSFIndices[i] = (opus_int8)in->nlsfIndices[i];
    }
    idx->lagIndex          = (opus_int16)in->lagIndex;
    idx->contourIndex      = (opus_int8)in->contourIndex;
    idx->NLSFInterpCoef_Q2 = (opus_int8)in->nlsfInterpCoef_Q2;
    idx->PERIndex          = (opus_int8)in->perIndex;
    idx->LTP_scaleIndex    = (opus_int8)in->ltpScaleIndex;
    idx->Seed              = (opus_int8)in->seed;
}

/* oracle_silkparams_read copies a decoded SideInfoIndices into the output struct. */
static void oracle_silkparams_read(oracle_silkparams_out *out, const SideInfoIndices *idx)
{
    int i;
    out->signalType       = idx->signalType;
    out->quantOffsetType  = idx->quantOffsetType;
    for (i = 0; i < MAX_NB_SUBFR; i++) {
        out->gainsIndices[i] = idx->GainsIndices[i];
        out->ltpIndex[i]     = idx->LTPIndex[i];
    }
    for (i = 0; i < MAX_LPC_ORDER + 1; i++) {
        out->nlsfIndices[i] = idx->NLSFIndices[i];
    }
    out->lagIndex          = idx->lagIndex;
    out->contourIndex      = idx->contourIndex;
    out->nlsfInterpCoef_Q2 = idx->NLSFInterpCoef_Q2;
    out->perIndex          = idx->PERIndex;
    out->ltpScaleIndex     = idx->LTP_scaleIndex;
    out->seed              = idx->Seed;
}

/*
 * oracle_silkparams_run encodes the input SideInfoIndices into buf via
 * silk_encode_indices, then decodes buf via silk_decode_indices +
 * silk_decode_parameters, filling out. Returns the encoded byte count, or -1 on
 * allocation failure. Encoder and decoder states are heap-allocated and zeroed so
 * the large silk_encoder_state / silk_decoder_state stay off the stack.
 */
static int oracle_silkparams_run(const oracle_silkparams_in *in,
    oracle_silkparams_out *out, unsigned char *buf, int bufcap)
{
    silk_encoder_state *psEnc;
    silk_decoder_state *psDec;
    silk_decoder_control decCtrl;
    ec_enc enc;
    ec_dec dec;
    const silk_NLSF_CB_struct *cb;
    const opus_uint8 *pitch_low, *pitch_contour;
    int i, nBytes;

    psEnc = (silk_encoder_state *)calloc(1, sizeof(*psEnc));
    psDec = (silk_decoder_state *)calloc(1, sizeof(*psDec));
    if (psEnc == NULL || psDec == NULL) {
        free(psEnc);
        free(psDec);
        return -1;
    }
    memset(&decCtrl, 0, sizeof(decCtrl));

    cb            = oracle_silkparams_select_cb(in->fs_kHz);
    pitch_low     = oracle_silkparams_pitch_low(in->fs_kHz);
    pitch_contour = oracle_silkparams_pitch_contour(in->fs_kHz, in->nb_subfr);

    /* ---- Encoder state (just enough for silk_encode_indices) ---- */
    psEnc->fs_kHz                   = in->fs_kHz;
    psEnc->nb_subfr                 = in->nb_subfr;
    psEnc->predictLPCOrder          = cb->order;
    psEnc->psNLSF_CB                = cb;
    psEnc->pitch_lag_low_bits_iCDF  = pitch_low;
    psEnc->pitch_contour_iCDF       = pitch_contour;
    psEnc->ec_prevSignalType        = in->ec_prevSignalType;
    psEnc->ec_prevLagIndex          = (opus_int16)in->ec_prevLagIndex;
    oracle_silkparams_fill(&psEnc->indices, in);

    memset(buf, 0, bufcap);
    ec_enc_init(&enc, buf, bufcap);
    silk_encode_indices(psEnc, &enc, in->frameIndex, in->decodeLBRR, in->condCoding);
    ec_enc_done(&enc);
    nBytes = (ec_tell(&enc) + 7) >> 3;

    /* ---- Decoder state (just enough for silk_decode_indices/parameters) ---- */
    psDec->fs_kHz                   = in->fs_kHz;
    psDec->nb_subfr                 = in->nb_subfr;
    psDec->LPC_order                = cb->order;
    psDec->psNLSF_CB                = cb;
    psDec->pitch_lag_low_bits_iCDF  = pitch_low;
    psDec->pitch_contour_iCDF       = pitch_contour;
    psDec->ec_prevSignalType        = in->ec_prevSignalType;
    psDec->ec_prevLagIndex          = (opus_int16)in->ec_prevLagIndex;
    psDec->VAD_flags[in->frameIndex] = in->vadFlag;
    psDec->LastGainIndex            = (opus_int8)in->lastGainIndex;
    psDec->first_frame_after_reset  = in->first_frame_after_reset;
    psDec->lossCnt                  = in->lossCnt;
    psDec->arch                     = 0;
    for (i = 0; i < cb->order; i++) {
        psDec->prevNLSF_Q15[i] = in->prevNLSF_Q15[i];
    }

    ec_dec_init(&dec, buf, bufcap);
    silk_decode_indices(psDec, &dec, in->frameIndex, in->decodeLBRR, in->condCoding);
    /* Snapshot the decoded indices + range-decoder end state right after
     * decode_indices, before decode_parameters mutates indices (PERIndex zeroing
     * for unvoiced, NLSFInterpCoef_Q2 override on first_frame_after_reset), so the
     * comparison pins the decode_indices output exactly. */
    out->tell              = ec_tell(&dec);
    out->rng               = dec.rng;
    out->ec_prevSignalType = psDec->ec_prevSignalType;
    out->ec_prevLagIndex   = psDec->ec_prevLagIndex;
    oracle_silkparams_read(out, &psDec->indices);

    silk_decode_parameters(psDec, &decCtrl, in->condCoding);

    for (i = 0; i < MAX_NB_SUBFR; i++) {
        out->pitchL[i]   = decCtrl.pitchL[i];
        out->gains_Q16[i] = decCtrl.Gains_Q16[i];
    }
    for (i = 0; i < MAX_LPC_ORDER; i++) {
        out->predCoef_Q12[0][i] = decCtrl.PredCoef_Q12[0][i];
        out->predCoef_Q12[1][i] = decCtrl.PredCoef_Q12[1][i];
    }
    for (i = 0; i < LTP_ORDER * MAX_NB_SUBFR; i++) {
        out->ltpCoef_Q14[i] = decCtrl.LTPCoef_Q14[i];
    }
    out->ltpScale_Q14  = decCtrl.LTP_scale_Q14;
    out->lastGainIndex = psDec->LastGainIndex;
    for (i = 0; i < MAX_LPC_ORDER; i++) {
        out->prevNLSF_Q15[i] = psDec->prevNLSF_Q15[i];
    }
    out->nBytes = nBytes;

    free(psEnc);
    free(psDec);
    return nBytes;
}

#endif /* GOOPUS_SILKPARAMS_SHIM_H */
