//go:build refc

// Compiles src/opus_multistream_encoder.c as its own translation unit for the FIXED_POINT +
// DISABLE_FLOAT_API libopus oracle. Named wm_* (not w_*, the gen_wrappers.sh set)
// so it survives that script's "rm -f w_*.c" and stays with the multistream
// differential test that needs it; gen_wrappers deliberately omits multistream
// from the portable oracle source set.
#include "libopus/src/opus_multistream_encoder.c"

/* oracle_ms_rate_allocation drives the static rate_allocation (in scope via the
 * #include above) for the plain family-0/255 MS encoder, so the Go port's
 * rateAllocation can be pinned bit-for-bit before any end-to-end packet test. It
 * creates a MAPPING_TYPE_NONE encoder for the layout, applies bitrate_bps through
 * the multistream SetBitrate clamp, writes rate_out[0..streams), and returns the
 * rate sum (always non-negative) or a negative Opus error code. Non-static so the
 * cgo file multistream_enc_cgo.go can declare it extern and link against it. */
int oracle_ms_rate_allocation(int Fs, int channels, int streams, int coupled,
    const unsigned char *mapping, int application, opus_int32 bitrate_bps,
    int frame_size, opus_int32 *rate_out)
{
    int err = OPUS_OK;
    OpusMSEncoder *st = opus_multistream_encoder_create((opus_int32)Fs, channels,
        streams, coupled, mapping, application, &err);
    if (st == NULL || err != OPUS_OK)
        return err != OPUS_OK ? err : OPUS_INTERNAL_ERROR;
    opus_multistream_encoder_ctl(st, OPUS_SET_BITRATE(bitrate_bps));
    opus_int32 sum = rate_allocation(st, rate_out, frame_size);
    opus_multistream_encoder_destroy(st);
    return sum;
}
