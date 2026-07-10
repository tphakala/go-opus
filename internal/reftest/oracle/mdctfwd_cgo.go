//go:build refc

package oracle

/*
#include "mdctfwd_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus forward FFT (opus_fft / opus_fft_impl)
// and forward MDCT (clt_mdct_forward) over the frozen static mode48000_960 as
// plain Go functions so mdctfwd_test.go can compare the pure-Go internal/celt
// port against the C oracle without importing "C" itself. The package-level cgo
// CFLAGS live in oracle_cgo.go; this file only pulls in the mdctfwd_shim.h
// wrappers and never edits the shared oracle surface or the existing mdct
// oracle files.

// cFFTNFFTFwd returns the FFT length of the mode's kfft[idx] (480/240/120/60).
func cFFTNFFTFwd(idx int) int { return int(C.mdctfwd_fft_nfft(C.int(idx))) }

// cFFTFwd runs the forward opus_fft on the mode's kfft[idx]. inR/inI have length
// nfft; the returned slices have the same length.
func cFFTFwd(idx int, inR, inI []int32) (outR, outI []int32) {
	n := len(inR)
	outR = make([]int32, n)
	outI = make([]int32, n)
	C.mdctfwd_ffft(C.int(idx),
		(*C.int32_t)(unsafe.Pointer(&inR[0])),
		(*C.int32_t)(unsafe.Pointer(&inI[0])),
		(*C.int32_t)(unsafe.Pointer(&outR[0])),
		(*C.int32_t)(unsafe.Pointer(&outI[0])))
	return outR, outI
}

// cFFTImplFwd runs opus_fft_impl directly on the mode's kfft[idx] with an
// explicit downshift, exercising the per-stage fixed-point downshift path.
func cFFTImplFwd(idx int, inR, inI []int32, downshift int) (outR, outI []int32) {
	n := len(inR)
	outR = make([]int32, n)
	outI = make([]int32, n)
	C.mdctfwd_fft_impl(C.int(idx),
		(*C.int32_t)(unsafe.Pointer(&inR[0])),
		(*C.int32_t)(unsafe.Pointer(&inI[0])),
		C.int(downshift),
		(*C.int32_t)(unsafe.Pointer(&outR[0])),
		(*C.int32_t)(unsafe.Pointer(&outI[0])))
	return outR, outI
}

// cMDCTForward runs clt_mdct_forward for LM (0..3) with output stride (1 or 2),
// non-transient. in has length N2+overlap = (120<<lm)+overlap; the returned
// slice is the strided output region, stride*(120<<lm) samples long.
func cMDCTForward(lm, stride int, in []int32) []int32 {
	n2 := 120 << uint(lm)
	outlen := stride * n2
	out := make([]int32, outlen)
	C.mdctfwd_forward(C.int(lm), C.int(stride),
		(*C.int32_t)(unsafe.Pointer(&in[0])),
		(*C.int32_t)(unsafe.Pointer(&out[0])))
	return out
}
