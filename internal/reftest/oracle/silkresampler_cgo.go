//go:build refc

package oracle

/*
#include "silkresampler_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus SILK resampler (silk/resampler.c and its
// private up2_HQ / IIR_FIR / down_FIR / AR2 filters over the resampler_rom.c tables)
// over plain Go-typed values so silkresampler_test.go can drive the pure-Go
// internal/silk resampler against the C oracle without importing "C" itself. The
// package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// silkresampler_shim.h.

// resamplerConfig is the scalar configuration silk_resampler_init derives.
type resamplerConfig struct {
	fsInKHz           int
	fsOutKHz          int
	batchSize         int
	invRatioQ16       int32
	firOrder          int
	firFracs          int
	resamplerFunction int
	inputDelay        int
	ret               int
}

// resamplerStateSnapshot is the persistent resampler filter state, canonicalized so it
// compares directly against the pure-Go ResamplerState (SIIR, SFIR, DelayBuf).
type resamplerStateSnapshot struct {
	sIIR     [6]int32
	sFIR     [36]int32
	delayBuf [96]int16
}

// resamplerC wraps a persistent C silk_resampler_state_struct. The struct's only
// pointer member (Coefs) points into the C ROM, never into Go memory, so holding it by
// value in a Go struct and passing &st to C on each call satisfies the cgo pointer
// rules. The filter memory inside carries across process calls, exactly as in a real
// decoding session.
type resamplerC struct {
	st C.silk_resampler_state_struct
}

// newResamplerC initializes a C resampler for the (fsIn, fsOut, forEnc) triple and
// returns the wrapper plus the derived configuration.
func newResamplerC(fsIn, fsOut int32, forEnc int) (*resamplerC, resamplerConfig) {
	r := &resamplerC{}
	var cfg C.oracle_resampler_config
	C.oracle_resampler_init(&r.st, C.int32_t(fsIn), C.int32_t(fsOut), C.int(forEnc), &cfg)
	return r, resamplerConfig{
		fsInKHz:           int(cfg.Fs_in_kHz),
		fsOutKHz:          int(cfg.Fs_out_kHz),
		batchSize:         int(cfg.batchSize),
		invRatioQ16:       int32(cfg.invRatio_Q16),
		firOrder:          int(cfg.FIR_Order),
		firFracs:          int(cfg.FIR_Fracs),
		resamplerFunction: int(cfg.resampler_function),
		inputDelay:        int(cfg.inputDelay),
		ret:               int(cfg.ret),
	}
}

// process runs one block of the C silk_resampler over in[:inLen] into out and returns
// the post-block filter state. out must have room for (inLen / Fs_in_kHz) * Fs_out_kHz
// samples; in must be at least inLen long.
func (r *resamplerC) process(out, in []int16, inLen int32) resamplerStateSnapshot {
	var st C.oracle_resampler_state
	var inPtr *C.int16_t
	if len(in) > 0 {
		inPtr = (*C.int16_t)(unsafe.Pointer(&in[0]))
	}
	C.oracle_resampler_process(&r.st,
		(*C.int16_t)(unsafe.Pointer(&out[0])), inPtr, C.int32_t(inLen), &st)

	var snap resamplerStateSnapshot
	for i := 0; i < 6; i++ {
		snap.sIIR[i] = int32(st.sIIR[i])
	}
	for i := 0; i < 36; i++ {
		snap.sFIR[i] = int32(st.sFIR[i])
	}
	for i := 0; i < 96; i++ {
		snap.delayBuf[i] = int16(st.delayBuf[i])
	}
	return snap
}
