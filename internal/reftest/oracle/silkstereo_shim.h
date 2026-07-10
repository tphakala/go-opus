//go:build refc

/*
 * silkstereo_shim.h - C-callable driver over the pinned libopus SILK stereo
 * un-mixing (silk/stereo_decode_pred.c + silk/stereo_MS_to_LR.c) for the
 * internal/silk stereo_decode_pred.go / stereo_ms_to_lr.go differential test.
 *
 * Two independent surfaces:
 *
 *   1. Predictor round-trip (oracle_stereo_pred_roundtrip): the SILK stereo predictor
 *      is symmetric, so we ENCODE caller-supplied quantization indices ix[2][3] and a
 *      mid-only flag with the REAL C encoder (silk_stereo_encode_pred +
 *      silk_stereo_encode_mid_only) to PRODUCE a genuine range-coded bitstream, then
 *      DECODE it with the REAL C silk_stereo_decode_pred + silk_stereo_decode_mid_only.
 *      The shim returns the bitstream plus the C-decoded pred_Q13[2], the mid-only flag
 *      and the range decoder's end state (rng, ec_tell). silkstereo_test.go feeds the
 *      identical bytes to the pure-Go StereoDecodePred / StereoDecodeMidOnly and asserts
 *      bit-identical pred_Q13, mid-only and coder end state, so the entropy decode path
 *      (joint iCDF split + uniform3/uniform5) and the fixed-point dequant are validated
 *      together against C.
 *
 *   2. Mid/side -> left/right (oracle_stereo_ms_to_lr): one frame of the REAL C
 *      silk_stereo_MS_to_LR over caller-supplied mid/side buffers and predictors, with
 *      the persistent stereo_dec_state (pred_prev_Q13, sMid, sSide) supplied and read
 *      back through a plain oracle_stereo_state so the Go test can carry it across a
 *      multi-frame SEQUENCE. Because the predictors change frame to frame the test
 *      exercises the 8 ms interpolation ramp; the sMid/sSide one-sample delay is carried
 *      exactly as in real decoding. silkstereo_test.go runs the identical sequence
 *      through the pure-Go StereoMStoLR and asserts the L/R int16 output and the full
 *      stereo state are bit-identical after every frame.
 *
 * This file never edits the shared oracle surface. Build flags (FIXED_POINT +
 * DISABLE_FLOAT_API) and include paths come from oracle_cgo.go; the stereo encoder,
 * decoder and un-mixer C sources are compiled via their w_silk_stereo_*.c wrappers.
 */
#ifndef GOOPUS_SILKSTEREO_SHIM_H
#define GOOPUS_SILKSTEREO_SHIM_H

#include <stdint.h>
#include <string.h>
#include "libopus/silk/main.h" /* silk_stereo_* + stereo_dec_state + ec_enc/ec_dec */

/* Plenty of room for the handful of range-coded symbols (joint + 2x uniform pair +
 * mid-only flag is well under 5 bytes). Both the C and Go decoders init over the whole
 * buffer, so its size is part of the compared coder state. */
#define STEREO_PRED_BUF_BYTES 64

/* oracle_stereo_pred_out bundles the produced bitstream and the C decode result. */
typedef struct {
    unsigned char buf[STEREO_PRED_BUF_BYTES]; /* the encoded range-coder bytes */
    int      nBytes;                          /* bytes the encoder used (informational) */
    int32_t  pred_Q13[2];                     /* C-decoded predictors */
    int      mid_only;                        /* C-decoded mid-only flag */
    uint32_t rng;                             /* ec_dec rng after decode */
    int      tell;                            /* ec_tell after decode */
} oracle_stereo_pred_out;

/* oracle_stereo_pred_roundtrip encodes ix_in (row-major ix[2][3]) + mid_only_in, then
 * decodes with the C reference. ix_in[0..2] is ix[0][*], ix_in[3..5] is ix[1][*]. The
 * caller must supply valid indices (ix[n][0] in [0,2], ix[n][1] in [0,4], ix[n][2] in
 * [0,4]); the encoder's celt_assert bounds hold there. Returns 0. */
static int oracle_stereo_pred_roundtrip(const int8_t ix_in[6], int mid_only_in,
    oracle_stereo_pred_out *o)
{
    ec_enc enc;
    ec_dec dec;
    opus_int8  ix[2][3];
    opus_int32 pred_Q13[2] = { 0, 0 };
    int decode_only_mid = 0;
    int i;

    for (i = 0; i < 3; i++) {
        ix[0][i] = ix_in[i];
        ix[1][i] = ix_in[3 + i];
    }

    /* ---- Encode ---- */
    memset(o->buf, 0, sizeof(o->buf));
    ec_enc_init(&enc, o->buf, sizeof(o->buf));
    silk_stereo_encode_pred(&enc, ix);
    silk_stereo_encode_mid_only(&enc, (opus_int8)mid_only_in);
    ec_enc_done(&enc);
    o->nBytes = (ec_tell(&enc) + 7) >> 3;

    /* ---- Decode with the C reference ---- */
    ec_dec_init(&dec, o->buf, sizeof(o->buf));
    silk_stereo_decode_pred(&dec, pred_Q13);
    silk_stereo_decode_mid_only(&dec, &decode_only_mid);

    o->pred_Q13[0] = pred_Q13[0];
    o->pred_Q13[1] = pred_Q13[1];
    o->mid_only    = decode_only_mid;
    o->rng         = dec.rng;
    o->tell        = ec_tell(&dec);
    return 0;
}

/* oracle_stereo_state is the persistent stereo_dec_state carried across frames, in a
 * plain layout the Go test owns. */
typedef struct {
    int16_t predPrevQ13[2];
    int16_t sMid[2];
    int16_t sSide[2];
} oracle_stereo_state;

/* oracle_stereo_ms_to_lr runs one frame of silk_stereo_MS_to_LR. x1/x2 are the
 * frameLength+2 mid/side buffers (this frame's samples at [2..frameLength+1]); on return
 * x1[1..frameLength]/x2[1..frameLength] hold left/right. st is reconstructed into a
 * stereo_dec_state, run through the C, then read back, so the caller-owned state carries
 * the pred_prev_Q13 / sMid / sSide memory to the next frame. */
static void oracle_stereo_ms_to_lr(oracle_stereo_state *st, int16_t *x1, int16_t *x2,
    const int32_t pred_Q13[2], int fs_kHz, int frame_length)
{
    stereo_dec_state s;
    opus_int32 pred[2];

    memset(&s, 0, sizeof(s));
    s.pred_prev_Q13[0] = st->predPrevQ13[0];
    s.pred_prev_Q13[1] = st->predPrevQ13[1];
    s.sMid[0]  = st->sMid[0];
    s.sMid[1]  = st->sMid[1];
    s.sSide[0] = st->sSide[0];
    s.sSide[1] = st->sSide[1];
    pred[0] = pred_Q13[0];
    pred[1] = pred_Q13[1];

    silk_stereo_MS_to_LR(&s, x1, x2, pred, fs_kHz, frame_length);

    st->predPrevQ13[0] = s.pred_prev_Q13[0];
    st->predPrevQ13[1] = s.pred_prev_Q13[1];
    st->sMid[0]  = s.sMid[0];
    st->sMid[1]  = s.sMid[1];
    st->sSide[0] = s.sSide[0];
    st->sSide[1] = s.sSide[1];
}

#endif /* GOOPUS_SILKSTEREO_SHIM_H */
