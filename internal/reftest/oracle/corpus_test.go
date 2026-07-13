//go:build refc

package oracle

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// CP10 CORPUS. The signal bank the phase 4 gate (opusenc_gate_test.go) sweeps its
// grid over. THREE TIERS, none of which downloads anything or adds a byte of
// testdata:
//
//  1. A SYNTHETIC BANK, committed as Go code. It is built ENTIRELY out of the
//     generators that already exist in celtdec_test.go (genTone, genSweep,
//     genNoise, genSilence, genImpulses, genBursts) plus a handful of pure
//     TRANSFORMS declared below (gain, DC offset, a pink-noise filter, a segmented
//     amplitude envelope, and the four stereo images). No new signal generator is
//     written here: the transforms only reshape what the existing generators
//     produce.
//
//  2. REAL AUDIO, ALREADY ON DISK: the RFC 8251 test vectors' DECODED references,
//     testdata/vectors/opus_newvectors/testvectorNN.dec. They are 48 kHz
//     interleaved-stereo int16 PCM (this is exactly how opus/conformance_test.go
//     reads them and compares them against a 2-channel decode), 12 clips of 21 to
//     32 seconds of genuine speech and music. sha256-pinned by
//     scripts/fetch-vectors.sh and gitignored, so the tests SKIP cleanly when they
//     are absent.
//
//  3. An OPTIONAL local corpus at testdata/corpus/, swept when present and skipped
//     when not.
//
// WHY THE MONO RFC CLIPS COME FROM .dec AND NOT FROM .m.dec. testvectorNNm.dec is
// the same byte length as testvectorNN.dec and is NOT a plain mono PCM file: it is
// stored in opus_compare's "the reference is always interleaved stereo"
// convention, which internal/opuscompare reproduces (compare.go:143-152, it
// downmixes 0.5*(L+R) on the fly). Feeding it to an encoder as raw PCM would be
// guessing at its layout. testvectorNN.dec's layout, by contrast, is pinned by the
// conformance gate, so the mono clips are its LEFT CHANNEL, deinterleaved. That is
// genuine real audio and it is unambiguous.

// corpusRate is the corpus sample rate. The encoder is bit-exact at 8/12/16/24/48
// kHz (opusenc_samplerate_test.go is that gate); the corpus itself is 48 kHz
// because that is what the RFC references are, and mixing rates into the grid
// would multiply it without adding a decision branch the rate sweep does not
// already cover.
const corpusRate = 48000

// corpusSeconds is the length of every TIER A clip. Two seconds is 100 frames at
// 20 ms and 800 at 2.5 ms, i.e. long enough that every cross-frame state machine
// (the delay ring, hp_mem, the CELT energy history, the VBR reservoir, the
// bandwidth and stereo hysteresis) is exercised as a SEQUENCE and not as a
// single-shot.
const corpusSeconds = 2

// corpusLen is the TIER A clip length in samples per channel.
const corpusLen = corpusRate * corpusSeconds

// corpusClip is one corpus item: interleaved int16 PCM at corpusRate.
type corpusClip struct {
	name     string
	channels int
	pcm      []int16
}

// samplesPerChannel is the clip length.
func (c corpusClip) samplesPerChannel() int { return len(c.pcm) / c.channels }

// countFrames is how many whole frameSize frames the clip holds.
func (c corpusClip) countFrames(frameSize int) int {
	return c.samplesPerChannel() / frameSize
}

// frameAt returns frame idx as an interleaved slice. It aliases the clip, which is
// safe: the encoders only read their input.
func (c corpusClip) frameAt(idx, frameSize int) []int16 {
	off := idx * frameSize * c.channels
	return c.pcm[off : off+frameSize*c.channels : off+frameSize*c.channels]
}

// ---------------------------------------------------------------------------
// Transforms. NOT generators: each one reshapes a signal an existing generator
// already produced.
// ---------------------------------------------------------------------------

// sat16 saturates to the int16 range, so a transform that overshoots CLIPS the way
// a real overloaded input does rather than wrapping.
func sat16(v float64) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}

// gainPCM scales a signal, saturating. A gain above unity is how the full-scale /
// clipping clip is built; a gain far below unity is how the -80 dBFS near-silence
// clip is built.
func gainPCM(in []int16, g float64) []int16 {
	out := make([]int16, len(in))
	for i, v := range in {
		out[i] = sat16(float64(v) * g)
	}
	return out
}

// offsetPCM adds a constant. Applied to genSilence it is pure DC, which is what
// the encoder's dc_reject high-pass is there for.
func offsetPCM(in []int16, dc int16) []int16 {
	out := make([]int16, len(in))
	for i, v := range in {
		out[i] = sat16(float64(v) + float64(dc))
	}
	return out
}

// pinkify turns white noise into pink noise (Paul Kellett's one-pole cascade),
// per channel. Pink noise has the 1/f spectral tilt of real program material, so
// it drives the band allocation and dynalloc away from white noise's flat answer.
func pinkify(in []int16, channels int) []int16 {
	out := make([]int16, len(in))
	var b [2][7]float64
	n := len(in) / channels
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			w := float64(in[i*channels+c])
			s := &b[c]
			s[0] = 0.99886*s[0] + w*0.0555179
			s[1] = 0.99332*s[1] + w*0.0750759
			s[2] = 0.96900*s[2] + w*0.1538520
			s[3] = 0.86650*s[3] + w*0.3104856
			s[4] = 0.55000*s[4] + w*0.5329522
			s[5] = -0.7616*s[5] - w*0.0168980
			v := s[0] + s[1] + s[2] + s[3] + s[4] + s[5] + s[6] + w*0.5362
			s[6] = w * 0.115926
			out[i*channels+c] = sat16(v * 0.4)
		}
	}
	return out
}

// envelopeStep cuts the signal into four segments with abruptly different gains.
// The steps are what dynalloc and the transient detector see; a steady-state clip
// never shows them.
func envelopeStep(in []int16, channels int) []int16 {
	gains := [4]float64{0.003, 1.0, 0.01, 1.0}
	out := make([]int16, len(in))
	n := len(in) / channels
	for i := 0; i < n; i++ {
		g := gains[(i*4)/n]
		for c := 0; c < channels; c++ {
			out[i*channels+c] = sat16(float64(in[i*channels+c]) * g)
		}
	}
	return out
}

// stereoImage renders a MONO signal into one of the four stereo images the mid/side
// machinery has to survive:
//
//	"identical"  L == R: mid carries everything, side is exactly zero (intensity
//	             stereo and the dual-stereo decision).
//	"antiphase"  R == -L: MID is exactly zero and side carries everything, which is
//	             the mid/side worst case.
//	"left"       R == 0: half the energy in each of mid and side.
//	"pan"        a slowly rotating pan, so the mid/side balance MOVES across the
//	             clip rather than sitting on one answer.
func stereoImage(mono []int16, image string) []int16 {
	n := len(mono)
	out := make([]int16, n*2)
	for i := 0; i < n; i++ {
		l, r := float64(mono[i]), 0.0
		switch image {
		case "identical":
			r = l
		case "antiphase":
			r = -l
		case "left":
			r = 0
		case "pan":
			// One full rotation over the clip. cos/sin are taken from the existing
			// sine table rather than math.Sin so this stays a transform.
			ph := (i * 256) / n
			c := float64(sineTab[(ph+64)&255]) / 6000.0
			s := float64(sineTab[ph&255]) / 6000.0
			r = l * s
			l *= c
		default:
			panic("unknown stereo image " + image)
		}
		out[i*2] = sat16(l)
		out[i*2+1] = sat16(r)
	}
	return out
}

// deinterleaveLeft extracts the left channel of an interleaved stereo signal.
func deinterleaveLeft(stereo []int16) []int16 {
	out := make([]int16, len(stereo)/2)
	for i := range out {
		out[i] = stereo[2*i]
	}
	return out
}

// ---------------------------------------------------------------------------
// TIER 1: the synthetic bank.
// ---------------------------------------------------------------------------

// syntheticBank is 28 deterministic 2 s clips, 14 mono and 14 stereo. Every one of
// them is there because it lands on a decision the others do not:
//
//	silence      the CELT silence flag and, under VBR, the 2-byte shrink
//	nearsilence  -80 dBFS: NOT silent, but low enough to drive the energy floor and
//	             the VBR rate cut, which the silence fast path would hide
//	dc           dc_reject's whole reason to exist
//	white        flat spectrum: the band allocation's null hypothesis
//	pink         the 1/f tilt of real program material
//	sweep        every band in turn, so no band is ever permanently empty
//	tone-*       tones parked ON CELT band edges (4800 and 15600 Hz are eBands
//	             boundaries at 48 kHz / 20 ms) and one just under the top edge:
//	             energy split exactly across a band boundary is the leakage case
//	impulses     transient detection, the prefilter, and the anti-collapse reserve
//	bursts       transients AT frame boundaries plus post-filter switching
//	ampstep      abrupt segment gain changes: dynalloc
//	fullscale    a tone driven past full scale, i.e. a hard-clipped input
//	stereo       identical / antiphase / left-only / rotating pan, plus a natively
//	             decorrelated pair, which are the four corners of the mid/side and
//	             intensity decisions
func syntheticBank() []corpusClip {
	const n = corpusLen

	whiteMono := genNoise(n, 1, 0xC0FFEE)
	sweepMono := genSweep(n, 1)
	toneMono := genTone(n, 1, 1000, 1000)

	mono := []corpusClip{
		{"syn-mono/silence", 1, genSilence(n, 1)},
		// 32768 * 10^(-80/20) == 3.28, and genNoise peaks at 9000.
		{"syn-mono/nearsilence-80dbfs", 1, gainPCM(genNoise(n, 1, 0x5EED), 3.28/9000)},
		{"syn-mono/dc", 1, offsetPCM(genSilence(n, 1), 8000)},
		{"syn-mono/white", 1, whiteMono},
		{"syn-mono/pink", 1, pinkify(genNoise(n, 1, 0xBEEF), 1)},
		{"syn-mono/sweep", 1, sweepMono},
		{"syn-mono/tone-1000", 1, toneMono},
		{"syn-mono/bandedge-4800", 1, genTone(n, 1, 4800, 4800)},
		{"syn-mono/bandedge-15600", 1, genTone(n, 1, 15600, 15600)},
		{"syn-mono/tone-19980", 1, genTone(n, 1, 19980, 19980)},
		{"syn-mono/impulses", 1, genImpulses(n, 1, 480)},
		{"syn-mono/bursts", 1, genBursts(n, 1)},
		{"syn-mono/ampstep", 1, envelopeStep(toneMono, 1)},
		{"syn-mono/fullscale", 1, gainPCM(genTone(n, 1, 997, 997), 4.0)},
	}

	stereo := []corpusClip{
		{"syn-stereo/silence", 2, genSilence(n, 2)},
		{"syn-stereo/nearsilence-80dbfs", 2, gainPCM(genNoise(n, 2, 0x5EED), 3.28/9000)},
		{"syn-stereo/dc", 2, offsetPCM(genSilence(n, 2), 8000)},
		{"syn-stereo/white-decorrelated", 2, genNoise(n, 2, 0xC0FFEE)},
		{"syn-stereo/pink", 2, pinkify(genNoise(n, 2, 0xBEEF), 2)},
		{"syn-stereo/sweep", 2, genSweep(n, 2)},
		{"syn-stereo/tones-lr", 2, genTone(n, 2, 1000, 1500)},
		{"syn-stereo/impulses-antiphase", 2, genImpulses(n, 2, 480)},
		{"syn-stereo/bursts", 2, genBursts(n, 2)},
		{"syn-stereo/identical-lr", 2, stereoImage(whiteMono, "identical")},
		{"syn-stereo/antiphase", 2, stereoImage(whiteMono, "antiphase")},
		{"syn-stereo/left-only", 2, stereoImage(sweepMono, "left")},
		{"syn-stereo/pan-rotate", 2, stereoImage(toneMono, "pan")},
		{"syn-stereo/fullscale", 2, gainPCM(genTone(n, 2, 997, 1499), 4.0)},
	}

	return append(mono, stereo...)
}

// ---------------------------------------------------------------------------
// TIER 2: the RFC vectors' decoded references, as real audio.
// ---------------------------------------------------------------------------

// rfcVectorsDir is the extracted RFC 8251 vector set, relative to this package.
const rfcVectorsDir = "../../../testdata/vectors/opus_newvectors"

// rfcVectorNames are the 12 vectors. Their .dec references cover speech (male and
// female, several languages), music, and mixed material, which is exactly the
// "speech, music" corpus the plan asks for.
var rfcVectorNames = []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12"}

// readInt16LE reads a little-endian int16 PCM file.
func readInt16LE(path string) ([]int16, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed, checked-in test path
	if err != nil {
		return nil, err
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out, nil
}

// rfcVectorsPresent reports whether the fetched vector set is on disk.
func rfcVectorsPresent() bool {
	_, err := os.Stat(rfcVectorsDir)
	return err == nil
}

// rfcCache memoizes the decoded references: they are 5 MB each and every gate tier
// wants them.
var rfcCache struct {
	sync.Mutex
	m map[string][]int16
}

// loadRFCStereo reads testvectorNN.dec: 48 kHz INTERLEAVED STEREO int16 (the same
// layout opus/conformance_test.go compares against a 2-channel decode).
func loadRFCStereo(t *testing.T, name string) []int16 {
	t.Helper()
	rfcCache.Lock()
	defer rfcCache.Unlock()
	if rfcCache.m == nil {
		rfcCache.m = map[string][]int16{}
	}
	if pcm, ok := rfcCache.m[name]; ok {
		return pcm
	}
	pcm, err := readInt16LE(filepath.Join(rfcVectorsDir, "testvector"+name+".dec"))
	if err != nil {
		t.Fatalf("read RFC vector %s: %v", name, err)
	}
	rfcCache.m[name] = pcm
	return pcm
}

// firstLoudFrame finds the first frame boundary at or before which the signal has
// really started, so a 2 s excerpt does not land on the leading digital silence
// that several vectors begin with (that silence is already in the synthetic bank).
// The offset is quantized to 960 samples so every frame size lands on it.
func firstLoudFrame(pcm []int16, channels, need int) int {
	total := len(pcm) / channels
	if total <= need {
		return 0
	}
	for i := 0; i < total; i++ {
		v := pcm[i*channels]
		if v > 2000 || v < -2000 {
			start := (i / 960) * 960
			if start+need > total {
				start = ((total - need) / 960) * 960
			}
			return start
		}
	}
	return 0
}

// rfcClips returns one excerpt per RFC vector, in BOTH stereo (the .dec as it
// stands) and mono (its left channel). seconds <= 0 means the whole vector.
func rfcClips(t *testing.T, seconds int) []corpusClip {
	t.Helper()
	var out []corpusClip
	for _, name := range rfcVectorNames {
		stereo := loadRFCStereo(t, name)
		total := len(stereo) / 2
		need := total
		if seconds > 0 && seconds*corpusRate < total {
			need = seconds * corpusRate
		}
		start := 0
		if need < total {
			start = firstLoudFrame(stereo, 2, need)
		}
		ex := stereo[start*2 : (start+need)*2]
		out = append(out,
			corpusClip{"rfc" + name + "-stereo", 2, ex},
			corpusClip{"rfc" + name + "-mono", 1, deinterleaveLeft(ex)},
		)
	}
	return out
}

// ---------------------------------------------------------------------------
// TIER 3: the optional local corpus.
// ---------------------------------------------------------------------------

// localCorpusDir is swept when it exists and ignored when it does not. Files are
// read as 48 kHz interleaved-stereo int16 LE (the raw format the rest of the
// corpus uses); each yields a stereo clip and its left-channel mono variant.
const localCorpusDir = "../../../testdata/corpus"

// localClips returns the local corpus, or nil when the directory is absent.
func localClips(t *testing.T, seconds int) []corpusClip {
	t.Helper()
	entries, err := os.ReadDir(localCorpusDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".raw", ".pcm", ".s16", ".dec":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []corpusClip
	for _, n := range names {
		pcm, err := readInt16LE(filepath.Join(localCorpusDir, n))
		if err != nil {
			t.Fatalf("read local corpus %s: %v", n, err)
		}
		total := len(pcm) / 2
		if total < 960 {
			continue
		}
		need := total
		if seconds > 0 && seconds*corpusRate < total {
			need = seconds * corpusRate
		}
		start := 0
		if need < total {
			start = firstLoudFrame(pcm, 2, need)
		}
		ex := pcm[start*2 : (start+need)*2]
		base := "local/" + n
		out = append(out,
			corpusClip{base + "-stereo", 2, ex},
			corpusClip{base + "-mono", 1, deinterleaveLeft(ex)},
		)
	}
	return out
}

// ---------------------------------------------------------------------------
// The corpus, assembled.
// ---------------------------------------------------------------------------

// gateCorpus is the TIER A corpus: the synthetic bank, one excerpt per RFC vector
// (mono and stereo), and the local corpus when present. It NEVER fails for want of
// the RFC vectors; it logs and carries on with the synthetic bank, because the
// synthetic bank alone still covers the whole decision surface.
func gateCorpus(t *testing.T) []corpusClip {
	t.Helper()
	clips := syntheticBank()
	if rfcVectorsPresent() {
		clips = append(clips, rfcClips(t, corpusSeconds)...)
	} else {
		t.Logf("TIER 2 SKIPPED: %s absent (run scripts/fetch-vectors.sh); the gate runs on "+
			"the synthetic bank alone", rfcVectorsDir)
	}
	if local := localClips(t, corpusSeconds); len(local) > 0 {
		t.Logf("TIER 3: %d clips from %s", len(local), localCorpusDir)
		clips = append(clips, local...)
	}
	return clips
}

// TestCorpusIsWellFormed pins the corpus itself. A gate is only as good as the
// signals it is swept over, so this asserts every clip is the length the grid
// assumes, that no clip is accidentally silent (except the two that are silent ON
// PURPOSE), and that the stereo images really are the images they claim to be. A
// corpus that had silently degenerated into 52 copies of digital silence would
// otherwise sail through a differential gate.
func TestCorpusIsWellFormed(t *testing.T) {
	clips := gateCorpus(t)
	if len(clips) < 28 {
		t.Fatalf("corpus has %d clips, want at least the 28 synthetic ones", len(clips))
	}

	seen := map[string]bool{}
	var mono, stereo int
	for _, c := range clips {
		if seen[c.name] {
			t.Fatalf("duplicate clip name %q", c.name)
		}
		seen[c.name] = true

		if c.channels != 1 && c.channels != 2 {
			t.Fatalf("%s: channels = %d", c.name, c.channels)
		}
		if c.channels == 1 {
			mono++
		} else {
			stereo++
		}
		if len(c.pcm)%c.channels != 0 {
			t.Fatalf("%s: %d samples is not a whole number of %d-channel frames",
				c.name, len(c.pcm), c.channels)
		}
		// Every clip must hold at least one 20 ms frame, and the synthetic ones must
		// be exactly corpusLen.
		if c.samplesPerChannel() < 960 {
			t.Fatalf("%s: %d samples per channel is shorter than one 20 ms frame",
				c.name, c.samplesPerChannel())
		}

		// Non-vacuity: only the clips that SAY they are silent may be silent.
		var peak int
		for _, v := range c.pcm {
			a := int(v)
			if a < 0 {
				a = -a
			}
			if a > peak {
				peak = a
			}
		}
		silent := peak == 0
		wantSilent := c.name == "syn-mono/silence" || c.name == "syn-stereo/silence"
		if silent != wantSilent {
			t.Fatalf("%s: peak = %d (silent = %v), want silent = %v", c.name, peak, silent, wantSilent)
		}
		if !wantSilent && peak < 2 {
			t.Fatalf("%s: peak = %d: the clip has collapsed to near-nothing", c.name, peak)
		}
	}
	if mono == 0 || stereo == 0 {
		t.Fatalf("corpus is not balanced: %d mono, %d stereo", mono, stereo)
	}

	// The stereo images, pinned. These are the corners of the mid/side decision, and
	// a transform bug would quietly turn all four into the same signal.
	byName := map[string]corpusClip{}
	for _, c := range clips {
		byName[c.name] = c
	}
	ident := byName["syn-stereo/identical-lr"]
	anti := byName["syn-stereo/antiphase"]
	left := byName["syn-stereo/left-only"]
	for i := 0; i < ident.samplesPerChannel(); i++ {
		if ident.pcm[2*i] != ident.pcm[2*i+1] {
			t.Fatalf("identical-lr: L != R at %d (%d vs %d)", i, ident.pcm[2*i], ident.pcm[2*i+1])
		}
		if l, r := anti.pcm[2*i], anti.pcm[2*i+1]; int(l)+int(r) != 0 {
			t.Fatalf("antiphase: L + R = %d at %d, want 0 (mid must be exactly zero)", int(l)+int(r), i)
		}
		if left.pcm[2*i+1] != 0 {
			t.Fatalf("left-only: R = %d at %d, want 0", left.pcm[2*i+1], i)
		}
	}

	// The near-silence clip must actually sit near -80 dBFS: loud enough not to be
	// digital silence, quiet enough to be below every energy threshold that matters.
	near := byName["syn-mono/nearsilence-80dbfs"]
	var nearPeak int16
	for _, v := range near.pcm {
		if v > nearPeak {
			nearPeak = v
		}
	}
	if nearPeak < 1 || nearPeak > 8 {
		t.Fatalf("nearsilence peak = %d, want 1..8 (about -80 dBFS of a 32768 full scale)", nearPeak)
	}

	// The full-scale clip must really be clipped.
	full := byName["syn-mono/fullscale"]
	clipped := 0
	for _, v := range full.pcm {
		if v == 32767 || v == -32768 {
			clipped++
		}
	}
	if clipped == 0 {
		t.Fatal("fullscale: nothing saturated, so the clipping case is not exercised")
	}

	var totalSec float64
	for _, c := range clips {
		totalSec += float64(c.samplesPerChannel()) / corpusRate
	}
	t.Logf("CORPUS: %d clips (%d mono, %d stereo), %.1f s of audio at %d Hz",
		len(clips), mono, stereo, totalSec, corpusRate)
	t.Log(corpusSummary(clips))
}

// corpusSummary is the one-line-per-clip inventory the gate declaration prints.
func corpusSummary(clips []corpusClip) string {
	s := "clips:"
	for _, c := range clips {
		s += fmt.Sprintf("\n  %-32s %dch %6.2fs", c.name, c.channels,
			float64(c.samplesPerChannel())/corpusRate)
	}
	return s
}
