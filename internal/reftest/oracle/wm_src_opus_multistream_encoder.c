//go:build refc

// Compiles src/opus_multistream_encoder.c as its own translation unit for the FIXED_POINT +
// DISABLE_FLOAT_API libopus oracle. Named wm_* (not w_*, the gen_wrappers.sh set)
// so it survives that script's "rm -f w_*.c" and stays with the multistream
// differential test that needs it; gen_wrappers deliberately omits multistream
// from the portable oracle source set.
#include "libopus/src/opus_multistream_encoder.c"
