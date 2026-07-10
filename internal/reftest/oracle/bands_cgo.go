//go:build refc

package oracle

/*
#include "bands_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT residual band quantizer
// (celt/bands.c: quant_all_bands, denormalise_bands, anti_collapse) over plain
// Go-typed functions so bands_test.go can drive the pure-Go internal/celt
// QuantAllBands / DenormaliseBands / AntiCollapse against the C oracle without
// importing "C" itself. The package-level cgo CFLAGS live in oracle_cgo.go; this
// file only pulls in the bands_shim.h wrappers.
//
// Go int is 64-bit but C int is 32-bit, so int-array parameters
// (offsets/tf_res/pulses) are marshalled through []C.int here.

// cBandsModeInfo returns the oracle CELTMode dimensions for mode48000_960.
func cBandsModeInfo() (nbEBands, effEBands, shortMdctSize int) {
	var nb, eff, sm C.int
	C.oracle_bands_mode_info(&nb, &eff, &sm)
	return int(nb), int(eff), int(sm)
}

// cBandsEBand returns eBands[i] (band boundary in shortMdctSize units).
func cBandsEBand(i int) int { return int(C.oracle_bands_eband(C.int(i))) }

// bandsPipelineResult holds one quant_all_bands mini-pipeline outcome.
type bandsPipelineResult struct {
	codedBands    int
	intensity     int
	dualStereo    int
	x             []int32
	collapseMasks []byte
	seed          uint32
	tell          int
	rng           uint32
}

// cBandsPipeline runs the C init_caps -> clt_compute_allocation ->
// quant_all_bands mini-pipeline once over buf (length len). encode!=0 quantizes
// xin (channels*N) using bandE (2*nbEBands) into buf; encode==0 decodes buf.
// offsets and tfRes have length nbEBands. The decoded X (channels*N) and collapse
// masks (channels*nbEBands) plus the range-coder end state are returned.
func cBandsPipeline(encode, start, end, LM, channels, isTransient, spread, intensityIn,
	dualStereoIn, disableInv, allocTrim, length int, offsets, tfRes []int, bandE, xin []int32,
	seedIn uint32, buf []byte, nbEBands, N int) bandsPipelineResult {
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

	// bandE / xin are only read on encode; pass a valid pointer regardless.
	var bandEPtr *C.int32_t
	if len(bandE) > 0 {
		bandEPtr = (*C.int32_t)(unsafe.Pointer(&bandE[0]))
	}
	var xinPtr *C.int32_t
	if len(xin) > 0 {
		xinPtr = (*C.int32_t)(unsafe.Pointer(&xin[0]))
	}

	codedBands := C.oracle_bands_pipeline(
		C.int(encode), C.int(start), C.int(end), C.int(LM), C.int(channels),
		C.int(isTransient), C.int(spread), C.int(intensityIn), C.int(dualStereoIn),
		C.int(disableInv), C.int(allocTrim), C.int(length),
		&cOffsets[0], &cTfRes[0], bandEPtr, xinPtr, C.uint32_t(seedIn),
		(*C.uchar)(unsafe.Pointer(&buf[0])),
		&cX[0], &cCollapse[0],
		&cSeed, &cTell, &cRng, &cCodedBands, &cIntensity, &cDualStereo)

	res := bandsPipelineResult{
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

// cDenormaliseBands runs the C denormalise_bands over xin (N) and bandLogE
// (nbEBands), returning the synthesis spectrum freq (N).
func cDenormaliseBands(xin, bandLogE []int32, start, end, LM, downsample, silence, N int) []int32 {
	cFreq := make([]C.int32_t, N)
	C.oracle_denormalise_bands(
		(*C.int32_t)(unsafe.Pointer(&xin[0])),
		(*C.int32_t)(unsafe.Pointer(&bandLogE[0])),
		C.int(start), C.int(end), C.int(LM), C.int(downsample), C.int(silence),
		&cFreq[0])
	freq := make([]int32, N)
	for i := range freq {
		freq[i] = int32(cFreq[i])
	}
	return freq
}

// cAntiCollapse runs the C anti_collapse over xin (channels*size) with the given
// collapse masks (channels*nbEBands), logE/prev energies (2*nbEBands each) and
// pulses (nbEBands), returning the modified X (channels*size).
func cAntiCollapse(xin []int32, collapseMasks []byte, LM, channels, size, start, end int,
	logE, prev1logE, prev2logE []int32, pulses []int, seed uint32, encode, nbEBands int) []int32 {
	cPulses := make([]C.int, nbEBands)
	for i := 0; i < nbEBands; i++ {
		cPulses[i] = C.int(pulses[i])
	}
	total := channels * size
	cXout := make([]C.int32_t, total)
	C.oracle_anti_collapse(
		(*C.int32_t)(unsafe.Pointer(&xin[0])),
		(*C.uchar)(unsafe.Pointer(&collapseMasks[0])),
		C.int(LM), C.int(channels), C.int(size), C.int(start), C.int(end),
		(*C.int32_t)(unsafe.Pointer(&logE[0])),
		(*C.int32_t)(unsafe.Pointer(&prev1logE[0])),
		(*C.int32_t)(unsafe.Pointer(&prev2logE[0])),
		&cPulses[0], C.uint32_t(seed), C.int(encode),
		&cXout[0])
	out := make([]int32, total)
	for i := range out {
		out[i] = int32(cXout[i])
	}
	return out
}
