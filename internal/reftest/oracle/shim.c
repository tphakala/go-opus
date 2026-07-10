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
#include "entcode.h"      /* ec_ctx, ec_tell, ec_tell_frac, ec_range_bytes */
#include "entenc.h"       /* ec_enc_* */
#include "entdec.h"       /* ec_dec_* */
#include <stdlib.h>
#include <string.h>

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

/* ------------------------------ range coder -------------------------------- */

struct oracle_ec {
    ec_ctx ec;           /* the real libopus encoder/decoder state */
    unsigned char *buf;  /* handle-owned storage; ec.buf points here */
    int size;
};

oracle_ec *oracle_ec_enc_create(int size)
{
    oracle_ec *h = (oracle_ec *)calloc(1, sizeof(*h));
    if (h == NULL) return NULL;
    h->size = size;
    h->buf = size > 0 ? (unsigned char *)calloc((size_t)size, 1) : NULL;
    ec_enc_init(&h->ec, h->buf, (opus_uint32)size);
    return h;
}

void oracle_ec_enc_encode(oracle_ec *h, unsigned fl, unsigned fh, unsigned ft)
{ ec_encode(&h->ec, fl, fh, ft); }

void oracle_ec_enc_encode_bin(oracle_ec *h, unsigned fl, unsigned fh, unsigned bits)
{ ec_encode_bin(&h->ec, fl, fh, bits); }

void oracle_ec_enc_bit_logp(oracle_ec *h, int val, unsigned logp)
{ ec_enc_bit_logp(&h->ec, val, logp); }

void oracle_ec_enc_icdf(oracle_ec *h, int s, const unsigned char *icdf, unsigned ftb)
{ ec_enc_icdf(&h->ec, s, icdf, ftb); }

void oracle_ec_enc_uint(oracle_ec *h, opus_uint32 fl, opus_uint32 ft)
{ ec_enc_uint(&h->ec, fl, ft); }

void oracle_ec_enc_bits(oracle_ec *h, opus_uint32 fl, unsigned bits)
{ ec_enc_bits(&h->ec, fl, bits); }

void oracle_ec_enc_patch_initial_bits(oracle_ec *h, unsigned val, unsigned nbits)
{ ec_enc_patch_initial_bits(&h->ec, val, nbits); }

void oracle_ec_enc_shrink(oracle_ec *h, opus_uint32 size)
{ ec_enc_shrink(&h->ec, size); }

void oracle_ec_enc_done(oracle_ec *h)
{ ec_enc_done(&h->ec); }

opus_uint32 oracle_ec_enc_range_bytes(oracle_ec *h)
{ return ec_range_bytes(&h->ec); }

oracle_ec *oracle_ec_dec_create(const unsigned char *buf, int size)
{
    oracle_ec *h = (oracle_ec *)calloc(1, sizeof(*h));
    if (h == NULL) return NULL;
    h->size = size;
    h->buf = size > 0 ? (unsigned char *)calloc((size_t)size, 1) : NULL;
    if (h->buf != NULL && buf != NULL) memcpy(h->buf, buf, (size_t)size);
    ec_dec_init(&h->ec, h->buf, (opus_uint32)size);
    return h;
}

unsigned oracle_ec_dec_decode(oracle_ec *h, unsigned ft)
{ return ec_decode(&h->ec, ft); }

unsigned oracle_ec_dec_decode_bin(oracle_ec *h, unsigned bits)
{ return ec_decode_bin(&h->ec, bits); }

void oracle_ec_dec_update(oracle_ec *h, unsigned fl, unsigned fh, unsigned ft)
{ ec_dec_update(&h->ec, fl, fh, ft); }

int oracle_ec_dec_bit_logp(oracle_ec *h, unsigned logp)
{ return ec_dec_bit_logp(&h->ec, logp); }

int oracle_ec_dec_icdf(oracle_ec *h, const unsigned char *icdf, unsigned ftb)
{ return ec_dec_icdf(&h->ec, icdf, ftb); }

opus_uint32 oracle_ec_dec_uint(oracle_ec *h, opus_uint32 ft)
{ return ec_dec_uint(&h->ec, ft); }

opus_uint32 oracle_ec_dec_bits(oracle_ec *h, unsigned bits)
{ return ec_dec_bits(&h->ec, bits); }

int oracle_ec_tell(oracle_ec *h)
{ return ec_tell(&h->ec); }

opus_uint32 oracle_ec_tell_frac(oracle_ec *h)
{ return ec_tell_frac(&h->ec); }

opus_uint32 oracle_ec_get_rng(oracle_ec *h)
{ return h->ec.rng; }

opus_uint32 oracle_ec_get_val(oracle_ec *h)
{ return h->ec.val; }

int oracle_ec_get_error(oracle_ec *h)
{ return h->ec.error; }

int oracle_ec_copy_out(oracle_ec *h, unsigned char *dst, int n)
{
    int m = n;
    if ((opus_uint32)m > h->ec.storage) m = (int)h->ec.storage;
    if (m > 0 && h->buf != NULL) memcpy(dst, h->buf, (size_t)m);
    return m;
}

void oracle_ec_write_in(oracle_ec *h, const unsigned char *src, int n)
{
    if (n > 0 && h->buf != NULL) memcpy(h->buf, src, (size_t)n);
}

void oracle_ec_copy_region(oracle_ec *h, unsigned char *dst, int start, int n)
{
    if (n > 0 && h->buf != NULL) memcpy(dst, h->buf + start, (size_t)n);
}

void oracle_ec_write_region(oracle_ec *h, const unsigned char *src, int start, int n)
{
    if (n > 0 && h->buf != NULL) memcpy(h->buf + start, src, (size_t)n);
}

void oracle_ec_get_state(oracle_ec *h, oracle_ec_state *s)
{
    s->storage = h->ec.storage;
    s->end_offs = h->ec.end_offs;
    s->end_window = h->ec.end_window;
    s->nend_bits = h->ec.nend_bits;
    s->nbits_total = h->ec.nbits_total;
    s->offs = h->ec.offs;
    s->rng = h->ec.rng;
    s->val = h->ec.val;
    s->ext = h->ec.ext;
    s->rem = h->ec.rem;
    s->error = h->ec.error;
}

void oracle_ec_set_state(oracle_ec *h, const oracle_ec_state *s)
{
    /* buf is intentionally left untouched: it is the same storage the handle
     * owns, exactly as C's `*enc = saved` keeps the buf pointer (both point into
     * the same buffer for the whole RDO dance). */
    h->ec.storage = s->storage;
    h->ec.end_offs = s->end_offs;
    h->ec.end_window = s->end_window;
    h->ec.nend_bits = s->nend_bits;
    h->ec.nbits_total = s->nbits_total;
    h->ec.offs = s->offs;
    h->ec.rng = s->rng;
    h->ec.val = s->val;
    h->ec.ext = s->ext;
    h->ec.rem = s->rem;
    h->ec.error = s->error;
}

void oracle_ec_destroy(oracle_ec *h)
{
    if (h == NULL) return;
    free(h->buf);
    free(h);
}
