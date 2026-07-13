//go:build refc

// NEUTRALIZED wrapper for celt/celt_encoder.c. Compiled SOLELY by
// celtenc_shim.h (celtenc_cgo.go) so the CP8a encoder differential test can
// reach its file-static stage functions; a second TU compiling it here would
// duplicate celt_encode_with_ec / celt_encoder_init / celt_preemphasis and fail
// to link. celtdec_shim.h and opusenc_shim.h reference celt_encode_with_ec
// extern and resolve against celtenc_shim.h at link time.
typedef int goopus_w_celt_celt_encoder_neutralized;
