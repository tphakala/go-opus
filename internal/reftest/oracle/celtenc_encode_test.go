//go:build refc

package oracle

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// CP8c wave 3: the differential test for celt_encode_with_ec
// (celt/celt_encoder.c:1725-2832), i.e. the WHOLE CELT encoder.
//
// This is the gate for the checkpoint, so it is deliberately paranoid:
//
//   - It drives a Go celt.Encoder and a LIVE libopus CELTEncoder (the handle in
//     celtenc_shim.h) in LOCKSTEP over multi-frame sequences. Cross-frame state
//     (the VBR reservoir/drift/offset/count, the prefilter memory, oldBandE /
//     oldLogE / oldLogE2 / energyError, spec_avg, stereo_saving, intensity,
//     consec_transient, delayedIntra, the spread/tapset hysteresis) is precisely
//     what a single-frame test cannot see, so single frames are never used.
//   - After EVERY frame it compares the return value, the packet LENGTH, EVERY
//     PACKET BYTE, st->rng and the full encoder state FIELD BY FIELD. Byte-for-
//     byte packet equality is the gate; the state hash is only a backstop. The
//     symbol order IS the bitstream: if it were wrong, every byte after the first
//     divergence would be wrong, so the byte index of the first mismatch is what
//     localizes the bug (see the symbol-order table at the top of
//     internal/celt/celt_encoder_encode.go).
//   - NON-VACUITY GUARDS: every documented branch of the driver is tracked in a
//     coverage set, and the test FAILS if any of them never fired. A green test
//     that never coded a silence bit, never turned the prefilter on, never
//     reserved an anti-collapse bit and never shrank the coder in VBR would be
//     worthless.

// --- signal generators -------------------------------------------------------

// sigKind selects a per-frame signal. The generators are deterministic given the
// seed, so a failure is exactly reproducible.
type sigKind int

const (
	sigSilence   sigKind = iota // all zeros -> silence==1 (:1973)
	sigNoise                    // white noise -> no pitch, no transient
	sigQuietNois                // very low-level noise (non-zero, but tiny)
	sigTone                     // strong periodic tone -> prefilter on, tonal
	sigLowTone                  // low-frequency tone
	sigTransient                // quiet noise with a hard burst -> transient
	sigStereoCor                // correlated stereo (drives stereo_saving)
	sigStereoUnc                // uncorrelated stereo (drives dual_stereo)
	sigChirp                    // sweep, keeps the energy moving between frames
	sigSoftT1                   // graded transients: mask_metric lands in the
	sigSoftT2                   // 200..600 window that transient_analysis calls a
	sigSoftT3                   // "weak transient" (:2050) for at least one of these
	sigSoftT4
)

// softBurstAmp is the burst amplitude of the graded sigSoftT* transients. The
// weak-transient branch (:2050) needs 200 < mask_metric < 600, which is a narrow
// window between "no transient" and "real transient"; sweeping the burst height
// over these four values brackets it.
var softBurstAmp = map[sigKind]float64{
	sigSoftT1: 400,
	sigSoftT2: 900,
	sigSoftT3: 1800,
	sigSoftT4: 3500,
}

// genFrame builds one interleaved CC-channel frame of frameSize samples per
// channel. t0 is the absolute sample index of the frame start so the periodic
// generators stay phase-continuous across a sequence.
func genFrame(kind sigKind, rng *rand.Rand, CC, frameSize, t0, frameIdx int) []int16 {
	pcm := make([]int16, CC*frameSize)
	switch kind {
	case sigSilence:
		// all zeros
	case sigNoise:
		for i := range pcm {
			pcm[i] = int16(rng.Intn(20001) - 10000)
		}
	case sigQuietNois:
		for i := range pcm {
			pcm[i] = int16(rng.Intn(7) - 3)
		}
	case sigTone:
		for n := 0; n < frameSize; n++ {
			ph := 2 * math.Pi * 440.0 * float64(t0+n) / 48000.0
			v := int16(9000 * math.Sin(ph))
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = v
			}
		}
	case sigLowTone:
		for n := 0; n < frameSize; n++ {
			ph := 2 * math.Pi * 90.0 * float64(t0+n) / 48000.0
			v := int16(12000 * math.Sin(ph))
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = v
			}
		}
	case sigTransient:
		burst := frameSize / 2
		for n := 0; n < frameSize; n++ {
			amp := 40.0
			if n >= burst && n < burst+frameSize/8 {
				amp = 20000.0
			}
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = int16(amp * (2*rng.Float64() - 1))
			}
		}
	case sigStereoCor:
		// right = alpha*left + a little noise: makes logXC2 (alloc_trim_analysis
		// :919) non-degenerate so st->stereo_saving actually evolves.
		alpha := 0.9 - 0.1*float64(frameIdx%5)
		for n := 0; n < frameSize; n++ {
			l := 8000 * math.Sin(2*math.Pi*300.0*float64(t0+n)/48000.0)
			l += 2000 * (2*rng.Float64() - 1)
			r := alpha*l + 500*(2*rng.Float64()-1)
			pcm[CC*n] = int16(l)
			if CC == 2 {
				pcm[CC*n+1] = int16(r)
			}
		}
	case sigStereoUnc:
		for n := 0; n < frameSize; n++ {
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = int16(rng.Intn(16001) - 8000)
			}
		}
	case sigChirp:
		for n := 0; n < frameSize; n++ {
			tt := float64(t0+n) / 48000.0
			f := 200.0 + 3000.0*math.Mod(tt, 0.5)/0.5
			v := int16(11000 * math.Sin(2*math.Pi*f*tt))
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = v
			}
		}
	case sigSoftT1, sigSoftT2, sigSoftT3, sigSoftT4:
		// A soft step: a steady tone whose amplitude jumps partway through the
		// frame. The jump height selects where mask_metric lands.
		hi := softBurstAmp[kind]
		burst := frameSize * 5 / 8
		for n := 0; n < frameSize; n++ {
			amp := 150.0
			if n >= burst {
				amp = hi
			}
			v := int16(amp * math.Sin(2*math.Pi*1200.0*float64(t0+n)/48000.0))
			for c := 0; c < CC; c++ {
				pcm[CC*n+c] = v
			}
		}
	}
	return pcm
}

// --- coverage ----------------------------------------------------------------

// encCoverage is the non-vacuity ledger. Every entry names a documented branch of
// celt_encode_with_ec; the test fails if any of them never fired across the whole
// sweep.
type encCoverage struct {
	m map[string]int
}

func newEncCoverage() *encCoverage { return &encCoverage{m: map[string]int{}} }

func (cv *encCoverage) hit(name string) { cv.m[name]++ }

func (cv *encCoverage) hitIf(cond bool, name string) {
	if cond {
		cv.m[name]++
	}
}

// require fails the test if any of the named branches never fired.
func (cv *encCoverage) require(t *testing.T, names ...string) {
	t.Helper()
	var missing []string
	for _, n := range names {
		if cv.m[n] == 0 {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("NON-VACUITY FAILURE: these documented branches never fired: %v\nfired: %v",
			missing, cv.m)
	}
}

// record folds one frame's witness into the ledger.
func (cv *encCoverage) record(w celt.EncodeWitness, cfg encCfg) {
	cv.hitIf(w.Silence, "sawSilence")
	cv.hitIf(!w.Silence, "sawNonSilence")
	cv.hitIf(w.IsTransient, "sawTransient")
	cv.hitIf(!w.IsTransient, "sawNonTransient")
	cv.hitIf(w.WeakTransient, "sawWeakTransient")
	cv.hitIf(w.TransientGotDisabled, "sawTransientGotDisabled")
	cv.hitIf(w.SecondMdct, "sawSecondMdct")
	cv.hitIf(w.PatchedTransient, "sawPatchedTransient")
	cv.hitIf(w.PfEnabled, "sawPfEnabled")
	cv.hitIf(w.PfOn, "sawPfOn")
	cv.hitIf(!w.PfOn, "sawPfOff")
	cv.hitIf(w.PitchChange, "sawPitchChange")
	cv.hitIf(w.EnableTfAnalysis, "sawEnableTfAnalysis")
	cv.hitIf(!w.EnableTfAnalysis, "sawTfAnalysisDisabled")
	cv.hitIf(w.TfSelect != 0, "sawTfSelect1")
	cv.hitIf(w.SpreadNoRoom, "sawSpreadNoRoom")
	cv.hitIf(w.SpreadFromDecision, "sawSpreadFromDecision")
	cv.hitIf(w.SpreadDecision == 0, "sawSpreadNone")
	cv.hitIf(w.SpreadDecision == 2, "sawSpreadNormal")
	cv.hitIf(w.SpreadDecision == 3, "sawSpreadAggressive")
	cv.hitIf(w.DynallocBoosted, "sawDynallocBoosted")
	cv.hitIf(w.TotBoost > 0, "sawTotBoost")
	cv.hitIf(w.AllocTrimCoded, "sawAllocTrimCoded")
	cv.hitIf(!w.AllocTrimCoded, "sawAllocTrimNoRoom")
	cv.hitIf(w.AllocTrimFromLfe, "sawAllocTrimForced")
	cv.hitIf(w.IntensityChanged, "sawIntensityChange")
	cv.hitIf(w.DualStereo, "sawDualStereo")
	cv.hitIf(w.AntiCollapseRsv > 0, "sawAntiCollapseRsv")
	cv.hitIf(w.AntiCollapseRsv > 0 && w.AntiCollapseOn, "sawAntiCollapseOn")
	cv.hitIf(w.AntiCollapseRsv > 0 && !w.AntiCollapseOn, "sawAntiCollapseOff")
	cv.hitIf(w.CoarseIntra, "sawCoarseIntra")
	cv.hitIf(!w.CoarseIntra, "sawCoarseInter")
	cv.hitIf(w.LfeBandEClamp, "sawLfe")
	cv.hitIf(w.SurroundBlock, "sawSurroundBlock")
	cv.hitIf(w.TemporalVbrBlock, "sawTemporalVbrBlock")
	cv.hitIf(w.TemporalVbr != 0, "sawTemporalVbrNonZero")
	cv.hitIf(w.CbrClamped, "sawCbrClamped")
	cv.hitIf(w.ConstrainedVbrShrink, "sawConstrainedVbrShrink")
	cv.hitIf(w.SilenceVbrShrink, "sawSilenceVbrShrink")
	cv.hitIf(w.VbrShrink, "sawVbrShrink")
	cv.hitIf(w.VbrReservoirClamped, "sawVbrReservoirClamped")
	cv.hitIf(w.MinAllowedWon, "sawMinAllowedWon")
	cv.hitIf(w.VbrRate > 0, "sawVbr")
	cv.hitIf(w.VbrRate == 0, "sawCbr")

	// Threshold-crossing coverage (the bitrate corners the brief calls out).
	cv.hitIf(w.NbAvailableBytes < 25, "sawNbAvail<25")
	cv.hitIf(w.NbAvailableBytes >= 25, "sawNbAvail>=25")
	cv.hitIf(w.NbAvailableBytes < 35, "sawNbAvail<35")
	cv.hitIf(w.NbAvailableBytes >= 35, "sawNbAvail>=35")
	cv.hitIf(w.EffectiveBytes < 30+5*w.LM, "sawEffBytes<30+5LM")
	cv.hitIf(w.EffectiveBytes >= 30+5*w.LM, "sawEffBytes>=30+5LM")
	cv.hitIf(w.NbAvailableBytes < 10*cfg.C, "sawNbAvail<10C")
	cv.hitIf(w.NbAvailableBytes >= 10*cfg.C, "sawNbAvail>=10C")

	// Geometry coverage.
	switch w.LM {
	case 0:
		cv.hit("sawLM0")
	case 1:
		cv.hit("sawLM1")
	case 2:
		cv.hit("sawLM2")
	case 3:
		cv.hit("sawLM3")
	}
	cv.hitIf(cfg.CC == 1, "sawCC1")
	cv.hitIf(cfg.CC == 2 && cfg.C == 2, "sawCC2C2")
	cv.hitIf(cfg.CC == 2 && cfg.C == 1, "sawCC2C1")
	cv.hitIf(cfg.Start > 0, "sawHybridStart")
	cv.hitIf(cfg.Complexity >= 8, "sawComplexity>=8")
	cv.hitIf(cfg.Complexity == 0, "sawComplexity0")
	cv.hitIf(cfg.ForceIntra != 0, "sawForceIntra")
	cv.hitIf(cfg.DisableInv != 0, "sawDisableInv")
	cv.hitIf(cfg.PacketLoss > 0, "sawPacketLoss")
}

// --- configs -----------------------------------------------------------------

// encCfg is one sequence: an encoder configuration plus the signal program to
// drive it with.
type encCfg struct {
	name       string
	CC         int // physical channels (the allocation channel count)
	C          int // coded channels (CELT_SET_CHANNELS); may be < CC
	FrameSize  int // 120/240/480/960 -> LM 0..3
	NbBytes    int // the nbCompressedBytes argument
	Complexity int
	VBR        int
	VBRCon     int
	Bitrate    int32
	LFE        int
	ForceIntra int
	PacketLoss int
	LsbDepth   int
	DisablePf  int
	Start      int
	End        int
	DisableInv int
	Mask       []int32 // OPUS_SET_ENERGY_MASK (surround), nil for none
	Frames     []sigKind
}

// toHandleConfig converts to the C handle's ctl set.
func (cfg encCfg) toHandleConfig() celtencConfig {
	hc := celtencDefaultConfig(cfg.C)
	hc.Complexity = cfg.Complexity
	hc.VBR = cfg.VBR
	hc.VBRConstraint = cfg.VBRCon
	hc.Bitrate = cfg.Bitrate
	hc.LFE = cfg.LFE
	hc.ForceIntra = cfg.ForceIntra
	hc.PacketLoss = cfg.PacketLoss
	hc.LsbDepth = cfg.LsbDepth
	hc.DisablePrefilter = cfg.DisablePf
	hc.Start = cfg.Start
	hc.End = cfg.End
	hc.DisableInv = cfg.DisableInv
	return hc
}

// newGoEncoder builds the Go encoder in the same state the C handle's ctl set
// leaves the libopus encoder in.
func newGoEncoder(t *testing.T, cfg encCfg) *celt.Encoder {
	t.Helper()
	e := celt.NewEncoder(48000, cfg.CC)
	if e == nil {
		t.Fatalf("%s: celt.NewEncoder(%d) = nil", cfg.name, cfg.CC)
	}
	if !e.SetStreamChannels(cfg.C) {
		t.Fatalf("%s: SetStreamChannels(%d) rejected", cfg.name, cfg.C)
	}
	e.SetComplexity(cfg.Complexity)
	e.SetVBR(cfg.VBR)
	e.SetConstrainedVBR(cfg.VBRCon)
	if !e.SetBitrate(cfg.Bitrate) {
		t.Fatalf("%s: SetBitrate(%d) rejected", cfg.name, cfg.Bitrate)
	}
	e.SetLFE(cfg.LFE)
	e.SetLossRate(cfg.PacketLoss)
	if !e.SetLsbDepth(cfg.LsbDepth) {
		t.Fatalf("%s: SetLsbDepth(%d) rejected", cfg.name, cfg.LsbDepth)
	}
	if !e.SetStartBand(cfg.Start) {
		t.Fatalf("%s: SetStartBand(%d) rejected", cfg.name, cfg.Start)
	}
	if !e.SetEndBand(cfg.End) {
		t.Fatalf("%s: SetEndBand(%d) rejected", cfg.name, cfg.End)
	}
	if !e.SetDisableInv(cfg.DisableInv) {
		t.Fatalf("%s: SetDisableInv(%d) rejected", cfg.name, cfg.DisableInv)
	}
	e.SetForceIntra(cfg.ForceIntra)
	e.SetDisablePf(cfg.DisablePf)
	if cfg.Mask != nil {
		e.SetEnergyMask(cfg.Mask)
	}
	return e
}

// baseCfg is the post-celt_encoder_init default set, mirroring
// celtencDefaultConfig, so a table entry only overrides what it sweeps.
func baseCfg(name string, CC, C, frameSize, nbBytes int, frames []sigKind) encCfg {
	return encCfg{
		name:       name,
		CC:         CC,
		C:          C,
		FrameSize:  frameSize,
		NbBytes:    nbBytes,
		Complexity: 5,
		VBR:        0,
		VBRCon:     1,
		Bitrate:    -1, // OPUS_BITRATE_MAX
		LsbDepth:   24,
		Start:      0,
		End:        21,
		Frames:     frames,
	}
}

// mixed / tonal / transient signal programs used by the sweep.
var (
	progMixed = []sigKind{
		sigNoise, sigTone, sigTone, sigTransient, sigNoise, sigSilence, sigSilence,
		sigTone, sigChirp, sigTransient, sigTransient, sigNoise, sigQuietNois,
		sigLowTone, sigSilence, sigTone,
	}
	progTonal     = []sigKind{sigTone, sigTone, sigTone, sigTone, sigLowTone, sigLowTone, sigTone, sigTone}
	progTransient = []sigKind{sigTransient, sigTransient, sigTransient, sigTransient, sigTransient, sigTransient}
	progStereo    = []sigKind{sigStereoCor, sigStereoCor, sigStereoUnc, sigStereoCor, sigStereoUnc, sigStereoCor, sigStereoCor, sigStereoUnc}
	progSilence   = []sigKind{sigSilence, sigSilence, sigNoise, sigSilence, sigSilence, sigSilence, sigTone, sigSilence}
)

// encConfigs is the sweep. Each entry is commented with what it is there to
// exercise.
func encConfigs() []encCfg {
	var cs []encCfg

	// --- LM 0..3 x CC/C combinations, CBR at max rate (the plain path). -------
	for _, fs := range []int{120, 240, 480, 960} {
		for _, ch := range [][2]int{{1, 1}, {2, 2}, {2, 1}} {
			c := baseCfg("cbrmax", ch[0], ch[1], fs, 400, progMixed)
			c.name = "cbrmax/" + encItoa(fs) + "/CC" + encItoa(ch[0]) + "C" + encItoa(ch[1])
			cs = append(cs, c)
		}
	}

	// --- complexity sweep (8..10 turn on theta RDO + second MDCT). ------------
	for _, cx := range []int{0, 2, 5, 8, 10} {
		c := baseCfg("cx", 2, 2, 960, 300, progMixed)
		c.Complexity = cx
		c.name = "cx" + encItoa(cx)
		cs = append(cs, c)
		c2 := baseCfg("cxmono", 1, 1, 480, 200, progMixed)
		c2.Complexity = cx
		c2.name = "cxmono" + encItoa(cx)
		cs = append(cs, c2)
	}

	// --- VBR (unconstrained) and constrained VBR across bitrates and LMs. -----
	for _, br := range []int32{6000, 12000, 24000, 64000, 128000, 320000} {
		for _, fs := range []int{240, 960} {
			v := baseCfg("vbr", 2, 2, fs, 1275, progMixed)
			v.VBR, v.VBRCon, v.Bitrate = 1, 0, br
			v.name = "vbr/" + encItoa(int(br)) + "/" + encItoa(fs)
			cs = append(cs, v)

			cv := baseCfg("cvbr", 2, 2, fs, 1275, progMixed)
			cv.VBR, cv.VBRCon, cv.Bitrate = 1, 1, br
			cv.name = "cvbr/" + encItoa(int(br)) + "/" + encItoa(fs)
			cs = append(cs, cv)

			cvm := baseCfg("cvbrmono", 1, 1, fs, 1275, progMixed)
			cvm.VBR, cvm.VBRCon, cvm.Bitrate = 1, 1, br
			cvm.name = "cvbrmono/" + encItoa(int(br)) + "/" + encItoa(fs)
			cs = append(cs, cvm)
		}
	}

	// --- constrained VBR + a lot of silence: this is what drives vbr_reservoir
	//     negative (target = 2*8<<BITRES on a silent frame, vbr_rate is large), so
	//     it is the only way to reach the :2520 refund. -------------------------
	for _, br := range []int32{16000, 96000} {
		s := baseCfg("cvbrsilence", 2, 2, 960, 1275, progSilence)
		s.VBR, s.VBRCon, s.Bitrate = 1, 1, br
		s.name = "cvbrsilence/" + encItoa(int(br))
		cs = append(cs, s)
		sm := baseCfg("cvbrsilencemono", 1, 1, 960, 1275, progSilence)
		sm.VBR, sm.VBRCon, sm.Bitrate = 1, 1, br
		sm.name = "cvbrsilencemono/" + encItoa(int(br))
		cs = append(cs, sm)
	}

	// --- CBR at explicit bitrates: crosses nbAvailableBytes<25, <35, <10*C and
	//     effectiveBytes<30+5*LM. At 20 ms, nbCompressedBytes ~= bitrate/400 - 1. -
	for _, br := range []int32{4000, 6000, 8000, 12000, 16000, 32000, 96000, 256000} {
		for _, ch := range [][2]int{{1, 1}, {2, 2}} {
			c := baseCfg("cbr", ch[0], ch[1], 960, 1275, progMixed)
			c.Bitrate = br
			c.name = "cbr/" + encItoa(int(br)) + "/C" + encItoa(ch[1])
			cs = append(cs, c)
		}
		// Same, at 2.5 ms, where effectiveBytes is tiny.
		c := baseCfg("cbrshort", 2, 2, 120, 1275, progMixed)
		c.Bitrate = br
		c.name = "cbrshort/" + encItoa(int(br))
		cs = append(cs, c)
	}

	// --- LFE (the bandE clamp at :2098, offsets[0] at :2352, forced trim/spread,
	//     signalBandwidth=1, and the !lfe gate on temporal VBR). ----------------
	for _, br := range []int32{-1, 6000, 32000} {
		l := baseCfg("lfe", 1, 1, 960, 1275, progMixed)
		l.LFE = 1
		l.Bitrate = br
		l.name = "lfe/" + encItoa(int(br))
		cs = append(cs, l)
	}
	lv := baseCfg("lfevbr", 2, 2, 960, 1275, progMixed)
	lv.LFE, lv.VBR, lv.VBRCon, lv.Bitrate = 1, 1, 1, 48000
	lv.name = "lfevbr"
	cs = append(cs, lv)

	// --- transient-heavy: anti_collapse_rsv/on/off and consec_transient. ------
	for _, fs := range []int{480, 960} {
		tr := baseCfg("transient", 2, 2, fs, 300, progTransient)
		tr.name = "transient/" + encItoa(fs)
		cs = append(cs, tr)
		trc := baseCfg("transientcx10", 2, 2, fs, 300, progTransient)
		trc.Complexity = 10
		trc.name = "transientcx10/" + encItoa(fs)
		cs = append(cs, trc)
	}
	// A transient right after quiet frames: consec_transient==0 -> anti_collapse_on.
	mix := baseCfg("transientmix", 1, 1, 960, 400, []sigKind{
		sigNoise, sigNoise, sigTransient, sigNoise, sigNoise, sigTransient,
		sigTone, sigTone, sigTransient, sigNoise,
	})
	cs = append(cs, mix)

	// --- tonal: prefilter on (pf_on), tapset/spread hysteresis, pitch_change. --
	for _, cx := range []int{5, 10} {
		tn := baseCfg("tonal", 1, 1, 960, 500, progTonal)
		tn.Complexity = cx
		tn.name = "tonal/cx" + encItoa(cx)
		cs = append(cs, tn)
		tns := baseCfg("tonalstereo", 2, 2, 960, 500, progTonal)
		tns.Complexity = cx
		tns.name = "tonalstereo/cx" + encItoa(cx)
		cs = append(cs, tns)
	}
	// Prefilter explicitly disabled.
	pf := baseCfg("nopf", 1, 1, 960, 500, progTonal)
	pf.DisablePf = 1
	pf.name = "nopf"
	cs = append(cs, pf)

	// --- stereo: stereo_saving, intensity hysteresis, dual_stereo. ------------
	for _, br := range []int32{-1, 24000, 64000, 192000} {
		s := baseCfg("stereo", 2, 2, 960, 1275, progStereo)
		s.Bitrate = br
		s.name = "stereo/" + encItoa(int(br))
		cs = append(cs, s)
	}
	// Ramp the bitrate frame to frame so hysteresis_decision actually moves
	// st->intensity (a constant equiv_rate never crosses a threshold twice).
	// Handled by the intensity-ramp test below.

	// --- force_intra / packet loss / lsb_depth / disable_inv. ------------------
	fi := baseCfg("forceintra", 2, 2, 960, 400, progMixed)
	fi.ForceIntra = 1
	fi.name = "forceintra"
	cs = append(cs, fi)
	fim := baseCfg("forceintramono", 1, 1, 240, 200, progMixed)
	fim.ForceIntra = 1
	fim.name = "forceintramono"
	cs = append(cs, fim)

	pl := baseCfg("loss", 1, 1, 960, 400, progTonal)
	pl.PacketLoss = 20 // >8: run_prefilter zeroes gain1; also biases coarse RDO
	pl.name = "loss20"
	cs = append(cs, pl)
	pl2 := baseCfg("loss3", 1, 1, 960, 400, progTonal)
	pl2.PacketLoss = 3 // >2: gain1 halved once
	pl2.name = "loss3"
	cs = append(cs, pl2)

	di := baseCfg("disableinv", 2, 2, 960, 300, progMixed)
	di.DisableInv = 1
	di.Complexity = 10
	di.name = "disableinv"
	cs = append(cs, di)

	ld := baseCfg("lsb8", 1, 1, 960, 400, progMixed)
	ld.LsbDepth = 8
	ld.name = "lsb8"
	cs = append(cs, ld)
	ld16 := baseCfg("lsb16", 2, 2, 960, 400, progMixed)
	ld16.LsbDepth = 16
	ld16.name = "lsb16"
	cs = append(cs, ld16)

	// --- end-band variants (effEnd/effEBands, the oldBandE clear loop). --------
	for _, e := range []int{13, 17, 19, 21} {
		eb := baseCfg("end", 2, 2, 960, 400, progMixed)
		eb.End = e
		eb.name = "end" + encItoa(e)
		cs = append(cs, eb)
	}

	// --- start>0 (hybrid). Unreachable from the CELT-only Opus encoder, but the
	//     branches exist in C (weak transients, the forced tf_res, the base_target
	//     bypass of compute_vbr, the min_allowed floor from tell0_frac, the forced
	//     alloc_trim/stereo_saving). enable_tf_analysis is false whenever hybrid,
	//     so tf_analysis' start>0 uninitialised-importance read is NOT triggered. --
	for _, br := range []int32{-1, 12000} {
		hb := baseCfg("hybrid", 2, 2, 960, 400, progMixed)
		hb.Start = 17
		hb.Bitrate = br
		hb.name = "hybrid/" + encItoa(int(br))
		cs = append(cs, hb)
	}
	hbv := baseCfg("hybridvbr", 1, 1, 960, 1275, progMixed)
	hbv.Start = 17
	hbv.VBR, hbv.VBRCon, hbv.Bitrate = 1, 1, 24000
	hbv.name = "hybridvbr"
	cs = append(cs, hbv)
	// hybrid + tiny effectiveBytes (<15): allow_weak_transients is 1, so
	// transient_analysis can report weak_transient (:2050) and the forced tf_res=1
	// branch at :2224 becomes reachable. The graded soft transients bracket the
	// 200 < mask_metric < 600 window that "weak" means.
	for _, br := range []int32{4000, 5000, 6000} {
		hbw := baseCfg("hybridweak", 1, 1, 960, 1275, []sigKind{
			sigQuietNois, sigSoftT1, sigSoftT2, sigSoftT3, sigSoftT4,
			sigTransient, sigSoftT2, sigSoftT3, sigTone, sigSoftT1,
		})
		hbw.Start = 17
		hbw.Bitrate = br
		hbw.name = "hybridweak/" + encItoa(int(br))
		cs = append(cs, hbw)
		hbw2 := baseCfg("hybridweakst", 2, 2, 960, 1275, []sigKind{
			sigQuietNois, sigSoftT1, sigSoftT2, sigSoftT3, sigSoftT4, sigSoftT2,
		})
		hbw2.Start = 17
		hbw2.Bitrate = br
		hbw2.name = "hybridweakst/" + encItoa(int(br))
		cs = append(cs, hbw2)
	}

	// --- surround masking (energy_mask). Structurally dead on the single-stream
	//     CELT path, but ported, so it is proven bit-exact when a mask IS set. ---
	for _, ch := range [][2]int{{1, 1}, {2, 2}} {
		mask := make([]int32, ch[1]*21)
		for i := range mask {
			// A sloped mask straddling 0, so both the unmask>GCONST(.25) dynalloc
			// path and the count_dynalloc>=3 correction can fire.
			mask[i] = int32((i%21 - 8)) * (1 << 24) / 4
		}
		sm := baseCfg("surround", ch[0], ch[1], 960, 1275, progMixed)
		sm.Mask = mask
		sm.VBR, sm.VBRCon, sm.Bitrate = 1, 0, 64000
		sm.name = "surround/C" + encItoa(ch[1])
		cs = append(cs, sm)
	}
	// A strongly negative mask, so count_dynalloc>=3 fires with mask_avg<=0.
	maskNeg := make([]int32, 2*21)
	for i := range maskNeg {
		maskNeg[i] = -int32(1+i%7) * (1 << 24) / 2
	}
	smn := baseCfg("surroundneg", 2, 2, 960, 1275, progMixed)
	smn.Mask = maskNeg
	smn.VBR, smn.VBRCon, smn.Bitrate = 1, 1, 96000
	smn.name = "surroundneg"
	cs = append(cs, smn)

	// --- tiny nbCompressedBytes: forces the "no room" branches (spread, trim). --
	for _, nb := range []int{2, 3, 5, 8, 12} {
		tb := baseCfg("tiny", 1, 1, 960, nb, progMixed)
		tb.name = "tiny/" + encItoa(nb)
		cs = append(cs, tb)
		tb2 := baseCfg("tinystereo", 2, 2, 240, nb, progMixed)
		tb2.name = "tinystereo/" + encItoa(nb)
		cs = append(cs, tb2)
	}

	return cs
}

// itoa avoids pulling strconv into the test's hot path for readability only.
func encItoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// --- state comparison --------------------------------------------------------

// eqInt fails the test if two ints differ.
func eqEncInt(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: Go=%d C=%d", label, got, want)
	}
}

// compareEncState diffs the whole encoder state FIELD BY FIELD, in the canonical
// order, so a mismatch localizes instead of just flipping a hash.
func compareEncState(t *testing.T, ctx string, got, want celt.EncoderState) {
	t.Helper()
	if got.Rng != want.Rng {
		t.Fatalf("%s: rng: Go=%#x C=%#x", ctx, got.Rng, want.Rng)
	}
	eqEncInt(t, ctx+": spread_decision", got.SpreadDecision, want.SpreadDecision)
	if got.DelayedIntra != want.DelayedIntra {
		t.Fatalf("%s: delayedIntra: Go=%d C=%d", ctx, got.DelayedIntra, want.DelayedIntra)
	}
	eqEncInt(t, ctx+": tonal_average", got.TonalAverage, want.TonalAverage)
	eqEncInt(t, ctx+": lastCodedBands", got.LastCodedBands, want.LastCodedBands)
	eqEncInt(t, ctx+": hf_average", got.HfAverage, want.HfAverage)
	eqEncInt(t, ctx+": tapset_decision", got.TapsetDecision, want.TapsetDecision)
	eqEncInt(t, ctx+": prefilter_period", got.PrefilterPeriod, want.PrefilterPeriod)
	if got.PrefilterGain != want.PrefilterGain {
		t.Fatalf("%s: prefilter_gain: Go=%d C=%d", ctx, got.PrefilterGain, want.PrefilterGain)
	}
	eqEncInt(t, ctx+": prefilter_tapset", got.PrefilterTapset, want.PrefilterTapset)
	eqEncInt(t, ctx+": consec_transient", got.ConsecTransient, want.ConsecTransient)
	for i := 0; i < 2; i++ {
		if got.PreemphMemE[i] != want.PreemphMemE[i] {
			t.Fatalf("%s: preemph_memE[%d]: Go=%d C=%d", ctx, i, got.PreemphMemE[i], want.PreemphMemE[i])
		}
		if got.PreemphMemD[i] != want.PreemphMemD[i] {
			t.Fatalf("%s: preemph_memD[%d]: Go=%d C=%d", ctx, i, got.PreemphMemD[i], want.PreemphMemD[i])
		}
	}
	if got.VbrReservoir != want.VbrReservoir {
		t.Fatalf("%s: vbr_reservoir: Go=%d C=%d", ctx, got.VbrReservoir, want.VbrReservoir)
	}
	if got.VbrDrift != want.VbrDrift {
		t.Fatalf("%s: vbr_drift: Go=%d C=%d", ctx, got.VbrDrift, want.VbrDrift)
	}
	if got.VbrOffset != want.VbrOffset {
		t.Fatalf("%s: vbr_offset: Go=%d C=%d", ctx, got.VbrOffset, want.VbrOffset)
	}
	if got.VbrCount != want.VbrCount {
		t.Fatalf("%s: vbr_count: Go=%d C=%d", ctx, got.VbrCount, want.VbrCount)
	}
	if got.OverlapMax != want.OverlapMax {
		t.Fatalf("%s: overlap_max: Go=%d C=%d", ctx, got.OverlapMax, want.OverlapMax)
	}
	if got.StereoSaving != want.StereoSaving {
		t.Fatalf("%s: stereo_saving: Go=%d C=%d", ctx, got.StereoSaving, want.StereoSaving)
	}
	eqEncInt(t, ctx+": intensity", got.Intensity, want.Intensity)
	if got.SpecAvg != want.SpecAvg {
		t.Fatalf("%s: spec_avg: Go=%d C=%d", ctx, got.SpecAvg, want.SpecAvg)
	}
	eqI32(t, ctx+": in_mem", got.InMem, want.InMem)
	eqI32(t, ctx+": prefilter_mem", got.PrefilterMem, want.PrefilterMem)
	eqI32(t, ctx+": oldBandE", got.OldBandE, want.OldBandE)
	eqI32(t, ctx+": oldLogE", got.OldLogE, want.OldLogE)
	eqI32(t, ctx+": oldLogE2", got.OldLogE2, want.OldLogE2)
	eqI32(t, ctx+": energyError", got.EnergyError, want.EnergyError)
	// Backstop: if every field above matched, the hashes must too.
	if got.Hash() != want.Hash() {
		t.Fatalf("%s: state hash differs (%#x vs %#x) although every field matched: "+
			"the hash and the field list have drifted apart", ctx, got.Hash(), want.Hash())
	}
}

// comparePacket diffs the packet BYTE BY BYTE and reports the first divergence
// with its index (the symbol order is the bitstream, so the first bad byte is
// what localizes a symbol-order bug).
func comparePacket(t *testing.T, ctx string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: packet LENGTH Go=%d C=%d\n  Go=%x\n   C=%x", ctx, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			lo := i - 4
			if lo < 0 {
				lo = 0
			}
			hi := i + 5
			if hi > len(want) {
				hi = len(want)
			}
			t.Fatalf("%s: packet BYTE %d/%d: Go=%#02x C=%#02x\n"+
				"  Go[%d:%d]=%x\n   C[%d:%d]=%x\n"+
				"  (first divergence; see the range-coder symbol order at the top of "+
				"internal/celt/celt_encoder_encode.go to identify which symbol this byte belongs to)",
				ctx, i, len(want), got[i], want[i], lo, hi, got[lo:hi], lo, hi, want[lo:hi])
		}
	}
}

// --- the gate ----------------------------------------------------------------

// TestCeltEncodeWithEcVsC is THE checkpoint gate: multi-frame sequences on paired
// Go and C encoders, compared per frame on return value, packet length, every
// packet byte, st->rng and the full state, over the whole configuration sweep.
func TestCeltEncodeWithEcVsC(t *testing.T) {
	cv := newEncCoverage()

	for _, cfg := range encConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			// One RNG per (config, side) so the Go and C encoders see BIT-IDENTICAL
			// PCM: the generators are driven twice from the same seed.
			const seed = 0x5eed_c8c3
			goRng := rand.New(rand.NewSource(seed))
			cRng := rand.New(rand.NewSource(seed))

			goEnc := newGoEncoder(t, cfg)

			h, err := newCCeltencHandle(cfg.CC)
			if err != nil {
				t.Fatalf("newCCeltencHandle: %v", err)
			}
			defer h.Close()
			if err := h.Configure(cfg.toHandleConfig()); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if cfg.Mask != nil {
				if err := h.SetEnergyMask(cfg.Mask); err != nil {
					t.Fatalf("SetEnergyMask: %v", err)
				}
			}

			// The initial states must already agree, or nothing downstream means
			// anything.
			compareEncState(t, cfg.name+"/init", goEnc.State(), h.State())

			t0 := 0
			for f, kind := range cfg.Frames {
				goPcm := genFrame(kind, goRng, cfg.CC, cfg.FrameSize, t0, f)
				cPcm := genFrame(kind, cRng, cfg.CC, cfg.FrameSize, t0, f)
				for i := range goPcm {
					if goPcm[i] != cPcm[i] {
						t.Fatalf("frame %d: generator is not deterministic at sample %d", f, i)
					}
				}
				t0 += cfg.FrameSize

				ctx := cfg.name + "/frame" + encItoa(f)

				cRet, cPkt, cRng32 := h.Encode(cPcm, cfg.FrameSize, cfg.NbBytes)
				goRet, goPkt, w := goEnc.EncodeFrame(goPcm, cfg.FrameSize, cfg.NbBytes)

				if goRet != cRet {
					t.Fatalf("%s: celt_encode_with_ec RETURN VALUE: Go=%d C=%d", ctx, goRet, cRet)
				}
				if cRet < 0 {
					// Both errored identically; nothing else to compare.
					continue
				}
				comparePacket(t, ctx, goPkt, cPkt)
				if goEnc.Rng() != cRng32 {
					t.Fatalf("%s: st->rng: Go=%#x C=%#x", ctx, goEnc.Rng(), cRng32)
				}
				compareEncState(t, ctx, goEnc.State(), h.State())

				cv.record(w, cfg)
			}
		})
	}

	// NON-VACUITY: every documented branch of celt_encode_with_ec must have fired
	// somewhere in the sweep, or the green result above is meaningless.
	cv.require(t,
		// core decisions
		"sawSilence", "sawNonSilence",
		"sawTransient", "sawNonTransient",
		"sawTransientGotDisabled",
		"sawSecondMdct",
		"sawWeakTransient",
		"sawPfEnabled", "sawPfOn", "sawPfOff",
		"sawPitchChange",
		"sawEnableTfAnalysis", "sawTfAnalysisDisabled",
		"sawTfSelect1",
		// spread
		"sawSpreadNoRoom", "sawSpreadFromDecision",
		"sawSpreadNone", "sawSpreadNormal", "sawSpreadAggressive",
		// dynalloc / trim / allocation
		"sawDynallocBoosted", "sawTotBoost",
		"sawAllocTrimCoded", "sawAllocTrimNoRoom", "sawAllocTrimForced",
		"sawIntensityChange", "sawDualStereo",
		// anti-collapse
		"sawAntiCollapseRsv", "sawAntiCollapseOn", "sawAntiCollapseOff",
		// coarse energy
		"sawCoarseIntra", "sawCoarseInter",
		// lfe / surround / temporal vbr
		"sawLfe", "sawSurroundBlock", "sawTemporalVbrBlock", "sawTemporalVbrNonZero",
		// rate control
		"sawCbr", "sawVbr",
		"sawCbrClamped",
		"sawConstrainedVbrShrink", "sawSilenceVbrShrink", "sawVbrShrink",
		"sawVbrReservoirClamped", "sawMinAllowedWon",
		// bitrate thresholds called out in the brief
		"sawNbAvail<25", "sawNbAvail>=25",
		"sawNbAvail<35", "sawNbAvail>=35",
		"sawEffBytes<30+5LM", "sawEffBytes>=30+5LM",
		"sawNbAvail<10C", "sawNbAvail>=10C",
		// geometry / config
		"sawLM0", "sawLM1", "sawLM2", "sawLM3",
		"sawCC1", "sawCC2C2", "sawCC2C1",
		"sawHybridStart",
		"sawComplexity0", "sawComplexity>=8",
		"sawForceIntra", "sawDisableInv", "sawPacketLoss",
	)

	// THE ONE BRANCH THAT MUST *NOT* FIRE. The "last chance to catch a transient"
	// block at celt_encoder.c:2211-2226 is guarded by patch_transient_decision,
	// which is STRUCTURALLY DEAD in this frozen config: at :498-501 it declares the
	// per-band terms `opus_val16 x1, x2` and assigns MAXG(0, <celt_glog>) into
	// them, so with 32-bit celt_glog each term truncates to 16 bits and is capped
	// at ~2^15. mean_diff is their average and can therefore never exceed
	// GCONST(1.f) == 2^24 (:506). It is an upstream bug, reproduced faithfully (see
	// TestCeltencPatchTransientMatchesC and internal/celt/celt_encoder.go:507).
	// Asserting it stays dead keeps the port honest: if libopus ever fixes the
	// truncation, this fires and forces the recompute path to be tested for real.
	if n := cv.m["sawPatchedTransient"]; n != 0 {
		t.Fatalf("patch_transient_decision fired %d times: it is supposed to be "+
			"structurally dead (celt_encoder.c:498-506, opus_val16 truncation). Either the "+
			"pinned libopus changed or the Go port's truncation is wrong.", n)
	}
	// vbr_count saturation (:2508) needs >970 frames and has its own test.
	t.Logf("branch coverage: %d distinct branches fired", len(cv.m))
}

// TestCeltEncodeIntensityRampVsC ramps the bitrate frame to frame so that
// equiv_rate/1000 sweeps up and down across the intensity_thresholds table. That
// is the only way to make hysteresis_decision (:2403) actually MOVE
// st->intensity, and st->intensity feeds both clt_compute_allocation (an in/out
// param, so it lands in the bitstream) and compute_vbr. It also re-crosses the
// nbAvailableBytes / effectiveBytes thresholds within one continuous state
// sequence, which the fixed-bitrate configs cannot do.
func TestCeltEncodeIntensityRampVsC(t *testing.T) {
	const (
		CC        = 2
		C         = 2
		frameSize = 960
		nbBytes   = 1275
	)
	rates := []int32{
		4000, 6000, 9000, 14000, 20000, 30000, 45000, 64000, 90000, 128000,
		190000, 260000, 190000, 128000, 90000, 64000, 45000, 30000, 20000,
		14000, 9000, 6000, 4000, 2000, 4000, 20000, 128000,
	}
	seq := []sigKind{sigStereoCor, sigStereoUnc, sigTone, sigNoise, sigTransient}

	for _, vbr := range []int{0, 1} {
		for _, con := range []int{0, 1} {
			if vbr == 0 && con == 1 {
				// CBR ignores the constraint; one CBR pass is enough.
				continue
			}
			name := "cbr"
			if vbr != 0 {
				name = "vbr"
				if con != 0 {
					name = "cvbr"
				}
			}
			t.Run(name, func(t *testing.T) {
				const seed = 0x17e5117
				goRng := rand.New(rand.NewSource(seed))
				cRng := rand.New(rand.NewSource(seed))

				cfg := baseCfg("ramp", CC, C, frameSize, nbBytes, nil)
				cfg.VBR, cfg.VBRCon = vbr, con
				cfg.Bitrate = rates[0]
				cfg.Complexity = 8

				goEnc := newGoEncoder(t, cfg)
				h, err := newCCeltencHandle(CC)
				if err != nil {
					t.Fatalf("newCCeltencHandle: %v", err)
				}
				defer h.Close()

				sawIntensityMove := false
				prevIntensity := goEnc.State().Intensity
				t0 := 0
				for f, br := range rates {
					cfg.Bitrate = br
					if err := h.Configure(cfg.toHandleConfig()); err != nil {
						t.Fatalf("Configure: %v", err)
					}
					if !goEnc.SetBitrate(br) {
						t.Fatalf("SetBitrate(%d) rejected", br)
					}

					kind := seq[f%len(seq)]
					goPcm := genFrame(kind, goRng, CC, frameSize, t0, f)
					cPcm := genFrame(kind, cRng, CC, frameSize, t0, f)
					t0 += frameSize

					ctx := name + "/frame" + encItoa(f) + "@" + encItoa(int(br))
					cRet, cPkt, cRng32 := h.Encode(cPcm, frameSize, nbBytes)
					goRet, goPkt, w := goEnc.EncodeFrame(goPcm, frameSize, nbBytes)

					if goRet != cRet {
						t.Fatalf("%s: RETURN VALUE Go=%d C=%d", ctx, goRet, cRet)
					}
					if cRet < 0 {
						continue
					}
					comparePacket(t, ctx, goPkt, cPkt)
					if goEnc.Rng() != cRng32 {
						t.Fatalf("%s: st->rng Go=%#x C=%#x", ctx, goEnc.Rng(), cRng32)
					}
					compareEncState(t, ctx, goEnc.State(), h.State())

					if w.Intensity != prevIntensity {
						sawIntensityMove = true
					}
					prevIntensity = w.Intensity
				}
				if !sawIntensityMove {
					t.Fatal("NON-VACUITY FAILURE: st->intensity never moved across the bitrate ramp")
				}
			})
		}
	}
}

// TestCeltEncodeVbrCountSaturationVsC runs long enough (>970 frames) for
// st->vbr_count to hit its ceiling at celt_encoder.c:2508, where alpha stops
// being celt_rcp(...) and becomes the constant QCONST16(.001f,15). That branch is
// INVISIBLE in any short sequence, and getting it wrong would only show up after
// ~20 seconds of audio.
func TestCeltEncodeVbrCountSaturationVsC(t *testing.T) {
	const (
		CC        = 1
		frameSize = 960
		nbBytes   = 1275
		frames    = 1010 // > 970, so vbr_count saturates and the branch flips
	)
	const seed = 0x9705a7

	cfg := baseCfg("vbrcount", CC, CC, frameSize, nbBytes, nil)
	cfg.VBR, cfg.VBRCon, cfg.Bitrate = 1, 1, 48000
	cfg.Complexity = 2 // keep it quick; the VBR loop does not depend on complexity

	goRng := rand.New(rand.NewSource(seed))
	cRng := rand.New(rand.NewSource(seed))

	goEnc := newGoEncoder(t, cfg)
	h, err := newCCeltencHandle(CC)
	if err != nil {
		t.Fatalf("newCCeltencHandle: %v", err)
	}
	defer h.Close()
	if err := h.Configure(cfg.toHandleConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	sawSaturated := false
	sawUnsaturated := false
	seq := []sigKind{sigNoise, sigTone, sigChirp, sigTransient, sigSilence, sigTone}
	t0 := 0
	for f := 0; f < frames; f++ {
		kind := seq[f%len(seq)]
		goPcm := genFrame(kind, goRng, CC, frameSize, t0, f)
		cPcm := genFrame(kind, cRng, CC, frameSize, t0, f)
		t0 += frameSize

		ctx := "vbrcount/frame" + encItoa(f)
		cRet, cPkt, cRng32 := h.Encode(cPcm, frameSize, nbBytes)
		goRet, goPkt, w := goEnc.EncodeFrame(goPcm, frameSize, nbBytes)

		if goRet != cRet {
			t.Fatalf("%s: RETURN VALUE Go=%d C=%d", ctx, goRet, cRet)
		}
		if cRet < 0 {
			continue
		}
		comparePacket(t, ctx, goPkt, cPkt)
		if goEnc.Rng() != cRng32 {
			t.Fatalf("%s: st->rng Go=%#x C=%#x", ctx, goEnc.Rng(), cRng32)
		}
		compareEncState(t, ctx, goEnc.State(), h.State())

		if w.VbrCountSaturated {
			sawSaturated = true
		} else {
			sawUnsaturated = true
		}
	}
	if !sawUnsaturated {
		t.Fatal("NON-VACUITY FAILURE: the vbr_count<970 (celt_rcp alpha) branch never fired")
	}
	if !sawSaturated {
		t.Fatalf("NON-VACUITY FAILURE: vbr_count never saturated after %d frames (state=%d)",
			frames, goEnc.State().VbrCount)
	}
	if got := goEnc.State().VbrCount; got != 970 {
		t.Fatalf("vbr_count = %d after %d frames, want the 970 ceiling", got, frames)
	}
}

// TestCeltEncodeBadArgsVsC pins the two OPUS_BAD_ARG guards at the head of
// celt_encode_with_ec (:1830 nbCompressedBytes<2, :1837 no LM matches the frame
// size) against the C, including the fact that neither touches the encoder state.
func TestCeltEncodeBadArgsVsC(t *testing.T) {
	cases := []struct {
		name      string
		frameSize int
		nbBytes   int
	}{
		{"nbBytes=1", 960, 1},   // nbCompressedBytes < 2 -> OPUS_BAD_ARG
		{"frame=100", 100, 100}, // no LM with shortMdctSize<<LM == 100
		{"frame=1920", 1920, 100},
		{"frame=0", 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg("badarg", 1, 1, tc.frameSize, tc.nbBytes, nil)
			goEnc := newGoEncoder(t, cfg)
			h, err := newCCeltencHandle(1)
			if err != nil {
				t.Fatalf("newCCeltencHandle: %v", err)
			}
			defer h.Close()
			if err := h.Configure(cfg.toHandleConfig()); err != nil {
				t.Fatalf("Configure: %v", err)
			}

			// A frame long enough that C never reads out of bounds even when it
			// rejects the size.
			pcm := make([]int16, 2048)
			nb := tc.nbBytes
			if nb < 1 {
				nb = 1 // the C shim memsets nb_bytes; give it a byte to memset
			}
			cRet, _, _ := h.Encode(pcm, tc.frameSize, nb)
			goRet, goPkt, _ := goEnc.EncodeFrame(pcm, tc.frameSize, nb)
			if goRet != cRet {
				t.Fatalf("%s: RETURN VALUE Go=%d C=%d", tc.name, goRet, cRet)
			}
			if cRet >= 0 {
				t.Fatalf("%s: expected an error return, got %d", tc.name, cRet)
			}
			if goPkt != nil {
				t.Fatalf("%s: expected no packet on error", tc.name)
			}
			compareEncState(t, tc.name+"/after-error", goEnc.State(), h.State())
		})
	}
}
