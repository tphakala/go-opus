//go:build refc

/*
 * shim.c - implementation of the oracle C surface declared in shim.h.
 *
 * This translation unit only includes libopus HEADERS (never a libopus .c), so
 * it never collides with the per-source wrapper TUs (w_*.c).
 */
#include "shim.h"

#include "opus.h"
#include "opus_private.h" /* MODE_CELT_ONLY, OPUS_SET_FORCE_MODE */
#include "arch.h"         /* OPUS_FAST_INT64 */

/*
 * Freeze the oracle configuration at compile time. A mis-flagged build (missing
 * -DFIXED_POINT, a stray float API, a 32-bit host, or an accidental CUSTOM_MODES
 * /QEXT) fails to compile instead of silently producing a non-oracle that would
 * poison every differential gate. Mirrors CLAUDE.md's frozen C configuration.
 */
#if !defined(FIXED_POINT)
#error "oracle must be built with -DFIXED_POINT"
#endif
#if !defined(DISABLE_FLOAT_API)
#error "oracle must be built with -DDISABLE_FLOAT_API"
#endif
#if !OPUS_FAST_INT64
#error "oracle requires OPUS_FAST_INT64 (64-bit host); see hard-parts.md section 4"
#endif
#if defined(CUSTOM_MODES)
#error "oracle must NOT define CUSTOM_MODES"
#endif
#if defined(ENABLE_QEXT)
#error "oracle must NOT define ENABLE_QEXT"
#endif

void oracle_get_build_config(oracle_build_config *cfg)
{
#if defined(FIXED_POINT)
    cfg->fixed_point = 1;
#else
    cfg->fixed_point = 0;
#endif
#if defined(DISABLE_FLOAT_API)
    cfg->disable_float_api = 1;
#else
    cfg->disable_float_api = 0;
#endif
    cfg->fast_int64 = OPUS_FAST_INT64;
#if defined(CUSTOM_MODES)
    cfg->custom_modes = 1;
#else
    cfg->custom_modes = 0;
#endif
#if defined(ENABLE_QEXT)
    cfg->enable_qext = 1;
#else
    cfg->enable_qext = 0;
#endif
    cfg->arch_int64 = (sizeof(opus_int64) == 8) ? 1 : 0;
}

const char *oracle_version_string(void)
{
    return opus_get_version_string();
}

OpusEncoder *oracle_encoder_create_celt(opus_int32 fs, int channels,
                                        opus_int32 bitrate, int complexity,
                                        int *err)
{
    int e = OPUS_OK;
    OpusEncoder *enc = opus_encoder_create(fs, channels, OPUS_APPLICATION_AUDIO, &e);
    if (enc == NULL || e != OPUS_OK) {
        if (err) *err = e;
        if (enc) opus_encoder_destroy(enc);
        return NULL;
    }
    /* Force CELT-only so this matches the phase-4 encoder oracle exactly. */
    e = opus_encoder_ctl(enc, OPUS_SET_FORCE_MODE(MODE_CELT_ONLY));
    if (e == OPUS_OK) e = opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
    if (e == OPUS_OK) e = opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(complexity));
    if (e != OPUS_OK) {
        if (err) *err = e;
        opus_encoder_destroy(enc);
        return NULL;
    }
    if (err) *err = OPUS_OK;
    return enc;
}

int oracle_encode(OpusEncoder *enc, const opus_int16 *pcm, int frame_size,
                  unsigned char *data, int max_data_bytes)
{
    return opus_encode(enc, pcm, frame_size, data, max_data_bytes);
}

opus_uint32 oracle_encoder_final_range(OpusEncoder *enc)
{
    opus_uint32 range = 0;
    opus_encoder_ctl(enc, OPUS_GET_FINAL_RANGE(&range));
    return range;
}

void oracle_encoder_destroy(OpusEncoder *enc)
{
    opus_encoder_destroy(enc);
}

OpusDecoder *oracle_decoder_create(opus_int32 fs, int channels, int *err)
{
    int e = OPUS_OK;
    OpusDecoder *dec = opus_decoder_create(fs, channels, &e);
    if (dec == NULL || e != OPUS_OK) {
        if (err) *err = e;
        if (dec) opus_decoder_destroy(dec);
        return NULL;
    }
    if (err) *err = OPUS_OK;
    return dec;
}

int oracle_decode(OpusDecoder *dec, const unsigned char *data, int len,
                  opus_int16 *pcm, int frame_size, int decode_fec)
{
    return opus_decode(dec, data, len, pcm, frame_size, decode_fec);
}

opus_uint32 oracle_decoder_final_range(OpusDecoder *dec)
{
    opus_uint32 range = 0;
    opus_decoder_ctl(dec, OPUS_GET_FINAL_RANGE(&range));
    return range;
}

void oracle_decoder_destroy(OpusDecoder *dec)
{
    opus_decoder_destroy(dec);
}

uint64_t oracle_encoder_state_hash(OpusEncoder *enc, int channels)
{
    /* FNV-1a over the full OpusEncoder allocation. See the doc comment on this
     * function in shim.h for scope and caveats. */
    const uint64_t FNV_OFFSET = 1469598103934665603ULL;
    const uint64_t FNV_PRIME = 1099511628211ULL;
    int size = opus_encoder_get_size(channels);
    const unsigned char *p = (const unsigned char *)enc;
    uint64_t h = FNV_OFFSET;
    int i;
    if (enc == NULL || size <= 0) return 0;
    for (i = 0; i < size; i++) {
        h ^= (uint64_t)p[i];
        h *= FNV_PRIME;
    }
    return h;
}
