//go:build refc

/*
 * silkpulses_shim.h - C-callable wrappers over the pinned libopus SILK
 * excitation coder (silk/encode_pulses.c, silk/decode_pulses.c,
 * silk/shell_coder.c, silk/code_signs.c) for the internal/silk differential test.
 *
 * The Go SILK port only implements the DECODE side (DecodePulses / silkShellDecoder
 * / silkDecodeSigns). To exercise it against C we need a valid coded excitation
 * bitstream, so this shim runs the C ENCODE helper silk_encode_pulses over a
 * caller-supplied random signed pulse vector to PRODUCE a bitstream, then runs the
 * C DECODE helper silk_decode_pulses over the same bitstream so the test can
 * cross-check Go against C (pulse vector bit-identical, decoder tell + rng, plus a
 * lossless round-trip back to the original excitation).
 *
 * Everything is static so the header pulls straight into silkpulses_cgo.go's
 * preamble; silk_encode_pulses / silk_decode_pulses (and the silk_shell_* /
 * silk_*_signs helpers they call) plus the ec_* symbols are linked in via the
 * existing w_silk_encode_pulses.c / w_silk_decode_pulses.c / w_silk_shell_coder.c /
 * w_silk_code_signs.c / w_celt_entenc.c / w_celt_entdec.c / w_celt_entcode.c
 * wrappers. This file never edits the shared oracle surface (shim.h/shim.c/
 * oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64) and include
 * paths come from oracle_cgo.go; silk/main.h declares silk_encode_pulses /
 * silk_decode_pulses and pulls in define.h (SHELL_CODEC_FRAME_LENGTH,
 * LOG2_SHELL_CODEC_FRAME_LENGTH, MAX_NB_SHELL_BLOCKS) plus entenc.h / entdec.h /
 * entcode.h (ec_enc / ec_dec, ec_enc_init/_done, ec_dec_init, ec_tell).
 */
#ifndef GOOPUS_SILKPULSES_SHIM_H
#define GOOPUS_SILKPULSES_SHIM_H

#include <stdint.h>
#include <string.h>
#include "libopus/silk/main.h" /* silk_encode_pulses/silk_decode_pulses + define.h + entenc/entdec */

/*
 * oracle_silk_encode_pulses range-encodes the length-frame_length signed pulse
 * vector pulses_in (opus_int8 domain) into buf (nbytes, zero-filled first) via
 * silk_encode_pulses. silk_encode_pulses may write SHELL_CODEC_FRAME_LENGTH
 * zero-pad samples at pulses[frame_length] for the 10 ms @ 12 kHz (frame_length ==
 * 120) case, so the input is copied into a padded local buffer sized
 * MAX_NB_SHELL_BLOCKS shell blocks. Returns the encoded byte count (ec_tell
 * rounded up to whole bytes); the buffer beyond that stays zero.
 */
static int oracle_silk_encode_pulses(const int8_t *pulses_in, int frame_length,
    int signalType, int quantOffsetType, unsigned char *buf, int nbytes)
{
    opus_int8 pulses[MAX_NB_SHELL_BLOCKS * SHELL_CODEC_FRAME_LENGTH];
    ec_enc enc;
    int i;
    memset(pulses, 0, sizeof(pulses));
    for (i = 0; i < frame_length; i++) {
        pulses[i] = (opus_int8)pulses_in[i];
    }
    memset(buf, 0, nbytes);
    ec_enc_init(&enc, buf, nbytes);
    silk_encode_pulses(&enc, signalType, quantOffsetType, pulses, frame_length);
    ec_enc_done(&enc);
    return (ec_tell(&enc) + 7) >> 3;
}

/*
 * oracle_silk_decode_pulses decodes buf (nbytes) with silk_decode_pulses into
 * pulses_out, reporting the decoder tell and rng after and returning the number
 * of pulse samples written (iter * SHELL_CODEC_FRAME_LENGTH, which for the 120-
 * sample 10 ms @ 12 kHz frame rounds up to 128). pulses_out must have room for
 * MAX_NB_SHELL_BLOCKS * SHELL_CODEC_FRAME_LENGTH samples.
 */
static int oracle_silk_decode_pulses(const unsigned char *buf, int nbytes,
    int frame_length, int signalType, int quantOffsetType,
    int16_t *pulses_out, int *tell_out, uint32_t *rng_out)
{
    opus_int16 pulses[MAX_NB_SHELL_BLOCKS * SHELL_CODEC_FRAME_LENGTH];
    ec_dec dec;
    int i, iter, nsamples;
    memset(pulses, 0, sizeof(pulses));
    ec_dec_init(&dec, (unsigned char *)buf, nbytes);
    silk_decode_pulses(&dec, pulses, signalType, quantOffsetType, frame_length);
    *tell_out = ec_tell(&dec);
    *rng_out = dec.rng;
    iter = frame_length >> LOG2_SHELL_CODEC_FRAME_LENGTH;
    if (iter * SHELL_CODEC_FRAME_LENGTH < frame_length) {
        iter++;
    }
    nsamples = iter * SHELL_CODEC_FRAME_LENGTH;
    for (i = 0; i < nsamples; i++) {
        pulses_out[i] = pulses[i];
    }
    return nsamples;
}

#endif /* GOOPUS_SILKPULSES_SHIM_H */
