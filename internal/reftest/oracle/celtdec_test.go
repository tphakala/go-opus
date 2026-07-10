//go:build refc

package oracle

import (
	"fmt"
	"math"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// This SEQUENCE-based differential test pins the pure-Go top-level CELT decoder
// (internal/celt: celt_decode_with_ec, celt_synthesis, comb_filter, deemphasis,
// tf_decode) to the pinned libopus C decoder (celt/celt_decoder.c). The CELT
// decoder is a cross-frame state machine (docs/hard-parts.md section 5), so
// single-frame tests cannot validate it: instead multi-second frame SEQUENCES
// are encoded with the raw C CELT encoder, then decoded through BOTH the C and
// Go decoders IN LOCKSTEP, asserting after EVERY frame that (a) the int16 PCM is
// bit-identical, (b) the range coder state (rng) matches, and (c) a hash of the
// full persistent decoder state (decode_mem overlap history, oldEBands/oldLogE/
// oldLogE2/backgroundLogE, postfilter/preemph memories) matches. Any divergence
// is therefore caught on the frame it first appears, not frames later.

const celtSampleRate = 48000

// --- deterministic signal generators (interleaved int16, n samples/channel) --

// lcg is a small deterministic PRNG so the test is reproducible without seeding
// the global rand and independent of Go's rand implementation.
type lcg struct{ s uint64 }

func (g *lcg) next() uint32 {
	g.s = g.s*6364136223846793005 + 1442695040888963407
	return uint32(g.s >> 32)
}

func (g *lcg) sym() float64 { return float64(int32(g.next()))/2147483648.0 }

// genTone produces steady sine tones (distinct per channel), which drive the
// encoder to enable the pitch post-filter.
func genTone(n, channels int, freqL, freqR float64) []int16 {
	out := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		l := int16(9000 * math.Sin(2*math.Pi*freqL*float64(i)/celtSampleRate))
		out[i*channels] = l
		if channels == 2 {
			out[i*channels+1] = int16(9000 * math.Sin(2*math.Pi*freqR*float64(i)/celtSampleRate))
		}
	}
	return out
}

// genSweep produces a linear chirp sweeping across the band.
func genSweep(n, channels int) []int16 {
	out := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		t := float64(i) / celtSampleRate
		f := 200.0 + 8000.0*t/(float64(n)/celtSampleRate)
		v := int16(8000 * math.Sin(2*math.Pi*f*t))
		out[i*channels] = v
		if channels == 2 {
			out[i*channels+1] = int16(float64(v) * 0.8)
		}
	}
	return out
}

// genNoise produces full-band white noise.
func genNoise(n, channels int, seed uint64) []int16 {
	g := lcg{s: seed}
	out := make([]int16, n*channels)
	for i := range out {
		out[i] = int16(g.sym() * 9000)
	}
	return out
}

// genSilence produces digital silence (exercises the CELT silence flag path).
func genSilence(n, channels int) []int16 { return make([]int16, n*channels) }

// genImpulses produces periodic loud impulses over a quiet bed, which reliably
// triggers the encoder's transient detection (short blocks) at LM>=1 and, at
// LM>=2, the anti-collapse reservation.
func genImpulses(n, channels, period int) []int16 {
	g := lcg{s: 0x9e3779b97f4a7c15}
	out := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		var v int16
		if i%period == 0 {
			v = 30000
		} else {
			v = int16(g.sym() * 500)
		}
		out[i*channels] = v
		if channels == 2 {
			out[i*channels+1] = -v
		}
	}
	return out
}

// genBursts alternates ~30 ms of silence and loud noise, forcing transients and
// post-filter changes at frame boundaries.
func genBursts(n, channels int) []int16 {
	g := lcg{s: 0xdeadbeefcafef00d}
	out := make([]int16, n*channels)
	block := celtSampleRate / 33
	for i := 0; i < n; i++ {
		var v int16
		if (i/block)%2 == 0 {
			v = int16(g.sym() * 12000)
		}
		out[i*channels] = v
		if channels == 2 {
			out[i*channels+1] = int16(g.sym() * float64(v) / 12000 * 12000)
		}
	}
	return out
}

// --- test configuration ------------------------------------------------------

type seqConfig struct {
	name       string
	channels   int
	frameSize  int // 120/240/480/960 -> LM 0/1/2/3
	complexity int
	bitrate    int // bits/s target -> CBR bytes/frame
	frames     int
	signal     func(n, channels int) []int16
}

// nbBytesFor computes the CBR byte budget per frame for a bitrate, clamped to a
// valid CELT packet size (>=8 so len>1 keeps the decoder off the PLC path).
func nbBytesFor(bitrate, frameSize int) int {
	b := bitrate * frameSize / (celtSampleRate * 8)
	if b < 8 {
		b = 8
	}
	if b > 1275 {
		b = 1275
	}
	return b
}

// coverage tracks which special decode paths the sequences exercised.
type coverage struct {
	transient, postfilter, antiCollapse, silence bool
	frames                                       int
}

// runSequence encodes and decodes one config in lockstep, asserting bit-exact
// agreement after every frame. It returns the coverage observed.
func runSequence(t *testing.T, cfg seqConfig) coverage {
	t.Helper()
	var cov coverage

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

	nSamples := cfg.frames * cfg.frameSize
	pcmIn := cfg.signal(nSamples, cfg.channels)
	nbBytes := nbBytesFor(cfg.bitrate, cfg.frameSize)

	for f := 0; f < cfg.frames; f++ {
		off := f * cfg.frameSize * cfg.channels
		frameIn := pcmIn[off : off+cfg.frameSize*cfg.channels]

		packet, err := enc.Encode(frameIn, cfg.frameSize, nbBytes)
		if err != nil {
			t.Fatalf("%s frame %d: encode: %v", cfg.name, f, err)
		}
		if len(packet) <= 1 {
			t.Fatalf("%s frame %d: encoder produced len<=1 packet (%d), would hit PLC",
				cfg.name, f, len(packet))
		}

		cOut, err := cDec.Decode(packet, cfg.frameSize)
		if err != nil {
			t.Fatalf("%s frame %d: C decode: %v", cfg.name, f, err)
		}

		goPCM := make([]int16, cfg.frameSize*cfg.channels)
		n, err := goDec.Decode(packet, goPCM, cfg.frameSize)
		if err != nil {
			t.Fatalf("%s frame %d: Go decode: %v", cfg.name, f, err)
		}
		goOut := goPCM[:n*cfg.channels]

		// (a) PCM bit-identical.
		if len(cOut) != len(goOut) {
			t.Fatalf("%s frame %d: sample count C=%d Go=%d", cfg.name, f, len(cOut), len(goOut))
		}
		for i := range cOut {
			if cOut[i] != goOut[i] {
				t.Fatalf("%s frame %d: PCM[%d] C=%d Go=%d (packet %d bytes)",
					cfg.name, f, i, cOut[i], goOut[i], len(packet))
			}
		}

		// (b) range coder state.
		cState := cDec.State()
		if goDec.Rng() != cState.Rng {
			t.Fatalf("%s frame %d: rng C=%#x Go=%#x", cfg.name, f, cState.Rng, goDec.Rng())
		}

		// (c) persistent decoder state: exact arrays (debuggable) then hash.
		goState := goDec.State()
		if d := firstStateDiff(goState, cState); d != "" {
			t.Fatalf("%s frame %d: persistent state diff: %s", cfg.name, f, d)
		}
		if goState.Hash() != cState.Hash() {
			t.Fatalf("%s frame %d: state hash Go=%#x C=%#x (arrays equal but hash differs?!)",
				cfg.name, f, goState.Hash(), cState.Hash())
		}

		cov.frames++
		cov.transient = cov.transient || goDec.LastFrameTransient()
		cov.postfilter = cov.postfilter || goDec.LastFramePostfilter()
		cov.antiCollapse = cov.antiCollapse || goDec.LastFrameAntiCollapse()
		if cState.OldEBands[0] == int32(-(28 << 24)) { // silence forces oldBandE to -GCONST(28)
			cov.silence = true
		}
	}
	return cov
}

// firstStateDiff returns a human-readable description of the first field/array
// element that differs between the Go and C persistent state, or "" if equal.
func firstStateDiff(g, c celt.State) string {
	if g.Rng != c.Rng {
		return fmt.Sprintf("rng g=%#x c=%#x", g.Rng, c.Rng)
	}
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
	return ""
}

// TestCELTDecodeSequenceDifferential runs the lockstep differential over a matrix
// of channel counts, frame sizes (LM 0-3), bitrates, complexities, and signal
// types, asserting bit-exact PCM + rng + state after every frame.
func TestCELTDecodeSequenceDifferential(t *testing.T) {
	// ~1.5-2 s of audio per config (frames scaled by frame size).
	framesFor := func(frameSize int) int { return int(2 * celtSampleRate / frameSize) }

	var configs []seqConfig
	frameSizes := []int{120, 240, 480, 960}
	for _, ch := range []int{1, 2} {
		for _, fs := range frameSizes {
			nf := framesFor(fs)
			configs = append(configs,
				seqConfig{fmt.Sprintf("tone/ch%d/fs%d", ch, fs), ch, fs, 10, 64000, nf, func(n, c int) []int16 { return genTone(n, c, 440, 587) }},
				seqConfig{fmt.Sprintf("sweep/ch%d/fs%d", ch, fs), ch, fs, 8, 96000, nf, genSweep},
				seqConfig{fmt.Sprintf("noise/ch%d/fs%d", ch, fs), ch, fs, 5, 128000, nf, func(n, c int) []int16 { return genNoise(n, c, 12345) }},
				seqConfig{fmt.Sprintf("impulses/ch%d/fs%d", ch, fs), ch, fs, 10, 80000, nf, func(n, c int) []int16 { return genImpulses(n, c, 200) }},
				seqConfig{fmt.Sprintf("bursts/ch%d/fs%d", ch, fs), ch, fs, 9, 110000, nf, genBursts},
				seqConfig{fmt.Sprintf("silence/ch%d/fs%d", ch, fs), ch, fs, 5, 64000, nf, genSilence},
				seqConfig{fmt.Sprintf("lowrate/ch%d/fs%d", ch, fs), ch, fs, 3, 12000, nf, genSweep},
			)
		}
	}

	var total coverage
	for _, cfg := range configs {
		cfg := cfg
		t.Run(cfg.name, func(t *testing.T) {
			cov := runSequence(t, cfg)
			total.frames += cov.frames
			total.transient = total.transient || cov.transient
			total.postfilter = total.postfilter || cov.postfilter
			total.antiCollapse = total.antiCollapse || cov.antiCollapse
			total.silence = total.silence || cov.silence
		})
	}

	t.Logf("decoded %d frames across %d configs; coverage: transient=%v postfilter=%v antiCollapse=%v silence=%v",
		total.frames, len(configs), total.transient, total.postfilter, total.antiCollapse, total.silence)
	if !total.transient {
		t.Error("no transient frame exercised across the whole matrix")
	}
	if !total.postfilter {
		t.Error("no post-filter-active frame exercised across the whole matrix")
	}
	if !total.antiCollapse {
		t.Error("no anti-collapse frame exercised across the whole matrix")
	}
	if !total.silence {
		t.Error("no silence frame exercised across the whole matrix")
	}
}

// TestCELTDecodeSequenceNonVacuity proves the differential comparison is not
// vacuous: a valid Go decode matches the C decode, but a one-sample / one-field
// mutation of that result is detected by both the PCM comparison and the state
// hash. If this test ever passes with the mutation still "equal", the harness
// above would be silently green regardless of correctness.
func TestCELTDecodeSequenceNonVacuity(t *testing.T) {
	cfg := seqConfig{"nonvacuity", 2, 960, 10, 96000, 8,
		func(n, c int) []int16 { return genImpulses(n, c, 200) }}

	enc, err := newCCELTEncoder(cfg.channels, cfg.complexity)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	cDec, err := newCCELTDecoder(cfg.channels)
	if err != nil {
		t.Fatal(err)
	}
	defer cDec.Close()
	goDec := celt.NewDecoder(cfg.channels)

	pcmIn := cfg.signal(cfg.frames*cfg.frameSize, cfg.channels)
	nbBytes := nbBytesFor(cfg.bitrate, cfg.frameSize)

	sawPCMDiffDetected := false
	sawHashDiffDetected := false

	for f := 0; f < cfg.frames; f++ {
		off := f * cfg.frameSize * cfg.channels
		frameIn := pcmIn[off : off+cfg.frameSize*cfg.channels]
		packet, err := enc.Encode(frameIn, cfg.frameSize, nbBytes)
		if err != nil {
			t.Fatal(err)
		}
		cOut, err := cDec.Decode(packet, cfg.frameSize)
		if err != nil {
			t.Fatal(err)
		}
		goPCM := make([]int16, cfg.frameSize*cfg.channels)
		n, err := goDec.Decode(packet, goPCM, cfg.frameSize)
		if err != nil {
			t.Fatal(err)
		}
		goOut := goPCM[:n*cfg.channels]

		// Real decode must match.
		if !equalI16(cOut, goOut) {
			t.Fatalf("frame %d: valid decode already differs; test setup broken", f)
		}
		cState := cDec.State()
		goState := goDec.State()
		if goState.Hash() != cState.Hash() {
			t.Fatalf("frame %d: valid decode state hash already differs", f)
		}

		// Mutate a copy of the Go PCM by one LSB on a nonzero sample and confirm
		// the comparison flags it.
		for i := range goOut {
			if goOut[i] != 0 {
				mut := append([]int16(nil), goOut...)
				mut[i]++
				if equalI16(cOut, mut) {
					t.Fatalf("frame %d: PCM mutation NOT detected at %d", f, i)
				}
				sawPCMDiffDetected = true
				break
			}
		}

		// Mutate one persistent-state field and confirm the hash changes.
		mutState := goState
		mutState.OldEBands = append([]int32(nil), goState.OldEBands...)
		mutState.OldEBands[0] ^= 1
		if mutState.Hash() == cState.Hash() {
			t.Fatalf("frame %d: state-hash mutation NOT detected", f)
		}
		sawHashDiffDetected = true
	}

	if !sawPCMDiffDetected {
		t.Error("mutation check never ran: no nonzero PCM sample produced")
	}
	if !sawHashDiffDetected {
		t.Error("state-hash mutation check never ran")
	}
}

func equalI16(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
