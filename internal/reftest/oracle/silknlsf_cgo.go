//go:build refc

package oracle

/*
#include "silknlsf_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK NLSF decode chain (silk/NLSF_decode.c,
// NLSF_unpack.c, NLSF_stabilize.c, NLSF2A.c, LPC_fit.c, LPC_inv_pred_gain.c,
// bwexpander.c, bwexpander_32.c, sort.c) over plain Go-typed functions so
// silknlsf_test.go can drive the pure-Go internal/silk port against the C oracle
// without importing "C" itself. The package-level cgo CFLAGS live in oracle_cgo.go;
// this file only pulls in the silknlsf_shim.h wrappers.

// cSilkNLSFDecode dequantizes and stabilizes the Q15 NLSF vector from the codebook
// path indices (length order+1) via the C silk_NLSF_decode, selecting the codebook
// by order (16 -> WB, else NB/MB). Returns the order-long Q15 NLSF vector.
func cSilkNLSFDecode(order int, indices []int8) []int16 {
	out := make([]int16, order)
	C.oracle_silk_NLSF_decode(C.int(order),
		(*C.int8_t)(unsafe.Pointer(&indices[0])),
		(*C.int16_t)(unsafe.Pointer(&out[0])))
	return out
}

// cSilkNLSF2A converts the Q15 NLSF vector (length d) to the Q12 LPC whitening
// filter (length d) via the C silk_NLSF2A.
func cSilkNLSF2A(nlsf []int16, d int) []int16 {
	aQ12 := make([]int16, d)
	C.oracle_silk_NLSF2A((*C.int16_t)(unsafe.Pointer(&nlsf[0])), C.int(d),
		(*C.int16_t)(unsafe.Pointer(&aQ12[0])))
	return aQ12
}

// cSilkLPCInversePredGain returns the inverse prediction gain (Q30) of the Q12
// coefficients aQ12 (length order) via the C silk_LPC_inverse_pred_gain_c, or 0 if
// the filter is unstable.
func cSilkLPCInversePredGain(aQ12 []int16, order int) int32 {
	return int32(C.oracle_silk_LPC_inverse_pred_gain(
		(*C.int16_t)(unsafe.Pointer(&aQ12[0])), C.int(order)))
}

// cSilkBwexpander chirps a copy of the Q12 int16 filter ar (length d) by chirp via
// the C silk_bwexpander and returns the expanded copy.
func cSilkBwexpander(ar []int16, d int, chirp int32) []int16 {
	out := append([]int16(nil), ar...)
	C.oracle_silk_bwexpander((*C.int16_t)(unsafe.Pointer(&out[0])), C.int(d), C.int32_t(chirp))
	return out
}

// cSilkBwexpander32 chirps a copy of the int32 filter ar (length d) by chirp via
// the C silk_bwexpander_32 and returns the expanded copy.
func cSilkBwexpander32(ar []int32, d int, chirp int32) []int32 {
	out := append([]int32(nil), ar...)
	C.oracle_silk_bwexpander_32((*C.int32_t)(unsafe.Pointer(&out[0])), C.int(d), C.int32_t(chirp))
	return out
}

// cSilkNLSFStabilize stabilizes a copy of the Q15 NLSF vector nlsf (length L) given
// the [L+1] minimum-distance vector ndeltamin via the C silk_NLSF_stabilize and
// returns the stabilized copy.
func cSilkNLSFStabilize(nlsf []int16, ndeltamin []int16, L int) []int16 {
	out := append([]int16(nil), nlsf...)
	C.oracle_silk_NLSF_stabilize((*C.int16_t)(unsafe.Pointer(&out[0])),
		(*C.int16_t)(unsafe.Pointer(&ndeltamin[0])), C.int(L))
	return out
}
