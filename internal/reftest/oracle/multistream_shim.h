//go:build refc

/*
 * multistream_shim.h - a C-callable driver over the pinned libopus multistream
 * codec (src/opus_multistream_encoder.c + src/opus_multistream_decoder.c) for the
 * internal/opusdec multistream decoder (opus_multistream_decoder.go) differential
 * test.
 *
 * Strategy: the pure-Go multistream decoder is a scheduler around N single-stream
 * decoders plus the channel scatter. To prove it byte-exact, this shim drives the
 * REAL C surround encoder to synthesize multistream packet SEQUENCES for the
 * standard mapping-family layouts, then exposes a STATEFUL C multistream decoder
 * handle so multistream_test.go can replay each packet through BOTH the C
 * opus_multistream_decode and the Go opusdec.OpusMSDecoder in lockstep, asserting
 * bit-identical int16 PCM and equal per-packet final range. The decoder handle
 * takes an explicit (streams, coupled, mapping) layout, so the test can also drive
 * a custom scatter mapping (duplicate and muted 255 channels) over the same
 * encoded streams.
 *
 * The libopus multistream sources are compiled by the wm_src_opus_multistream*.c
 * wrappers (their own translation units); this header only DECLARES and calls the
 * public opus_multistream API, so there is a single definition of each symbol.
 * Build flags and include paths come from oracle_cgo.go; this header is pulled
 * into multistream_cgo.go's preamble and never edits the shared oracle files.
 */
#ifndef GOOPUS_MULTISTREAM_SHIM_H
#define GOOPUS_MULTISTREAM_SHIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "opus.h"
#include "opus_multistream.h"
#include "opus_private.h" /* MODE_CELT_ONLY, OPUS_SET_FORCE_MODE */

/* oracle_ms_surround_encode_seq builds a fresh mapping_family surround encoder at
 * Fs/channels with the given application and bitrate, writes the derived layout
 * (streams, coupled, mapping[channels]), and encodes num_frames frames of
 * interleaved int16 PCM. pcm_in is channels*frame_size*num_frames samples; frame i
 * is written to packets[i*max_packet_bytes ..] with its length in packet_lens[i].
 * Returns 0 on success or a negative Opus error code. */
static int oracle_ms_surround_encode_seq(
    int family, int channels, int Fs, int application, int bitrate,
    int frame_size, const int16_t *pcm_in, int num_frames,
    int *streams, int *coupled, unsigned char *mapping,
    unsigned char *packets, int32_t *packet_lens, int max_packet_bytes)
{
    int err = OPUS_OK;
    int st_streams = 0, st_coupled = 0;
    int i;
    OpusMSEncoder *enc = opus_multistream_surround_encoder_create(
        (opus_int32)Fs, channels, family, &st_streams, &st_coupled,
        mapping, application, &err);
    if (enc == NULL || err != OPUS_OK)
        return err != OPUS_OK ? err : OPUS_INTERNAL_ERROR;

    opus_multistream_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
    *streams = st_streams;
    *coupled = st_coupled;

    for (i = 0; i < num_frames; i++) {
        const int16_t *frame = pcm_in + (size_t)i * channels * frame_size;
        int n = opus_multistream_encode(enc, frame, frame_size,
            packets + (size_t)i * max_packet_bytes, max_packet_bytes);
        if (n < 0) {
            opus_multistream_encoder_destroy(enc);
            return n;
        }
        packet_lens[i] = n;
    }
    opus_multistream_encoder_destroy(enc);
    return 0;
}

/* oracle_ms_surround_encode_seq_opts is oracle_ms_surround_encode_seq with extra
 * encoder controls, so the differential test can force in-band FEC / LBRR, DTX, a
 * bandwidth and a signal type. complexity, vbr, bitrate are always applied; use_fec
 * and use_dtx toggle OPUS_SET_INBAND_FEC / OPUS_SET_DTX; packet_loss_perc sets the
 * loss hint (only when > 0); bandwidth and signal_type set OPUS_SET_BANDWIDTH /
 * OPUS_SET_SIGNAL when nonzero (0 means leave at OPUS_AUTO). Layout and packet
 * outputs match oracle_ms_surround_encode_seq. */
static int oracle_ms_surround_encode_seq_opts(
    int family, int channels, int Fs, int application, int bitrate,
    int complexity, int vbr, int use_fec, int packet_loss_perc, int use_dtx,
    int bandwidth, int signal_type,
    int frame_size, const int16_t *pcm_in, int num_frames,
    int *streams, int *coupled, unsigned char *mapping,
    unsigned char *packets, int32_t *packet_lens, int max_packet_bytes)
{
    int err = OPUS_OK;
    int st_streams = 0, st_coupled = 0;
    int i;
    OpusMSEncoder *enc = opus_multistream_surround_encoder_create(
        (opus_int32)Fs, channels, family, &st_streams, &st_coupled,
        mapping, application, &err);
    if (enc == NULL || err != OPUS_OK)
        return err != OPUS_OK ? err : OPUS_INTERNAL_ERROR;

    opus_multistream_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
    opus_multistream_encoder_ctl(enc, OPUS_SET_COMPLEXITY(complexity));
    opus_multistream_encoder_ctl(enc, OPUS_SET_VBR(vbr));
    if (use_fec)
        opus_multistream_encoder_ctl(enc, OPUS_SET_INBAND_FEC(1));
    if (packet_loss_perc > 0)
        opus_multistream_encoder_ctl(enc, OPUS_SET_PACKET_LOSS_PERC(packet_loss_perc));
    if (use_dtx)
        opus_multistream_encoder_ctl(enc, OPUS_SET_DTX(1));
    if (bandwidth != 0)
        opus_multistream_encoder_ctl(enc, OPUS_SET_BANDWIDTH(bandwidth));
    if (signal_type != 0)
        opus_multistream_encoder_ctl(enc, OPUS_SET_SIGNAL(signal_type));
    *streams = st_streams;
    *coupled = st_coupled;

    for (i = 0; i < num_frames; i++) {
        const int16_t *frame = pcm_in + (size_t)i * channels * frame_size;
        int n = opus_multistream_encode(enc, frame, frame_size,
            packets + (size_t)i * max_packet_bytes, max_packet_bytes);
        if (n < 0) {
            opus_multistream_encoder_destroy(enc);
            return n;
        }
        packet_lens[i] = n;
    }
    opus_multistream_encoder_destroy(enc);
    return 0;
}

/* oracle_ms_dec_create / _decode / _final_range / _destroy expose a stateful C
 * multistream decoder over a void* handle (avoids cgo incomplete-type friction).
 * The layout is explicit so the test can decode with a custom mapping. */
static void *oracle_ms_dec_create(int Fs, int channels, int streams,
    int coupled, const unsigned char *mapping, int *err)
{
    return (void *)opus_multistream_decoder_create((opus_int32)Fs, channels,
        streams, coupled, mapping, err);
}

static int oracle_ms_dec_decode(void *st, const unsigned char *data,
    int32_t len, int16_t *pcm, int frame_size, int decode_fec)
{
    return opus_multistream_decode((OpusMSDecoder *)st, data, len, pcm,
        frame_size, decode_fec);
}

static uint32_t oracle_ms_dec_final_range(void *st)
{
    opus_uint32 r = 0;
    opus_multistream_decoder_ctl((OpusMSDecoder *)st, OPUS_GET_FINAL_RANGE(&r));
    return (uint32_t)r;
}

static void oracle_ms_dec_destroy(void *st)
{
    opus_multistream_decoder_destroy((OpusMSDecoder *)st);
}

/* oracle_ms_plain_enc_* expose a stateful C multistream ENCODER over the PLAIN
 * (family 0/255, MAPPING_TYPE_NONE) constructor opus_multistream_encoder_create,
 * for the internal/opusenc multistream encoder (opus_multistream_encoder.go)
 * differential test: encode frame by frame through BOTH the C and the Go plain MS
 * encoders and assert byte-identical packets and equal final range. The layout is
 * explicit (streams, coupled, mapping), exactly the Go NewMSEncoder arguments. Only
 * the public opus_multistream API is used here, so there is one definition of each
 * symbol; the static rate_allocation probe lives in the wm_ translation unit. */
static void *oracle_ms_plain_enc_create(int Fs, int channels, int streams,
    int coupled, const unsigned char *mapping, int application, int *err)
{
    return (void *)opus_multistream_encoder_create((opus_int32)Fs, channels,
        streams, coupled, mapping, application, err);
}

static void oracle_ms_plain_enc_set_bitrate(void *st, int32_t v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_BITRATE(v));
}

static void oracle_ms_plain_enc_set_vbr(void *st, int v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_VBR(v));
}

static void oracle_ms_plain_enc_set_vbr_constraint(void *st, int v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_VBR_CONSTRAINT(v));
}

static void oracle_ms_plain_enc_set_complexity(void *st, int v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_COMPLEXITY(v));
}

static void oracle_ms_plain_enc_set_dtx(void *st, int v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_DTX(v));
}

static void oracle_ms_plain_enc_set_force_mode(void *st, int v)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_SET_FORCE_MODE(v));
}

static int oracle_ms_plain_enc_encode(void *st, const int16_t *pcm, int frame_size,
    unsigned char *data, int32_t max_bytes)
{
    return opus_multistream_encode((OpusMSEncoder *)st, pcm, frame_size, data, max_bytes);
}

static uint32_t oracle_ms_plain_enc_final_range(void *st)
{
    opus_uint32 r = 0;
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_GET_FINAL_RANGE(&r));
    return (uint32_t)r;
}

static void oracle_ms_plain_enc_reset(void *st)
{
    opus_multistream_encoder_ctl((OpusMSEncoder *)st, OPUS_RESET_STATE);
}

static void oracle_ms_plain_enc_destroy(void *st)
{
    opus_multistream_encoder_destroy((OpusMSEncoder *)st);
}

#endif /* GOOPUS_MULTISTREAM_SHIM_H */
