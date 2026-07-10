//go:build refc

package oracle

/*
#include "vqenc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT PVQ ENCODE side (celt/vq.c alg_quant
// / stereo_itheta, celt/cwrs.c encode_pulses / icwrs) over plain Go-typed
// functions, so vqenc_test.go can drive the pure-Go internal/celt encode path
// against C without importing "C" itself. The package-level cgo CFLAGS live in
// oracle_cgo.go; this file only pulls in the vqenc_shim.h wrappers (which include
// vq_shim.h). The V(N,K) recurrence and pulse-cap helpers (cPVQTrueV / cPVQMaxK)
// are defined in vq_cgo.go and reused directly from the tests.

// vqencBufBytes is the coder buffer size shared by the Go and C encode side. It
// MUST match on both sides so ec_enc_uint's raw low bits (written from the END of
// the buffer) line up byte-for-byte. 128 bytes is ample for a single codeword.
const vqencBufBytes = 128

// cPVQAlgQuantEnc runs the C alg_quant over Xin (length N), returning the
// finalized packet, the collapse mask, the coded pulse vector (recovered by
// decoding the codeword), and the reconstructed band Xout (meaningful only when
// resynth != 0).
func cPVQAlgQuantEnc(Xin []int32, n, k, spread, b int, gain int32, resynth int) (packet []byte, cm uint32, iy, Xout []int32) {
	packet = make([]byte, vqencBufBytes)
	iy = make([]int32, n)
	Xout = make([]int32, n)
	c := C.oracle_pvqenc_alg_quant(
		(*C.int32_t)(unsafe.Pointer(&Xin[0])),
		C.int(n), C.int(k), C.int(spread), C.int(b),
		C.int32_t(gain), C.int(resynth),
		(*C.uchar)(unsafe.Pointer(&packet[0])), C.int(vqencBufBytes),
		(*C.int32_t)(unsafe.Pointer(&iy[0])),
		(*C.int32_t)(unsafe.Pointer(&Xout[0])))
	return packet, uint32(c), iy, Xout
}

// cPVQEncodePulses runs the C encode_pulses over the pulse vector iy (length N, k
// pulses) and returns the finalized packet.
func cPVQEncodePulses(iy []int32, n, k int) []byte {
	packet := make([]byte, vqencBufBytes)
	C.oracle_pvqenc_encode_pulses(
		(*C.int32_t)(unsafe.Pointer(&iy[0])),
		C.int(n), C.int(k),
		(*C.uchar)(unsafe.Pointer(&packet[0])), C.int(vqencBufBytes))
	return packet
}

// cPVQIcwrs returns the C icwrs index for the pulse vector iy (length N, k
// pulses) over the ft = V(N,K) alphabet.
func cPVQIcwrs(iy []int32, n, k int, ft uint32) uint32 {
	return uint32(C.oracle_pvqenc_icwrs(
		(*C.int32_t)(unsafe.Pointer(&iy[0])),
		C.int(n), C.int(k), C.uint32_t(ft)))
}

// cPVQStereoItheta runs the C stereo_itheta over X, Y (length N) in the given
// stereo mode and returns itheta.
func cPVQStereoItheta(X, Y []int32, stereo, n int) int32 {
	return int32(C.oracle_pvqenc_stereo_itheta(
		(*C.int32_t)(unsafe.Pointer(&X[0])),
		(*C.int32_t)(unsafe.Pointer(&Y[0])),
		C.int(stereo), C.int(n)))
}
