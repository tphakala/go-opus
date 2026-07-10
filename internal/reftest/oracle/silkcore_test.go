//go:build refc

package oracle

// Sequence-based differential test for the pure-Go SILK core decoder
// (internal/silk decode_core.go + decode_frame.go) against the pinned libopus C
// (silk/decode_core.c + silk/decode_frame.c), driven by real SILK bitstreams the C
// SILK encoder (silk/enc_API.c silk_Encode) produces from generated PCM.
//
// SILK is a cross-frame state machine (LPC history sLPC_Q14_buf, the LTP re-whitening
// buffer outBuf, prevNLSF, prev gains, lagPrev), so per hard-parts.md 5 a single-frame
// test cannot validate it. This test encodes multi-second PCM sequences, then decodes
// each packet with BOTH the C silk_decode_frame and the Go DecodeFrame in lockstep,
// asserting after EVERY frame that the decoded int16 output is bit-identical and a
// hash of the persistent SILK decoder state matches, so a divergence is caught on the
// frame it first appears. Coverage spans NB/MB/WB, 10/20/40/60 ms, voiced / unvoiced /
// inactive signals, conditional coding (multi-frame packets), and the first frame
// after reset. TestSilkDecodeCoreSequenceMutationDetected proves the assertions are
// not vacuous.

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// silkcoreGoHash reproduces the C oracle_silkcore_state_hash FNV-1a over the Go
// decoder state, in the identical canonical field order, so the two hashes compare
// directly. Each value is folded as its int64 little-endian byte sequence.
func silkcoreGoHash(d *silk.DecoderState) uint64 {
	h := uint64(14695981039346656037)
	add := func(v int64) {
		x := uint64(v)
		for b := 0; b < 8; b++ {
			h ^= (x >> (8 * b)) & 0xff
			h *= 1099511628211
		}
	}

	for i := range d.SLPCQ14Buf {
		add(int64(d.SLPCQ14Buf[i]))
	}
	for i := range d.OutBuf {
		add(int64(d.OutBuf[i]))
	}
	for i := range d.ExcQ14 {
		add(int64(d.ExcQ14[i]))
	}
	for i := range d.PrevNLSFQ15 {
		add(int64(d.PrevNLSFQ15[i]))
	}

	add(int64(d.LagPrev))
	add(int64(d.LastGainIndex))
	add(int64(d.PrevGainQ16))
	add(int64(d.PrevSignalType))
	add(int64(d.LossCnt))
	add(int64(d.FirstFrameAfterReset))
	add(int64(d.ECPrevSignalType))
	add(int64(d.ECPrevLagIndex))

	ix := &d.Indices
	add(int64(ix.SignalType))
	add(int64(ix.QuantOffsetType))
	for i := range ix.GainsIndices {
		add(int64(ix.GainsIndices[i]))
	}
	for i := range ix.LTPIndex {
		add(int64(ix.LTPIndex[i]))
	}
	for i := range ix.NLSFIndices {
		add(int64(ix.NLSFIndices[i]))
	}
	add(int64(ix.LagIndex))
	add(int64(ix.ContourIndex))
	add(int64(ix.NLSFInterpCoefQ2))
	add(int64(ix.PERIndex))
	add(int64(ix.LTPScaleIndex))
	add(int64(ix.Seed))

	return h
}

// newSilkcoreGoDec sets up a Go SILK decoder state the way silk_init_decoder +
// silk_decoder_set_fs do for the C side: prev_gain_Q16 = 65536 and
// first_frame_after_reset = 1 from init, then DecoderSetFs applies the rate geometry
// and the rate-change reset (lagPrev = 100, LastGainIndex = 10, prevSignalType = 0,
// cleared outBuf and sLPC_Q14_buf).
func newSilkcoreGoDec(fsKHz, nFramesPerPacket, nbSubfr int) *silk.DecoderState {
	d := &silk.DecoderState{}
	d.PrevGainQ16 = 65536
	d.FirstFrameAfterReset = 1
	d.NFramesPerPacket = nFramesPerPacket
	d.DecoderSetFs(fsKHz, nbSubfr)
	return d
}

// goDecodePacket replays the mono, no-loss silk_Decode driver over the packet with
// the pure-Go decoder: reset nFramesDecoded, parse the VAD + LBRR header, then call
// DecodeFrame per frame. It returns one silkcoreFrame per SILK frame. The LBRR flag
// must be 0 (FEC is disabled in the encoder).
func goDecodePacket(t *testing.T, d *silk.DecoderState, buf []byte, nFramesPerPacket int) []silkcoreFrame {
	t.Helper()
	d.NFramesDecoded = 0
	var dec rangecoding.Decoder
	dec.Init(buf)

	for i := 0; i < nFramesPerPacket; i++ {
		d.VADFlags[i] = dec.DecBitLogp(1)
	}
	if lbrr := dec.DecBitLogp(1); lbrr != 0 {
		t.Fatalf("unexpected LBRR flag set (FEC disabled)")
	}

	frames := make([]silkcoreFrame, nFramesPerPacket)
	pOut := make([]int16, d.FrameLength)
	for n := 0; n < nFramesPerPacket; n++ {
		condCoding := 0 // CODE_INDEPENDENTLY
		if d.NFramesDecoded > 0 {
			condCoding = 2 // CODE_CONDITIONALLY
		}
		silk.DecodeFrame(d, &dec, pOut, 0 /* FLAG_DECODE_NORMAL */, condCoding)
		d.NFramesDecoded++

		out := make([]int16, d.FrameLength)
		copy(out, pOut[:d.FrameLength])
		frames[n] = silkcoreFrame{
			out:                  out,
			signalType:           int(d.Indices.SignalType),
			quantOffsetType:      int(d.Indices.QuantOffsetType),
			stateHash:            silkcoreGoHash(d),
			rng:                  dec.Rng(),
			tell:                 dec.Tell(),
			prevGainQ16:          d.PrevGainQ16,
			lagPrev:              d.LagPrev,
			lastGainIndex:        int(d.LastGainIndex),
			prevSignalType:       d.PrevSignalType,
			firstFrameAfterReset: d.FirstFrameAfterReset,
			ecPrevSignalType:     d.ECPrevSignalType,
			ecPrevLagIndex:       int(d.ECPrevLagIndex),
		}
	}
	return frames
}

// framesEqual reports whether a C-decoded frame and a Go-decoded frame agree on the
// decoded samples and the observable end/persistent state.
func framesEqual(c, g silkcoreFrame) bool {
	if len(c.out) != len(g.out) {
		return false
	}
	for i := range c.out {
		if c.out[i] != g.out[i] {
			return false
		}
	}
	return c.stateHash == g.stateHash && c.rng == g.rng && c.tell == g.tell &&
		c.signalType == g.signalType && c.quantOffsetType == g.quantOffsetType &&
		c.prevGainQ16 == g.prevGainQ16 && c.lagPrev == g.lagPrev &&
		c.lastGainIndex == g.lastGainIndex && c.prevSignalType == g.prevSignalType &&
		c.firstFrameAfterReset == g.firstFrameAfterReset &&
		c.ecPrevSignalType == g.ecPrevSignalType && c.ecPrevLagIndex == g.ecPrevLagIndex
}

// ---- PCM generators (int16 mono at fsHz) --------------------------------------

// scVoiced makes a sustained, harmonic-rich buzz (fundamental 140 Hz, 6 harmonics
// with mild vibrato) that the SILK VAD/pitch analysis classifies as voiced speech.
func scVoiced(fsHz, n int) []int16 {
	out := make([]int16, n)
	f0 := 140.0
	for i := 0; i < n; i++ {
		tt := float64(i) / float64(fsHz)
		vib := 1.0 + 0.01*math.Sin(2*math.Pi*5*tt)
		var s float64
		for h := 1; h <= 6; h++ {
			s += (1.0 / float64(h)) * math.Sin(2*math.Pi*f0*vib*float64(h)*tt)
		}
		out[i] = clampI16(s * 6000)
	}
	return out
}

// scTone makes a pure sine tone at freq Hz.
func scTone(fsHz, n int, freq float64) []int16 {
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		tt := float64(i) / float64(fsHz)
		out[i] = clampI16(8000 * math.Sin(2*math.Pi*freq*tt))
	}
	return out
}

// scNoise makes seeded white noise (biases the encoder toward the unvoiced type).
func scNoise(seed int64) func(fsHz, n int) []int16 {
	return func(fsHz, n int) []int16 {
		r := rand.New(rand.NewSource(seed))
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			out[i] = int16(r.Intn(2*5000+1) - 5000)
		}
		return out
	}
}

// scSilence makes pure silence (exercises the inactive / TYPE_NO_VOICE_ACTIVITY path).
func scSilence(_, n int) []int16 { return make([]int16, n) }

// scSweep makes a linear chirp from 200 Hz to 3000 Hz.
func scSweep(fsHz, n int) []int16 {
	out := make([]int16, n)
	dur := float64(n) / float64(fsHz)
	f0, f1 := 200.0, 3000.0
	for i := 0; i < n; i++ {
		tt := float64(i) / float64(fsHz)
		inst := f0 + (f1-f0)*tt/dur
		phase := 2 * math.Pi * (f0*tt + 0.5*(inst-f0)*tt)
		out[i] = clampI16(7000 * math.Sin(phase))
	}
	return out
}

// scMixed concatenates ~250 ms segments of voiced, noise, silence and tone so a
// single sequence sweeps through signal types and the state carry between them.
func scMixed(fsHz, n int) []int16 {
	out := make([]int16, n)
	seg := fsHz / 4 // 250 ms
	if seg < 1 {
		seg = 1
	}
	noise := scNoise(0xBEEF)
	for start := 0; start < n; start += seg {
		end := start + seg
		if end > n {
			end = n
		}
		length := end - start
		var chunk []int16
		switch (start / seg) % 4 {
		case 0:
			chunk = scVoiced(fsHz, length)
		case 1:
			chunk = noise(fsHz, length)
		case 2:
			chunk = scSilence(fsHz, length)
		default:
			chunk = scTone(fsHz, length, 700)
		}
		copy(out[start:end], chunk)
	}
	return out
}

func clampI16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// ---- The differential test -----------------------------------------------------

// TestSilkDecodeCoreSequenceMatchesC is the primary deliverable: across every
// internal rate, frame length and signal type, the C SILK encoder produces real
// bitstreams and the pure-Go decode_core / decode_frame match the C bit-for-bit on
// every decoded sample and on the per-frame persistent-state hash, over multi-second
// sequences.
func TestSilkDecodeCoreSequenceMatchesC(t *testing.T) {
	type genSpec struct {
		name string
		gen  func(fsHz, n int) []int16
	}
	gens := []genSpec{
		{"voiced", scVoiced},
		{"noise", scNoise(0x51CC0)},
		{"silence", scSilence},
		{"sweep", scSweep},
		{"mixed", scMixed},
	}
	fsRates := []int{8, 12, 16}
	payloads := []int{10, 20, 40, 60}
	const durationMs = 1500

	// Coverage flags: assert the interesting branches actually ran.
	var (
		frames                   int
		rateSeen                 = map[int]bool{}
		nb2Seen, nb4Seen         bool
		condSeen, firstFrameSeen bool
		voicedSeen, unvoicedSeen bool
		inactiveSeen             bool
		multiFramePacket         bool
	)

	for _, fs := range fsRates {
		for _, payload := range payloads {
			for _, gs := range gens {
				bitrate := (fs / 8) * 12000 // ~12k (NB) .. 24k (WB)
				ctx := newSilkcoreCtx(fs, payload, bitrate, 5, 0)
				if ctx == nil {
					t.Fatalf("newSilkcoreCtx(%d,%d) failed", fs, payload)
				}

				nPackets := durationMs / payload
				samplesPerPacket := payload * fs // payload_ms * fs_kHz
				pcm := gs.gen(fs*1000, nPackets*samplesPerPacket)

				goDec := newSilkcoreGoDec(fs, ctx.nFramesPerPacket, ctx.nbSubfr)

				for p := 0; p < nPackets; p++ {
					chunk := pcm[p*samplesPerPacket : (p+1)*samplesPerPacket]
					buf := ctx.encode(chunk)
					if buf == nil {
						continue // DTX / empty packet (useDTX is off, so not expected)
					}
					if got := ctx.internalRate(); got != fs*1000 {
						t.Fatalf("fs=%d: encoder chose internal rate %d, want %d", fs, got, fs*1000)
					}

					cFrames := ctx.decodePacket(buf)
					goFrames := goDecodePacket(t, goDec, buf, ctx.nFramesPerPacket)
					if len(cFrames) != len(goFrames) {
						t.Fatalf("fs=%d payload=%d gen=%s pkt=%d: frame count C=%d Go=%d",
							fs, payload, gs.name, p, len(cFrames), len(goFrames))
					}
					if len(cFrames) > 1 {
						multiFramePacket = true
					}

					for f := range cFrames {
						c, g := cFrames[f], goFrames[f]
						if !framesEqual(c, g) {
							t.Fatalf("MISMATCH fs=%d payload=%d gen=%s pkt=%d frame=%d (global %d):\n"+
								"  sig C=%d G=%d  hash C=%016x G=%016x  rng C=%08x G=%08x  tell C=%d G=%d\n"+
								"  prevGain C=%d G=%d  lagPrev C=%d G=%d  LastGain C=%d G=%d  ffar C=%d G=%d  firstDiff=%s",
								fs, payload, gs.name, p, f, frames,
								c.signalType, g.signalType, c.stateHash, g.stateHash, c.rng, g.rng, c.tell, g.tell,
								c.prevGainQ16, g.prevGainQ16, c.lagPrev, g.lagPrev, c.lastGainIndex, g.lastGainIndex,
								c.firstFrameAfterReset, g.firstFrameAfterReset, firstOutputDiff(c.out, g.out))
						}

						// Coverage bookkeeping (drive off the matched result).
						frames++
						rateSeen[fs] = true
						if ctx.nbSubfr == 2 {
							nb2Seen = true
						} else {
							nb4Seen = true
						}
						if f > 0 {
							condSeen = true
						}
						if p == 0 && f == 0 {
							firstFrameSeen = true
						}
						switch g.signalType {
						case 2:
							voicedSeen = true
						case 1:
							unvoicedSeen = true
						case 0:
							inactiveSeen = true
						}
					}
				}
				ctx.close()
			}
		}
	}

	// Non-vacuity: assert the branches we care about were exercised.
	if frames < 1000 {
		t.Fatalf("too few frames decoded (%d); sequence coverage is suspect", frames)
	}
	for _, fs := range fsRates {
		if !rateSeen[fs] {
			t.Errorf("rate %d kHz never exercised", fs)
		}
	}
	if !nb2Seen {
		t.Errorf("10 ms (nb_subfr=2) frames never exercised")
	}
	if !nb4Seen {
		t.Errorf("20/40/60 ms (nb_subfr=4) frames never exercised")
	}
	if !condSeen || !multiFramePacket {
		t.Errorf("conditional coding (multi-frame packet) never exercised")
	}
	if !firstFrameSeen {
		t.Errorf("first-frame-after-reset path never exercised")
	}
	if !voicedSeen {
		t.Errorf("voiced frames never exercised")
	}
	if !unvoicedSeen {
		t.Errorf("unvoiced frames never exercised")
	}
	if !inactiveSeen {
		t.Errorf("inactive (TYPE_NO_VOICE_ACTIVITY) frames never exercised")
	}
	t.Logf("decoded %d frames bit-exact across NB/MB/WB, 10/20/40/60 ms, voiced/unvoiced/inactive", frames)
}

// firstOutputDiff returns a short description of the first differing output sample.
func firstOutputDiff(c, g []int16) string {
	n := len(c)
	if len(g) < n {
		n = len(g)
	}
	for i := 0; i < n; i++ {
		if c[i] != g[i] {
			return sprintDiff(i, c[i], g[i])
		}
	}
	if len(c) != len(g) {
		return "length differs"
	}
	return "output identical (state-only divergence)"
}

func sprintDiff(i int, c, g int16) string {
	return "sample[" + itoa(i) + "] C=" + itoa(int(c)) + " G=" + itoa(int(g))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	p := len(b)
	for v > 0 {
		p--
		b[p] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// TestSilkDecodeCoreSequenceMutationDetected proves the differential comparison is not
// vacuous: after decoding a real frame with both C and Go (which must agree), a
// deliberate one-bit perturbation of the Go output, and separately of the Go state
// hash, is detected by framesEqual.
func TestSilkDecodeCoreSequenceMutationDetected(t *testing.T) {
	ctx := newSilkcoreCtx(16, 20, 24000, 5, 0)
	if ctx == nil {
		t.Fatal("newSilkcoreCtx failed")
	}
	defer ctx.close()
	goDec := newSilkcoreGoDec(16, ctx.nFramesPerPacket, ctx.nbSubfr)

	samplesPerPacket := 20 * 16
	pcm := scVoiced(16000, 6*samplesPerPacket)

	var c, g silkcoreFrame
	got := false
	for p := 0; p < 6; p++ {
		buf := ctx.encode(pcm[p*samplesPerPacket : (p+1)*samplesPerPacket])
		if buf == nil {
			continue
		}
		cFrames := ctx.decodePacket(buf)
		goFrames := goDecodePacket(t, goDec, buf, ctx.nFramesPerPacket)
		c, g = cFrames[len(cFrames)-1], goFrames[len(goFrames)-1]
		got = true
	}
	if !got {
		t.Fatal("no frame decoded")
	}
	if !framesEqual(c, g) {
		t.Fatal("baseline C/Go frame unexpectedly differ")
	}
	if len(g.out) == 0 {
		t.Fatal("empty output frame")
	}

	// Mutate one output sample: must now be detected.
	mOut := g
	mOut.out = append([]int16(nil), g.out...)
	mOut.out[0] ^= 1
	if framesEqual(c, mOut) {
		t.Fatal("mutation of an output sample was NOT detected")
	}

	// Mutate the persistent-state hash: must now be detected.
	mHash := g
	mHash.stateHash ^= 1
	if framesEqual(c, mHash) {
		t.Fatal("mutation of the state hash was NOT detected")
	}
}
