//go:build refc

package oracle

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// This differential test pins the pure-Go SILK frame index/parameter decode
// (internal/silk decode_indices.go / decode_parameters.go / gain_quant.go /
// decode_pitch.go: DecodeIndices, DecodeParameters and their helpers) to the pinned
// libopus C oracle (silk/decode_indices.c, decode_parameters.c, gain_quant.c,
// decode_pitch.c). The Go encoder does not exist yet, so the C silk_encode_indices
// PRODUCES the side-information bitstream from a random-but-valid SideInfoIndices;
// both C and Go then DECODE the same bitstream and every output is asserted
// bit-identical: the decoded SideInfoIndices, the range-decoder end state (tell +
// rng) and the updated entropy state after decode_indices, then the decoded
// parameters (Gains_Q16, PredCoef_Q12 for both subframes, pitchL, LTPCoef_Q14,
// LTP_scale_Q14) and the updated cross-frame state (LastGainIndex, prevNLSF_Q15)
// after decode_parameters.

// silkParamsCB maps an internal rate (kHz) to the NLSF filter order it selects.
func silkParamsOrder(fsKHz int) int {
	if fsKHz == 16 {
		return 16
	}
	return 10
}

// silkParamsContourSize returns the pitch-contour codebook size (and thus the valid
// contourIndex range) for the given internal rate and subframe count, matching the
// silk_encode_indices contour-index asserts.
func silkParamsContourSize(fsKHz, nbSubfr int) int {
	if fsKHz == 8 {
		if nbSubfr == 4 {
			return 11
		}
		return 3
	}
	if nbSubfr == 4 {
		return 34
	}
	return 12
}

// genSilkParamsInput builds a random-but-valid input for the given coverage axes.
// Every index respects the range the matching ec_enc_icdf table allows, so
// silk_encode_indices never reads past a table. The VAD flag is derived from the
// signal type (the encoder uses the VAD type-offset table iff signalType >= 1), so
// the encoder and both decoders agree on the type-offset table.
func genSilkParamsInput(r *rand.Rand, fsKHz, nbSubfr, signalType, quantOffsetType, condCoding,
	firstFrame, lossCnt int) silkParamsIn {
	var in silkParamsIn
	in.fsKHz = fsKHz
	in.nbSubfr = nbSubfr
	in.condCoding = condCoding
	in.frameIndex = 0
	in.decodeLBRR = 0
	in.signalType = signalType
	in.quantOffsetType = quantOffsetType
	in.firstFrameAfterReset = firstFrame
	in.lossCnt = lossCnt
	if signalType >= 1 {
		in.vadFlag = 1
	}

	// Gains: first subframe is delta-coded [0,40] under CODE_CONDITIONALLY, else
	// the full independent index [0,63]; remaining subframes are always delta [0,40].
	if condCoding == 2 { // CODE_CONDITIONALLY
		in.gainsIndices[0] = r.Intn(41)
	} else {
		in.gainsIndices[0] = r.Intn(64)
	}
	for k := 1; k < nbSubfr; k++ {
		in.gainsIndices[k] = r.Intn(41)
	}
	in.lastGainIndex = r.Intn(64)

	// NLSFs: CB1 index [0,32); residual indices [-10,10] to exercise the NLSF_EXT
	// paths (|value| >= NLSF_QUANT_MAX_AMPLITUDE == 4).
	order := silkParamsOrder(fsKHz)
	in.nlsfIndices[0] = r.Intn(32)
	for i := 1; i <= order; i++ {
		in.nlsfIndices[i] = r.Intn(21) - 10
	}
	if nbSubfr == 4 {
		in.nlsfInterpCoef = r.Intn(5) // 0..4; < 4 interpolates, == 4 copies
	} else {
		in.nlsfInterpCoef = 4 // not coded for 10 ms frames
	}
	// Previous NLSF vector: a sorted in-range ramp (only read when interpolation is
	// active). NLSF2A is deterministic on both sides regardless of the exact values.
	for i := 0; i < order; i++ {
		in.prevNLSFQ15[i] = int16((i + 1) * 32767 / (order + 1))
	}

	in.ecPrevSignalType = 0
	in.ecPrevLagIndex = 0
	if signalType == 2 { // TYPE_VOICED: pitch + LTP fields
		half := fsKHz / 2
		in.lagIndex = r.Intn(32 * half) // pitch_high_bits = lagIndex/half < 32
		in.contourIndex = r.Intn(silkParamsContourSize(fsKHz, nbSubfr))
		in.perIndex = r.Intn(3)
		for k := 0; k < nbSubfr; k++ {
			in.ltpIndex[k] = r.Intn(8 << in.perIndex)
		}
		if condCoding == 0 { // CODE_INDEPENDENTLY encodes LTP scaling
			in.ltpScaleIndex = r.Intn(3)
		}
		// Sometimes make the previous frame voiced with a nearby lag so the
		// CODE_CONDITIONALLY delta-lag path is exercised; otherwise absolute.
		if condCoding == 2 && r.Intn(2) == 0 {
			in.ecPrevSignalType = 2 // TYPE_VOICED
			delta := r.Intn(20) - 8 // [-8,11] -> encoder delta-codes
			pl := in.lagIndex - delta
			if pl < 0 {
				pl = 0
			}
			in.ecPrevLagIndex = pl
		}
	}
	in.seed = r.Intn(4)
	return in
}

// silkParamsMismatch compares the pure-Go decode (indices snapshot after
// DecodeIndices, control after DecodeParameters, plus end/cross-frame state)
// against the C oracle output field-by-field and returns a description of the first
// mismatch, or "" when bit-identical.
func silkParamsMismatch(gi *silk.SideInfoIndices, ctrl *silk.DecoderControl,
	goTell int, goRng uint32, goEcSig int, goEcLag int16, goLastGain int8,
	goPrevNLSF *[16]int16, order int, out *silkParamsOut) string {
	// Range-decoder + entropy state after decode_indices.
	if goTell != out.tell {
		return fmt.Sprintf("tell Go=%d C=%d", goTell, out.tell)
	}
	if goRng != out.rng {
		return fmt.Sprintf("rng Go=%d C=%d", goRng, out.rng)
	}
	if goEcSig != out.ecPrevSignalType {
		return fmt.Sprintf("ec_prevSignalType Go=%d C=%d", goEcSig, out.ecPrevSignalType)
	}
	if int(goEcLag) != out.ecPrevLagIndex {
		return fmt.Sprintf("ec_prevLagIndex Go=%d C=%d", goEcLag, out.ecPrevLagIndex)
	}

	// Decoded SideInfoIndices (snapshot after decode_indices).
	if int(gi.SignalType) != out.signalType {
		return fmt.Sprintf("signalType Go=%d C=%d", gi.SignalType, out.signalType)
	}
	if int(gi.QuantOffsetType) != out.quantOffsetType {
		return fmt.Sprintf("quantOffsetType Go=%d C=%d", gi.QuantOffsetType, out.quantOffsetType)
	}
	for k := 0; k < 4; k++ {
		if int(gi.GainsIndices[k]) != out.gainsIndices[k] {
			return fmt.Sprintf("GainsIndices[%d] Go=%d C=%d", k, gi.GainsIndices[k], out.gainsIndices[k])
		}
		if int(gi.LTPIndex[k]) != out.ltpIndex[k] {
			return fmt.Sprintf("LTPIndex[%d] Go=%d C=%d", k, gi.LTPIndex[k], out.ltpIndex[k])
		}
	}
	for i := 0; i < 17; i++ {
		if int(gi.NLSFIndices[i]) != out.nlsfIndices[i] {
			return fmt.Sprintf("NLSFIndices[%d] Go=%d C=%d", i, gi.NLSFIndices[i], out.nlsfIndices[i])
		}
	}
	if int(gi.LagIndex) != out.lagIndex {
		return fmt.Sprintf("lagIndex Go=%d C=%d", gi.LagIndex, out.lagIndex)
	}
	if int(gi.ContourIndex) != out.contourIndex {
		return fmt.Sprintf("contourIndex Go=%d C=%d", gi.ContourIndex, out.contourIndex)
	}
	if int(gi.NLSFInterpCoefQ2) != out.nlsfInterpCoef {
		return fmt.Sprintf("NLSFInterpCoef_Q2 Go=%d C=%d", gi.NLSFInterpCoefQ2, out.nlsfInterpCoef)
	}
	if int(gi.PERIndex) != out.perIndex {
		return fmt.Sprintf("PERIndex Go=%d C=%d", gi.PERIndex, out.perIndex)
	}
	if int(gi.LTPScaleIndex) != out.ltpScaleIndex {
		return fmt.Sprintf("LTP_scaleIndex Go=%d C=%d", gi.LTPScaleIndex, out.ltpScaleIndex)
	}
	if int(gi.Seed) != out.seed {
		return fmt.Sprintf("Seed Go=%d C=%d", gi.Seed, out.seed)
	}

	// Decoded parameters (after decode_parameters).
	for k := 0; k < 4; k++ {
		if ctrl.PitchL[k] != out.pitchL[k] {
			return fmt.Sprintf("pitchL[%d] Go=%d C=%d", k, ctrl.PitchL[k], out.pitchL[k])
		}
		if ctrl.GainsQ16[k] != out.gainsQ16[k] {
			return fmt.Sprintf("Gains_Q16[%d] Go=%d C=%d", k, ctrl.GainsQ16[k], out.gainsQ16[k])
		}
	}
	for h := 0; h < 2; h++ {
		for i := 0; i < 16; i++ {
			if ctrl.PredCoefQ12[h][i] != out.predCoefQ12[h][i] {
				return fmt.Sprintf("PredCoef_Q12[%d][%d] Go=%d C=%d", h, i, ctrl.PredCoefQ12[h][i], out.predCoefQ12[h][i])
			}
		}
	}
	for i := 0; i < 20; i++ {
		if ctrl.LTPCoefQ14[i] != out.ltpCoefQ14[i] {
			return fmt.Sprintf("LTPCoef_Q14[%d] Go=%d C=%d", i, ctrl.LTPCoefQ14[i], out.ltpCoefQ14[i])
		}
	}
	if ctrl.LTPScaleQ14 != out.ltpScaleQ14 {
		return fmt.Sprintf("LTP_scale_Q14 Go=%d C=%d", ctrl.LTPScaleQ14, out.ltpScaleQ14)
	}

	// Cross-frame state updated by decode_parameters.
	if int(goLastGain) != out.lastGainIndex {
		return fmt.Sprintf("LastGainIndex Go=%d C=%d", goLastGain, out.lastGainIndex)
	}
	for i := 0; i < order; i++ {
		if goPrevNLSF[i] != out.prevNLSFQ15[i] {
			return fmt.Sprintf("prevNLSF_Q15[%d] Go=%d C=%d", i, goPrevNLSF[i], out.prevNLSFQ15[i])
		}
	}
	return ""
}

// runSilkParamsGo configures a Go decoder state from the input, decodes the shared
// buffer with the pure-Go DecodeIndices + DecodeParameters, and returns the decoded
// indices snapshot (after DecodeIndices), the control, the range-decoder end state,
// the updated entropy state and the updated cross-frame state.
func runSilkParamsGo(in *silkParamsIn, buf []byte) (silk.SideInfoIndices, silk.DecoderControl,
	int, uint32, int, int16, int8, [16]int16) {
	var psDec silk.DecoderState
	psDec.DecoderSetFs(in.fsKHz, in.nbSubfr)
	psDec.ECPrevSignalType = in.ecPrevSignalType
	psDec.ECPrevLagIndex = int16(in.ecPrevLagIndex)
	psDec.VADFlags[in.frameIndex] = in.vadFlag
	psDec.LastGainIndex = int8(in.lastGainIndex)
	psDec.FirstFrameAfterReset = in.firstFrameAfterReset
	psDec.LossCnt = in.lossCnt
	for i := 0; i < psDec.LPCOrder; i++ {
		psDec.PrevNLSFQ15[i] = in.prevNLSFQ15[i]
	}

	var dec rangecoding.Decoder
	dec.Init(buf)
	silk.DecodeIndices(&psDec, &dec, in.frameIndex, in.decodeLBRR, in.condCoding)
	goIdx := psDec.Indices
	goTell := dec.Tell()
	goRng := dec.Rng()
	goEcSig := psDec.ECPrevSignalType
	goEcLag := psDec.ECPrevLagIndex

	var ctrl silk.DecoderControl
	silk.DecodeParameters(&psDec, &ctrl, in.condCoding)
	return goIdx, ctrl, goTell, goRng, goEcSig, goEcLag, psDec.LastGainIndex, psDec.PrevNLSFQ15
}

// TestSilkDecodeParamsMatchesC is the primary check: across every internal rate,
// frame length, signal type, quantizer offset, conditional-coding mode, first-frame
// and packet-loss combination, the C silk_encode_indices produces a bitstream, and
// the pure-Go decode of that bitstream matches the C decode bit-for-bit on every
// index, parameter and end/cross-frame state field. The coverage flags assert the
// interesting branches (voiced/unvoiced/inactive, interpolation on/off, packet
// loss, conditional gains, first frame after reset) actually ran.
func TestSilkDecodeParamsMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC0A))
	var (
		cases                               int
		voiced, unvoiced, inactive          bool
		interpOn, interpOff                 bool
		lossSeen, condGains, firstFrameSeen bool
		nb10, nb20                          bool
		rateSeen                            = map[int]bool{}
	)

	fsRates := []int{8, 12, 16}
	nbSubfrs := []int{2, 4}
	signalTypes := []int{0, 1, 2}
	quantOffsets := []int{0, 1}
	condCodings := []int{0, 1, 2} // INDEPENDENTLY / _NO_LTP_SCALING / CONDITIONALLY

	for _, fsKHz := range fsRates {
		for _, nbSubfr := range nbSubfrs {
			for _, signalType := range signalTypes {
				for _, quantOffsetType := range quantOffsets {
					for _, condCoding := range condCodings {
						for _, firstFrame := range []int{0, 1} {
							for _, lossCnt := range []int{0, 1} {
								for trial := 0; trial < 3; trial++ {
									in := genSilkParamsInput(r, fsKHz, nbSubfr, signalType,
										quantOffsetType, condCoding, firstFrame, lossCnt)
									buf, out := cSilkParamsRun(&in)
									if out.nBytes <= 0 {
										t.Fatalf("fs=%d nb=%d sig=%d: encoder produced %d bytes",
											fsKHz, nbSubfr, signalType, out.nBytes)
									}

									gi, ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain, goPrevNLSF :=
										runSilkParamsGo(&in, buf)
									order := silkParamsOrder(fsKHz)
									if msg := silkParamsMismatch(&gi, &ctrl, goTell, goRng, goEcSig,
										goEcLag, goLastGain, &goPrevNLSF, order, &out); msg != "" {
										t.Fatalf("fs=%d nb=%d sig=%d qoff=%d cond=%d first=%d loss=%d: %s",
											fsKHz, nbSubfr, signalType, quantOffsetType, condCoding,
											firstFrame, lossCnt, msg)
									}

									// Coverage bookkeeping.
									switch signalType {
									case 0:
										inactive = true
									case 1:
										unvoiced = true
									case 2:
										voiced = true
									}
									if nbSubfr == 4 {
										nb20 = true
										if out.nlsfInterpCoef < 4 {
											interpOn = true
										} else {
											interpOff = true
										}
									} else {
										nb10 = true
									}
									if lossCnt != 0 {
										lossSeen = true
									}
									if condCoding == 2 {
										condGains = true
									}
									if firstFrame != 0 {
										firstFrameSeen = true
									}
									rateSeen[fsKHz] = true
									cases++
								}
							}
						}
					}
				}
			}
		}
	}

	for _, want := range []struct {
		ok   bool
		name string
	}{
		{voiced, "voiced"}, {unvoiced, "unvoiced"}, {inactive, "inactive"},
		{interpOn, "NLSF interpolation on"}, {interpOff, "NLSF interpolation off"},
		{lossSeen, "packet loss (BWE)"}, {condGains, "conditional gains"},
		{firstFrameSeen, "first frame after reset"},
		{nb10, "10 ms frames"}, {nb20, "20 ms frames"},
		{rateSeen[8], "NB (8 kHz)"}, {rateSeen[12], "MB (12 kHz)"}, {rateSeen[16], "WB (16 kHz)"},
	} {
		if !want.ok {
			t.Fatalf("coverage gap: %s never exercised", want.name)
		}
	}
	t.Logf("silk_decode_indices/parameters differential cases: %d", cases)
}

// TestSilkDecodeParamsMutation is the non-vacuity check: decoding the same
// bitstream under the WRONG conditional-coding mode reparses the gains (and LTP
// scaling), and decoding under the WRONG frame length reparses everything, so the
// Go decode under a mutated parameter must diverge from the correct C decode in at
// least some case. The correct decode is asserted to match first so the test cannot
// pass vacuously.
func TestSilkDecodeParamsMutation(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC0B))
	condMutationCaught := false
	byteMutationCaught := false
	for trial := 0; trial < 64; trial++ {
		fsKHz := []int{8, 12, 16}[trial%3]
		in := genSilkParamsInput(r, fsKHz, 4, 2 /*voiced*/, trial&1, 0 /*CODE_INDEPENDENTLY*/, 0, 0)
		buf, out := cSilkParamsRun(&in)
		if out.nBytes <= 0 {
			t.Fatalf("trial %d: encoder produced %d bytes", trial, out.nBytes)
		}

		// Baseline: the correct decode must match (guards against a vacuous test).
		gi, ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain, goPrevNLSF := runSilkParamsGo(&in, buf)
		order := silkParamsOrder(fsKHz)
		if msg := silkParamsMismatch(&gi, &ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain,
			&goPrevNLSF, order, &out); msg != "" {
			t.Fatalf("trial %d: baseline Go decode diverged from C: %s", trial, msg)
		}

		// Mutation A: wrong conditional-coding mode (CONDITIONALLY delta-codes the
		// first gain and suppresses LTP scaling), so the parse must diverge.
		mut := in
		mut.condCoding = 2 // CODE_CONDITIONALLY
		mgi, mctrl, mTell, mRng, mEcSig, mEcLag, mLastGain, mPrevNLSF := runSilkParamsGo(&mut, buf)
		if silkParamsMismatch(&mgi, &mctrl, mTell, mRng, mEcSig, mEcLag, mLastGain,
			&mPrevNLSF, order, &out) != "" {
			condMutationCaught = true
		}

		// Mutation B: flip a payload byte and confirm the Go decode diverges from the
		// (unmutated) C decode.
		if out.nBytes > 0 {
			mbuf := append([]byte(nil), buf...)
			mbuf[0] ^= 0xFF
			bgi, bctrl, bTell, bRng, bEcSig, bEcLag, bLastGain, bPrevNLSF := runSilkParamsGo(&in, mbuf)
			if silkParamsMismatch(&bgi, &bctrl, bTell, bRng, bEcSig, bEcLag, bLastGain,
				&bPrevNLSF, order, &out) != "" {
				byteMutationCaught = true
			}
		}
	}
	if !condMutationCaught {
		t.Fatal("wrong conditional-coding mode never diverged; the differential assertion may be vacuous")
	}
	if !byteMutationCaught {
		t.Fatal("payload-byte mutation never diverged; the differential assertion may be vacuous")
	}
}

// TestSilkDecodeParamsComparatorCatchesPerturbation is a harness self-check: a
// single-field perturbation of a correct C output must be flagged by the
// comparator, proving the bit-exact assertions are not vacuous.
func TestSilkDecodeParamsComparatorCatchesPerturbation(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC0C))
	in := genSilkParamsInput(r, 16, 4, 2 /*voiced*/, 0, 0, 0, 0)
	buf, out := cSilkParamsRun(&in)
	gi, ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain, goPrevNLSF := runSilkParamsGo(&in, buf)
	if msg := silkParamsMismatch(&gi, &ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain,
		&goPrevNLSF, 16, &out); msg != "" {
		t.Fatalf("baseline decode diverged: %s", msg)
	}
	// Perturb one output field and confirm the comparator flags it.
	mut := out
	mut.gainsQ16[0]++
	if silkParamsMismatch(&gi, &ctrl, goTell, goRng, goEcSig, goEcLag, goLastGain,
		&goPrevNLSF, 16, &mut) == "" {
		t.Fatal("comparator failed to detect a one-field perturbation")
	}
}
