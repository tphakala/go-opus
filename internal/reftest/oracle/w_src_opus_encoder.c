//go:build refc

// NEUTRALIZED wrapper for src/opus_encoder.c. Compiled SOLELY by
// opusenc_shim.h (opusenc_cgo.go) so the CP9 top-level encoder differential
// test can reach `struct OpusEncoder` (defined in the .c, not in any header)
// and the file statics gen_toc / dc_reject / stereo_fade /
// user_bitrate_to_bitrate / compute_equiv_rate; a second TU compiling it here
// would duplicate opus_encode / opus_encoder_create / opus_encoder_ctl /
// opus_encoder_init / frame_size_select / downmix_int and fail to link.
// shim.c and opusdec_shim.h call the public opus.h prototypes and resolve
// against opusenc_shim.h at link time.
typedef int goopus_w_src_opus_encoder_neutralized;
