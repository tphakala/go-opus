//go:build refc

package oracle

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// msIdentityMapping is the mapping for a layout with channels == streams+coupled:
// each stream channel feeds exactly one API channel in order, which every valid
// family-0/255 sweep layout below uses.
func msIdentityMapping(channels int) []byte {
	m := make([]byte, channels)
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

// msLayoutCase is a plain (MAPPING_TYPE_NONE) multistream layout for the sweeps.
type msLayoutCase struct {
	channels int
	streams  int
	coupled  int
}

// msLayoutSweep covers mono, stereo, discrete-mono (family 255) and mixed
// coupled/uncoupled layouts up to 8 channels. Every case has channels ==
// streams+coupled with the identity mapping, so validate_layout and
// validate_encoder_layout both pass.
var msLayoutSweep = []msLayoutCase{
	{1, 1, 0}, // mono
	{2, 1, 1}, // stereo (one coupled stream)
	{2, 2, 0}, // two discrete mono streams
	{3, 2, 1}, // one coupled + one mono
	{4, 3, 1},
	{5, 3, 2},
	{6, 4, 2},
	{7, 4, 3},
	{8, 8, 0}, // eight discrete mono streams (family 255)
	{8, 5, 3},
}

// msFrameSizesFor returns the per-channel sample counts of the nine Opus frame
// durations (2.5, 5, 10, 20, 40, 60, 80, 100, 120 ms) at Fs. rate_allocation reads
// Fs and frame_size, so the sweep varies both.
func msFrameSizesFor(Fs int) []int {
	return []int{Fs / 400, Fs / 200, Fs / 100, Fs / 50, Fs / 25, 3 * Fs / 50, 4 * Fs / 50, 5 * Fs / 50, 6 * Fs / 50}
}

// msSampleRates is the full set of sample rates the encoder accepts. rateAllocation
// depends on Fs (through channel_offset and the OPUS_AUTO/BITRATE_MAX bitrate
// formulas), so every rate is swept, not just 48 kHz.
var msSampleRates = []int{8000, 12000, 16000, 24000, 48000}

// TestMSRateAllocationMatchesC pins the pure-Go rateAllocation against the C static
// rate_allocation in isolation, across the sample-rate / layout / frame-size /
// bitrate sweep, before the end-to-end packet gate. rate_allocation is pure integer
// math that silently mis-rates every stream if a shift or clip diverges, so it is
// checked on its own first (the sub-encoder bitrates it produces feed every packet's
// bytes).
func TestMSRateAllocationMatchesC(t *testing.T) {
	bitrates := []int32{
		int32(opusenc.OpusAuto),
		int32(opusenc.OpusBitrateMax),
		6000, 12000, 32000, 64000, 128000, 256000, 510000,
	}

	for _, Fs := range msSampleRates {
		frameSizes := msFrameSizesFor(Fs)
		for _, L := range msLayoutSweep {
			mapping := msIdentityMapping(L.channels)
			for _, fs := range frameSizes {
				for _, br := range bitrates {
					goRates, goSum, err := opusenc.RateAllocationSeam(int32(Fs), L.channels, L.streams, L.coupled, oAppAudio, mapping, br, fs)
					if err != nil {
						t.Fatalf("Go RateAllocationSeam(Fs=%d ch=%d s=%d c=%d fs=%d br=%d): %v",
							Fs, L.channels, L.streams, L.coupled, fs, br, err)
					}
					cRates, cSum, err := RateAllocationC(Fs, L.channels, L.streams, L.coupled, mapping, oAppAudio, br, fs)
					if err != nil {
						t.Fatalf("C RateAllocationC(Fs=%d ch=%d s=%d c=%d fs=%d br=%d): %v",
							Fs, L.channels, L.streams, L.coupled, fs, br, err)
					}
					if goSum != cSum {
						t.Errorf("rate sum mismatch (Fs=%d ch=%d s=%d c=%d fs=%d br=%d): Go=%d C=%d",
							Fs, L.channels, L.streams, L.coupled, fs, br, goSum, cSum)
					}
					for s := range cRates {
						if goRates[s] != cRates[s] {
							t.Errorf("rate[%d] mismatch (Fs=%d ch=%d s=%d c=%d fs=%d br=%d): Go=%d C=%d",
								s, Fs, L.channels, L.streams, L.coupled, fs, br, goRates[s], cRates[s])
						}
					}
				}
			}
		}
	}
}

// genMSPCM builds interleaved int16 PCM with a DISTINCT signal on every channel: a
// per-channel tone frequency, phase and noise seed. Distinctness is essential for
// the demux check, since identical channels would hide a mapping-traversal bug that
// routes the wrong channel to a stream.
func genMSPCM(nFrames, frameSize, channels int) []int16 {
	total := nFrames * frameSize
	out := make([]int16, total*channels)
	for c := 0; c < channels; c++ {
		lcg := uint32(0x9e3779b9)*uint32(c+1) + 0x1234
		step := 5 + 2*c
		phase := 37 * c
		for n := 0; n < total; n++ {
			lcg = lcg*1664525 + 1013904223
			noise := int32(int16(lcg>>16)) / 48
			s := sineTab[((n*step)+phase)&255]
			v := int32(s)/2 + noise
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			out[n*channels+c] = int16(v)
		}
	}
	return out
}

// msEncCfg is a sub-encoder configuration applied identically to the Go and C
// multistream encoders before a differential run.
type msEncCfg struct {
	name       string
	bitrate    int32
	vbr        int
	vbrConstr  int
	complexity int
	dtx        int
}

// applyGoCfg applies cfg to the pure-Go multistream encoder. The frozen build is
// CELT-only, so force mode is pinned to MODE_CELT_ONLY like the phase-4 gate.
func applyGoCfg(t *testing.T, e *opusenc.OpusMSEncoder, c msEncCfg) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("apply Go cfg %q: %v", c.name, err)
		}
	}
	must(e.SetForceMode(opusenc.ModeCeltOnly))
	must(e.SetComplexity(c.complexity))
	must(e.SetVBR(c.vbr))
	must(e.SetVBRConstraint(c.vbrConstr))
	must(e.SetBitrate(c.bitrate))
	if c.dtx != 0 {
		must(e.SetDTX(c.dtx))
	}
}

// applyCCfg applies cfg to the C oracle multistream encoder in the same order and
// with the same values, so both encoders reach an identical configuration.
func applyCCfg(e *CPlainMSEncoder, c msEncCfg) {
	e.SetForceMode(oModeCeltOnly)
	e.SetComplexity(c.complexity)
	e.SetVBR(c.vbr)
	e.SetVBRConstraint(c.vbrConstr)
	e.SetBitrate(c.bitrate)
	if c.dtx != 0 {
		e.SetDTX(c.dtx)
	}
}

// runMSEncDiffPCM is the core differential driver: it encodes the given interleaved
// PCM (len(pcm)/(frameSize*channels) frames) through both the Go and C multistream
// encoders for an explicit layout and mapping, and asserts byte-identical packets
// and equal final range on every frame. Byte-identical is strictly stronger than the
// decoder gate's PCM check: it pins the demux (including a non-identity or muted
// mapping), rate allocation, curr_max budget, CBR last-stream override and
// self-delimited framing simultaneously.
func runMSEncDiffPCM(t *testing.T, Fs, channels, streams, coupled int, mapping []byte, c msEncCfg, frameSize int, pcm []int16) {
	t.Helper()
	nFrames := len(pcm) / (frameSize * channels)
	// Room for the worst case: each stream can return six 20 ms sub-frames.
	maxBytes := streams*(6*1275+16) + 16

	goEnc, err := opusenc.NewMSEncoder(int32(Fs), channels, streams, coupled, oAppAudio, mapping)
	if err != nil {
		t.Fatalf("NewMSEncoder: %v", err)
	}
	applyGoCfg(t, goEnc, c)

	cEnc, err := NewCPlainMSEncoder(Fs, channels, streams, coupled, mapping, oAppAudio)
	if err != nil {
		t.Fatalf("NewCPlainMSEncoder: %v", err)
	}
	defer cEnc.Destroy()
	applyCCfg(cEnc, c)

	goBuf := make([]byte, maxBytes)
	for f := 0; f < nFrames; f++ {
		frame := pcm[f*frameSize*channels : (f+1)*frameSize*channels]
		gn, gerr := goEnc.Encode(frame, frameSize, goBuf, maxBytes)
		if gerr != nil {
			t.Fatalf("frame %d Go Encode: %v", f, gerr)
		}
		cPkt, cerr := cEnc.Encode(frame, frameSize, maxBytes)
		if cerr != nil {
			t.Fatalf("frame %d C Encode: %v", f, cerr)
		}
		if !bytes.Equal(goBuf[:gn], cPkt) {
			t.Fatalf("frame %d packet mismatch (%d Go vs %d C bytes)\nGo=%x\n C=%x",
				f, gn, len(cPkt), goBuf[:gn], cPkt)
		}
		if gr, cr := goEnc.FinalRange(), cEnc.FinalRange(); gr != cr {
			t.Fatalf("frame %d final range mismatch: Go=%08x C=%08x", f, gr, cr)
		}
	}
}

// runMSEncDiff is runMSEncDiffPCM over the identity mapping with distinct-per-channel
// PCM, the common case for the layout/config sweeps.
func runMSEncDiff(t *testing.T, Fs int, L msLayoutCase, c msEncCfg, frameSize, nFrames int) {
	t.Helper()
	runMSEncDiffPCM(t, Fs, L.channels, L.streams, L.coupled, msIdentityMapping(L.channels), c,
		frameSize, genMSPCM(nFrames, frameSize, L.channels))
}

// TestMSEncoderMatchesC pins the pure-Go multistream encoder byte-for-byte against
// the C plain (family-0/255) multistream encoder across layouts, bitrate modes and
// frame sizes. Stage 1 encodes at OPUS_BITRATE_MAX + VBR, which removes the
// byte-budget clamp so a mismatch is purely the self-delimited framing or demux,
// over every layout. Stage 2 sweeps the full config space over representative
// layouts, exercising the CBR max_data_bytes shrink, the per-stream curr_max
// budget, and the CBR last-stream bitrate override.
func TestMSEncoderMatchesC(t *testing.T) {
	const Fs = 48000
	const nFrames = 4

	framingCfg := msEncCfg{name: "max/vbr", bitrate: int32(opusenc.OpusBitrateMax), vbr: 1, complexity: 10}
	for _, L := range msLayoutSweep {
		t.Run(fmt.Sprintf("framing/ch%d_s%d_c%d", L.channels, L.streams, L.coupled), func(t *testing.T) {
			runMSEncDiff(t, Fs, L, framingCfg, 960, nFrames)
		})
	}

	// 20, 10, 60, 100, 120 ms. 100 ms (Fs/frame_size==10) exercises the extra ToC
	// byte reserved per stream; 60/120 ms exercise the multi-frame repacketizer cat.
	frameSizes := []struct {
		name string
		n    int
	}{
		{"20ms", 960}, {"10ms", 480}, {"60ms", 2880}, {"100ms", 4800}, {"120ms", 5760},
	}
	cfgs := []msEncCfg{
		{name: "vbr64k", bitrate: 64000, vbr: 1, complexity: 10},
		{name: "cvbr64k", bitrate: 64000, vbr: 1, vbrConstr: 1, complexity: 10},
		{name: "cbr64k", bitrate: 64000, vbr: 0, complexity: 10},
		{name: "cbr24k_c5", bitrate: 24000, vbr: 0, complexity: 5},
		{name: "cbr128k_c0", bitrate: 128000, vbr: 0, complexity: 0},
		{name: "auto_vbr", bitrate: int32(opusenc.OpusAuto), vbr: 1, complexity: 9},
		{name: "auto_cbr", bitrate: int32(opusenc.OpusAuto), vbr: 0, complexity: 9},
	}
	repLayouts := []msLayoutCase{{1, 1, 0}, {2, 1, 1}, {2, 2, 0}, {3, 2, 1}, {6, 4, 2}, {8, 8, 0}}
	for _, L := range repLayouts {
		for _, c := range cfgs {
			for _, fsz := range frameSizes {
				t.Run(fmt.Sprintf("ch%d_s%d_c%d/%s/%s", L.channels, L.streams, L.coupled, c.name, fsz.name), func(t *testing.T) {
					runMSEncDiff(t, Fs, L, c, fsz.n, nFrames)
				})
			}
		}
	}
}

// TestMSEncoderNonIdentityMapping pins the channel demux for mappings that are NOT
// the identity: a permuted mapping exercises the non-linear get_{left,right,mono}
// _channel search (which the identity sweep never does, since identity finds every
// stream at index==value), a 255-muted channel exercises the drop path, and a
// duplicate mapping exercises "first match wins". Byte-identical output vs C over
// distinct-per-channel PCM proves the traversal routes the right channel to the
// right stream; the round-trip FinalRange check cannot see a routing bug.
func TestMSEncoderNonIdentityMapping(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960
		nFrames   = 4
	)
	cfg := msEncCfg{name: "vbr64k", bitrate: 64000, vbr: 1, complexity: 10}
	cases := []struct {
		name                       string
		channels, streams, coupled int
		mapping                    []byte
	}{
		{"swap-two-mono", 2, 2, 0, []byte{1, 0}},               // streams fed from swapped channels
		{"permute-stereo-plus-mono", 3, 2, 1, []byte{1, 0, 2}}, // coupled L/R swapped
		{"muted-middle-channel", 3, 2, 0, []byte{0, 255, 1}},   // channel 1 dropped, not encoded
		{"duplicate-left-channel", 3, 1, 1, []byte{0, 1, 0}},   // channel 2 duplicates stream 0 left
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := genMSPCM(nFrames, frameSize, tc.channels)
			runMSEncDiffPCM(t, Fs, tc.channels, tc.streams, tc.coupled, tc.mapping, cfg, frameSize, pcm)
		})
	}
}

// TestMSEncoderDTX pins the multistream framing when DTX shrinks sub-packets to a
// single TOC byte. With OPUS_SET_DTX and exact digital silence past the ~200 ms
// threshold, each sub-encoder emits 1-byte packets, so this exercises the
// self-delimited concatenation of minimum-size sub-packets (the small-curr_max end
// of the byte budget) that the signal-bearing sweeps never reach. The zero-PCM input
// is what triggers generalized DTX; a config with dtx=1 but audio would not.
func TestMSEncoderDTX(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960
		nFrames   = 25 // well past the ~200 ms (10-frame) silence threshold
	)
	cfg := msEncCfg{name: "cbr-dtx", bitrate: 32000, vbr: 0, complexity: 10, dtx: 1}
	cases := []struct{ channels, streams, coupled int }{
		{1, 1, 0}, {2, 1, 1}, {2, 2, 0}, {4, 2, 2},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("ch%d_s%d_c%d", tc.channels, tc.streams, tc.coupled), func(t *testing.T) {
			silence := make([]int16, nFrames*frameSize*tc.channels) // exact digital silence
			runMSEncDiffPCM(t, Fs, tc.channels, tc.streams, tc.coupled, msIdentityMapping(tc.channels), cfg, frameSize, silence)
		})
	}
}

// TestMSEncoderNon48k pins the byte-exact encoder at the non-48 kHz sample rates the
// API accepts (8/12/16/24 kHz), which the main sweep (48 kHz only) never exercises.
// rate_allocation, the frame-size domain and the CELT upsample factor all depend on
// Fs, so a divergence there would be invisible at 48 kHz alone.
func TestMSEncoderNon48k(t *testing.T) {
	const nFrames = 4
	cfgs := []msEncCfg{
		{name: "vbr64k", bitrate: 64000, vbr: 1, complexity: 10},
		{name: "cbr32k", bitrate: 32000, vbr: 0, complexity: 10},
	}
	layouts := []msLayoutCase{{1, 1, 0}, {2, 1, 1}, {3, 2, 1}}
	for _, Fs := range []int{8000, 12000, 16000, 24000} {
		frameSize := Fs / 50 // 20 ms
		for _, L := range layouts {
			for _, c := range cfgs {
				t.Run(fmt.Sprintf("%dHz/ch%d_s%d_c%d/%s", Fs, L.channels, L.streams, L.coupled, c.name), func(t *testing.T) {
					runMSEncDiff(t, Fs, L, c, frameSize, nFrames)
				})
			}
		}
	}
}

// TestMSEncoderTightBuffer pins the per-stream curr_max byte budget, including the
// self-delimited reserve (curr_max > 253 ? 2 : 1). The other sweeps never make
// curr_max the binding constraint (their packets sit below budget), so they do not
// exercise the reserve; here a multi-coupled-stream CBR encoder at OPUS_BITRATE_MAX
// (which fills the whole output budget) is run across a range of tight
// max_data_bytes so curr_max clamps the non-last stream and crosses 253 bytes. A
// fresh encoder pair per budget keeps each case independent of the last.
func TestMSEncoderTightBuffer(t *testing.T) {
	const Fs = 48000
	const frameSize = 960
	L := msLayoutCase{4, 2, 2} // two coupled streams, so a non-last stream exists
	mapping := msIdentityMapping(L.channels)
	pcm := genMSPCM(1, frameSize, L.channels)
	cfg := msEncCfg{name: "cbr-max", bitrate: int32(opusenc.OpusBitrateMax), vbr: 0, complexity: 10}

	for maxBytes := 200; maxBytes <= 1200; maxBytes += 3 {
		goEnc, err := opusenc.NewMSEncoder(Fs, L.channels, L.streams, L.coupled, oAppAudio, mapping)
		if err != nil {
			t.Fatalf("NewMSEncoder: %v", err)
		}
		applyGoCfg(t, goEnc, cfg)
		cEnc, err := NewCPlainMSEncoder(Fs, L.channels, L.streams, L.coupled, mapping, oAppAudio)
		if err != nil {
			t.Fatalf("NewCPlainMSEncoder: %v", err)
		}
		applyCCfg(cEnc, cfg)

		goBuf := make([]byte, maxBytes)
		gn, gerr := goEnc.Encode(pcm, frameSize, goBuf, maxBytes)
		cPkt, cerr := cEnc.Encode(pcm, frameSize, maxBytes)
		var cRange uint32
		if cerr == nil {
			cRange = cEnc.FinalRange()
		}
		cEnc.Destroy()

		if (gerr == nil) != (cerr == nil) {
			t.Fatalf("maxBytes=%d: success disagreement: Go err=%v, C err=%v", maxBytes, gerr, cerr)
		}
		if gerr != nil {
			continue // both rejected the budget as too small
		}
		if !bytes.Equal(goBuf[:gn], cPkt) {
			t.Fatalf("maxBytes=%d: packet mismatch (%d Go vs %d C bytes)\nGo=%x\n C=%x",
				maxBytes, gn, len(cPkt), goBuf[:gn], cPkt)
		}
		if gr := goEnc.FinalRange(); gr != cRange {
			t.Fatalf("maxBytes=%d: final range mismatch: Go=%08x C=%08x", maxBytes, gr, cRange)
		}
	}
}

// TestMSEncoderBufferTooSmall pins the smallest_packet gate at its exact boundary,
// differentially against C, for both 20 ms and 100 ms frames. The threshold is
// 2*streams-1 bytes, plus streams more at 100 ms (Fs/frameSize==10). Below it the Go
// encoder must return ErrBufferTooSmall (as C returns OPUS_BUFFER_TOO_SMALL) before
// encoding any stream; at or above it, Go and C must agree on success/failure and,
// on success, on the exact bytes. A one-off in smallestPacket (either direction, and
// including a dropped 100 ms adjustment) breaks the agreement.
func TestMSEncoderBufferTooSmall(t *testing.T) {
	const Fs = 48000
	L := msLayoutCase{6, 4, 2}
	mapping := msIdentityMapping(L.channels)
	cfg := msEncCfg{name: "cbr64k", bitrate: 64000, vbr: 0, complexity: 10}

	for _, fr := range []struct {
		name      string
		frameSize int
	}{{"20ms", 960}, {"100ms", 4800}} {
		t.Run(fr.name, func(t *testing.T) {
			smallest := 2*L.streams - 1
			if Fs/fr.frameSize == 10 {
				smallest += L.streams
			}
			pcm := genMSPCM(1, fr.frameSize, L.channels)
			for maxBytes := smallest - 3; maxBytes <= smallest+3; maxBytes++ {
				if maxBytes < 1 {
					continue
				}
				goEnc, err := opusenc.NewMSEncoder(Fs, L.channels, L.streams, L.coupled, oAppAudio, mapping)
				if err != nil {
					t.Fatalf("NewMSEncoder: %v", err)
				}
				applyGoCfg(t, goEnc, cfg)
				cEnc, err := NewCPlainMSEncoder(Fs, L.channels, L.streams, L.coupled, mapping, oAppAudio)
				if err != nil {
					t.Fatalf("NewCPlainMSEncoder: %v", err)
				}
				applyCCfg(cEnc, cfg)

				goBuf := make([]byte, maxBytes)
				gn, gerr := goEnc.Encode(pcm, fr.frameSize, goBuf, maxBytes)
				cPkt, cerr := cEnc.Encode(pcm, fr.frameSize, maxBytes)
				cEnc.Destroy()

				if maxBytes < smallest {
					if !errors.Is(gerr, opusenc.ErrBufferTooSmall) {
						t.Fatalf("maxBytes=%d (<smallest=%d): Go err=%v, want ErrBufferTooSmall", maxBytes, smallest, gerr)
					}
					if cerr == nil {
						t.Fatalf("maxBytes=%d (<smallest=%d): C accepted but Go rejected", maxBytes, smallest)
					}
					continue
				}
				if (gerr == nil) != (cerr == nil) {
					t.Fatalf("maxBytes=%d (>=smallest=%d): success disagreement Go err=%v C err=%v", maxBytes, smallest, gerr, cerr)
				}
				if gerr == nil && !bytes.Equal(goBuf[:gn], cPkt) {
					t.Fatalf("maxBytes=%d: packet mismatch\nGo=%x\n C=%x", maxBytes, goBuf[:gn], cPkt)
				}
			}
		})
	}
}
