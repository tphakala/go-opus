//go:build refc

package oracle

/*
#include "energyenc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT encode-side band-energy path
// (celt/bands.c compute_band_energies / normalise_bands, celt/quant_bands.c
// amp2Log2 / quant_coarse_energy / quant_fine_energy / quant_energy_finalise,
// celt/laplace.c ec_laplace_encode) over the frozen static mode48000_960 as
// plain Go functions, so energyenc_test.go can drive the pure-Go internal/celt
// ENCODE port against the C oracle without importing "C" itself. Package-level
// cgo CFLAGS live in oracle_cgo.go; this file only pulls in energyenc_shim.h.

// cEENbEBands returns the mode's band count (21).
func cEENbEBands() int { return int(C.oracle_ee_nbebands()) }

// cEEShortMdctSize returns the mode's short-MDCT size (120).
func cEEShortMdctSize() int { return int(C.oracle_ee_shortmdctsize()) }

// laplaceSeqResult holds the C oracle output for a Laplace symbol sequence.
type laplaceSeqResult struct {
	packet     []byte  // finalized bitstream, length nbBytes
	values     []int32 // per-symbol write-back values (in/out clamp)
	rng        uint32  // range register captured before ec_enc_done
	valReg     uint32  // low end (val) captured before ec_enc_done
	rangeBytes uint32  // ec_range_bytes before done
}

// cEELaplaceSeq encodes the n symbols (values[i], fss[i], decays[i]) with the C
// ec_laplace_encode and reports the finalized packet, the write-back values, and
// the pre-done range state.
func cEELaplaceSeq(values []int32, fss []uint32, decays []int32, nbBytes int) laplaceSeqResult {
	n := len(values)
	packet := make([]byte, nbBytes)
	valOut := make([]int32, n)
	var rng, valReg, rangeBytes C.uint32_t
	C.oracle_ee_laplace_seq(
		C.int(n), C.int(nbBytes),
		(*C.int32_t)(unsafe.Pointer(&values[0])),
		(*C.uint32_t)(unsafe.Pointer(&fss[0])),
		(*C.int32_t)(unsafe.Pointer(&decays[0])),
		(*C.uchar)(unsafe.Pointer(&packet[0])),
		(*C.int32_t)(unsafe.Pointer(&valOut[0])),
		&rng, &valReg, &rangeBytes)
	return laplaceSeqResult{
		packet:     packet,
		values:     valOut,
		rng:        uint32(rng),
		valReg:     uint32(valReg),
		rangeBytes: uint32(rangeBytes),
	}
}

// cEEComputeBandEnergies runs the C compute_band_energies over X (celt_sig,
// length channels*(shortMdctSize<<lm)) and returns bandE (channels*nbEBands).
func cEEComputeBandEnergies(lm, channels, end int, X []int32) []int32 {
	bandE := make([]int32, channels*cEENbEBands())
	C.oracle_ee_compute_band_energies(
		C.int(lm), C.int(channels), C.int(end),
		(*C.int32_t)(unsafe.Pointer(&X[0])),
		(*C.int32_t)(unsafe.Pointer(&bandE[0])))
	return bandE
}

// cEENormaliseBands runs the C normalise_bands over freq (celt_sig) using bandE
// and returns X (celt_norm, length channels*((1<<lm)*shortMdctSize)).
func cEENormaliseBands(lm, channels, end int, freq, bandE []int32) []int32 {
	X := make([]int32, len(freq))
	C.oracle_ee_normalise_bands(
		C.int(lm), C.int(channels), C.int(end),
		(*C.int32_t)(unsafe.Pointer(&freq[0])),
		(*C.int32_t)(unsafe.Pointer(&bandE[0])),
		(*C.int32_t)(unsafe.Pointer(&X[0])))
	return X
}

// cEEAmp2Log2 runs the C amp2Log2 over bandE and returns bandLogE, both length
// channels*nbEBands.
func cEEAmp2Log2(effEnd, end, channels int, bandE []int32) []int32 {
	bandLogE := make([]int32, channels*cEENbEBands())
	C.oracle_ee_amp2log2(
		C.int(effEnd), C.int(end), C.int(channels),
		(*C.int32_t)(unsafe.Pointer(&bandE[0])),
		(*C.int32_t)(unsafe.Pointer(&bandLogE[0])))
	return bandLogE
}

// energyEncResult holds one frame's C encode outputs.
type energyEncResult struct {
	packet     []byte  // coded bitstream, length nbBytes
	oldOut     []int32 // reconstructed oldEBands (channels*nbEBands)
	errOut     []int32 // error[] after this frame (channels*nbEBands)
	delayedOut int32   // updated delayedIntra
	intraDec   int     // intra flag read back from the bitstream
}

// cEECoarse runs the C quant_coarse_energy for one frame, carrying the given
// prediction base (oldIn) and RDO bias (delayedIn). start/end/effEnd select the
// coded band window (hybrid start!=0, narrowband end<nbEBands, effEnd<end for
// loss_distortion); preBits emits that many range-coded bits before coarse energy
// so ec_tell>0 at entry.
func cEECoarse(lm, channels, nbBytes, forceIntra, twoPass, lossRate, lfe, start, end, effEnd, preBits int,
	eIn, oldIn []int32, delayedIn int32) energyEncResult {
	n := channels * cEENbEBands()
	packet := make([]byte, nbBytes)
	oldOut := make([]int32, n)
	errOut := make([]int32, n)
	var delayedOut C.int32_t
	intra := C.oracle_ee_coarse(
		C.int(lm), C.int(channels), C.int(nbBytes), C.int(forceIntra), C.int(twoPass),
		C.int(lossRate), C.int(lfe), C.int(start), C.int(end), C.int(effEnd), C.int(preBits),
		(*C.int32_t)(unsafe.Pointer(&eIn[0])),
		(*C.int32_t)(unsafe.Pointer(&oldIn[0])),
		(*C.int32_t)(unsafe.Pointer(&oldOut[0])),
		(*C.int32_t)(unsafe.Pointer(&errOut[0])),
		C.int32_t(delayedIn), &delayedOut,
		(*C.uchar)(unsafe.Pointer(&packet[0])))
	return energyEncResult{
		packet:     packet,
		oldOut:     oldOut,
		errOut:     errOut,
		delayedOut: int32(delayedOut),
		intraDec:   int(intra),
	}
}

// cEEFull runs the C coarse+fine+finalise energy path for one frame.
func cEEFull(lm, channels, nbBytes, forceIntra, twoPass, lossRate, lfe int,
	eIn, oldIn []int32, delayedIn int32, fineQuant, finePriority []int32) energyEncResult {
	n := channels * cEENbEBands()
	packet := make([]byte, nbBytes)
	oldOut := make([]int32, n)
	errOut := make([]int32, n)
	var delayedOut C.int32_t
	intra := C.oracle_ee_full(
		C.int(lm), C.int(channels), C.int(nbBytes), C.int(forceIntra), C.int(twoPass),
		C.int(lossRate), C.int(lfe),
		(*C.int32_t)(unsafe.Pointer(&eIn[0])),
		(*C.int32_t)(unsafe.Pointer(&oldIn[0])),
		(*C.int32_t)(unsafe.Pointer(&oldOut[0])),
		(*C.int32_t)(unsafe.Pointer(&errOut[0])),
		C.int32_t(delayedIn), &delayedOut,
		(*C.int32_t)(unsafe.Pointer(&fineQuant[0])),
		(*C.int32_t)(unsafe.Pointer(&finePriority[0])),
		(*C.uchar)(unsafe.Pointer(&packet[0])))
	return energyEncResult{
		packet:     packet,
		oldOut:     oldOut,
		errOut:     errOut,
		delayedOut: int32(delayedOut),
		intraDec:   int(intra),
	}
}
