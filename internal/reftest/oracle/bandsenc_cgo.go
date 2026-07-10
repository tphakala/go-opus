//go:build refc

package oracle

/*
#include "bandsenc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT residual band ENCODER
// (celt/bands.c: quant_all_bands, encode=1) over a plain Go-typed function so
// bandsenc_test.go can drive the pure-Go internal/celt QuantAllBandsEncode
// against the C oracle without importing "C" itself. The package-level cgo
// CFLAGS live in oracle_cgo.go; this file only pulls in bandsenc_shim.h (which
// itself reuses the decode bands_shim.h mode helpers). It does not edit any
// shared oracle file.
//
// Go int is 64-bit but C int is 32-bit, so int-array parameters
// (offsets/tf_res) are marshalled through []C.int here.

// bandsEncResult holds one quant_all_bands ENCODE mini-pipeline outcome: the
// finalized bitstream plus the resynth spectrum and end state.
type bandsEncResult struct {
	codedBands    int
	intensity     int
	dualStereo    int
	x             []int32
	collapseMasks []byte
	seed          uint32
	tell          int
	rng           uint32
}

// cBandsEncPipeline runs the C init_caps -> clt_compute_allocation(encode=1) ->
// quant_all_bands(encode=1) -> ec_enc_done pipeline once, quantizing xin
// (channels*N) with bandE (2*nbEBands) into buf (length len). buf is filled with
// the FINALIZED bitstream on return. offsets and tfRes have length nbEBands. The
// resynth X (channels*N), collapse masks (channels*nbEBands) and range-coder end
// state are returned.
func cBandsEncPipeline(start, end, LM, channels, isTransient, spread, intensityIn,
	dualStereoIn, disableInv, allocTrim, complexity, length int, offsets, tfRes []int,
	bandE, xin []int32, seedIn uint32, buf []byte, nbEBands, N int) bandsEncResult {
	cOffsets := make([]C.int, nbEBands)
	cTfRes := make([]C.int, nbEBands)
	for i := 0; i < nbEBands; i++ {
		cOffsets[i] = C.int(offsets[i])
		cTfRes[i] = C.int(tfRes[i])
	}
	cX := make([]C.int32_t, channels*N)
	cCollapse := make([]C.uchar, channels*nbEBands)
	var cSeed C.uint32_t
	var cTell, cCodedBands, cIntensity, cDualStereo C.int
	var cRng C.uint32_t

	var bandEPtr *C.int32_t
	if len(bandE) > 0 {
		bandEPtr = (*C.int32_t)(unsafe.Pointer(&bandE[0]))
	}
	var xinPtr *C.int32_t
	if len(xin) > 0 {
		xinPtr = (*C.int32_t)(unsafe.Pointer(&xin[0]))
	}

	codedBands := C.oracle_bandsenc_pipeline(
		C.int(start), C.int(end), C.int(LM), C.int(channels),
		C.int(isTransient), C.int(spread), C.int(intensityIn), C.int(dualStereoIn),
		C.int(disableInv), C.int(allocTrim), C.int(complexity), C.int(length),
		&cOffsets[0], &cTfRes[0], bandEPtr, xinPtr, C.uint32_t(seedIn),
		(*C.uchar)(unsafe.Pointer(&buf[0])),
		&cX[0], &cCollapse[0],
		&cSeed, &cTell, &cRng, &cCodedBands, &cIntensity, &cDualStereo)

	res := bandsEncResult{
		codedBands:    int(codedBands),
		intensity:     int(cIntensity),
		dualStereo:    int(cDualStereo),
		x:             make([]int32, channels*N),
		collapseMasks: make([]byte, channels*nbEBands),
		seed:          uint32(cSeed),
		tell:          int(cTell),
		rng:           uint32(cRng),
	}
	for i := range res.x {
		res.x[i] = int32(cX[i])
	}
	for i := range res.collapseMasks {
		res.collapseMasks[i] = byte(cCollapse[i])
	}
	return res
}

// cSpreadingDecision runs the C spreading_decision over xin
// (channels*(M*shortMdctSize)) with the given running state (passed by value in,
// returned out) and spread weights (nbEBands). Returns the SPREAD_* decision and
// the updated average / hf_average / tapset_decision.
func cSpreadingDecision(xin []int32, averageIn, lastDecision, hfAverageIn, tapsetIn,
	updateHf, end, channels, M int, spreadWeight []int, nbEBands int) (decision, avgOut, hfOut, tapOut int) {
	cAvg := C.int(averageIn)
	cHf := C.int(hfAverageIn)
	cTap := C.int(tapsetIn)
	cSW := make([]C.int, nbEBands)
	for i := 0; i < nbEBands; i++ {
		cSW[i] = C.int(spreadWeight[i])
	}
	d := C.oracle_spreading_decision(
		(*C.int32_t)(unsafe.Pointer(&xin[0])), &cAvg, C.int(lastDecision), &cHf, &cTap,
		C.int(updateHf), C.int(end), C.int(channels), C.int(M), &cSW[0], C.int(len(xin)))
	return int(d), int(cAvg), int(cHf), int(cTap)
}

// cBandsLogN returns the oracle mode's logN[i] (used by the avoid_split_noise
// snap witness to reconstruct compute_theta's pulse_cap / qn).
func cBandsLogN(i int) int { return int(C.oracle_bands_logN(C.int(i))) }

// cBandsCacheMax returns cache[cache[0]] for band i at LM (the max PVQ bits used
// by quant_partition's split condition b > cache[cache[0]]+12).
func cBandsCacheMax(LM, i int) int { return int(C.oracle_bands_cache_max(C.int(LM), C.int(i))) }
