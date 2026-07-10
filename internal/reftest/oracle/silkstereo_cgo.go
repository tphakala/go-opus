//go:build refc

package oracle

/*
#include "silkstereo_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK stereo un-mixing (silk/stereo_decode_pred.c
// + silk/stereo_MS_to_LR.c) over plain Go-typed values so silkstereo_test.go can drive
// the pure-Go internal/silk stereo port against the C oracle without importing "C"
// itself. The package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// silkstereo_shim.h.

// stereoPredBufBytes is the range-coder buffer size (must equal the shim's
// STEREO_PRED_BUF_BYTES). Both the C and Go decoders init over the whole buffer, so the
// size is part of the compared coder state.
const stereoPredBufBytes = C.STEREO_PRED_BUF_BYTES

// stereoPredResult is the C encode+decode round-trip result for one predictor+flag.
type stereoPredResult struct {
	buf     []byte   // the encoded range-coder bytes (fed verbatim to the Go decoder)
	predQ13 [2]int32 // C-decoded predictors
	midOnly int      // C-decoded mid-only flag
	rng     uint32   // ec_dec rng after the C decode
	tell    int      // ec_tell after the C decode
	nBytes  int      // bytes the encoder used (informational)
}

// stereoPredRoundtripC encodes ix (row-major ix[2][3]) + midOnly with the C stereo
// encoder and decodes the result with the C stereo decoder. The caller must supply valid
// indices (ix[n][0] in [0,2], ix[n][1] in [0,4], ix[n][2] in [0,4]).
func stereoPredRoundtripC(ix [6]int8, midOnly int) stereoPredResult {
	var o C.oracle_stereo_pred_out
	var cix [6]C.int8_t
	for i := 0; i < 6; i++ {
		cix[i] = C.int8_t(ix[i])
	}
	C.oracle_stereo_pred_roundtrip(&cix[0], C.int(midOnly), &o)

	buf := make([]byte, int(stereoPredBufBytes))
	for i := range buf {
		buf[i] = byte(o.buf[i])
	}
	return stereoPredResult{
		buf:     buf,
		predQ13: [2]int32{int32(o.pred_Q13[0]), int32(o.pred_Q13[1])},
		midOnly: int(o.mid_only),
		rng:     uint32(o.rng),
		tell:    int(o.tell),
		nBytes:  int(o.nBytes),
	}
}

// stereoState mirrors the C stereo_dec_state (pred_prev_Q13, sMid, sSide): the persistent
// un-mixing state the test carries across a frame sequence on the C side.
type stereoState struct {
	predPrevQ13 [2]int16
	sMid        [2]int16
	sSide       [2]int16
}

// msToLR runs one frame of the C silk_stereo_MS_to_LR over x1/x2 (frameLength+2 mid/side
// buffers, this frame's samples at [2:frameLength+2]) with predQ13, updating x1/x2 in
// place (left at x1[1:frameLength+1], right at x2[1:frameLength+1]) and the state.
func (s *stereoState) msToLR(x1, x2 []int16, predQ13 [2]int32, fsKHz, frameLength int) {
	var cst C.oracle_stereo_state
	cst.predPrevQ13[0] = C.int16_t(s.predPrevQ13[0])
	cst.predPrevQ13[1] = C.int16_t(s.predPrevQ13[1])
	cst.sMid[0] = C.int16_t(s.sMid[0])
	cst.sMid[1] = C.int16_t(s.sMid[1])
	cst.sSide[0] = C.int16_t(s.sSide[0])
	cst.sSide[1] = C.int16_t(s.sSide[1])

	var cpred [2]C.int32_t
	cpred[0] = C.int32_t(predQ13[0])
	cpred[1] = C.int32_t(predQ13[1])

	C.oracle_stereo_ms_to_lr(&cst,
		(*C.int16_t)(unsafe.Pointer(&x1[0])),
		(*C.int16_t)(unsafe.Pointer(&x2[0])),
		&cpred[0], C.int(fsKHz), C.int(frameLength))

	s.predPrevQ13[0] = int16(cst.predPrevQ13[0])
	s.predPrevQ13[1] = int16(cst.predPrevQ13[1])
	s.sMid[0] = int16(cst.sMid[0])
	s.sMid[1] = int16(cst.sMid[1])
	s.sSide[0] = int16(cst.sSide[0])
	s.sSide[1] = int16(cst.sSide[1])
}
