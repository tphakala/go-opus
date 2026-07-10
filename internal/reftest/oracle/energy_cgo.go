//go:build refc

package oracle

/*
#include "energy_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT band-energy quantizer
// (celt/quant_bands.c) over the frozen static mode48000_960 as a plain Go
// function so energy_test.go can drive the pure-Go internal/celt decode against
// the C oracle without importing "C" itself. The package-level cgo CFLAGS live in
// oracle_cgo.go; this file only pulls in the energy_shim.h wrappers.

// cEnergyNbEBands returns the mode's band count (21).
func cEnergyNbEBands() int { return int(C.oracle_energy_nbebands()) }

// energyResult holds the C oracle's encode+decode outputs for one frame.
type energyResult struct {
	packet   []byte  // coded bitstream, length nbBytes
	oldEnc   []int32 // encoder's reconstructed oldEBands (channels*nbEBands)
	oldDec   []int32 // decoder's reconstructed oldEBands (channels*nbEBands)
	intraDec int     // intra flag actually read back from the bitstream
}

// cEnergyRoundtrip encodes eBands (with prior state oldInit) into a nbBytes
// packet using the C encoder, then decodes it back with the C decoder. eBands
// and oldInit have length channels*nbEBands; fineQuant and finePriority have
// length nbEBands. It returns the packet plus both C reconstructions.
func cEnergyRoundtrip(lm, channels, intra, nbBytes int,
	eBands, oldInit []int32, fineQuant, finePriority []int32) energyResult {
	nb := cEnergyNbEBands()
	n := channels * nb
	packet := make([]byte, nbBytes)
	oldEnc := make([]int32, n)
	oldDec := make([]int32, n)

	intraDec := C.oracle_energy_roundtrip(
		C.int(lm), C.int(channels), C.int(intra), C.int(nbBytes),
		(*C.int32_t)(unsafe.Pointer(&eBands[0])),
		(*C.int32_t)(unsafe.Pointer(&oldInit[0])),
		(*C.int32_t)(unsafe.Pointer(&fineQuant[0])),
		(*C.int32_t)(unsafe.Pointer(&finePriority[0])),
		(*C.uchar)(unsafe.Pointer(&packet[0])),
		(*C.int32_t)(unsafe.Pointer(&oldEnc[0])),
		(*C.int32_t)(unsafe.Pointer(&oldDec[0])))

	return energyResult{
		packet:   packet,
		oldEnc:   oldEnc,
		oldDec:   oldDec,
		intraDec: int(intraDec),
	}
}
