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

#endif /* GOOPUS_ORACLE_SHIM_H */
