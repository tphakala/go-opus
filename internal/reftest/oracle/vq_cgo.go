//go:build refc

package oracle

/*
#include "vq_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT PVQ codec (celt/vq.c, celt/cwrs.c)
// over plain Go-typed functions so vq_test.go can drive the pure-Go internal/celt
// decode against the C oracle without importing "C" itself. The package-level cgo
// CFLAGS live in oracle_cgo.go; this file only pulls in the vq_shim.h wrappers.

// vqBufBytes is the fixed coder buffer size used on both the encode and decode
// side. A single PVQ codeword is at most ~32 bits, but ec_enc_uint writes its raw
// low bits from the END of the buffer, so encode and decode MUST agree on the
// storage size for those raw bits to line up. 128 bytes is ample.
const vqBufBytes = 128

// cPVQAlgQuant runs the C alg_quant over Xin (length N) to produce a coded PVQ
// codeword, returning the vqBufBytes-long packet and the collapse mask.
func cPVQAlgQuant(Xin []int32, n, k, spread, b int) ([]byte, uint32) {
	buf := make([]byte, vqBufBytes)
	cm := C.oracle_pvq_alg_quant(
		(*C.int32_t)(unsafe.Pointer(&Xin[0])),
		C.int(n), C.int(k), C.int(spread), C.int(b),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(vqBufBytes))
	return buf, uint32(cm)
}

// cPVQAlgUnquant decodes buf with the C alg_unquant into a length-N vector, also
// returning the collapse mask and the decoder's tell/rng end state.
func cPVQAlgUnquant(buf []byte, n, k, spread, b int, gain int32) (Xout []int32, cm uint32, tell int, rng uint32) {
	Xout = make([]int32, n)
	var cTell C.int
	var cRng C.uint32_t
	c := C.oracle_pvq_alg_unquant(
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		C.int(n), C.int(k), C.int(spread), C.int(b), C.int32_t(gain),
		(*C.int32_t)(unsafe.Pointer(&Xout[0])), &cTell, &cRng)
	return Xout, uint32(c), int(cTell), uint32(cRng)
}

// cPVQDecodePulses decodes buf with the C decode_pulses into a length-N pulse
// vector, returning Ryy and the decoder's tell/rng end state.
func cPVQDecodePulses(buf []byte, n, k int) (y []int32, ryy int32, tell int, rng uint32) {
	y = make([]int32, n)
	var cTell C.int
	var cRng C.uint32_t
	r := C.oracle_pvq_decode_pulses(
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		C.int(n), C.int(k),
		(*C.int32_t)(unsafe.Pointer(&y[0])), &cTell, &cRng)
	return y, int32(r), int(cTell), uint32(cRng)
}

// cPVQEncodeIndex writes a raw PVQ codeword index over the ft alphabet into a
// fresh vqBufBytes packet via the C ec_enc_uint.
func cPVQEncodeIndex(index, ft uint32) []byte {
	buf := make([]byte, vqBufBytes)
	C.oracle_pvq_encode_index(C.uint32_t(index), C.uint32_t(ft),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(vqBufBytes))
	return buf
}

// cPVQExpRotation runs the C exp_rotation over a copy of Xin (length len) and
// returns the rotated buffer.
func cPVQExpRotation(Xin []int32, length, dir, stride, k, spread int) []int32 {
	Xout := make([]int32, length)
	C.oracle_pvq_exp_rotation(
		(*C.int32_t)(unsafe.Pointer(&Xin[0])), C.int(length), C.int(dir),
		C.int(stride), C.int(k), C.int(spread),
		(*C.int32_t)(unsafe.Pointer(&Xout[0])))
	return Xout
}

// cPVQRenormalise runs the C renormalise_vector over a copy of Xin (length N)
// with the given Q31 gain and returns the result.
func cPVQRenormalise(Xin []int32, n int, gain int32) []int32 {
	Xout := make([]int32, n)
	C.oracle_pvq_renormalise(
		(*C.int32_t)(unsafe.Pointer(&Xin[0])), C.int(n), C.int32_t(gain),
		(*C.int32_t)(unsafe.Pointer(&Xout[0])))
	return Xout
}

// cPVQTrueV returns V(N,K) computed by the independent recurrence in the shim, or
// 0 if not representable in 32 bits.
func cPVQTrueV(n, k int) uint32 {
	return uint32(C.oracle_true_pvq_v(C.int(n), C.int(k)))
}

// cPVQMaxK returns the table-valid pulse cap for a band of size N.
func cPVQMaxK(n int) int {
	return int(C.oracle_pvq_max_k(C.int(n)))
}
