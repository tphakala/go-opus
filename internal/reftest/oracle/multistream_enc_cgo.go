//go:build refc

package oracle

/*
#include "multistream_shim.h"

// oracle_ms_rate_allocation is defined in wm_src_opus_multistream_encoder.c, which
// includes opus_multistream_encoder.c so the static rate_allocation is in scope
// there. Declare it here so cgo links this call against that translation unit.
extern int oracle_ms_rate_allocation(int Fs, int channels, int streams, int coupled,
    const unsigned char *mapping, int application, opus_int32 bitrate_bps,
    int frame_size, opus_int32 *rate_out);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// This file exposes the pinned libopus PLAIN (family 0/255, MAPPING_TYPE_NONE)
// multistream encoder and its static rate_allocation over pure Go-typed values, so
// multistream_enc_test.go can drive the pure-Go opusenc.OpusMSEncoder against the C
// oracle in lockstep: byte-identical packets and equal per-packet final range, plus
// the rate_allocation array in isolation. The libopus multistream encoder is
// compiled by wm_src_opus_multistream_encoder.c; this file only pulls in
// multistream_shim.h. Build flags and include paths come from oracle_cgo.go.

// CPlainMSEncoder is a stateful C multistream encoder over the plain
// opus_multistream_encoder_create constructor, with an explicit (streams, coupled,
// mapping) layout, exactly the Go NewMSEncoder arguments. Destroy it when done.
type CPlainMSEncoder struct {
	st       unsafe.Pointer
	channels int
}

// NewCPlainMSEncoder creates a libopus plain multistream encoder at Fs/channels
// with the given (streams, coupled, mapping) layout and application.
func NewCPlainMSEncoder(Fs, channels, streams, coupled int, mapping []byte, application int) (*CPlainMSEncoder, error) {
	if channels < 1 || streams < 1 {
		return nil, fmt.Errorf("channels and streams must be positive, got %d and %d", channels, streams)
	}
	if len(mapping) < channels {
		return nil, fmt.Errorf("mapping has %d entries, need %d", len(mapping), channels)
	}
	var cerr C.int
	st := C.oracle_ms_plain_enc_create(C.int(Fs), C.int(channels), C.int(streams),
		C.int(coupled), (*C.uchar)(unsafe.Pointer(&mapping[0])), C.int(application), &cerr)
	if st == nil || cerr != 0 {
		return nil, fmt.Errorf("oracle_ms_plain_enc_create: error %d", int(cerr))
	}
	return &CPlainMSEncoder{st: st, channels: channels}, nil
}

// SetBitrate applies OPUS_SET_BITRATE (the multistream-level clamp).
func (e *CPlainMSEncoder) SetBitrate(v int32) {
	C.oracle_ms_plain_enc_set_bitrate(e.st, C.int32_t(v))
}

// SetVBR applies OPUS_SET_VBR (0 for CBR).
func (e *CPlainMSEncoder) SetVBR(v int) { C.oracle_ms_plain_enc_set_vbr(e.st, C.int(v)) }

// SetVBRConstraint applies OPUS_SET_VBR_CONSTRAINT.
func (e *CPlainMSEncoder) SetVBRConstraint(v int) {
	C.oracle_ms_plain_enc_set_vbr_constraint(e.st, C.int(v))
}

// SetComplexity applies OPUS_SET_COMPLEXITY.
func (e *CPlainMSEncoder) SetComplexity(v int) {
	C.oracle_ms_plain_enc_set_complexity(e.st, C.int(v))
}

// SetDTX applies OPUS_SET_DTX.
func (e *CPlainMSEncoder) SetDTX(v int) { C.oracle_ms_plain_enc_set_dtx(e.st, C.int(v)) }

// SetForceMode applies OPUS_SET_FORCE_MODE (the frozen build forces MODE_CELT_ONLY).
func (e *CPlainMSEncoder) SetForceMode(v int) { C.oracle_ms_plain_enc_set_force_mode(e.st, C.int(v)) }

// Encode encodes one frame of interleaved int16 PCM (frameSize samples per
// channel) into a fresh maxBytes buffer and returns the packet bytes.
func (e *CPlainMSEncoder) Encode(pcm []int16, frameSize, maxBytes int) ([]byte, error) {
	if e.st == nil {
		return nil, fmt.Errorf("CPlainMSEncoder: encoder is destroyed")
	}
	if frameSize <= 0 || len(pcm) < frameSize*e.channels {
		return nil, fmt.Errorf("CPlainMSEncoder: pcm holds %d samples, need %d (frameSize=%d, channels=%d)",
			len(pcm), frameSize*e.channels, frameSize, e.channels)
	}
	if maxBytes < 1 { // a zero/negative budget would panic at make or &buf[0]
		return nil, fmt.Errorf("CPlainMSEncoder: maxBytes must be positive, got %d", maxBytes)
	}
	buf := make([]byte, maxBytes)
	n := C.oracle_ms_plain_enc_encode(e.st, (*C.int16_t)(unsafe.Pointer(&pcm[0])),
		C.int(frameSize), (*C.uchar)(unsafe.Pointer(&buf[0])), C.int32_t(maxBytes))
	if int(n) < 0 {
		return nil, fmt.Errorf("opus_multistream_encode: error %d", int(n))
	}
	return buf[:int(n)], nil
}

// FinalRange returns the encoder's OPUS_GET_FINAL_RANGE (XOR across streams).
func (e *CPlainMSEncoder) FinalRange() uint32 {
	return uint32(C.oracle_ms_plain_enc_final_range(e.st))
}

// Reset applies OPUS_RESET_STATE.
func (e *CPlainMSEncoder) Reset() { C.oracle_ms_plain_enc_reset(e.st) }

// Destroy frees the C encoder.
func (e *CPlainMSEncoder) Destroy() {
	if e.st != nil {
		C.oracle_ms_plain_enc_destroy(e.st)
		e.st = nil
	}
}

// RateAllocationC runs the C static rate_allocation for a plain family-0/255 MS
// encoder at the given layout and bitrate, returning the per-stream bitrate array
// (length streams) and the rate sum. It pins the Go rateAllocation in isolation.
func RateAllocationC(Fs, channels, streams, coupled int, mapping []byte, application int, bitrateBps int32, frameSize int) ([]int32, int32, error) {
	if channels < 1 || streams < 1 {
		return nil, 0, fmt.Errorf("channels and streams must be positive, got %d and %d", channels, streams)
	}
	if len(mapping) < channels {
		return nil, 0, fmt.Errorf("mapping has %d entries, need %d", len(mapping), channels)
	}
	rateOut := make([]int32, streams)
	sum := C.oracle_ms_rate_allocation(C.int(Fs), C.int(channels), C.int(streams),
		C.int(coupled), (*C.uchar)(unsafe.Pointer(&mapping[0])), C.int(application),
		C.opus_int32(bitrateBps), C.int(frameSize),
		(*C.opus_int32)(unsafe.Pointer(&rateOut[0])))
	if int(sum) < 0 {
		return nil, 0, fmt.Errorf("oracle_ms_rate_allocation: error %d", int(sum))
	}
	return rateOut, int32(sum), nil
}
