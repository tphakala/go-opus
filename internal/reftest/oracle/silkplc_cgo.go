//go:build refc

package oracle

/*
#include "silkplc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK encoder + per-frame decode with the
// packet-loss branch (silk/decode_frame.c silk_decode_frame -> silk_PLC / silk_CNG /
// silk_PLC_glue_frames) over plain Go-typed values so silkplc_test.go can drive the
// pure-Go internal/silk DecodeFrame loss path against the C oracle without importing
// "C" itself. The persistent encoder + decoder live in a C context that carries
// cross-frame/cross-packet state (including sPLC/sCNG) over the whole sequence. The
// package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// silkplc_shim.h.

// silkplcBufBytes is the per-packet range-coder buffer size (must equal the shim's
// SILKPLC_BUF_BYTES).
const silkplcBufBytes = C.SILKPLC_BUF_BYTES

// silkplcMaxFrames is the largest number of SILK frames in one packet (60 ms => 3).
const silkplcMaxFrames = C.SILKPLC_MAX_FRAMES

// silkplcFrame is the C decode result for one SILK frame (concealed or received).
type silkplcFrame struct {
	out                  []int16
	signalType           int
	quantOffsetType      int
	stateHash            uint64
	rng                  uint32
	tell                 int
	prevGainQ16          int32
	lagPrev              int
	lastGainIndex        int
	prevSignalType       int
	firstFrameAfterReset int
	lossCnt              int
	plcLastFrameLost     int
	plcRandSeed          int32
	cngRandSeed          int32
	lost                 bool
}

// silkplcCtx wraps the persistent C encoder + decoder for one sequence.
type silkplcCtx struct {
	c                *C.oracle_silkplc_ctx
	fsKHz            int
	payloadMs        int
	nFramesPerPacket int
	nbSubfr          int
}

// newSilkplcCtx builds a sequence context: a SILK encoder forced to internal rate
// fsKHz (8/12/16) producing payloadMs (10/20/40/60) packets at the given bitrate and
// complexity, plus a matching decoder. Returns nil on allocation failure.
func newSilkplcCtx(fsKHz, payloadMs, bitrate, complexity, useDTX int) *silkplcCtx {
	c := C.oracle_silkplc_create(C.int(fsKHz), C.int(payloadMs), C.int(bitrate),
		C.int(complexity), C.int(useDTX))
	if c == nil {
		return nil
	}
	return &silkplcCtx{
		c:                c,
		fsKHz:            fsKHz,
		payloadMs:        payloadMs,
		nFramesPerPacket: int(c.nFramesPerPacket),
		nbSubfr:          int(c.nb_subfr),
	}
}

// frameLength returns the internal-rate SILK frame length (samples) per frame.
func (s *silkplcCtx) frameLength() int { return s.nbSubfr * 5 * s.fsKHz }

// internalRate returns the encoder's chosen internal sampling rate (Hz).
func (s *silkplcCtx) internalRate() int { return int(s.c.encControl.internalSampleRate) }

// close frees the C context. Safe to call once.
func (s *silkplcCtx) close() {
	if s.c != nil {
		C.oracle_silkplc_destroy(s.c)
		s.c = nil
	}
}

// encode encodes one packet of PCM with the C silk_Encode. Returns the packet bytes,
// or nil for a DTX/empty packet. Panics on an encoder error.
func (s *silkplcCtx) encode(pcm []int16) []byte {
	buf := make([]byte, silkplcBufBytes)
	var pcmPtr *C.int16_t
	if len(pcm) > 0 {
		pcmPtr = (*C.int16_t)(unsafe.Pointer(&pcm[0]))
	}
	n := int(C.oracle_silkplc_encode(s.c, pcmPtr, C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(silkplcBufBytes)))
	if n < 0 {
		panic("silk_Encode failed")
	}
	if n == 0 {
		return nil
	}
	return buf
}

// decodePacket runs the C silk_decode_frame driver over one packet and returns one
// silkplcFrame per SILK frame. When lost is true the packet is concealed
// (FLAG_PACKET_LOST) and buf is ignored; otherwise buf must be the exact
// silkplcBufBytes-sized buffer returned by encode.
func (s *silkplcCtx) decodePacket(buf []byte, lost bool) []silkplcFrame {
	var couts [silkplcMaxFrames]C.oracle_silkplc_frame_out
	var bufPtr *C.uchar
	var bufLen C.int
	if buf != nil {
		bufPtr = (*C.uchar)(unsafe.Pointer(&buf[0]))
		bufLen = C.int(len(buf))
	} else {
		// Lost packet: provide a valid but unused pointer.
		var dummy [1]byte
		bufPtr = (*C.uchar)(unsafe.Pointer(&dummy[0]))
		bufLen = 0
	}
	lostC := C.int(0)
	if lost {
		lostC = 1
	}
	nf := int(C.oracle_silkplc_decode_packet(s.c, bufPtr, bufLen, lostC, &couts[0]))

	frames := make([]silkplcFrame, nf)
	for i := 0; i < nf; i++ {
		o := &couts[i]
		fl := int(o.frameLength)
		out := make([]int16, fl)
		for j := 0; j < fl; j++ {
			out[j] = int16(o.out[j])
		}
		frames[i] = silkplcFrame{
			out:                  out,
			signalType:           int(o.signalType),
			quantOffsetType:      int(o.quantOffsetType),
			stateHash:            uint64(o.stateHash),
			rng:                  uint32(o.rng),
			tell:                 int(o.tell),
			prevGainQ16:          int32(o.prevGainQ16),
			lagPrev:              int(o.lagPrev),
			lastGainIndex:        int(o.lastGainIndex),
			prevSignalType:       int(o.prevSignalType),
			firstFrameAfterReset: int(o.firstFrameAfterReset),
			lossCnt:              int(o.lossCnt),
			plcLastFrameLost:     int(o.plcLastFrameLost),
			plcRandSeed:          int32(o.plcRandSeed),
			cngRandSeed:          int32(o.cngRandSeed),
			lost:                 lost,
		}
	}
	return frames
}
