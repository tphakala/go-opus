//go:build refc

package oracle

/*
#include "celtenc_shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/tphakala/go-opus/internal/celt"
)

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

// ---------------------------------------------------------------------------
// CP8b: the five analysis-stage statics.
//
// Go int is 64-bit but C int is 32-bit, so every int-array parameter
// (importance / spread_weight / offsets / tf_res) is staged through a []C.int
// copy, the same way cSpreadingDecision does in bandsenc_cgo.go.
// ---------------------------------------------------------------------------

// cCeltencTfAnalysis runs tf_analysis (celt_encoder.c:663) over x (channels*N0
// celt_norm, read-only) with importance[0..len). It returns tf_select and the
// tf_res[0..len) decisions. tf_analysis is stateless.
func cCeltencTfAnalysis(length, isTransient, lambda int, x []int32, N0, LM, tfEstimate,
	tfChan int, importance []int) (tfSelect int, tfRes []int) {
	cTfRes := make([]C.int, length)
	// The C reads importance[0..len); size the staging buffer to cover that even
	// if the caller passed a shorter slice, so C can never read past Go memory.
	cImp := make([]C.int, max(len(importance), length))
	for i := range importance {
		cImp[i] = C.int(importance[i])
	}
	sel := C.oracle_celtenc_tf_analysis(
		C.int(length), C.int(isTransient), &cTfRes[0], C.int(lambda),
		(*C.int32_t)(unsafe.Pointer(&x[0])), C.int(N0), C.int(LM),
		C.int(tfEstimate), C.int(tfChan), &cImp[0])
	tfRes = make([]int, length)
	for i := range tfRes {
		tfRes[i] = int(cTfRes[i])
	}
	return int(sel), tfRes
}

// celtencTfEncodeResult holds one tf_encode (celt_encoder.c:824) outcome. tfRes
// is the array AFTER tf_encode's in-place mutation (budget-forced entries at
// :849 plus the tf_select_table remap at :859-860) -- the caller's own input
// slice is NOT touched, the mutated array comes back here. tell / rng / val are
// captured after tf_encode but BEFORE ec_enc_done.
type celtencTfEncodeResult struct {
	tfRes      []int
	tellBefore int
	tell       int
	rng        uint32
	val        uint32
	errFlag    int
}

// cCeltencTfEncode runs tf_encode over a fresh ec_enc bound to buf (buf's length
// is the coder storage, so the unsigned budget = storage*8 arithmetic is real).
// prefillBits ec_enc_bit_logp(bit, 1) calls are issued first, with bit i taken
// from bit (i&31) of prefillPat, to advance `tell` before tf_encode runs. buf is
// overwritten in place with the FINALIZED bitstream (after ec_enc_done).
func cCeltencTfEncode(start, end, isTransient int, tfRes []int, LM, tfSelect,
	prefillBits int, prefillPat uint32, buf []byte) celtencTfEncodeResult {
	// The C touches tf_res[start..end); size the staging buffer to cover that
	// even if the caller passed a shorter slice.
	cTfRes := make([]C.int, max(len(tfRes), end))
	for i := range tfRes {
		cTfRes[i] = C.int(tfRes[i])
	}
	var cTellBefore, cTell, cErr C.int
	var cRng, cVal C.uint32_t
	C.oracle_celtenc_tf_encode(
		C.int(start), C.int(end), C.int(isTransient), &cTfRes[0], C.int(LM),
		C.int(tfSelect), C.int(prefillBits), C.uint32_t(prefillPat),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)),
		&cTellBefore, &cTell, &cRng, &cVal, &cErr)
	out := celtencTfEncodeResult{
		tfRes:      make([]int, len(cTfRes)),
		tellBefore: int(cTellBefore),
		tell:       int(cTell),
		rng:        uint32(cRng),
		val:        uint32(cVal),
		errFlag:    int(cErr),
	}
	for i := range out.tfRes {
		out.tfRes[i] = int(cTfRes[i])
	}
	return out
}

// celtencDynallocResult holds one dynalloc_analysis (celt_encoder.c:1049)
// outcome. importance is only DEFINED by the C over [start,end) (the rest is
// left at the caller-supplied value, here zero); offsets is OPUS_CLEAR'd over
// [0,nbEBands) inside; spreadWeight is written over [0,end).
type celtencDynallocResult struct {
	maxDepth     int32
	totBoost     int32
	offsets      []int
	importance   []int
	spreadWeight []int
}

// cCeltencDynallocAnalysis runs dynalloc_analysis. bandLogE / bandLogE2 /
// oldBandE are channels*nbEBands celt_glog; surroundDynalloc is nbEBands
// celt_glog. logN and eBands come from the frozen 48 kHz / 960 mode inside the
// shim. There is NO qext_scale parameter (ARG_QEXT expands to nothing).
func cCeltencDynallocAnalysis(bandLogE, bandLogE2, oldBandE []int32, nbEBands, start, end,
	channels, lsbDepth, isTransient, vbr, constrainedVbr, LM, effectiveBytes, lfe int,
	surroundDynalloc []int32, toneFreq int, toneishness int32) celtencDynallocResult {
	cOffsets := make([]C.int, nbEBands)
	cImp := make([]C.int, nbEBands)
	cSW := make([]C.int, nbEBands)
	var cTotBoost C.int32_t
	maxDepth := C.oracle_celtenc_dynalloc_analysis(
		(*C.int32_t)(unsafe.Pointer(&bandLogE[0])),
		(*C.int32_t)(unsafe.Pointer(&bandLogE2[0])),
		(*C.int32_t)(unsafe.Pointer(&oldBandE[0])),
		C.int(nbEBands), C.int(start), C.int(end), C.int(channels), &cOffsets[0],
		C.int(lsbDepth), C.int(isTransient), C.int(vbr), C.int(constrainedVbr),
		C.int(LM), C.int(effectiveBytes), &cTotBoost, C.int(lfe),
		(*C.int32_t)(unsafe.Pointer(&surroundDynalloc[0])),
		&cImp[0], &cSW[0], C.int(toneFreq), C.int32_t(toneishness))
	res := celtencDynallocResult{
		maxDepth:     int32(maxDepth),
		totBoost:     int32(cTotBoost),
		offsets:      make([]int, nbEBands),
		importance:   make([]int, nbEBands),
		spreadWeight: make([]int, nbEBands),
	}
	for i := 0; i < nbEBands; i++ {
		res.offsets[i] = int(cOffsets[i])
		res.importance[i] = int(cImp[i])
		res.spreadWeight[i] = int(cSW[i])
	}
	return res
}

// cCeltencAllocTrimAnalysis runs alloc_trim_analysis (celt_encoder.c:865) over x
// (channels*N0 celt_norm) and bandLogE (channels*nbEBands celt_glog). stereoSaving
// is the in/out opus_val16 the C mutates at :919 (C==2 only); it is threaded
// through as an int32 so the Go test can drive a multi-frame sequence. Returns
// trim_index (0..10) and the updated stereo_saving.
//
// NOTE for the sequence test: with UNCORRELATED random stereo x, sum and minXC
// both collapse to ~0, so logXC2 ~= 0 and :919 degenerates to
// MIN16(stereo_saving+QCONST16(.25,8), 0) == 0 on every frame -- stereo_saving
// never moves and the sequence is vacuous. Feed CORRELATED channels (e.g.
// right = alpha*left + noise, sweeping alpha) to make stereo_saving evolve.
func cCeltencAllocTrimAnalysis(x, bandLogE []int32, end, LM, channels, N0 int,
	stereoSaving int32, tfEstimate, intensity int, surroundTrim, equivRate int32) (trimIndex int, stereoSavingOut int32) {
	cSS := C.int32_t(stereoSaving)
	trim := C.oracle_celtenc_alloc_trim_analysis(
		(*C.int32_t)(unsafe.Pointer(&x[0])),
		(*C.int32_t)(unsafe.Pointer(&bandLogE[0])),
		C.int(end), C.int(LM), C.int(channels), C.int(N0), &cSS,
		C.int(tfEstimate), C.int(intensity), C.int32_t(surroundTrim), C.int32_t(equivRate))
	return int(trim), int32(cSS)
}

// cCeltencStereoAnalysis runs stereo_analysis (celt_encoder.c:957) over x
// (2*N0 celt_norm) and returns the dual_stereo decision (0/1). Stateless.
func cCeltencStereoAnalysis(x []int32, LM, N0 int) int {
	return int(C.oracle_celtenc_stereo_analysis(
		(*C.int32_t)(unsafe.Pointer(&x[0])), C.int(LM), C.int(N0)))
}

// ---------------------------------------------------------------------------
// CP8c: compute_vbr, hysteresis_decision, and the CELT encoder handle.
// ---------------------------------------------------------------------------

// cCeltencComputeVbr runs compute_vbr (celt_encoder.c:1604) and returns the VBR
// target in 8th bits per frame. compute_vbr is a PURE function of its arguments
// (it reads no st-> field), so no handle is involved: all the cross-frame VBR
// state lives in the caller at :2435-2533. There is no enable_qext parameter in
// this build (ARG_QEXT expands to nothing). stereoSaving and tfEstimate are
// opus_val16 in C and truncate to 16 bits.
func cCeltencComputeVbr(baseTarget int32, LM int, bitrate int32, lastCodedBands, C_, intensity,
	constrainedVbr int, stereoSaving int16, totBoost int, tfEstimate int16, pitchChange int,
	maxDepth int32, lfe, hasSurroundMask int, surroundMasking, temporalVbr int32) int32 {
	return int32(C.oracle_celtenc_compute_vbr(
		C.int32_t(baseTarget), C.int(LM), C.int32_t(bitrate), C.int(lastCodedBands),
		C.int(C_), C.int(intensity), C.int(constrainedVbr), C.int(stereoSaving),
		C.int(totBoost), C.int(tfEstimate), C.int(pitchChange), C.int32_t(maxDepth),
		C.int(lfe), C.int(hasSurroundMask), C.int32_t(surroundMasking),
		C.int32_t(temporalVbr)))
}

// cCeltencHysteresisDecision runs hysteresis_decision (bands.c:46). thresholds
// and hysteresis are N opus_val16; val is truncated to opus_val16 by the shim,
// like the encoder's own (opus_val16)(equiv_rate/1000) cast at :2403.
func cCeltencHysteresisDecision(val int, thresholds, hysteresis []int16, N, prev int) int {
	return int(C.oracle_celtenc_hysteresis_decision(C.int(val),
		(*C.int16_t)(unsafe.Pointer(&thresholds[0])),
		(*C.int16_t)(unsafe.Pointer(&hysteresis[0])),
		C.int(N), C.int(prev)))
}

// celtencConfig is the full ctl set the encoder handle applies. The zero value is
// NOT valid (end must be >= 1); build it from celtencDefaultConfig.
type celtencConfig struct {
	StreamChannels   int // CELT_SET_CHANNELS: coded channels C (<= CC)
	Complexity       int // OPUS_SET_COMPLEXITY, 0..10
	VBR              int // OPUS_SET_VBR
	VBRConstraint    int // OPUS_SET_VBR_CONSTRAINT
	Bitrate          int32
	LFE              int // OPUS_SET_LFE
	ForceIntra       int // st->force_intra (see below)
	PacketLoss       int // OPUS_SET_PACKET_LOSS_PERC, 0..100
	LsbDepth         int // OPUS_SET_LSB_DEPTH, 8..24
	DisablePrefilter int // st->disable_pf (see below)
	Start            int // CELT_SET_START_BAND
	End              int // CELT_SET_END_BAND
	DisableInv       int // OPUS_SET_PHASE_INVERSION_DISABLED, 0/1
}

// celtencDefaultConfig returns the post-celt_encoder_init defaults
// (celt_encoder.c:194-221) for the given coded-channel count, so a test only has
// to override what it is sweeping.
func celtencDefaultConfig(streamChannels int) celtencConfig {
	return celtencConfig{
		StreamChannels: streamChannels,
		Complexity:     5,
		VBR:            0,
		VBRConstraint:  1,
		Bitrate:        -1, // OPUS_BITRATE_MAX
		LsbDepth:       24,
		Start:          0,
		End:            21, // mode->effEBands
	}
}

// cCeltencHandle is a live libopus CELT encoder that a test drives frame by
// frame in lockstep with the Go celt.Encoder, reading its full cross-frame state
// after every frame. Close it when done.
type cCeltencHandle struct {
	h        unsafe.Pointer
	channels int // CC
	nbEBands int
	overlap  int
}

// newCCeltencHandle creates a C CELT encoder for CC channels, in the state
// celt_encoder_init leaves it in. Apply a config with Configure before encoding.
func newCCeltencHandle(channels int) (*cCeltencHandle, error) {
	p := C.oracle_celtenc_h_create(C.int(channels))
	if p == nil {
		return nil, fmt.Errorf("oracle_celtenc_h_create(channels=%d) failed", channels)
	}
	return &cCeltencHandle{
		h:        p,
		channels: channels,
		nbEBands: cCeltencNbebands(),
		overlap:  cCeltencOverlap(),
	}, nil
}

// Configure applies the full ctl set. force_intra and disable_prefilter are
// written to the struct directly (the only ctl that reaches them,
// CELT_SET_PREDICTION, couples the two); everything else goes through the real
// opus_custom_encoder_ctl, including its validation and the OPUS_SET_BITRATE
// clamp to 750000*channels. The Go celt.Encoder setters mirror the same
// semantics.
func (e *cCeltencHandle) Configure(cfg celtencConfig) error {
	rc := C.oracle_celtenc_h_configure(e.h,
		C.int(cfg.StreamChannels), C.int(cfg.Complexity), C.int(cfg.VBR),
		C.int(cfg.VBRConstraint), C.int32_t(cfg.Bitrate), C.int(cfg.LFE),
		C.int(cfg.ForceIntra), C.int(cfg.PacketLoss), C.int(cfg.LsbDepth),
		C.int(cfg.DisablePrefilter), C.int(cfg.Start), C.int(cfg.End),
		C.int(cfg.DisableInv))
	if rc != 0 {
		return fmt.Errorf("oracle_celtenc_h_configure: ctl #%d rejected (%+v)", -int(rc), cfg)
	}
	return nil
}

// SetEnergyMask installs a copy of the surround masking curve (C*nbEBands
// celt_glog), or clears it when mask is empty. Only the multistream encoder ever
// sets it, so the frozen CELT-only path leaves it nil and the surround-masking
// block of celt_encode_with_ec is inert.
func (e *cCeltencHandle) SetEnergyMask(mask []int32) error {
	var p *C.int32_t
	if len(mask) > 0 {
		p = (*C.int32_t)(unsafe.Pointer(&mask[0]))
	}
	if rc := C.oracle_celtenc_h_set_energy_mask(e.h, p, C.int(len(mask))); rc != 0 {
		return fmt.Errorf("oracle_celtenc_h_set_energy_mask(len=%d) rejected", len(mask))
	}
	return nil
}

// Encode encodes one interleaved int16 frame (frameSize samples per PHYSICAL
// channel) into at most nbBytes bytes with enc==NULL, the path the Go driver
// takes. It returns the raw celt_encode_with_ec return value (packet length, or
// a negative OPUS_* error), the packet bytes (nil when ret<0) and st->rng.
func (e *cCeltencHandle) Encode(pcm []int16, frameSize, nbBytes int) (ret int, pkt []byte, rng uint32) {
	buf := make([]byte, nbBytes)
	var cRng C.uint32_t
	n := int(C.oracle_celtenc_h_encode(e.h,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(frameSize), C.int(nbBytes),
		(*C.uchar)(unsafe.Pointer(&buf[0])), &cRng))
	if n < 0 {
		return n, nil, uint32(cRng)
	}
	return n, buf[:n], uint32(cRng)
}

// State dumps the C encoder's persistent cross-frame state into a
// celt.EncoderState, in the canonical field order documented on that type (and
// on oracle_celtenc_h_dump). Both sides are then hashed by the SAME Go
// EncoderState.Hash, so there is no cross-language serialization risk; the test
// can also diff field by field to localize a mismatch.
func (e *cCeltencHandle) State() celt.EncoderState {
	const nScalars = 23 // ORACLE_ENC_NSCALARS
	nb := e.channels * e.nbEBands
	scalars := make([]int32, nScalars)
	inMem := make([]int32, e.channels*e.overlap)
	prefilterMem := make([]int32, e.channels*cCeltencMaxperiod())
	oldBandE := make([]int32, nb)
	oldLogE := make([]int32, nb)
	oldLogE2 := make([]int32, nb)
	energyError := make([]int32, nb)

	C.oracle_celtenc_h_dump(e.h,
		(*C.int32_t)(unsafe.Pointer(&scalars[0])),
		(*C.int32_t)(unsafe.Pointer(&inMem[0])),
		(*C.int32_t)(unsafe.Pointer(&prefilterMem[0])),
		(*C.int32_t)(unsafe.Pointer(&oldBandE[0])),
		(*C.int32_t)(unsafe.Pointer(&oldLogE[0])),
		(*C.int32_t)(unsafe.Pointer(&oldLogE2[0])),
		(*C.int32_t)(unsafe.Pointer(&energyError[0])))

	return celt.EncoderState{
		Rng:             uint32(scalars[0]),
		SpreadDecision:  int(scalars[1]),
		DelayedIntra:    scalars[2],
		TonalAverage:    int(scalars[3]),
		LastCodedBands:  int(scalars[4]),
		HfAverage:       int(scalars[5]),
		TapsetDecision:  int(scalars[6]),
		PrefilterPeriod: int(scalars[7]),
		PrefilterGain:   int16(scalars[8]),
		PrefilterTapset: int(scalars[9]),
		ConsecTransient: int(scalars[10]),
		PreemphMemE:     [2]int32{scalars[11], scalars[12]},
		PreemphMemD:     [2]int32{scalars[13], scalars[14]},
		VbrReservoir:    scalars[15],
		VbrDrift:        scalars[16],
		VbrOffset:       scalars[17],
		VbrCount:        scalars[18],
		OverlapMax:      scalars[19],
		StereoSaving:    int16(scalars[20]),
		Intensity:       int(scalars[21]),
		SpecAvg:         scalars[22],
		InMem:           inMem,
		PrefilterMem:    prefilterMem,
		OldBandE:        oldBandE,
		OldLogE:         oldLogE,
		OldLogE2:        oldLogE2,
		EnergyError:     energyError,
	}
}

// Close frees the C encoder.
func (e *cCeltencHandle) Close() {
	if e.h != nil {
		C.oracle_celtenc_h_destroy(e.h)
		e.h = nil
	}
}
