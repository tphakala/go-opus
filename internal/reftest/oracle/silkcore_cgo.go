//go:build refc

package oracle

/*
#include "silkcore_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK encoder + per-frame decode
// (silk/enc_API.c silk_Encode, silk/decode_frame.c silk_decode_frame) over plain
// Go-typed values so silkcore_test.go can drive the pure-Go internal/silk
// DecodeFrame against the C oracle without importing "C" itself. The persistent
// encoder + decoder live in a C context that carries cross-frame/cross-packet state
// over the whole sequence. The package-level cgo CFLAGS live in oracle_cgo.go; this
// file only pulls in silkcore_shim.h.

// silkcoreBufBytes is the per-packet range-coder buffer size (must equal the shim's
// SILKCORE_BUF_BYTES). The Go decoder is fed this exact buffer.
const silkcoreBufBytes = C.SILKCORE_BUF_BYTES

// silkcoreMaxFrames is the largest number of SILK frames in one packet (60 ms => 3).
const silkcoreMaxFrames = C.SILKCORE_MAX_FRAMES

// silkcoreFrame is the C decode result for one SILK frame.
type silkcoreFrame struct {
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
	ecPrevSignalType     int
	ecPrevLagIndex       int
}

// silkcoreCtx wraps the persistent C encoder + decoder for one sequence.
type silkcoreCtx struct {
	c                *C.oracle_silkcore_ctx
	fsKHz            int
	payloadMs        int
	nFramesPerPacket int
	nbSubfr          int
}

// newSilkcoreCtx builds a sequence context: a SILK encoder forced to internal rate
// fsKHz (8/12/16) producing payloadMs (10/20/40/60) packets at the given bitrate and
// complexity, plus a matching decoder. useDTX enables discontinuous transmission
// (silent packets then encode to zero bytes). Returns nil on allocation failure.
func newSilkcoreCtx(fsKHz, payloadMs, bitrate, complexity, useDTX int) *silkcoreCtx {
	c := C.oracle_silkcore_create(C.int(fsKHz), C.int(payloadMs), C.int(bitrate),
		C.int(complexity), C.int(useDTX))
	if c == nil {
		return nil
	}
	return &silkcoreCtx{
		c:                c,
		fsKHz:            fsKHz,
		payloadMs:        payloadMs,
		nFramesPerPacket: int(c.nFramesPerPacket),
		nbSubfr:          int(c.nb_subfr),
	}
}

// frameLength returns the internal-rate SILK frame length (samples) the decoder
// produces per frame: nb_subfr * SUB_FRAME_LENGTH_MS * fs_kHz.
func (s *silkcoreCtx) frameLength() int { return s.nbSubfr * 5 * s.fsKHz }

// internalRate returns the encoder's chosen internal sampling rate (Hz), an output
// of silk_Encode. With min==max==desired forced, it must equal fsKHz*1000.
func (s *silkcoreCtx) internalRate() int { return int(s.c.encControl.internalSampleRate) }

// close frees the C context. Safe to call once.
func (s *silkcoreCtx) close() {
	if s.c != nil {
		C.oracle_silkcore_destroy(s.c)
		s.c = nil
	}
}

// encode encodes one packet of PCM (payloadMs of int16 samples at fsKHz*1000 Hz) with
// the C silk_Encode. It returns the packet bytes, or nil for a DTX/empty packet (zero
// bytes produced). It panics on an encoder error, which indicates a misconfigured test.
func (s *silkcoreCtx) encode(pcm []int16) []byte {
	buf := make([]byte, silkcoreBufBytes)
	var pcmPtr *C.int16_t
	if len(pcm) > 0 {
		pcmPtr = (*C.int16_t)(unsafe.Pointer(&pcm[0]))
	}
	n := int(C.oracle_silkcore_encode(s.c, pcmPtr, C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(silkcoreBufBytes)))
	if n < 0 {
		panic("silk_Encode failed")
	}
	if n == 0 {
		return nil
	}
	return buf
}

// decodePacket runs the C silk_decode_frame driver over the packet buffer and returns
// one silkcoreFrame per SILK frame in the packet. buf must be the exact
// silkcoreBufBytes-sized buffer returned by encode.
func (s *silkcoreCtx) decodePacket(buf []byte) []silkcoreFrame {
	var couts [silkcoreMaxFrames]C.oracle_silkcore_frame_out
	nf := int(C.oracle_silkcore_decode_packet(s.c,
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)), &couts[0]))

	frames := make([]silkcoreFrame, nf)
	for i := 0; i < nf; i++ {
		o := &couts[i]
		fl := int(o.frameLength)
		out := make([]int16, fl)
		for j := 0; j < fl; j++ {
			out[j] = int16(o.out[j])
		}
		frames[i] = silkcoreFrame{
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
			ecPrevSignalType:     int(o.ecPrevSignalType),
			ecPrevLagIndex:       int(o.ecPrevLagIndex),
		}
	}
	return frames
}
