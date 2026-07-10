//go:build refc

package oracle

/*
#include "celtenc_shim.h"
*/
import "C"

import "unsafe"

// This file exposes the pinned libopus CELT encoder FRONT-HALF stage functions
// (celt/celt_encoder.c: celt_preemphasis, transient_analysis,
// patch_transient_decision, compute_mdcts, tone_detect, run_prefilter) over
// plain Go-typed functions so celtenc_test.go can drive the pure-Go
// internal/celt encoder stages against the C oracle without importing "C" itself.
// The package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// celtenc_shim.h (the SOLE translation unit that compiles celt_encoder.c). It
// does not edit any shared oracle file.

// Mode geometry getters (the frozen 48 kHz / 960 CELT mode).
func cCeltencOverlap() int   { return int(C.oracle_celtenc_overlap()) }
func cCeltencNbebands() int  { return int(C.oracle_celtenc_nbebands()) }
func cCeltencMaxperiod() int { return int(C.oracle_celtenc_maxperiod()) }
func cCeltencShortmdct() int { return int(C.oracle_celtenc_shortmdct()) }

// cCeltencPreemphasis pre-emphasizes one channel of interleaved int16 PCM
// (stride CC) into N celt_sig, threading the filter memory. Returns the output
// and the updated memory.
func cCeltencPreemphasis(pcm []int16, N, CC, upsample, clip int, mem int32) ([]int32, int32) {
	inp := make([]int32, N)
	cMem := C.int32_t(mem)
	C.oracle_celtenc_preemphasis(
		(*C.int16_t)(unsafe.Pointer(&pcm[0])),
		(*C.int32_t)(unsafe.Pointer(&inp[0])),
		C.int(N), C.int(CC), C.int(upsample), C.int(clip), &cMem)
	return inp, int32(cMem)
}

// cCeltencTransient runs transient_analysis over in (C*length celt_sig) and
// returns is_transient, tf_estimate, tf_chan and the weak_transient flag.
func cCeltencTransient(in []int32, length, channels, allowWeak, toneFreq int, toneishness int32) (isTransient int, tfEstimate int32, tfChan, weak int) {
	var cTf C.int32_t
	var cChan, cWeak C.int
	is := C.oracle_celtenc_transient(
		(*C.int32_t)(unsafe.Pointer(&in[0])), C.int(length), C.int(channels),
		C.int(allowWeak), C.int(toneFreq), C.int32_t(toneishness),
		&cTf, &cChan, &cWeak)
	return int(is), int32(cTf), int(cChan), int(cWeak)
}

// cCeltencPatchTransient runs patch_transient_decision and returns 0/1.
func cCeltencPatchTransient(newE, oldE []int32, nbEBands, start, end, channels int) int {
	return int(C.oracle_celtenc_patch_transient(
		(*C.int32_t)(unsafe.Pointer(&newE[0])),
		(*C.int32_t)(unsafe.Pointer(&oldE[0])),
		C.int(nbEBands), C.int(start), C.int(end), C.int(channels)))
}

// cCeltencComputeMdcts runs compute_mdcts over in (CC*(N+overlap) celt_sig) and
// returns the outLen (CC*N) interleaved frequency-domain output.
func cCeltencComputeMdcts(shortBlocks int, in []int32, channels, CC, LM, upsample, outLen int) []int32 {
	out := make([]int32, outLen)
	C.oracle_celtenc_compute_mdcts(C.int(shortBlocks),
		(*C.int32_t)(unsafe.Pointer(&in[0])), (*C.int32_t)(unsafe.Pointer(&out[0])),
		C.int(channels), C.int(CC), C.int(LM), C.int(upsample))
	return out
}

// cCeltencToneDetect runs tone_detect over in (CC*N celt_sig) and returns the
// tone frequency and the toneishness.
func cCeltencToneDetect(in []int32, CC, N int) (int, int32) {
	var cTone C.int32_t
	f := C.oracle_celtenc_tone_detect((*C.int32_t)(unsafe.Pointer(&in[0])),
		C.int(CC), C.int(N), &cTone)
	return int(f), int32(cTone)
}

// cCeltencRemoveDoubling forwards to remove_doubling. x is read-only; returns the
// pitch gain and the refined *T0.
func cCeltencRemoveDoubling(x []int16, maxperiod, minperiod, N, T0, prevPeriod, prevGain int) (pg, t0Out int) {
	cT0 := C.int(T0)
	p := C.oracle_celtenc_remove_doubling(
		(*C.int16_t)(unsafe.Pointer(&x[0])), C.int(maxperiod), C.int(minperiod), C.int(N),
		&cT0, C.int(prevPeriod), C.int(prevGain))
	return int(p), int(cT0)
}

// cCeltencRunPrefilter drives run_prefilter with a fresh CELTEncoder seeded to
// the given previous-frame state. in / inMem / prefilterMem are updated in place.
// Returns pitch, gain, qgain, pf_on and the (mutated) prefilter period.
func cCeltencRunPrefilter(channels, N, complexity, lossRate, prefilterPeriod, prefilterGain, prefilterTapset,
	prefilterTapsetIn, enabled, tfEstimate, nbAvailableBytes, toneFreq int, toneishness int32,
	in, inMem, prefilterMem []int32) (pitch, gain, qgain, pfOn, prefilterPeriodOut int) {
	var cPitch, cGain, cQg, cPeriodOut C.int
	pf := C.oracle_celtenc_run_prefilter(
		C.int(channels), C.int(N), C.int(complexity), C.int(lossRate),
		C.int(prefilterPeriod), C.int(prefilterGain), C.int(prefilterTapset),
		C.int(prefilterTapsetIn), C.int(enabled), C.int(tfEstimate), C.int(nbAvailableBytes),
		C.int(toneFreq), C.int32_t(toneishness),
		(*C.int32_t)(unsafe.Pointer(&in[0])),
		(*C.int32_t)(unsafe.Pointer(&inMem[0])),
		(*C.int32_t)(unsafe.Pointer(&prefilterMem[0])),
		&cPitch, &cGain, &cQg, &cPeriodOut)
	return int(cPitch), int(cGain), int(cQg), int(pf), int(cPeriodOut)
}
