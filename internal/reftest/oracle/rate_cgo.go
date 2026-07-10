//go:build refc

package oracle

/*
#include "rate_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT bit-allocation (celt/rate.c,
// celt/celt.c init_caps) over plain Go-typed functions so rate_test.go can drive
// the pure-Go internal/celt ComputeAllocation against the C oracle without
// importing "C" itself. The package-level cgo CFLAGS live in oracle_cgo.go; this
// file only pulls in the rate_shim.h wrappers.
//
// Go int is 64-bit but C int is 32-bit, so the int-array parameters
// (offsets/caps/pulses/ebits/fine_priority) are marshalled through []C.int here.
// The channel count parameter is named `channels` (not `C`) so it does not shadow
// the cgo pseudo-package.

// rateAllocBufBytes is the coder buffer size for one clt_compute_allocation run.
// clt_compute_allocation emits at most ~end-start skip flags plus one intensity
// symbol and one dual-stereo bit (a couple dozen bits total), so 1024 bytes is
// ample and the same size is used for encode and decode so the bytes line up.
const rateAllocBufBytes = 1024

// cRateModeInfo returns the oracle CELTMode dimensions (nbEBands, nbAllocVectors,
// effEBands) for mode48000_960.
func cRateModeInfo() (nbEBands, nbAllocVectors, effEBands int) {
	var nb, nv, eff C.int
	C.oracle_rate_mode_info(&nb, &nv, &eff)
	return int(nb), int(nv), int(eff)
}

// cRateInitCaps returns the C init_caps output caps[0..nbEBands) for (LM, channels).
func cRateInitCaps(LM, channels, nbEBands int) []int {
	caps := make([]C.int, nbEBands)
	C.oracle_rate_init_caps(C.int(LM), C.int(channels), &caps[0])
	out := make([]int, nbEBands)
	for i := range caps {
		out[i] = int(caps[i])
	}
	return out
}

// rateAllocResult holds one clt_compute_allocation outcome from either side.
type rateAllocResult struct {
	codedBands   int
	pulses       []int
	ebits        []int
	finePriority []int
	intensity    int
	dualStereo   int
	balance      int
	tell         int
	rng          uint32
}

// cRateAlloc runs the C clt_compute_allocation once over buf (length
// rateAllocBufBytes). encode!=0 emits the flags into buf (which is mutated in
// place and finalized); encode==0 reads them back. offsets and caps have length
// nbEBands.
func cRateAlloc(start, end int, offsets, caps []int, allocTrim, intensityIn, dualStereoIn,
	total, channels, LM, encode, prev, signalBandwidth int, buf []byte) rateAllocResult {
	nbEBands := len(offsets)
	cOffsets := make([]C.int, nbEBands)
	cCaps := make([]C.int, nbEBands)
	for i := 0; i < nbEBands; i++ {
		cOffsets[i] = C.int(offsets[i])
		cCaps[i] = C.int(caps[i])
	}
	cPulses := make([]C.int, nbEBands)
	cEbits := make([]C.int, nbEBands)
	cFinePriority := make([]C.int, nbEBands)
	var cIntensity, cDualStereo, cBalance, cTell C.int
	var cRng C.uint32_t

	codedBands := C.oracle_rate_alloc(
		C.int(start), C.int(end),
		&cOffsets[0], &cCaps[0],
		C.int(allocTrim), C.int(intensityIn), C.int(dualStereoIn),
		C.int(total), C.int(channels), C.int(LM),
		C.int(encode), C.int(prev), C.int(signalBandwidth),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		&cPulses[0], &cEbits[0], &cFinePriority[0],
		&cIntensity, &cDualStereo, &cBalance, &cTell, &cRng)

	res := rateAllocResult{
		codedBands:   int(codedBands),
		pulses:       make([]int, nbEBands),
		ebits:        make([]int, nbEBands),
		finePriority: make([]int, nbEBands),
		intensity:    int(cIntensity),
		dualStereo:   int(cDualStereo),
		balance:      int(cBalance),
		tell:         int(cTell),
		rng:          uint32(cRng),
	}
	for i := 0; i < nbEBands; i++ {
		res.pulses[i] = int(cPulses[i])
		res.ebits[i] = int(cEbits[i])
		res.finePriority[i] = int(cFinePriority[i])
	}
	return res
}

// cGetPulses, cBits2Pulses, cPulses2Bits wrap the rate.h cache helpers.
func cGetPulses(i int) int { return int(C.oracle_get_pulses(C.int(i))) }

func cBits2Pulses(band, LM, bits int) int {
	return int(C.oracle_bits2pulses(C.int(band), C.int(LM), C.int(bits)))
}

func cPulses2Bits(band, LM, pulses int) int {
	return int(C.oracle_pulses2bits(C.int(band), C.int(LM), C.int(pulses)))
}
