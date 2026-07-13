package oggopus

import (
	"bytes"
	"testing"
)

// This file holds the assertions that a bit-exact PACKET gate structurally cannot
// make. Every quantity here (the pre-skip written into OpusHead, the 48 kHz
// duration the seam reports per packet, each page's granule, the final granule and
// the end-trim, and therefore the stream's duration and its sample-accurate
// alignment) is invisible to a differential encoder test, because none of them is
// a packet byte. Getting any of them wrong produces packets that are byte-identical
// to libopus inside a container that plays back misaligned, too long, too short, or
// that a strict demuxer rejects outright. They are pinned here instead.

const (
	// expectedPreSkip is the pre-skip every stream this encoder writes must carry.
	// OPUS_GET_LOOKAHEAD is Fs/400 (the CELT MDCT overlap) + Fs/250 (the
	// delay-compensation ring) AT the coding rate, and RFC 7845 section 5.1 defines
	// the pre-skip field at 48 kHz, so the container scales it by 48000/Fs. Both
	// terms are proportional to Fs, so the scaled result is 312 at every rate:
	// 8k 20+32=52 x6, 16k 40+64=104 x3, 48k 120+192=312 x1.
	expectedPreSkip = 312
	// frame48k is the coded duration of one 20 ms frame at 48 kHz.
	frame48k = 960
)

// int16sToBytes packs interleaved samples into the little-endian bytes Encoder
// consumes.
func int16sToBytes(s []int16) []byte {
	b := make([]byte, 2*len(s))
	for i, v := range s {
		b[2*i] = byte(uint16(v))
		b[2*i+1] = byte(uint16(v) >> 8)
	}
	return b
}

// bytesToInt16s unpacks the little-endian bytes Decoder produces.
func bytesToInt16s(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return s
}

// encodeOgg runs interleaved PCM through the full public encoder and returns the
// Ogg Opus stream.
func encodeOgg(t *testing.T, cfg Config, pcm []int16) []byte { //nolint:gocritic // by value to match the public NewEncoder signature the tests exercise
	t.Helper()
	var buf bytes.Buffer
	e, err := NewEncoder(&buf, cfg)
	if err != nil {
		t.Fatalf("NewEncoder(%+v): %v", cfg, err)
	}
	if _, err := e.Write(int16sToBytes(pcm)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// decodeOgg runs an Ogg Opus stream through the full public decoder, which drops
// the pre-skip and applies the granule end-trim, and returns interleaved 48 kHz
// PCM plus the parsed stream info.
func decodeOgg(t *testing.T, stream []byte) ([]int16, Info) {
	t.Helper()
	d, err := NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.WriteTo(&out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return bytesToInt16s(out.Bytes()), d.Info()
}

// impulseSignal returns n interleaved samples per channel that are silent except
// for a full-scale impulse at sample k on every channel.
func impulseSignal(n, channels, k int) []int16 {
	pcm := make([]int16, n*channels)
	for c := range channels {
		pcm[k*channels+c] = 32767
	}
	return pcm
}

// peakIndex returns the per-channel index of the largest absolute sample, which
// for an impulse coded and decoded through Opus is where the impulse landed.
func peakIndex(pcm []int16, channels int) int {
	best, bestVal := -1, -1
	for i := 0; i < len(pcm); i += channels {
		v := int(pcm[i])
		if v < 0 {
			v = -v
		}
		if v > bestVal {
			bestVal, best = v, i/channels
		}
	}
	return best
}

// TestOggOpusPreSkipIs312 pins F3, the pre-skip scaling. opusenc.Lookahead reports
// the delay AT the coding rate, so a container that writes it unscaled puts 52 in
// OpusHead for an 8 kHz stream instead of 312 and misaligns playback by 260 samples.
// Before CP10 every 312 in this repository was a test fixture; this is the first
// assertion on the value the encoder actually COMPUTES.
func TestOggOpusPreSkipIs312(t *testing.T) {
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		for _, ch := range []int{1, 2} {
			t.Run(rateChName(rate, ch), func(t *testing.T) {
				// One 20 ms frame at this rate, so the stream is well-formed.
				pcm := make([]int16, rate/50*ch)
				stream := encodeOgg(t, Config{SampleRate: rate, Channels: ch}, pcm)

				cr, _ := readContainer(t, bytes.NewReader(stream))
				if got := int(cr.head.preSkip); got != expectedPreSkip {
					t.Fatalf("OpusHead pre-skip = %d, want %d (Lookahead(%d) scaled by 48000/%d)",
						got, expectedPreSkip, rate, rate)
				}
				// OpusHead also records the ORIGINAL rate, which is informational and
				// must NOT be 48000 just because the granules are.
				if got := int(cr.head.inputSampleRate); got != rate {
					t.Fatalf("OpusHead input sample rate = %d, want %d", got, rate)
				}
			})
		}
	}
}

func rateChName(rate, ch int) string {
	name := "mono"
	if ch == 2 {
		name = "stereo"
	}
	switch rate {
	case 8000:
		return "8k_" + name
	case 12000:
		return "12k_" + name
	case 16000:
		return "16k_" + name
	case 24000:
		return "24k_" + name
	default:
		return "48k_" + name
	}
}

// TestOggOpusSampleAccurateAlignment is the test that PROVES the pre-skip is right,
// and the only one that can: opus_compare's spectral sensitivity to a 2.5 ms shift
// is weak, so a 192-vs-312 pre-skip slips past every quality metric. Encode an
// impulse at a known sample, decode the container back (which drops the pre-skip),
// and the impulse must land on exactly the sample it started on. A wrong pre-skip
// moves it by exactly the error.
func TestOggOpusSampleAccurateAlignment(t *testing.T) {
	const (
		n = 9600 // 200 ms at 48 kHz
		k = 4800 // impulse at 100 ms, far from both edges
	)
	for _, ch := range []int{1, 2} {
		t.Run(rateChName(48000, ch), func(t *testing.T) {
			pcm := impulseSignal(n, ch, k)
			// A high bitrate keeps the coded impulse sharp; alignment is a timing
			// property, not a quality one, but a crisp peak makes the assertion exact.
			cfg := Config{SampleRate: 48000, Channels: ch, Bitrate: 256000}
			got, info := decodeOgg(t, encodeOgg(t, cfg, pcm))

			if info.PreSkip != expectedPreSkip {
				t.Fatalf("pre-skip = %d, want %d", info.PreSkip, expectedPreSkip)
			}
			if len(got)/ch != n {
				t.Fatalf("decoded %d samples per channel, want %d", len(got)/ch, n)
			}
			peak := peakIndex(got, ch)
			if peak != k {
				t.Fatalf("impulse decoded at sample %d, want %d (off by %d)\n"+
					"a pre-skip of %d instead of %d shifts by exactly %d samples",
					peak, k, peak-k, info.PreSkip, expectedPreSkip, expectedPreSkip-info.PreSkip)
			}
			t.Logf("impulse encoded at %d, decoded at %d (pre-skip %d, %d samples out)",
				k, peak, info.PreSkip, len(got)/ch)
		})
	}
}

// f2Lengths sweeps the input lengths that straddle the RFC 7845 end-padding
// boundary. The encoder codes ceil(N/960) frames, so the coded audio overshoots the
// input by (960 - N mod 960) mod 960, anywhere in [0, 959]. The final granule claims
// preSkip + N. Whenever the overshoot is SMALLER than the pre-skip (312), the granule
// claims samples that were never coded, which RFC 7845 section 4.5 makes invalid.
// That is N mod 960 == 0 (overshoot 0) and N mod 960 in [649, 959] (overshoot 1..311):
// 32.5% of all lengths. 648 is the exact boundary (overshoot 312, the smallest legal
// one) and 649 the first failing residue.
var f2Lengths = []struct {
	n       int
	residue int
	pads    bool // whether the fix must emit an extra silent frame
}{
	{960, 0, true},     // the canonical break: 1 frame coded, granule 1272 claimed
	{3840, 0, true},    // 4 frames coded (3840), granule 4152 claimed
	{3841, 1, false},   // 5 frames coded (4800), granule 4153: already covered
	{4320, 480, false}, // 5 frames (4800), granule 4632: covered
	{4488, 648, false}, // 5 frames (4800), granule 4800: exactly covered, the boundary
	{4489, 649, true},  // 5 frames (4800), granule 4801: one sample over. The first break.
	{4799, 959, true},  // 5 frames (4800), granule 5111: 311 samples over
}

// TestOggOpusGranuleRFC7845 pins F2. Every stream the encoder writes must satisfy
// RFC 7845: the final granule is preSkip + input length (section 7), granules are
// monotone, and NO page may claim a granule beyond the samples actually coded before
// it (section 4.5). The last clause is the one that used to fail for a third of all
// input lengths, and the fix is the end padding in Encoder.Close.
func TestOggOpusGranuleRFC7845(t *testing.T) {
	for _, ch := range []int{1, 2} {
		for _, tc := range f2Lengths {
			t.Run(rateChName(48000, ch)+"_n"+itoa(tc.n)+"_mod"+itoa(tc.residue), func(t *testing.T) {
				if tc.n%frame48k != tc.residue {
					t.Fatalf("test table is wrong: %d mod %d = %d, not %d", tc.n, frame48k, tc.n%frame48k, tc.residue)
				}
				pcm := impulseSignal(tc.n, ch, tc.n/2)
				stream := encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: 128000}, pcm)

				cr, pkts := readContainer(t, bytes.NewReader(stream))
				coded := int64(len(pkts)) * frame48k
				final := cr.finalGranule
				want := int64(expectedPreSkip + tc.n)

				if final != want {
					t.Fatalf("final granule = %d, want preSkip+N = %d+%d = %d", final, expectedPreSkip, tc.n, want)
				}
				// THE RFC 7845 section 4.5 LEGALITY CHECK. This is what F2 broke.
				if final > coded {
					t.Fatalf("final granule %d exceeds the %d samples actually coded (%d packets): "+
						"the stream claims audio it never encoded, which RFC 7845 section 4.5 makes INVALID",
						final, coded, len(pkts))
				}
				// The padding must be minimal: at most one extra 20 ms frame beyond what
				// the input and the pre-skip require.
				naturalFrames := (tc.n + frame48k - 1) / frame48k
				gotPads := len(pkts) > naturalFrames
				if gotPads != tc.pads {
					t.Fatalf("emitted %d packets for %d natural frames (padding=%v), want padding=%v",
						len(pkts), naturalFrames, gotPads, tc.pads)
				}
				if len(pkts) > naturalFrames+1 {
					t.Fatalf("emitted %d packets, more than one silent frame beyond the natural %d",
						len(pkts), naturalFrames)
				}

				checkPageGranules(t, cr, len(pkts))

				// The decoded stream must be exactly as long as the input.
				got, info := decodeOgg(t, stream)
				if info.PreSkip != expectedPreSkip {
					t.Fatalf("pre-skip = %d, want %d", info.PreSkip, expectedPreSkip)
				}
				if len(got)/ch != tc.n {
					t.Fatalf("decoded %d samples per channel, want exactly %d", len(got)/ch, tc.n)
				}
				// Stream duration is finalGranule - preSkip, and must be the input length.
				if dur := final - int64(expectedPreSkip); dur != int64(tc.n) {
					t.Fatalf("stream duration (finalGranule - preSkip) = %d, want %d", dur, tc.n)
				}
			})
		}
	}
}

// TestOggOpusGranuleRFC7845AllRates extends the F2 sweep to every sample rate. The
// padding boundary is NOT rate-independent: at rate Fs a frame is Fs/50 input
// samples but always 960 coded samples, so the overshoot is scaled by 48000/Fs.
// At 8 kHz one input sample is worth 6 coded ones, so the coded audio overshoots
// the input in steps of 6 and the pre-skip is crossed at a different residue than
// at 48 kHz: padding is needed when n mod 160 is 0 or above 108, not above 648.
// Testing only at 48 kHz would leave the low rates, where the container writes the
// same 312 pre-skip against a six-times-coarser frame, entirely unproven.
func TestOggOpusGranuleRFC7845AllRates(t *testing.T) {
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		frameLen := rate / 50 // input samples per 20 ms frame
		scale := sampleRate48k / rate
		// r == 0 and r == frameLen-1 are the extremes of the residue: the first
		// overshoots by nothing, the second by a single input sample (scale coded
		// samples). Both are below the 312-sample pre-skip at every rate, so both
		// MUST pad. The midpoint overshoots by about half a frame and must not.
		for _, r := range []int{0, frameLen - 1, frameLen / 2} {
			n := 4*frameLen + r
			t.Run(rateChName(rate, 1)+"_r"+itoa(r), func(t *testing.T) {
				pcm := impulseSignal(n, 1, n/2)
				stream := encodeOgg(t, Config{SampleRate: rate, Channels: 1, Bitrate: 96000}, pcm)

				cr, pkts := readContainer(t, bytes.NewReader(stream))
				n48 := int64(n * scale)
				coded := int64(len(pkts)) * frame48k
				final := cr.finalGranule

				if want := int64(expectedPreSkip) + n48; final != want {
					t.Fatalf("final granule = %d, want preSkip+N48 = %d+%d = %d", final, expectedPreSkip, n48, want)
				}
				if final > coded {
					t.Fatalf("final granule %d exceeds the %d samples coded by %d packets at %d Hz "+
						"(RFC 7845 section 4.5)", final, coded, len(pkts), rate)
				}
				// The overshoot the input itself provides, before any padding.
				overshoot := int64((frameLen-r)%frameLen) * int64(scale)
				wantPad := overshoot < expectedPreSkip
				naturalFrames := (n + frameLen - 1) / frameLen
				if gotPad := len(pkts) > naturalFrames; gotPad != wantPad {
					t.Fatalf("at %d Hz, n=%d (r=%d, overshoot %d coded samples): padded=%v, want %v",
						rate, n, r, overshoot, gotPad, wantPad)
				}
				checkPageGranules(t, cr, len(pkts))

				got, _ := decodeOgg(t, stream)
				if int64(len(got)) != n48 {
					t.Fatalf("decoded %d samples at 48 kHz, want %d (%d input samples at %d Hz)",
						len(got), n48, n, rate)
				}
			})
		}
	}
}

// checkPageGranules walks the parsed pages and asserts the container-level granule
// invariants: header pages carry granule 0, audio granules never regress, no page
// claims more samples than the packets that have completed by then could hold, and
// the last page is flagged end-of-stream.
func checkPageGranules(t *testing.T, cr *containerReader, audioPackets int) {
	t.Helper()
	if len(cr.pages) < 3 {
		t.Fatalf("expected at least 3 pages (OpusHead, OpusTags, audio), got %d", len(cr.pages))
	}
	// Pages 0 and 1 carry the two header packets, each forced onto its own page.
	for i := range 2 {
		if cr.pages[i].granule != 0 {
			t.Fatalf("header page %d has granule %d, want 0", i, cr.pages[i].granule)
		}
	}
	cumPkts := 0
	prev := int64(0)
	for i, pg := range cr.pages {
		cumPkts += pg.packets
		if i < 2 {
			continue
		}
		if pg.granule == granuleNone {
			continue // no packet completed here; the page carries no granule
		}
		avail := int64(cumPkts-2) * frame48k // minus the two header packets
		if pg.granule > avail {
			t.Fatalf("page %d granule %d exceeds the %d samples coded by the %d audio packets "+
				"completed through it (RFC 7845 section 4.5)", i, pg.granule, avail, cumPkts-2)
		}
		if pg.granule < prev {
			t.Fatalf("page %d granule %d regressed below the previous %d", i, pg.granule, prev)
		}
		prev = pg.granule
	}
	if got := cumPkts - 2; got != audioPackets {
		t.Fatalf("pages account for %d audio packets, reader returned %d", got, audioPackets)
	}
	last := cr.pages[len(cr.pages)-1]
	if last.flags&flagEOS == 0 {
		t.Fatalf("last page is not flagged end-of-stream (flags %#x)", last.flags)
	}
	if cr.pages[0].flags&flagBOS == 0 {
		t.Fatalf("first page is not flagged beginning-of-stream (flags %#x)", cr.pages[0].flags)
	}
}

// itoa keeps the subtest names readable without pulling in strconv at every call
// site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
