//go:build refc

package oracle

/*
#include "mdct_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus inverse FFT (opus_ifft) and inverse MDCT
// (clt_mdct_backward) over the frozen static mode48000_960 as plain Go functions
// so mdct_test.go can compare the pure-Go internal/celt port against the C
// oracle without importing "C" itself. The package-level cgo CFLAGS live in
// oracle_cgo.go; this file only pulls in the mdct_shim.h wrappers.

// cFFTNFFT returns the FFT length of the mode's kfft[idx] (480/240/120/60).
func cFFTNFFT(idx int) int { return int(C.oracle_fft_nfft(C.int(idx))) }

// cIFFT runs opus_ifft on the mode's kfft[idx]. inR/inI have length nfft; the
// returned slices have the same length.
func cIFFT(idx int, inR, inI []int32) (outR, outI []int32) {
	n := len(inR)
	outR = make([]int32, n)
	outI = make([]int32, n)
	C.oracle_ifft(C.int(idx),
		(*C.int32_t)(unsafe.Pointer(&inR[0])),
		(*C.int32_t)(unsafe.Pointer(&inI[0])),
		(*C.int32_t)(unsafe.Pointer(&outR[0])),
		(*C.int32_t)(unsafe.Pointer(&outI[0])))
	return outR, outI
}

// cFFT runs the forward opus_fft on the mode's kfft[idx].
func cFFT(idx int, inR, inI []int32) (outR, outI []int32) {
	n := len(inR)
	outR = make([]int32, n)
	outI = make([]int32, n)
	C.oracle_ffft(C.int(idx),
		(*C.int32_t)(unsafe.Pointer(&inR[0])),
		(*C.int32_t)(unsafe.Pointer(&inI[0])),
		(*C.int32_t)(unsafe.Pointer(&outR[0])),
		(*C.int32_t)(unsafe.Pointer(&outI[0])))
	return outR, outI
}

// cFFTImpl runs opus_fft_impl directly on the mode's kfft[idx] with an explicit
// downshift, exercising the per-stage fixed-point downshift path.
func cFFTImpl(idx int, inR, inI []int32, downshift int) (outR, outI []int32) {
	n := len(inR)
	outR = make([]int32, n)
	outI = make([]int32, n)
	C.oracle_fft_impl(C.int(idx),
		(*C.int32_t)(unsafe.Pointer(&inR[0])),
		(*C.int32_t)(unsafe.Pointer(&inI[0])),
		C.int(downshift),
		(*C.int32_t)(unsafe.Pointer(&outR[0])),
		(*C.int32_t)(unsafe.Pointer(&outI[0])))
	return outR, outI
}

// cMDCTBackward runs clt_mdct_backward for LM (0..3), mono/non-transient. in has
// length N2 = 120<<lm; the returned slice is the written output region,
// overlap/2 + N2 = 60 + (120<<lm) samples long.
func cMDCTBackward(lm int, in []int32) []int32 {
	outlen := 60 + (120 << uint(lm))
	out := make([]int32, outlen)
	C.oracle_mdct_backward(C.int(lm),
		(*C.int32_t)(unsafe.Pointer(&in[0])),
		(*C.int32_t)(unsafe.Pointer(&out[0])))
	return out
}
