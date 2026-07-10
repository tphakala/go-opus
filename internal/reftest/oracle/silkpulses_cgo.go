//go:build refc

package oracle

/*
#include "silkpulses_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK excitation coder (silk/encode_pulses.c,
// silk/decode_pulses.c, silk/shell_coder.c, silk/code_signs.c) over plain Go-typed
// functions so silkpulses_test.go can drive the pure-Go internal/silk decode
// against the C oracle without importing "C" itself. The package-level cgo CFLAGS
// live in oracle_cgo.go; this file only pulls in the silkpulses_shim.h wrappers.

// silkPulsesBufBytes is the fixed coder buffer size used on both the encode and
// decode side. One SILK excitation frame is a fraction of an Opus packet (<= 1275
// bytes); 2048 is ample even for a full 20 ms wideband frame at near-maximum
// energy with several LSB shifts. Encode and decode share the exact same byte
// slice, so any trailing zero padding is identical on both sides.
const silkPulsesBufBytes = 2048

// silkPulsesMaxSamples is MAX_NB_SHELL_BLOCKS * SHELL_CODEC_FRAME_LENGTH, the most
// samples silk_decode_pulses can write (a 20 ms wideband frame, 320 samples).
const silkPulsesMaxSamples = 20 * 16

// cSilkEncodePulses range-encodes a signed pulse vector (length frameLength, int8
// domain) via the C silk_encode_pulses, returning the fixed-size packet and its
// encoded byte length.
func cSilkEncodePulses(pulses []int8, frameLength, signalType, quantOffsetType int) (buf []byte, nbytes int) {
	buf = make([]byte, silkPulsesBufBytes)
	var pin *C.int8_t
	if len(pulses) > 0 {
		pin = (*C.int8_t)(unsafe.Pointer(&pulses[0]))
	}
	n := C.oracle_silk_encode_pulses(pin, C.int(frameLength),
		C.int(signalType), C.int(quantOffsetType),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(silkPulsesBufBytes))
	return buf, int(n)
}

// cSilkDecodePulses decodes buf with the C silk_decode_pulses into a pulse vector
// (iter * SHELL_CODEC_FRAME_LENGTH samples long), also returning the decoder's
// tell/rng end state.
func cSilkDecodePulses(buf []byte, frameLength, signalType, quantOffsetType int) (pulses []int16, tell int, rng uint32) {
	out := make([]int16, silkPulsesMaxSamples)
	var cTell C.int
	var cRng C.uint32_t
	n := C.oracle_silk_decode_pulses(
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		C.int(frameLength), C.int(signalType), C.int(quantOffsetType),
		(*C.int16_t)(unsafe.Pointer(&out[0])), &cTell, &cRng)
	return out[:int(n)], int(cTell), uint32(cRng)
}
