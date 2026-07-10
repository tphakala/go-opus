//go:build refc

package oracle

/*
#include "opusdec_shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// This file exposes the pinned libopus top-level Opus encoder (src/opus_encoder.c
// opus_encode) with per-frame FORCE_MODE / bandwidth control over plain Go-typed
// values, so opusdec_test.go can synthesize packet sequences that force
// SILK<->hybrid<->CELT transitions (and the redundancy frames) and carry FEC/LBRR,
// then decode them through both the C opus_decode (the shared oracle Decoder) and
// the pure-Go opus.Decoder in lockstep. The package-level cgo CFLAGS live in
// oracle_cgo.go; this file only pulls in opusdec_shim.h.

// C mode constants (src/opus_private.h) for the encoder force-mode script.
const (
	oModeSilkOnly = 1000 // MODE_SILK_ONLY
	oModeHybrid   = 1001 // MODE_HYBRID
	oModeCeltOnly = 1002 // MODE_CELT_ONLY
)

// C bandwidth constants (include/opus_defines.h) for the force-bandwidth script.
const (
	oBandwidthNarrowband   = 1101 // OPUS_BANDWIDTH_NARROWBAND
	oBandwidthWideband     = 1103 // OPUS_BANDWIDTH_WIDEBAND
	oBandwidthSuperwideband = 1104 // OPUS_BANDWIDTH_SUPERWIDEBAND
	oBandwidthFullband     = 1105 // OPUS_BANDWIDTH_FULLBAND
)

// OpusEncoder is a persistent top-level libopus encoder for one sequence, with
// per-frame force-mode control.
type OpusEncoder struct {
	c        *C.oracle_opusenc_ctx
	channels int
}

// NewOpusEncoder builds an OPUS_APPLICATION_AUDIO encoder at Fs / channels with
// VBR off (forced bitrate honored frame to frame). useFEC enables in-band FEC /
// LBRR (SILK/hybrid frames carry redundancy). Close it when done.
func NewOpusEncoder(Fs, channels, bitrate, complexity int, useFEC bool) (*OpusEncoder, error) {
	fec := C.int(0)
	if useFEC {
		fec = 1
	}
	c := C.oracle_opusenc_create(C.int(Fs), C.int(channels), C.int(bitrate), C.int(complexity), fec)
	if c == nil {
		return nil, fmt.Errorf("oracle_opusenc_create(Fs=%d,ch=%d) failed", Fs, channels)
	}
	return &OpusEncoder{c: c, channels: channels}, nil
}

// Encode encodes one frame (frameSize samples per channel) of interleaved int16
// PCM, forcing forceMode (a MODE_* value, or 0 for OPUS_AUTO) and forceBandwidth
// (an OPUS_BANDWIDTH_* value, or 0 for OPUS_AUTO) for this frame. It returns the
// packet bytes, or nil for a DTX/empty packet.
func (e *OpusEncoder) Encode(pcm []int16, frameSize, forceMode, forceBandwidth int) ([]byte, error) {
	if len(pcm) < frameSize*e.channels {
		return nil, fmt.Errorf("pcm has %d samples, need %d", len(pcm), frameSize*e.channels)
	}
	const maxDataBytes = 4000
	buf := make([]byte, maxDataBytes)
	n := C.oracle_opusenc_encode(e.c,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(frameSize),
		C.int(forceMode), C.int(forceBandwidth),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(maxDataBytes))
	if n < 0 {
		return nil, fmt.Errorf("opus_encode: error %d", int(n))
	}
	if n == 0 {
		return nil, nil
	}
	return buf[:int(n)], nil
}

// FinalRange returns the encoder's range coder state after the last frame
// (OPUS_GET_FINAL_RANGE).
func (e *OpusEncoder) FinalRange() uint32 {
	return uint32(C.oracle_opusenc_final_range(e.c))
}

// Close frees the encoder. Safe to call once; the pointer is nilled.
func (e *OpusEncoder) Close() {
	if e.c != nil {
		C.oracle_opusenc_destroy(e.c)
		e.c = nil
	}
}
