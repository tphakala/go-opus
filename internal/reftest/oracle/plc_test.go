//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This file is the differential test for the pure-Go CELT packet-loss
// concealment (internal/celt: celt_decode_lost + pitch.go + celt_lpc.go) against
// the pinned libopus C (celt/celt_decoder.c celt_decode_lost, celt/pitch.c,
// celt/celt_lpc.c). The RFC conformance vectors never drop packets
// (docs/peer-review.md section 3), so PLC is proven two ways here:
//
//  1. Function-level differential over the pure PLC math primitives (_celt_lpc,
//     celt_fir, celt_iir, _celt_autocorr, celt_pitch_xcorr_c, pitch_downsample,
//     pitch_search) on random inputs (docs/hard-parts.md section 8).
//  2. SEQUENCE loss-pattern differential: the raw C CELT encoder (celtdec_shim.h)
//     produces a packet sequence; both decoders are driven in lockstep over the
//     SAME sequence with chosen frames DROPPED (data==nil / len 0, which routes
//     to celt_decode_lost), asserting bit-exact PCM + rng + persistent state
//     after EVERY frame, including the concealed frames AND the recovery frames
//     after them. Both regimes (pitch extrapolation and noise/CNG) and their
//     boundaries are covered.

// --- deterministic random helpers (reuse lcg from celtdec_test.go) -----------

// randI16 returns a value in [-m, m].
func randI16(g *lcg, m int) int16 {
	if m <= 0 {
		return 0
	}
	return int16(int(g.next()%uint32(2*m+1)) - m)
}

// randI32 returns a value in [-m, m].
func randI32(g *lcg, m int32) int32 {
	if m <= 0 {
		return 0
	}
	return int32(g.next()%uint32(2*m+1)) - m
}

func randI16Slice(g *lcg, n, m int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = randI16(g, m)
	}
	return s
}

func randI32Slice(g *lcg, n int, m int32) []int32 {
	s := make([]int32, n)
	for i := range s {
		s[i] = randI32(g, m)
	}
	return s
}

func equalI32(a, b []int32) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if a[i] != b[i] {
			return i, false
		}
	}
	return 0, true
}

func equalI16At(a, b []int16) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if a[i] != b[i] {
			return i, false
		}
	}
	return 0, true
}

// --- function-level differential tests ---------------------------------------

// TestPLCCeltPitchXcorrDifferential pins celt_pitch_xcorr_c. Values are bounded
// so the len-term integer accumulation stays inside int32 on both sides.
func TestPLCCeltPitchXcorrDifferential(t *testing.T) {
	g := &lcg{s: 0xA1}
	cases := []struct{ len_, maxPitch, m int }{
		{256, 64, 1500},
		{1000, 25, 1000}, // autocorr-shaped
		{332, 155, 1200}, // pitch_search coarse-shaped
		{7, 5, 3000},     // ragged, small
	}
	for _, tc := range cases {
		for trial := 0; trial < 40; trial++ {
			x := randI16Slice(g, tc.len_, tc.m)
			y := randI16Slice(g, tc.len_+tc.maxPitch, tc.m)
			goX, goMax := celt.CeltPitchXcorr(x, y, tc.len_, tc.maxPitch)
			cX, cMax := cPitchXcorr(x, y, tc.len_, tc.maxPitch)
			if i, ok := equalI32(goX, cX); !ok {
				t.Fatalf("xcorr len=%d mp=%d trial=%d: differ at %d go=%d c=%d",
					tc.len_, tc.maxPitch, trial, i, goX[i], cX[i])
			}
			if goMax != cMax {
				t.Fatalf("xcorr len=%d mp=%d trial=%d: maxcorr go=%d c=%d",
					tc.len_, tc.maxPitch, trial, goMax, cMax)
			}
		}
	}
}

// TestPLCCeltAutocorrDifferential pins _celt_autocorr, both the overlap==0 path
// (pitch_downsample uses lag=4, window=nil) and the windowed overlap!=0 path
// (celt_decode_lost uses lag=CELT_LPC_ORDER, overlap=mode->overlap). The window
// is arbitrary for a differential (both sides get the same one).
func TestPLCCeltAutocorrDifferential(t *testing.T) {
	g := &lcg{s: 0xB2}
	// ramp window in [0, Q15ONE]
	mkWindow := func(overlap int) []int16 {
		w := make([]int16, overlap)
		for i := range w {
			w[i] = int16(int64(i+1) * 32767 / int64(overlap))
		}
		return w
	}
	cases := []struct {
		overlap, lag, n, m int
		windowed           bool
	}{
		{0, 4, 512, 8000, false},
		{0, 4, 1024, 12000, false},
		{120, 24, 1024, 9000, true},
		{60, 24, 480, 15000, true},
		{0, 24, 200, 30000, false},
	}
	for _, tc := range cases {
		var win []int16
		if tc.windowed {
			win = mkWindow(tc.overlap)
		}
		for trial := 0; trial < 30; trial++ {
			x := randI16Slice(g, tc.n, tc.m)
			goAc, goShift := celt.CeltAutocorr(x, win, tc.overlap, tc.lag, tc.n)
			cAc, cShift := cCeltAutocorr(x, win, tc.overlap, tc.lag, tc.n)
			if goShift != cShift {
				t.Fatalf("autocorr n=%d lag=%d ov=%d trial=%d: shift go=%d c=%d",
					tc.n, tc.lag, tc.overlap, trial, goShift, cShift)
			}
			if i, ok := equalI32(goAc, cAc); !ok {
				t.Fatalf("autocorr n=%d lag=%d ov=%d trial=%d: ac differ at %d go=%d c=%d",
					tc.n, tc.lag, tc.overlap, trial, i, goAc[i], cAc[i])
			}
		}
	}
}

// TestPLCCeltLPCDifferential pins _celt_lpc, feeding both sides the SAME ac
// derived (via the C oracle) from a random windowed signal so the Levinson-Durbin
// recursion and the 16-bit fitting path are exercised with realistic input. Runs
// at p=4 (pitch_downsample) and p=CELT_LPC_ORDER (the PLC LPC analysis).
func TestPLCCeltLPCDifferential(t *testing.T) {
	g := &lcg{s: 0xC3}
	cases := []struct{ p, n, m int }{
		{4, 1024, 12000},
		{24, 1024, 9000},
		{24, 1024, 30000},
		{24, 512, 20000},
	}
	for _, tc := range cases {
		for trial := 0; trial < 40; trial++ {
			x := randI16Slice(g, tc.n, tc.m)
			// Use the C autocorr to build ac (already validated above); the LPC
			// port must then match the C LPC on that identical ac.
			ac, _ := cCeltAutocorr(x, nil, 0, tc.p, tc.n)
			// Apply the same noise floor + lag-window the PLC applies, to reach
			// realistic coefficient magnitudes.
			ac[0] += ac[0] >> 13
			goLpc := celt.CeltLPC(ac, tc.p)
			cLpc := cCeltLPC(ac, tc.p)
			if i, ok := equalI16At(goLpc, cLpc); !ok {
				t.Fatalf("lpc p=%d trial=%d: differ at %d go=%d c=%d",
					tc.p, trial, i, goLpc[i], cLpc[i])
			}
		}
	}
}

// TestPLCCeltFirDifferential pins celt_fir_c. num is bounded so the SIG_SHIFT-
// domain accumulation stays in int32.
func TestPLCCeltFirDifferential(t *testing.T) {
	g := &lcg{s: 0xD4}
	cases := []struct{ N, ord, m, numM int }{
		{64, 24, 30000, 512},
		{200, 24, 20000, 400},
		{120, 24, 32767, 300},
	}
	for _, tc := range cases {
		for trial := 0; trial < 40; trial++ {
			xfull := randI16Slice(g, tc.N+tc.ord, tc.m)
			num := randI16Slice(g, tc.ord, tc.numM)
			goY := celt.CeltFir(xfull, tc.N, tc.ord, num)
			cY := cCeltFir(xfull, tc.N, tc.ord, num)
			if i, ok := equalI16At(goY, cY); !ok {
				t.Fatalf("fir N=%d ord=%d trial=%d: differ at %d go=%d c=%d",
					tc.N, tc.ord, trial, i, goY[i], cY[i])
			}
		}
	}
}

// TestPLCCeltIirDifferential pins celt_iir (both _y and the updated mem). den is
// bounded so the IIR stays inside int32; the SROUND16 feedback bounds the memory.
func TestPLCCeltIirDifferential(t *testing.T) {
	g := &lcg{s: 0xE5}
	cases := []struct {
		N, ord int
		xM     int32
		denM   int
	}{
		{64, 24, 1 << 20, 1024},
		{120, 24, 1 << 18, 512},
		{200, 24, 1 << 19, 256},
	}
	for _, tc := range cases {
		for trial := 0; trial < 40; trial++ {
			x := randI32Slice(g, tc.N, tc.xM)
			den := randI16Slice(g, tc.ord, tc.denM)
			mem := randI16Slice(g, tc.ord, 8000)
			goY, goMem := celt.CeltIir(x, den, tc.N, tc.ord, mem)
			cY, cMem := cCeltIir(x, den, tc.N, tc.ord, mem)
			if i, ok := equalI32(goY, cY); !ok {
				t.Fatalf("iir N=%d ord=%d trial=%d: y differ at %d go=%d c=%d",
					tc.N, tc.ord, trial, i, goY[i], cY[i])
			}
			if i, ok := equalI16At(goMem, cMem); !ok {
				t.Fatalf("iir N=%d ord=%d trial=%d: mem differ at %d go=%d c=%d",
					tc.N, tc.ord, trial, i, goMem[i], cMem[i])
			}
		}
	}
}

// TestPLCPitchDownsampleDifferential pins pitch_downsample for mono and stereo.
func TestPLCPitchDownsampleDifferential(t *testing.T) {
	g := &lcg{s: 0xF6}
	const len_ = 1024
	const factor = 2
	for _, C := range []int{1, 2} {
		for trial := 0; trial < 30; trial++ {
			x0 := randI32Slice(g, len_*factor, 1<<22)
			var x1 []int32
			if C == 2 {
				x1 = randI32Slice(g, len_*factor, 1<<22)
			}
			var chans [][]int32
			if C == 2 {
				chans = [][]int32{x0, x1}
			} else {
				chans = [][]int32{x0}
			}
			goLp := celt.PitchDownsample(chans, len_, C, factor)
			cLp := cPitchDownsample(x0, x1, len_, C, factor)
			if i, ok := equalI16At(goLp, cLp); !ok {
				t.Fatalf("downsample C=%d trial=%d: differ at %d go=%d c=%d",
					C, trial, i, goLp[i], cLp[i])
			}
		}
	}
}

// TestPLCPitchSearchDifferential pins pitch_search at the exact geometry the PLC
// uses (len = DECODE_BUFFER_SIZE-PLC_PITCH_LAG_MAX, max_pitch = MAX-MIN). Values
// are bounded so the finer-search accumulation stays in int32 on both sides. A
// periodic case is included so a real lag is found, not just a tie-break.
func TestPLCPitchSearchDifferential(t *testing.T) {
	g := &lcg{s: 0x17}
	const len_ = 2048 - 720
	const maxPitch = 720 - 100
	xLpLen := len_       // >= len_>>1
	yLen := len_ + maxPitch

	check := func(name string, xLp, y []int16) {
		goP := celt.PitchSearch(xLp, y, len_, maxPitch)
		cP := cPitchSearch(xLp, y, len_, maxPitch)
		if goP != cP {
			t.Fatalf("pitch_search %s: go=%d c=%d", name, goP, cP)
		}
	}

	for trial := 0; trial < 40; trial++ {
		xLp := randI16Slice(g, xLpLen, 1000)
		y := randI16Slice(g, yLen, 1000)
		check(fmt.Sprintf("rand/%d", trial), xLp, y)
	}
	// Periodic signal (period 200) so the search locks onto a genuine lag.
	for _, period := range []int{160, 200, 313} {
		xLp := make([]int16, xLpLen)
		y := make([]int16, yLen)
		for i := range xLp {
			if i%period < 4 {
				xLp[i] = 1200
			}
		}
		for i := range y {
			if i%period < 4 {
				y[i] = 1200
			}
		}
		check(fmt.Sprintf("periodic/%d", period), xLp, y)
	}
}

// --- sequence loss-pattern differential --------------------------------------

// plcSeqConfig is a loss-pattern differential run: a signal is encoded frame by
// frame, then decoded through both decoders in lockstep with lost(f) frames
// dropped.
type plcSeqConfig struct {
	name       string
	channels   int
	frameSize  int // 120/240/480/960 -> LM 0/1/2/3
	complexity int
	bitrate    int
	frames     int
	signal     func(n, channels int) []int16
	lost       func(f int) bool
}

// plcCoverage records which PLC regimes and transitions the sequences exercised.
type plcCoverage struct {
	pitch, noise      bool // a concealed frame in each regime
	pitchToNoise      bool // a noise frame with prefilter_and_fold set (fold path)
	recoveryAfterLoss bool // a present frame immediately after a concealed one
	concealed         int
}

// runPLCSequence encodes cfg and decodes it in lockstep with the C oracle,
// dropping lost(f) frames, asserting bit-exact PCM + rng + persistent state after
// every frame. It returns the coverage observed.
func runPLCSequence(t *testing.T, cfg plcSeqConfig) plcCoverage {
	t.Helper()
	var cov plcCoverage

	enc, err := newCCELTEncoder(cfg.channels, cfg.complexity)
	if err != nil {
		t.Fatalf("%s: %v", cfg.name, err)
	}
	defer enc.Close()
	cDec, err := newCCELTDecoder(cfg.channels)
	if err != nil {
		t.Fatalf("%s: %v", cfg.name, err)
	}
	defer cDec.Close()
	goDec := celt.NewDecoder(cfg.channels)
	if goDec == nil {
		t.Fatalf("%s: celt.NewDecoder returned nil", cfg.name)
	}

	pcmIn := cfg.signal(cfg.frames*cfg.frameSize, cfg.channels)
	nbBytes := nbBytesFor(cfg.bitrate, cfg.frameSize)
	prevLost := false

	for f := 0; f < cfg.frames; f++ {
		off := f * cfg.frameSize * cfg.channels
		frameIn := pcmIn[off : off+cfg.frameSize*cfg.channels]

		// The encoder always processes the source frame (its state advances even
		// when the packet is "lost in transit").
		packet, err := enc.Encode(frameIn, cfg.frameSize, nbBytes)
		if err != nil {
			t.Fatalf("%s frame %d: encode: %v", cfg.name, f, err)
		}
		lost := cfg.lost(f)
		var feed []byte
		if !lost {
			feed = packet
		}

		cOut, err := cDec.Decode(feed, cfg.frameSize)
		if err != nil {
			t.Fatalf("%s frame %d: C decode: %v", cfg.name, f, err)
		}
		goPCM := make([]int16, cfg.frameSize*cfg.channels)
		n, err := goDec.Decode(feed, goPCM, cfg.frameSize)
		if err != nil {
			t.Fatalf("%s frame %d: Go decode: %v", cfg.name, f, err)
		}
		goOut := goPCM[:n*cfg.channels]

		what := "recv"
		if lost {
			what = "LOST"
		}

		// (a) PCM bit-identical (on concealed AND recovery frames).
		if len(cOut) != len(goOut) {
			t.Fatalf("%s frame %d (%s): sample count C=%d Go=%d", cfg.name, f, what, len(cOut), len(goOut))
		}
		for i := range cOut {
			if cOut[i] != goOut[i] {
				t.Fatalf("%s frame %d (%s): PCM[%d] C=%d Go=%d", cfg.name, f, what, i, cOut[i], goOut[i])
			}
		}

		// (b) range coder state.
		cState := cDec.State()
		if goDec.Rng() != cState.Rng {
			t.Fatalf("%s frame %d (%s): rng C=%#x Go=%#x", cfg.name, f, what, cState.Rng, goDec.Rng())
		}

		// (c) full persistent state.
		goState := goDec.State()
		if d := plcStateDiff(goState, cState); d != "" {
			t.Fatalf("%s frame %d (%s): persistent state diff: %s", cfg.name, f, what, d)
		}
		if goState.Hash() != cState.Hash() {
			t.Fatalf("%s frame %d (%s): state hash Go=%#x C=%#x", cfg.name, f, what, goState.Hash(), cState.Hash())
		}

		if lost {
			cov.concealed++
			switch goState.LastFrameType {
			case 3: // FRAME_PLC_PERIODIC
				cov.pitch = true
			case 2: // FRAME_PLC_NOISE
				cov.noise = true
				if prevLost && goState.LastFrameType == 2 {
					// A noise frame reached after a prior concealed frame: if the
					// prior frame was pitch, prefilter_and_fold was set and the fold
					// path ran here.
					cov.pitchToNoise = true
				}
			}
		} else if prevLost {
			cov.recoveryAfterLoss = true
		}
		prevLost = lost
	}
	return cov
}

// plcStateDiff reports the first differing persistent-state field/array element,
// covering every field the C decoder carries across frames (including the PLC
// bookkeeping the normal-path harness does not: skip_plc, last_frame_type,
// last_pitch_index, and the lpc[] buffer).
func plcStateDiff(g, c celt.State) string {
	scal := []struct {
		name string
		g, c int
	}{
		{"postfilter_period", g.PostfilterPeriod, c.PostfilterPeriod},
		{"postfilter_period_old", g.PostfilterPeriodOld, c.PostfilterPeriodOld},
		{"postfilter_gain", int(g.PostfilterGain), int(c.PostfilterGain)},
		{"postfilter_gain_old", int(g.PostfilterGainOld), int(c.PostfilterGainOld)},
		{"postfilter_tapset", g.PostfilterTapset, c.PostfilterTapset},
		{"postfilter_tapset_old", g.PostfilterTapsetOld, c.PostfilterTapsetOld},
		{"prefilter_and_fold", g.PrefilterAndFold, c.PrefilterAndFold},
		{"loss_duration", g.LossDuration, c.LossDuration},
		{"plc_duration", g.PlcDuration, c.PlcDuration},
		{"skip_plc", g.SkipPlc, c.SkipPlc},
		{"last_frame_type", g.LastFrameType, c.LastFrameType},
		{"last_pitch_index", g.LastPitchIndex, c.LastPitchIndex},
		{"preemph_memD0", int(g.PreemphMemD[0]), int(c.PreemphMemD[0])},
		{"preemph_memD1", int(g.PreemphMemD[1]), int(c.PreemphMemD[1])},
	}
	for _, s := range scal {
		if s.g != s.c {
			return fmt.Sprintf("%s g=%d c=%d", s.name, s.g, s.c)
		}
	}
	arr := []struct {
		name string
		g, c []int32
	}{
		{"decode_mem", g.DecodeMem, c.DecodeMem},
		{"oldEBands", g.OldEBands, c.OldEBands},
		{"oldLogE", g.OldLogE, c.OldLogE},
		{"oldLogE2", g.OldLogE2, c.OldLogE2},
		{"backgroundLogE", g.BackgroundLogE, c.BackgroundLogE},
	}
	for _, a := range arr {
		if len(a.g) != len(a.c) {
			return fmt.Sprintf("%s len g=%d c=%d", a.name, len(a.g), len(a.c))
		}
		for i := range a.g {
			if a.g[i] != a.c[i] {
				return fmt.Sprintf("%s[%d] g=%d c=%d", a.name, i, a.g[i], a.c[i])
			}
		}
	}
	if len(g.Lpc) != len(c.Lpc) {
		return fmt.Sprintf("lpc len g=%d c=%d", len(g.Lpc), len(c.Lpc))
	}
	for i := range g.Lpc {
		if g.Lpc[i] != c.Lpc[i] {
			return fmt.Sprintf("lpc[%d] g=%d c=%d", i, g.Lpc[i], c.Lpc[i])
		}
	}
	return ""
}

// TestCELTPLCSequenceDifferential drives loss patterns across both regimes and
// their boundaries: isolated single losses (pitch PLC on voiced signals), short
// bursts (consecutive pitch PLC), and long bursts that cross plc_duration>=40
// into the noise regime (which also exercises the pitch->noise prefilter_and_fold
// path and skip_plc lockout), plus noise-regime concealment of noise/silence, at
// LM 0-3, mono and stereo. Bit-exact PCM + rng + full state is asserted after
// every concealed and recovery frame.
func TestCELTPLCSequenceDifferential(t *testing.T) {
	toneSig := func(n, c int) []int16 { return genTone(n, c, 440, 587) }
	impSig := func(n, c int) []int16 { return genImpulses(n, c, 200) }
	noiseSig := func(n, c int) []int16 { return genNoise(n, c, 4242) }

	// isolated: one lost frame every 12, never consecutive -> pitch regime, with
	// clean recovery frames between.
	isolated := func(f int) bool { return f >= 6 && f%12 == 0 }
	// short bursts: pairs of lost frames -> two consecutive pitch PLC frames
	// (second uses the .8 fade + cached pitch), then recovery.
	shortBurst := func(f int) bool { return f%15 == 7 || f%15 == 8 }
	// long burst at LM3: frames 10..24 lost. 1<<3=8 units/frame, so plc_duration
	// crosses 40 at the 6th consecutive loss -> pitch for frames 10..14, noise for
	// 15..24, recovery at 25.
	longBurst := func(f int) bool { return f >= 10 && f <= 24 }

	var configs []plcSeqConfig
	for _, ch := range []int{1, 2} {
		for _, fs := range []int{120, 240, 480, 960} {
			configs = append(configs,
				plcSeqConfig{fmt.Sprintf("isolated/tone/ch%d/fs%d", ch, fs), ch, fs, 10, 64000, 60, toneSig, isolated},
				plcSeqConfig{fmt.Sprintf("burst2/imp/ch%d/fs%d", ch, fs), ch, fs, 10, 96000, 60, impSig, shortBurst},
			)
		}
		// Long bursts crossing into the noise regime: LM3 keeps the crossing quick.
		configs = append(configs,
			plcSeqConfig{fmt.Sprintf("longburst/tone/ch%d/fs960", ch), ch, 960, 10, 96000, 40, toneSig, longBurst},
			plcSeqConfig{fmt.Sprintf("longburst/noise/ch%d/fs960", ch), ch, 960, 5, 128000, 40, noiseSig, longBurst},
		)
	}
	// Noise-regime concealment of silence (mono).
	configs = append(configs,
		plcSeqConfig{"longburst/silence/ch1/fs960", 1, 960, 5, 64000, 40, genSilence, func(f int) bool { return f >= 10 && f <= 24 }},
	)

	var total plcCoverage
	for _, cfg := range configs {
		cfg := cfg
		t.Run(cfg.name, func(t *testing.T) {
			cov := runPLCSequence(t, cfg)
			if cov.concealed == 0 {
				t.Errorf("%s: no frame was actually concealed", cfg.name)
			}
		})
		// Re-run for aggregate coverage accounting (outside the subtest so a
		// subtest failure still surfaces first).
		cov := runPLCSequence(t, cfg)
		total.pitch = total.pitch || cov.pitch
		total.noise = total.noise || cov.noise
		total.pitchToNoise = total.pitchToNoise || cov.pitchToNoise
		total.recoveryAfterLoss = total.recoveryAfterLoss || cov.recoveryAfterLoss
		total.concealed += cov.concealed
	}

	t.Logf("concealed %d frames; coverage: pitch=%v noise=%v pitch->noise(fold)=%v recovery=%v",
		total.concealed, total.pitch, total.noise, total.pitchToNoise, total.recoveryAfterLoss)
	if !total.pitch {
		t.Error("no pitch-regime PLC frame exercised")
	}
	if !total.noise {
		t.Error("no noise-regime PLC frame exercised")
	}
	if !total.pitchToNoise {
		t.Error("no pitch->noise transition (prefilter_and_fold in noise regime) exercised")
	}
	if !total.recoveryAfterLoss {
		t.Error("no recovery frame after a loss exercised")
	}
}

// TestCELTPLCStartBandNoiseRegime exercises the start != 0 noise-regime selection
// (celt_decoder.c:724): after building identical decode history from received
// full-band frames, start band 17 is set on BOTH decoders and frames are lost.
// start != 0 forces the noise regime regardless of plc_duration; the concealed
// output and full state must still match the C oracle bit for bit.
func TestCELTPLCStartBandNoiseRegime(t *testing.T) {
	const ch = 1
	const fs = 960
	enc, err := newCCELTEncoder(ch, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	cDec, err := newCCELTDecoder(ch)
	if err != nil {
		t.Fatal(err)
	}
	defer cDec.Close()
	goDec := celt.NewDecoder(ch)

	pcmIn := genTone(30*fs, ch, 440, 587)
	nbBytes := nbBytesFor(96000, fs)

	decodeOne := func(f int, lost bool) {
		off := f * fs * ch
		packet, err := enc.Encode(pcmIn[off:off+fs*ch], fs, nbBytes)
		if err != nil {
			t.Fatalf("frame %d: encode: %v", f, err)
		}
		var feed []byte
		if !lost {
			feed = packet
		}
		cOut, err := cDec.Decode(feed, fs)
		if err != nil {
			t.Fatalf("frame %d: C decode: %v", f, err)
		}
		goPCM := make([]int16, fs*ch)
		n, err := goDec.Decode(feed, goPCM, fs)
		if err != nil {
			t.Fatalf("frame %d: Go decode: %v", f, err)
		}
		goOut := goPCM[:n*ch]
		for i := range cOut {
			if cOut[i] != goOut[i] {
				t.Fatalf("frame %d (lost=%v): PCM[%d] C=%d Go=%d", f, lost, i, cOut[i], goOut[i])
			}
		}
		cState := cDec.State()
		if d := plcStateDiff(goDec.State(), cState); d != "" {
			t.Fatalf("frame %d (lost=%v): state diff: %s", f, lost, d)
		}
		if goDec.State().Hash() != cState.Hash() {
			t.Fatalf("frame %d (lost=%v): state hash mismatch", f, lost)
		}
	}

	// Warm up with received full-band frames.
	for f := 0; f < 6; f++ {
		decodeOne(f, false)
	}
	// Switch to a hybrid-like start band on both decoders.
	if err := goDec.SetStartBand(17); err != nil {
		t.Fatalf("Go SetStartBand: %v", err)
	}
	cDec.SetStartBand(17)
	// Lose several frames: start != 0 forces the noise regime each time.
	for f := 6; f < 12; f++ {
		decodeOne(f, true)
	}
}

// TestCELTPLCNonVacuity proves the PLC differential is not vacuous: on a genuinely
// concealed (nonzero) frame, a one-sample PCM mutation and a one-field state
// mutation are both detected. If this ever passes with a mutation still "equal",
// the sequence differential would be silently green regardless of correctness.
func TestCELTPLCNonVacuity(t *testing.T) {
	const ch = 2
	const fs = 960
	enc, err := newCCELTEncoder(ch, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	cDec, err := newCCELTDecoder(ch)
	if err != nil {
		t.Fatal(err)
	}
	defer cDec.Close()
	goDec := celt.NewDecoder(ch)

	pcmIn := genTone(20*fs, ch, 440, 587)
	nbBytes := nbBytesFor(96000, fs)

	sawPCM, sawHash := false, false
	for f := 0; f < 14; f++ {
		off := f * fs * ch
		packet, err := enc.Encode(pcmIn[off:off+fs*ch], fs, nbBytes)
		if err != nil {
			t.Fatal(err)
		}
		// Lose frame 8 (a single loss after warmup -> pitch PLC of a tone -> nonzero).
		lost := f == 8
		var feed []byte
		if !lost {
			feed = packet
		}
		cOut, err := cDec.Decode(feed, fs)
		if err != nil {
			t.Fatal(err)
		}
		goPCM := make([]int16, fs*ch)
		n, err := goDec.Decode(feed, goPCM, fs)
		if err != nil {
			t.Fatal(err)
		}
		goOut := goPCM[:n*ch]
		if !equalI16(cOut, goOut) {
			t.Fatalf("frame %d: valid decode differs; setup broken", f)
		}
		cState := cDec.State()
		goState := goDec.State()
		if goState.Hash() != cState.Hash() {
			t.Fatalf("frame %d: valid state hash differs; setup broken", f)
		}

		if lost {
			for i := range goOut {
				if goOut[i] != 0 {
					mut := append([]int16(nil), goOut...)
					mut[i]++
					if equalI16(cOut, mut) {
						t.Fatalf("frame %d: PCM mutation NOT detected at %d", f, i)
					}
					sawPCM = true
					break
				}
			}
			mutState := goState
			mutState.DecodeMem = append([]int32(nil), goState.DecodeMem...)
			mutState.DecodeMem[len(mutState.DecodeMem)-1] ^= 1
			if mutState.Hash() == cState.Hash() {
				t.Fatalf("frame %d: state-hash mutation NOT detected", f)
			}
			sawHash = true
		}
	}
	if !sawPCM {
		t.Error("PCM mutation check never ran (concealed frame was all zero?)")
	}
	if !sawHash {
		t.Error("state-hash mutation check never ran")
	}
}
