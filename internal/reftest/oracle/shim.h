//go:build refc

/*
 * shim.h - thin C-callable surface over the pinned libopus oracle.
 *
 * Everything the Go bindings (oracle_cgo.go) touch goes through these plain
 * functions so the OPUS_SET_FORCE_MODE / CTL varargs macros stay on the C side
 * where they are declared (src/opus_private.h), and cgo only ever sees ordinary
 * function calls and POD types.
 *
 * The oracle is built FIXED_POINT + DISABLE_FLOAT_API with OPUS_FAST_INT64 (see
 * README.md); shim.c enforces that at compile time.
 */
#ifndef GOOPUS_ORACLE_SHIM_H
#define GOOPUS_ORACLE_SHIM_H

#include <stdint.h>
#include "opus.h"

/* Build-config snapshot surfaced to Go so a test can assert (and print) the
 * exact oracle configuration. See oracle_get_build_config in shim.c. */
typedef struct {
    int fixed_point;       /* FIXED_POINT defined            (must be 1) */
    int disable_float_api; /* DISABLE_FLOAT_API defined       (must be 1) */
    int fast_int64;        /* celt/arch.h OPUS_FAST_INT64      (must be 1) */
    int custom_modes;      /* CUSTOM_MODES defined            (must be 0) */
    int enable_qext;       /* ENABLE_QEXT defined             (must be 0) */
    int arch_int64;        /* sizeof(opus_int64) == 8         (must be 1) */
} oracle_build_config;

void oracle_get_build_config(oracle_build_config *cfg);
const char *oracle_version_string(void);

/* Encoder: CELT-only forced mode, OPUS_APPLICATION_AUDIO. On failure returns
 * NULL and writes the opus error code to *err. */
OpusEncoder *oracle_encoder_create_celt(opus_int32 fs, int channels,
                                        opus_int32 bitrate, int complexity,
                                        int *err);
int oracle_encode(OpusEncoder *enc, const opus_int16 *pcm, int frame_size,
                  unsigned char *data, int max_data_bytes);
opus_uint32 oracle_encoder_final_range(OpusEncoder *enc);
void oracle_encoder_destroy(OpusEncoder *enc);

/* Decoder. */
OpusDecoder *oracle_decoder_create(opus_int32 fs, int channels, int *err);
int oracle_decode(OpusDecoder *dec, const unsigned char *data, int len,
                  opus_int16 *pcm, int frame_size, int decode_fec);
opus_uint32 oracle_decoder_final_range(OpusDecoder *dec);
void oracle_decoder_destroy(OpusDecoder *dec);

/*
 * ----------------------------------------------------------------------------
 * The "invisible to the packet gate" surface (CP10 audit).
 * ----------------------------------------------------------------------------
 *
 * Everything below returns a value that NEVER REACHES A PACKET BYTE, so the
 * bit-exact encoder/decoder gates are structurally blind to an error in it. That
 * blind spot has already shipped one real bug in this repo (the encoder lookahead
 * was 192 instead of 312, which would have misaligned every Ogg stream by 120
 * samples), so each of these quantities gets its own differential assertion
 * against the C.
 */

/* OPUS_GET_LAST_PACKET_DURATION (src/opus_decoder.c:1167): st->last_packet_duration,
 * the per-channel sample count the last opus_decode produced. It is written on five
 * distinct paths in opus_decode_native (:776 the multi-frame loop, :806/:812 the
 * FEC save/restore, :829 the PLC/DTX path, :869 the normal path), none of which the
 * PCM comparison can distinguish from the others. */
int oracle_decoder_last_packet_duration(OpusDecoder *dec);

/* OPUS_RESET_STATE on a decoder (src/opus_decoder.c). Returns an OPUS_* code. The
 * encoder's reset lives on the CP9 handle (oracle_topenc_reset). */
int oracle_decoder_reset(OpusDecoder *dec);

/* opus_packet_get_nb_frames (src/opus_decoder.c:1273) and
 * opus_packet_get_nb_samples (:1289), the two pure packet-inspection functions the
 * public opus.PacketFrames / opus.PacketDuration mirror. Both return a NEGATIVE
 * OPUS_* error code (OPUS_BAD_ARG for len < 1, OPUS_INVALID_PACKET for a truncated
 * code-3 packet or a duration over 120 ms) rather than a count, so the Go side's
 * error mapping is part of what is being compared. data may be NULL when len is 0. */
int oracle_packet_get_nb_frames(const unsigned char *data, int len);
int oracle_packet_get_nb_samples(const unsigned char *data, int len, opus_int32 fs);

/*
 * Per-frame persistent-state hash tap (hard-parts.md section 5 and 7).
 *
 * STATUS: phase-0 stub. Hashes the whole OpusEncoder allocation (which embeds
 * all the cross-frame CELT state: vbr_reservoir/drift/offset, oldBandE/oldLogE/
 * oldLogE2, energyError, prefilter memory, rng, consec_transient, delayedIntra,
 * etc.) with FNV-1a. `channels` is needed because the allocation size comes from
 * opus_encoder_get_size(channels).
 *
 * This is a real, working within-run determinism probe: encode a multi-frame
 * sequence and the hash changes exactly when persistent state changes, so a
 * sequence test catches divergence on the frame it appears. Two caveats,
 * documented for the phase-4 refinement:
 *   1. The allocation includes the embedded `const CELTMode *mode` pointer, so
 *      absolute hash values are NOT stable across process runs (ASLR). Compare
 *      frame-to-frame within one run, or Go-vs-C within a design that hashes the
 *      same named fields (see below), not against a golden literal.
 *   2. Cross-language comparison against the Go port needs identical field-level
 *      hashing, not a raw struct dump (layouts differ). The phase-4 version will
 *      #include "celt/celt_encoder.c" into this shim (excluded from the wrapper
 *      set to avoid a duplicate TU) and hash the named fields from hard-parts.md
 *      section 5 in a fixed order; the Go encoder taps the same fields.
 */
uint64_t oracle_encoder_state_hash(OpusEncoder *enc, int channels);

/*
 * ----------------------------------------------------------------------------
 * Range coder (celt/entenc.c, entdec.c, entcode.c) direct differential surface.
 * ----------------------------------------------------------------------------
 *
 * These wrap the real libopus ec_enc/ec_dec (already compiled into the oracle
 * via w_celt_entenc.c / w_celt_entdec.c / w_celt_entcode.c) one primitive at a
 * time, so the Go test can drive the C coder and the pure-Go coder in lockstep
 * and compare bytes + tell() + tell_frac() at every step. The handle owns the
 * ec_ctx and its output buffer; the ec_ctx.buf pointer is stable for the life of
 * the handle, so oracle_ec_get_state / oracle_ec_set_state (which save/restore
 * every mutable field EXCEPT buf) reproduce C's `saved = *enc` / `*enc = saved`
 * snapshot-restore, and oracle_ec_copy_out / oracle_ec_write_in reproduce the
 * ec_get_buffer()+ec_range_bytes() byte splice (docs/hard-parts.md 1).
 *
 * The prototypes deliberately use only plain C / opus_types types (never the
 * internal ec_ctx type) so shim.h stays a header that pulls in nothing beyond
 * opus.h; the handle is opaque here and defined in shim.c.
 */
typedef struct oracle_ec oracle_ec;

/* Mutable ec_ctx state, minus the buf pointer (which the handle owns and keeps
 * fixed). Field order and types mirror struct ec_ctx in celt/entcode.h. */
typedef struct {
    opus_uint32 storage;
    opus_uint32 end_offs;
    opus_uint32 end_window;
    int         nend_bits;
    int         nbits_total;
    opus_uint32 offs;
    opus_uint32 rng;
    opus_uint32 val;
    opus_uint32 ext;
    int         rem;
    int         error;
} oracle_ec_state;

/* Encoder. */
oracle_ec *oracle_ec_enc_create(int size);
void oracle_ec_enc_encode(oracle_ec *h, unsigned fl, unsigned fh, unsigned ft);
void oracle_ec_enc_encode_bin(oracle_ec *h, unsigned fl, unsigned fh, unsigned bits);
void oracle_ec_enc_bit_logp(oracle_ec *h, int val, unsigned logp);
void oracle_ec_enc_icdf(oracle_ec *h, int s, const unsigned char *icdf, unsigned ftb);
void oracle_ec_enc_uint(oracle_ec *h, opus_uint32 fl, opus_uint32 ft);
void oracle_ec_enc_bits(oracle_ec *h, opus_uint32 fl, unsigned bits);
void oracle_ec_enc_patch_initial_bits(oracle_ec *h, unsigned val, unsigned nbits);
void oracle_ec_enc_shrink(oracle_ec *h, opus_uint32 size);
void oracle_ec_enc_done(oracle_ec *h);
opus_uint32 oracle_ec_enc_range_bytes(oracle_ec *h);

/* Decoder. buf is copied from the caller so the handle owns its storage. */
oracle_ec *oracle_ec_dec_create(const unsigned char *buf, int size);
unsigned oracle_ec_dec_decode(oracle_ec *h, unsigned ft);
unsigned oracle_ec_dec_decode_bin(oracle_ec *h, unsigned bits);
void oracle_ec_dec_update(oracle_ec *h, unsigned fl, unsigned fh, unsigned ft);
int oracle_ec_dec_bit_logp(oracle_ec *h, unsigned logp);
int oracle_ec_dec_icdf(oracle_ec *h, const unsigned char *icdf, unsigned ftb);
opus_uint32 oracle_ec_dec_uint(oracle_ec *h, opus_uint32 ft);
opus_uint32 oracle_ec_dec_bits(oracle_ec *h, unsigned bits);

/* Shared accessors / state (work on either an encoder or a decoder handle). */
int oracle_ec_tell(oracle_ec *h);
opus_uint32 oracle_ec_tell_frac(oracle_ec *h);
opus_uint32 oracle_ec_get_rng(oracle_ec *h);
opus_uint32 oracle_ec_get_val(oracle_ec *h);
int oracle_ec_get_error(oracle_ec *h);
/* Copies min(n, storage) buffer bytes to dst; returns the number copied. */
int oracle_ec_copy_out(oracle_ec *h, unsigned char *dst, int n);
/* Copies n bytes from src over buf[0..n) (the head-window splice). */
void oracle_ec_write_in(oracle_ec *h, const unsigned char *src, int n);
/* Copies buf[start..start+n) out / over, for the arbitrary-window RDO splice
 * (coarse energy saves buf[start_offs..intra_offs); theta saves
 * buf[start_offs..storage)). */
void oracle_ec_copy_region(oracle_ec *h, unsigned char *dst, int start, int n);
void oracle_ec_write_region(oracle_ec *h, const unsigned char *src, int start, int n);
void oracle_ec_get_state(oracle_ec *h, oracle_ec_state *s);
void oracle_ec_set_state(oracle_ec *h, const oracle_ec_state *s);
void oracle_ec_destroy(oracle_ec *h);

#endif /* GOOPUS_ORACLE_SHIM_H */
