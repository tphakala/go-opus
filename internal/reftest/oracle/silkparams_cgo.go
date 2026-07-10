//go:build refc

package oracle

/*
#include "silkparams_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK frame index/parameter decode
// (silk/decode_indices.c, decode_parameters.c, gain_quant.c, decode_pitch.c) over
// plain Go-typed structs so silkparams_test.go can drive the pure-Go internal/silk
// port against the C oracle without importing "C" itself. The C shim encodes a
// fully specified SideInfoIndices with silk_encode_indices, then decodes the same
// bitstream with silk_decode_indices + silk_decode_parameters. The package-level
// cgo CFLAGS live in oracle_cgo.go; this file only pulls in silkparams_shim.h.

// silkParamsBufBytes is the coder buffer size shared by the encode and decode side.
// One frame of SILK side information is a few dozen bytes at most; 256 is ample and
// keeps encode/decode over the exact same byte slice.
const silkParamsBufBytes = 256

// silkParamsIn fully specifies one frame's SideInfoIndices plus the decoder
// configuration and cross-frame state the encode/decode path reads.
type silkParamsIn struct {
	fsKHz                int
	nbSubfr              int
	condCoding           int
	frameIndex           int
	decodeLBRR           int
	vadFlag              int
	ecPrevSignalType     int
	ecPrevLagIndex       int
	firstFrameAfterReset int
	lossCnt              int
	lastGainIndex        int

	signalType      int
	quantOffsetType int
	gainsIndices    [4]int  // MAX_NB_SUBFR
	ltpIndex        [4]int  // MAX_NB_SUBFR
	nlsfIndices     [17]int // MAX_LPC_ORDER + 1
	lagIndex        int
	contourIndex    int
	nlsfInterpCoef  int
	perIndex        int
	ltpScaleIndex   int
	seed            int

	prevNLSFQ15 [16]int16 // MAX_LPC_ORDER
}

// silkParamsOut is the C decode result: the decoded SideInfoIndices, the decoded
// prediction parameters, the updated cross-frame state and the range-decoder end
// state.
type silkParamsOut struct {
	signalType      int
	quantOffsetType int
	gainsIndices    [4]int
	ltpIndex        [4]int
	nlsfIndices     [17]int
	lagIndex        int
	contourIndex    int
	nlsfInterpCoef  int
	perIndex        int
	ltpScaleIndex   int
	seed            int

	pitchL      [4]int
	gainsQ16    [4]int32
	predCoefQ12 [2][16]int16
	ltpCoefQ14  [20]int16 // LTP_ORDER * MAX_NB_SUBFR
	ltpScaleQ14 int

	lastGainIndex    int
	ecPrevSignalType int
	ecPrevLagIndex   int
	prevNLSFQ15      [16]int16

	tell   int
	rng    uint32
	nBytes int
}

// cSilkParamsRun encodes the input SideInfoIndices with the C silk_encode_indices,
// decodes the same bitstream with silk_decode_indices + silk_decode_parameters, and
// returns the shared byte buffer and the decoded output. The returned buffer is the
// exact bitstream both C and the Go port decode.
func cSilkParamsRun(in *silkParamsIn) (buf []byte, out silkParamsOut) {
	var cin C.oracle_silkparams_in
	var cout C.oracle_silkparams_out

	cin.fs_kHz = C.int(in.fsKHz)
	cin.nb_subfr = C.int(in.nbSubfr)
	cin.condCoding = C.int(in.condCoding)
	cin.frameIndex = C.int(in.frameIndex)
	cin.decodeLBRR = C.int(in.decodeLBRR)
	cin.vadFlag = C.int(in.vadFlag)
	cin.ec_prevSignalType = C.int(in.ecPrevSignalType)
	cin.ec_prevLagIndex = C.int(in.ecPrevLagIndex)
	cin.first_frame_after_reset = C.int(in.firstFrameAfterReset)
	cin.lossCnt = C.int(in.lossCnt)
	cin.lastGainIndex = C.int(in.lastGainIndex)

	cin.signalType = C.int(in.signalType)
	cin.quantOffsetType = C.int(in.quantOffsetType)
	for i := 0; i < 4; i++ {
		cin.gainsIndices[i] = C.int(in.gainsIndices[i])
		cin.ltpIndex[i] = C.int(in.ltpIndex[i])
	}
	for i := 0; i < 17; i++ {
		cin.nlsfIndices[i] = C.int(in.nlsfIndices[i])
	}
	cin.lagIndex = C.int(in.lagIndex)
	cin.contourIndex = C.int(in.contourIndex)
	cin.nlsfInterpCoef_Q2 = C.int(in.nlsfInterpCoef)
	cin.perIndex = C.int(in.perIndex)
	cin.ltpScaleIndex = C.int(in.ltpScaleIndex)
	cin.seed = C.int(in.seed)
	for i := 0; i < 16; i++ {
		cin.prevNLSF_Q15[i] = C.int16_t(in.prevNLSFQ15[i])
	}

	buf = make([]byte, silkParamsBufBytes)
	C.oracle_silkparams_run(&cin, &cout,
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(silkParamsBufBytes))

	out.signalType = int(cout.signalType)
	out.quantOffsetType = int(cout.quantOffsetType)
	for i := 0; i < 4; i++ {
		out.gainsIndices[i] = int(cout.gainsIndices[i])
		out.ltpIndex[i] = int(cout.ltpIndex[i])
		out.pitchL[i] = int(cout.pitchL[i])
		out.gainsQ16[i] = int32(cout.gains_Q16[i])
	}
	for i := 0; i < 17; i++ {
		out.nlsfIndices[i] = int(cout.nlsfIndices[i])
	}
	out.lagIndex = int(cout.lagIndex)
	out.contourIndex = int(cout.contourIndex)
	out.nlsfInterpCoef = int(cout.nlsfInterpCoef_Q2)
	out.perIndex = int(cout.perIndex)
	out.ltpScaleIndex = int(cout.ltpScaleIndex)
	out.seed = int(cout.seed)
	for i := 0; i < 16; i++ {
		out.predCoefQ12[0][i] = int16(cout.predCoef_Q12[0][i])
		out.predCoefQ12[1][i] = int16(cout.predCoef_Q12[1][i])
		out.prevNLSFQ15[i] = int16(cout.prevNLSF_Q15[i])
	}
	for i := 0; i < 20; i++ {
		out.ltpCoefQ14[i] = int16(cout.ltpCoef_Q14[i])
	}
	out.ltpScaleQ14 = int(cout.ltpScale_Q14)
	out.lastGainIndex = int(cout.lastGainIndex)
	out.ecPrevSignalType = int(cout.ec_prevSignalType)
	out.ecPrevLagIndex = int(cout.ec_prevLagIndex)
	out.tell = int(cout.tell)
	out.rng = uint32(cout.rng)
	out.nBytes = int(cout.nBytes)
	return buf, out
}
