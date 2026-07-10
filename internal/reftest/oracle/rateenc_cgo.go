//go:build refc

package oracle

/*
#include "rateenc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT bit-allocation in the ENCODE
// direction (celt/rate.c clt_compute_allocation with encode=1, celt/celt.c
// init_caps) over plain Go-typed functions so rateenc_test.go can drive the
// pure-Go internal/celt ComputeAllocation against the C oracle without importing
// "C" itself. It is the Checkpoint 6 counterpart to rate_cgo.go (which owns the
// decode-direction and the four-way shared-buffer sweep); the wrappers here carry
// unique cRateEnc* names and pull in only rateenc_shim.h, so this checkpoint's
// oracle surface is self-contained and touches no shared file.
//
// Go int is 64-bit but C int is 32-bit, so the int-array parameters
// (offsets/caps/pulses/ebits/fine_priority) are marshalled through []C.int here.
// The channel count parameter is named `channels` (not `C`) so it does not shadow
// the cgo pseudo-package.

// rateEncBufBytes is the coder buffer size for one encode run. The allocation
// emits at most ~end-start skip flags plus one intensity symbol and one
// dual-stereo bit (a couple dozen bits total), so 1024 bytes is ample; the same
// size is used for the C-encode, Go-encode, and Go round-trip decode so the bytes
// line up.
const rateEncBufBytes = 1024

// cRateEncModeInfo returns the oracle CELTMode dimensions (nbEBands,
// nbAllocVectors, effEBands) for mode48000_960.
func cRateEncModeInfo() (nbEBands, nbAllocVectors, effEBands int) {
	var nb, nv, eff C.int
	C.oracle_rateenc_mode_info(&nb, &nv, &eff)
	return int(nb), int(nv), int(eff)
}

// cRateEncInitCaps returns the C init_caps output caps[0..nbEBands) for (LM, channels).
func cRateEncInitCaps(LM, channels, nbEBands int) []int {
	caps := make([]C.int, nbEBands)
	C.oracle_rateenc_init_caps(C.int(LM), C.int(channels), &caps[0])
	out := make([]int, nbEBands)
	for i := range caps {
		out[i] = int(caps[i])
	}
	return out
}

// rateEncResult holds one clt_compute_allocation outcome (encode or the Go
// round-trip decode). pulses/ebits/finePriority are length nbEBands.
type rateEncResult struct {
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

// cRateEncAlloc runs the C clt_compute_allocation once with encode=1 over buf
// (length rateEncBufBytes). The flags are emitted into buf (which is mutated in
// place and finalized). offsets and caps have length nbEBands.
func cRateEncAlloc(start, end int, offsets, caps []int, allocTrim, intensityIn, dualStereoIn,
	total, channels, LM, prev, signalBandwidth int, buf []byte) rateEncResult {
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

	codedBands := C.oracle_rateenc_alloc(
		C.int(start), C.int(end),
		&cOffsets[0], &cCaps[0],
		C.int(allocTrim), C.int(intensityIn), C.int(dualStereoIn),
		C.int(total), C.int(channels), C.int(LM),
		C.int(prev), C.int(signalBandwidth),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		&cPulses[0], &cEbits[0], &cFinePriority[0],
		&cIntensity, &cDualStereo, &cBalance, &cTell, &cRng)

	res := rateEncResult{
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
