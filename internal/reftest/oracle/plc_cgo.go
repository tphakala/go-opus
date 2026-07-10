//go:build refc

package oracle

/*
#include "plc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT PLC math primitives (celt/pitch.c,
// celt/celt_lpc.c) and a raw-CELT decoder start-band setter as plain Go-typed
// functions, so plc_test.go can pin the pure-Go internal/celt PLC ports against
// the C oracle. Package-level cgo CFLAGS live in oracle_cgo.go; this file only
// pulls in plc_shim.h. It never touches the shared oracle surface.

// i16Ptr returns a C pointer to s[0], or nil for an empty slice.
func i16Ptr(s []int16) *C.int16_t {
	if len(s) == 0 {
		return nil
	}
	return (*C.int16_t)(unsafe.Pointer(&s[0]))
}

// i32Ptr returns a C pointer to s[0], or nil for an empty slice.
func i32Ptr(s []int32) *C.int32_t {
	if len(s) == 0 {
		return nil
	}
	return (*C.int32_t)(unsafe.Pointer(&s[0]))
}

// cCeltLPC is _celt_lpc: ac has p+1 entries, returns p coefficients.
func cCeltLPC(ac []int32, p int) []int16 {
	lpc := make([]int16, p)
	C.oracle_celt_lpc(i32Ptr(ac), C.int(p), i16Ptr(lpc))
	return lpc
}

// cCeltFir is celt_fir_c: xfull holds ord history samples then N samples
// (length N+ord); returns N filtered outputs.
func cCeltFir(xfull []int16, N, ord int, num []int16) []int16 {
	y := make([]int16, N)
	C.oracle_celt_fir(i16Ptr(xfull), C.int(N), C.int(ord), i16Ptr(num), i16Ptr(y))
	return y
}

// cCeltIir is celt_iir: x length N; den,mem length ord (ord a multiple of 4).
// Returns the N outputs and the updated mem (a fresh copy).
func cCeltIir(x []int32, den []int16, N, ord int, mem []int16) (y []int32, memOut []int16) {
	y = make([]int32, N)
	memOut = append([]int16(nil), mem...)
	C.oracle_celt_iir(i32Ptr(x), i16Ptr(den), i32Ptr(y), C.int(N), C.int(ord), i16Ptr(memOut))
	return y, memOut
}

// cCeltAutocorr is _celt_autocorr: returns the lag+1 autocorrelation values and
// the scaling shift. window may be nil (with overlap 0).
func cCeltAutocorr(x []int16, window []int16, overlap, lag, n int) (ac []int32, shift int) {
	ac = make([]int32, lag+1)
	shift = int(C.oracle_celt_autocorr(i16Ptr(x), i32Ptr(ac), i16Ptr(window),
		C.int(overlap), C.int(lag), C.int(n)))
	return ac, shift
}

// cPitchXcorr is celt_pitch_xcorr_c: returns maxPitch cross-correlations and the
// running max.
func cPitchXcorr(x, y []int16, len_, maxPitch int) (xcorr []int32, maxcorr int32) {
	xcorr = make([]int32, maxPitch)
	maxcorr = int32(C.oracle_pitch_xcorr(i16Ptr(x), i16Ptr(y), i32Ptr(xcorr),
		C.int(len_), C.int(maxPitch)))
	return xcorr, maxcorr
}

// cPitchDownsample is pitch_downsample: x0/x1 are celt_sig buffers of length
// len_*factor (x1 nil for mono). Returns len_ downsampled samples.
func cPitchDownsample(x0, x1 []int32, len_, C_, factor int) []int16 {
	xLp := make([]int16, len_)
	C.oracle_pitch_downsample(i32Ptr(x0), i32Ptr(x1), i16Ptr(xLp),
		C.int(len_), C.int(C_), C.int(factor))
	return xLp
}

// cPitchSearch is pitch_search: returns the estimated pitch lag.
func cPitchSearch(xLp, y []int16, len_, maxPitch int) int {
	return int(C.oracle_pitch_search(i16Ptr(xLp), i16Ptr(y), C.int(len_), C.int(maxPitch)))
}

// cCELTDecoderSetStart sets the raw CELT decoder start band (CELT_SET_START_BAND).
func (d *cCELTDecoder) SetStartBand(start int) {
	C.oracle_celtdec_set_start(d.dec, C.int(start))
}
