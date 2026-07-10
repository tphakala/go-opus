//go:build refc

package oracle

/*
#include "silkdecapi_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus top-level SILK decoder (silk/dec_API.c
// silk_Decode) plus the SILK encoder (silk/enc_API.c silk_Encode) over plain Go-typed
// values so silkdecapi_test.go can drive the pure-Go silk.Decoder against the C oracle
// in lockstep without importing "C" itself. The persistent encoder + decoder live in a C
// context that carries cross-frame / cross-packet state over the whole sequence. The
// package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// silkdecapi_shim.h.

// silkdecapiBufBytes is the per-packet range-coder buffer size (must equal the shim's
// SILKDECAPI_BUF_BYTES).
const silkdecapiBufBytes = C.SILKDECAPI_BUF_BYTES

// silkdecapiMaxFrames is the largest number of SILK frames one decode operation yields.
const silkdecapiMaxFrames = C.SILKDECAPI_MAX_FRAMES

// Lost-flag values (silk/control.h), re-exported for the test's decode scripts.
const (
	silkFlagDecodeNormal = 0 // FLAG_DECODE_NORMAL
	silkFlagPacketLost   = 1 // FLAG_PACKET_LOST
	silkFlagDecodeLBRR   = 2 // FLAG_DECODE_LBRR
)

// silkdecapiFrame is the C decode result for one SILK frame within a decode operation.
type silkdecapiFrame struct {
	pcm          []int16 // interleaved API-rate samples ([0 : nSamplesOut*nChannelsAPI))
	nSamplesOut  int     // per-channel API-rate sample count
	stateHash    uint64
	rng          uint32
	tell         int
	prevPitchLag int
}

// silkdecapiCtx wraps the persistent C encoder + decoder for one sequence.
type silkdecapiCtx struct {
	c                *C.oracle_silkdecapi_ctx
	fsKHz            int
	apiRate          int
	payloadMs        int
	nChannels        int
	nFramesPerPacket int
}

// newSilkdecapiCtx builds a sequence context: a SILK encoder forced to internal rate
// fsKHz (8/12/16) producing payloadMs (10/20/40/60) packets for nChannels (1/2) channels
// at the given bitrate and complexity, plus a matching decoder run at apiRate Hz
// (8000/12000/16000/24000/48000). useDTX enables discontinuous transmission; useFEC
// enables in-band FEC / LBRR coding. Returns nil on allocation/size-check failure.
func newSilkdecapiCtx(fsKHz, apiRate, payloadMs, nChannels, bitrate, complexity, useDTX, useFEC int) *silkdecapiCtx {
	c := C.oracle_silkdecapi_create(C.int(fsKHz), C.int(apiRate), C.int(payloadMs),
		C.int(nChannels), C.int(bitrate), C.int(complexity), C.int(useDTX), C.int(useFEC))
	if c == nil {
		return nil
	}
	nfpp := 1
	switch payloadMs {
	case 40:
		nfpp = 2
	case 60:
		nfpp = 3
	}
	return &silkdecapiCtx{
		c:                c,
		fsKHz:            fsKHz,
		apiRate:          apiRate,
		payloadMs:        payloadMs,
		nChannels:        nChannels,
		nFramesPerPacket: nfpp,
	}
}

// close frees the C context. Safe to call once.
func (s *silkdecapiCtx) close() {
	if s.c != nil {
		C.oracle_silkdecapi_destroy(s.c)
		s.c = nil
	}
}

// encode encodes one packet of interleaved PCM (payloadMs of int16 samples per channel at
// fsKHz*1000 Hz) with the C silk_Encode. The voice-activity decision is derived from the
// PCM (silent chunk => inactive), which lets DTX emit empty packets on silence. It returns
// the packet bytes, or nil for a DTX/empty packet. It panics on an encoder error.
func (s *silkdecapiCtx) encode(pcm []int16) []byte {
	buf := make([]byte, silkdecapiBufBytes)
	var pcmPtr *C.int16_t
	if len(pcm) > 0 {
		pcmPtr = (*C.int16_t)(unsafe.Pointer(&pcm[0]))
	}
	activity := 0
	for _, v := range pcm {
		if v != 0 {
			activity = 1
			break
		}
	}
	nPerChannel := len(pcm) / s.nChannels
	n := int(C.oracle_silkdecapi_encode(s.c, pcmPtr, C.int(nPerChannel),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(silkdecapiBufBytes), C.int(activity)))
	if n < 0 {
		panic("silk_Encode failed")
	}
	if n == 0 {
		return nil
	}
	return buf
}

// hasLBRR reports whether the packet buffer carries LBRR (in-band FEC) data for any
// internal channel, peeking the VAD/LBRR header non-destructively.
func (s *silkdecapiCtx) hasLBRR(buf []byte) bool {
	return C.oracle_silkdecapi_has_lbrr(s.c, (*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf))) != 0
}

// decode runs the C silk_Decode driver for one decode operation (replicating
// opus_decode_frame's per-frame loop) and returns one silkdecapiFrame per SILK frame.
// lostFlag selects normal / packet-loss / FEC decoding; buf is the packet (may be nil on
// the packet-loss path); frameSizeMs is the requested output duration. It panics on a
// silk_Decode error.
func (s *silkdecapiCtx) decode(lostFlag int, buf []byte, frameSizeMs int) []silkdecapiFrame {
	var couts [silkdecapiMaxFrames]C.oracle_silkdecapi_frame_out
	var bufPtr *C.uchar
	bufLen := 0
	if buf != nil {
		bufPtr = (*C.uchar)(unsafe.Pointer(&buf[0]))
		bufLen = len(buf)
	}
	nf := int(C.oracle_silkdecapi_decode(s.c, C.int(lostFlag), bufPtr, C.int(bufLen),
		C.int(frameSizeMs), &couts[0]))
	if nf < 0 {
		panic("silk_Decode failed")
	}

	frames := make([]silkdecapiFrame, nf)
	for i := 0; i < nf; i++ {
		o := &couts[i]
		ns := int(o.nSamplesOut)
		total := ns * s.nChannels
		pcm := make([]int16, total)
		for j := 0; j < total; j++ {
			pcm[j] = int16(o.pcm[j])
		}
		frames[i] = silkdecapiFrame{
			pcm:          pcm,
			nSamplesOut:  ns,
			stateHash:    uint64(o.stateHash),
			rng:          uint32(o.rng),
			tell:         int(o.tell),
			prevPitchLag: int(o.prevPitchLag),
		}
	}
	return frames
}
