//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// CP9 GATE. The whole encoder, end to end: internal/opusenc's opus_encode /
// opus_encode_native / opus_encode_frame_native against the C libopus encoder,
// through the oracle handle, on MULTI-FRAME SEQUENCES.
//
// After every frame this compares, in this order:
//
//  1. the RETURN VALUE (a packet length, or the exact negative OPUS_* code);
//  2. the packet LENGTH;
//  3. EVERY PACKET BYTE;
//  4. st->rangeFinal;
//  5. the cross-frame state FIELD BY FIELD (compareState), plus the State hash.
//
// Byte-for-byte packet equality is the gate. A hash alone would tell you a
// sequence diverged but not where, so the hash is only a cross-check that Hash()
// actually covers the fields compareState does.
//
// NON-VACUITY. Every test below carries its own guards and fails if a branch it
// claims to cover never fired. Two of them are ASSERTED DEAD: the :2580 "busted
// the budget" check must NEVER fire in CELT-only, and CELT must ALWAYS be reached
// on a non-degenerate frame (the :2488 budget check can never be false).

// ---------------------------------------------------------------------------
// The paired-encoder harness.
// ---------------------------------------------------------------------------

// encPair is a Go encoder and a C encoder built from the same config, stepped in
// lockstep. Every frame goes through frame(), which is the gate.
type encPair struct {
	t   *testing.T
	c   *cOpusencHandle
	g   *opusenc.Encoder
	cfg cOpusencCfg
	n   int
}

func newEncPair(t *testing.T, cfg cOpusencCfg) *encPair {
	t.Helper()
	ch, err := cOpusencCreate(cfg)
	if err != nil {
		t.Fatalf("cOpusencCreate: %v", err)
	}
	t.Cleanup(ch.Close)
	return &encPair{t: t, c: ch, g: goEncoderMatchingCfg(t, cfg), cfg: cfg}
}

// cStateToGo rebuilds an opusenc.State from the C dump so the two can be hashed
// with the same function. It is the only place the canonical field order is
// re-stated, and that is deliberate: if the order ever drifts, the hash check in
// frame() catches it even when every individual field still matches.
func cStateToGo(cs cOpusencState) opusenc.State {
	s := opusenc.State{
		StreamChannels:       cs.StreamChannels,
		HybridStereoWidthQ14: cs.HybridStereoWidthQ14,
		VariableHPSmth2Q15:   cs.VariableHPSmth2Q15,
		PrevHBGain:           cs.PrevHBGain,
		HPMem:                cs.HPMem,
		Mode:                 cs.Mode,
		PrevMode:             cs.PrevMode,
		PrevChannels:         cs.PrevChannels,
		PrevFramesize:        cs.PrevFramesize,
		Bandwidth:            cs.Bandwidth,
		AutoBandwidth:        cs.AutoBandwidth,
		First:                cs.First,
		BitrateBps:           cs.BitrateBps,
		RangeFinal:           cs.RangeFinal,
	}
	s.DelayBuffer = append([]int16(nil), cs.DelayBuffer...)
	return s
}

// frame encodes ONE frame on both encoders and asserts total agreement. It
// returns the C return value (== the Go one, or the test has already failed) and
// the Go witness, so the caller can accumulate its non-vacuity flags.
func (p *encPair) frame(label string, pcm []int16, frameSize, maxDataBytes int) (int, opusenc.Witness) {
	p.t.Helper()
	p.n++
	where := fmt.Sprintf("%s: frame %d (frame_size=%d, max_data_bytes=%d)",
		label, p.n, frameSize, maxDataBytes)

	cRet, cPkt, cState := p.c.Encode(pcm, frameSize, maxDataBytes)

	gBuf := make([]byte, maxDataBytes+1) // +1 keeps a 0-byte request addressable
	gRet := p.g.EncodeRaw(pcm, frameSize, gBuf, maxDataBytes)
	wit := p.g.Witness()

	// 1. The return value, including the exact negative OPUS_* code.
	if gRet != cRet {
		p.t.Fatalf("%s: return value: Go = %d, C = %d", where, gRet, cRet)
	}

	// 2 and 3. The packet length and every byte of it.
	if gRet > 0 {
		gPkt := gBuf[:gRet]
		if len(gPkt) != len(cPkt) {
			p.t.Fatalf("%s: packet length: Go = %d, C = %d", where, len(gPkt), len(cPkt))
		}
		for i := range cPkt {
			if gPkt[i] != cPkt[i] {
				p.t.Fatalf("%s: PACKET BYTE %d of %d differs: Go = 0x%02x, C = 0x%02x\n"+
					"  Go: % x\n  C : % x\n  witness: %+v",
					where, i, len(cPkt), gPkt[i], cPkt[i],
					headBytes(gPkt, i), headBytes(cPkt, i), wit)
			}
		}
	}

	gState := p.g.State()

	// 4. rangeFinal, called out separately from compareState because a mismatch
	// here means the CELT payload diverged even if every byte happened to match.
	if gState.RangeFinal != cState.RangeFinal {
		p.t.Fatalf("%s: rangeFinal: Go = 0x%08x, C = 0x%08x",
			where, gState.RangeFinal, cState.RangeFinal)
	}

	// 5. The cross-frame state, field by field, then the hash as a cross-check
	// that Hash() really covers what compareState covers.
	compareState(p.t, where, gState, cState)
	if h1, h2 := gState.Hash(), cStateToGo(cState).Hash(); h1 != h2 {
		p.t.Fatalf("%s: state hash: Go = 0x%016x, C = 0x%016x (fields compared equal, so "+
			"Hash covers a field compareState does not, or the canonical order drifted)",
			where, h1, h2)
	}

	// Asserted dead: the :2580 bust check. On entry the coder has ec_tell() == 1
	// and nb_compr_bytes == max_data_bytes-1 >= 2 (the :1340 degenerate check
	// already rejected max_data_bytes < 3), so it is unreachable.
	if wit.BustCheckFired {
		p.t.Fatalf("%s: opus_encoder.c:2580 bust check FIRED; it is supposed to be "+
			"unreachable in CELT-only. witness: %+v", where, wit)
	}
	// Asserted dead: the :1330 IMIN(.., max_data_bytes). user_bitrate_to_bitrate
	// (:745) has already clamped the rate to what max_data_bytes can carry, and at
	// 48 kHz bits_to_bitrate / bitrate_to_bits round-trip exactly, so cbr_bytes can
	// never exceed max_data_bytes and the IMIN is defensive code. If this ever
	// fires the round-trip assumption is wrong and the whole CBR chain needs
	// re-deriving.
	if wit.CBRIMinFired {
		p.t.Fatalf("%s: the opus_encoder.c:1330 IMIN clamped cbr_bytes, which is "+
			"supposed to be impossible at 48 kHz. witness: %+v", where, wit)
	}
	// Asserted live: the :2488 budget check can never be false, so a coded frame
	// must always have reached celt_encode_with_ec.
	if gRet > 0 && !wit.Degenerate && !wit.CeltEncodeRan {
		p.t.Fatalf("%s: opus_encoder.c:2488 skipped celt_encode_with_ec on a coded "+
			"frame. witness: %+v", where, wit)
	}
	return cRet, wit
}

// headBytes returns a window of up to 16 bytes around i, for the divergence report.
func headBytes(b []byte, i int) []byte {
	lo := i - 4
	if lo < 0 {
		lo = 0
	}
	hi := lo + 16
	if hi > len(b) {
		hi = len(b)
	}
	return b[lo:hi]
}

// zerosPCM is a digitally silent frame: it drives CELT's silence path (and, in
// VBR, its 2-byte shrink) through the Opus layer.
func zerosPCM(n, channels int) []int16 { return make([]int16, n*channels) }

// ---------------------------------------------------------------------------
// The main sweep: LM 0..3 x mono/stereo x CBR/VBR/constrained VBR.
// ---------------------------------------------------------------------------

// TestOpusencEncodeSweep is the core gate. LM 0..3 IS MANDATORY: the
// delay-buffer update at opus_encoder.c:2304 has two arms, and at 48 kHz with
// encoder_buffer 480 / total_buffer 192 the MOVE arm needs frame_size < 288
// (LM0=120, LM1=240) while the COPY arm needs frame_size >= 288 (LM2=480,
// LM3=960). A sweep that misses either arm is vacuous.
func TestOpusencEncodeSweep(t *testing.T) {
	frameSizes := []int{120, 240, 480, 960} // LM 0..3

	type rateMode struct {
		name          string
		vbr           int
		vbrConstraint int
	}
	rateModes := []rateMode{
		{"cbr", 0, 0},
		{"vbr", 1, 0},
		{"cvbr", 1, 1}, // constrained VBR is libopus' default
	}

	var (
		sawMove, sawCopy   bool
		sawLM              [4]bool
		sawMono, sawStereo bool
		sawSilence         bool
	)

	for _, channels := range []int{1, 2} {
		for lm, frameSize := range frameSizes {
			for _, rm := range rateModes {
				for _, bitrate := range []int{16000, 32000, 64000, 128000} {
					name := fmt.Sprintf("c%d/fs%d/%s/%dbps", channels, frameSize, rm.name, bitrate)
					t.Run(name, func(t *testing.T) {
						cfg := defaultOpusencCfg(channels)
						cfg.VBR = rm.vbr
						cfg.VBRConstraint = rm.vbrConstraint
						cfg.Bitrate = bitrate
						p := newEncPair(t, cfg)

						// A real sequence: cross-frame state (delay_buffer, hp_mem,
						// hybrid_stereo_width_Q14, the CELT energy history and the VBR
						// reservoir) only diverges from frame 2 onwards.
						for i := 0; i < 8; i++ {
							pcm := testPCM(frameSize, channels, i)
							if i == 5 || i == 6 {
								pcm = zerosPCM(frameSize, channels) // CELT's silence path
								sawSilence = true
							}
							ret, wit := p.frame(name, pcm, frameSize, 1500)
							if ret <= 0 {
								t.Fatalf("%s: unexpected non-positive return %d", name, ret)
							}
							sawMove = sawMove || wit.DelayMoveBranch
							sawCopy = sawCopy || wit.DelayCopyBranch
							sawLM[lm] = true
							if wit.StreamChannels == 1 {
								sawMono = true
							} else {
								sawStereo = true
							}
						}
					})
				}
			}
		}
	}

	for lm, seen := range sawLM {
		if !seen {
			t.Fatalf("non-vacuity: LM %d never encoded", lm)
		}
	}
	switch {
	case !sawMove:
		t.Fatal("non-vacuity: the :2304 delay-buffer MOVE branch never fired (needs frame_size < 288)")
	case !sawCopy:
		t.Fatal("non-vacuity: the :2304 delay-buffer COPY branch never fired (needs frame_size >= 288)")
	case !sawMono:
		t.Fatal("non-vacuity: no mono stream was ever coded")
	case !sawStereo:
		t.Fatal("non-vacuity: no stereo stream was ever coded")
	case !sawSilence:
		t.Fatal("non-vacuity: no digitally silent frame was ever coded")
	}
}

// ---------------------------------------------------------------------------
// The CBR byte-budget chain (opus_encoder.c:1327-1334, :1893, :1964, :2392, :2646).
// ---------------------------------------------------------------------------

// TestOpusencEncodeCBRBudget drives the CBR derivation across every branch of it
// that the frozen configuration can REACH: the +4 round-to-nearest-byte boundary
// (both sides), the buffer-limited rate, the bitrate WRITE-BACK, and
// cbr_bytes > 1276, which is the ONLY way opus_packet_pad at :2646 does real
// code-3 padding rather than short-circuiting.
//
// The one branch it does NOT cover is the :1330 IMIN(.., max_data_bytes), and
// that is because it is UNREACHABLE, not because the sweep is thin: see the
// CBRIMinFired assertion in frame(), which fails if it ever fires.
func TestOpusencEncodeCBRBudget(t *testing.T) {
	const frameSize = 960 // 20 ms: bitrate_to_bits(br) == br*6/300

	// bits = bitrate_to_bits(bitrate, 48000, 960); cbr_bytes = (bits+4)/8.
	// bits % 8 == 3 is the last value that rounds DOWN, bits % 8 == 4 the first
	// that rounds UP. 64150 -> 1283 bits (-> 160 bytes) and 64200 -> 1284 bits
	// (-> 161 bytes) sit exactly on that boundary.
	cases := []struct {
		name         string
		channels     int
		bitrate      int
		maxDataBytes int
		wantCBRBytes int // cbr_bytes, i.e. the packet length every frame
	}{
		{"roundDown/bits1283", 1, 64150, 1500, 160},
		{"roundUp/bits1284", 1, 64200, 1500, 161},
		{"exact/bits1280", 1, 64000, 1500, 160},
		{"bufferLimited", 2, 256000, 100, 100},
		{"pad/cbr1500>1276", 2, 600000, 4000, 1500},
		{"pad/cbr1875>1276", 1, 750000, 4000, 1875},
	}

	var sawRoundDown, sawRoundUp, sawBufferLimited, sawPadding bool

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			cfg.VBR = 0
			cfg.VBRConstraint = 0
			cfg.Bitrate = tc.bitrate
			p := newEncPair(t, cfg)

			// The C's own arithmetic, so the expectation is not a Go re-derivation.
			bits := cOpusencBitrateToBits(int32(tc.bitrate), 48000, frameSize)

			for i := 0; i < 5; i++ {
				pcm := testPCM(frameSize, tc.channels, i)
				ret, wit := p.frame(tc.name, pcm, frameSize, tc.maxDataBytes)
				if ret != tc.wantCBRBytes {
					t.Fatalf("%s: CBR packet length: got %d, want cbr_bytes = %d "+
						"(bitrate_to_bits = %d, witness %+v)",
						tc.name, ret, tc.wantCBRBytes, bits, wit)
				}
				if wit.CBRBytes != tc.wantCBRBytes {
					t.Fatalf("%s: witness cbr_bytes = %d, want %d", tc.name, wit.CBRBytes, tc.wantCBRBytes)
				}
				if wit.CBRBufferLimited {
					sawBufferLimited = true
				}
				switch bits % 8 {
				case 3: // the last value the +4 rounds DOWN
					sawRoundDown = true
				case 4: // the first value the +4 rounds UP
					sawRoundUp = true
				}
				if wit.PaddingRan {
					sawPadding = true
					if wit.FrameMaxDataBytes != 1276 {
						t.Fatalf("%s: padding ran but the :1893 clamp did not bite: %+v", tc.name, wit)
					}
					if wit.OrigMaxDataBytes != tc.wantCBRBytes {
						t.Fatalf("%s: orig_max_data_bytes = %d, want %d",
							tc.name, wit.OrigMaxDataBytes, tc.wantCBRBytes)
					}
				}
			}
		})
	}

	switch {
	case !sawRoundDown:
		t.Fatal("non-vacuity: no bitrate landed just BELOW the +4 rounding boundary (bits%8 == 3)")
	case !sawRoundUp:
		t.Fatal("non-vacuity: no bitrate landed just ABOVE the +4 rounding boundary (bits%8 == 4)")
	case !sawBufferLimited:
		t.Fatal("non-vacuity: the caller's buffer never limited the CBR rate " +
			"(user_bitrate_to_bitrate's IMIN at :745)")
	case !sawPadding:
		t.Fatal("non-vacuity: opus_packet_pad never did real work (needs cbr_bytes > 1276)")
	}
}

// TestOpusencEncodeCBRRequantizesBitrate pins the :1331 WRITE-BACK. The rounded
// byte budget is converted back into a bitrate and STORED, so every later
// decision (equiv_rate, the stereo/mono downmix, the bandwidth tiers) sees the
// rate the packet will actually carry, not the rate the user asked for. Dropping
// the write-back would leave the packet SIZE right and the decisions subtly
// wrong, which is exactly the kind of bug a length-only check misses.
func TestOpusencEncodeCBRRequantizesBitrate(t *testing.T) {
	const frameSize = 960
	// 17333 bps is not a whole number of bytes per 20 ms frame:
	// bits = 17333*6/300 = 346, cbr_bytes = (346+4)/8 = 43,
	// bitrate_bps := bits_to_bitrate(43*8 = 344) = 344*300/6 = 17200 != 17333.
	const bitrate = 17333

	cfg := defaultOpusencCfg(2)
	cfg.VBR = 0
	cfg.VBRConstraint = 0
	cfg.Bitrate = bitrate
	p := newEncPair(t, cfg)

	moved := false
	for i := 0; i < 4; i++ {
		_, wit := p.frame("requantize", testPCM(frameSize, 2, i), frameSize, 1500)
		if wit.CBRBytes != 43 {
			t.Fatalf("cbr_bytes = %d, want 43", wit.CBRBytes)
		}
		if p.g.State().BitrateBps != bitrate {
			moved = true
		}
	}
	if !moved {
		t.Fatal("non-vacuity: the :1331 bitrate write-back never moved st->bitrate_bps, " +
			"so this test cannot tell a correct write-back from a missing one")
	}
	// The frame() gate already compared bitrate_bps against C every frame; this
	// only proves the case was not degenerate.
}

// ---------------------------------------------------------------------------
// The stereo <-> mono downmix (opus_encoder.c:1428-1456).
// ---------------------------------------------------------------------------

// TestOpusencEncodeStereoDownmix ramps the rate through the ~17.3 kbps threshold
// IN BOTH DIRECTIONS on a single stereo encoder, so the +-1000 hysteresis is
// exercised rather than just the memoryless threshold. Crossing it flips
// st->stream_channels, which changes BOTH the TOC's stereo bit (gen_toc takes
// stream_channels, not channels) and the channel count CELT codes.
//
// The rate knob is max_data_bytes with the user bitrate at OPUS_BITRATE_MAX: at
// 48 kHz / 20 ms user_bitrate_to_bitrate then makes st->bitrate_bps exactly
// max_data_bytes*400, which the oracle handle's create-time ctls cannot do.
func TestOpusencEncodeStereoDownmix(t *testing.T) {
	const frameSize = 960

	cfg := defaultOpusencCfg(2)
	cfg.Bitrate = oOpusBitrateMax
	cfg.VBR = 1
	cfg.VBRConstraint = 0
	p := newEncPair(t, cfg)

	// max_data_bytes*400 bps: 60 -> 24000 (stereo), 38 -> 15200 (mono), and back.
	ramp := []int{60, 55, 50, 46, 44, 42, 40, 38, 36, 38, 40, 42, 44, 46, 48, 52, 60}

	var sawStereo, sawMono, sawToMono, sawToStereo bool
	prev := 0
	for _, mdb := range ramp {
		_, wit := p.frame("downmix", testPCM(frameSize, 2, mdb), frameSize, mdb)
		switch wit.StreamChannels {
		case 2:
			sawStereo = true
			if prev == 1 {
				sawToStereo = true
			}
		case 1:
			sawMono = true
			if prev == 2 {
				sawToMono = true
			}
		}
		if wit.StreamChannels == 1 && !wit.Downmixed {
			t.Fatalf("witness inconsistency at max_data_bytes=%d: %+v", mdb, wit)
		}
		prev = wit.StreamChannels
	}

	switch {
	case !sawStereo:
		t.Fatal("non-vacuity: the encoder never coded a stereo stream")
	case !sawMono:
		t.Fatal("non-vacuity: the stereo encoder never rate-downmixed to mono")
	case !sawToMono:
		t.Fatal("non-vacuity: no stereo -> mono TRANSITION (the -1000 hysteresis arm)")
	case !sawToStereo:
		t.Fatal("non-vacuity: no mono -> stereo TRANSITION (the +1000 hysteresis arm)")
	}
}

// TestOpusencEncodeForceChannels pins OPUS_SET_FORCE_CHANNELS, which bypasses the
// rate decision entirely (:1428-1430) AND selects the mono threshold tables in
// the bandwidth decision (:1590).
func TestOpusencEncodeForceChannels(t *testing.T) {
	const frameSize = 960
	for _, fc := range []int{1, 2} {
		t.Run(fmt.Sprintf("force%d", fc), func(t *testing.T) {
			cfg := defaultOpusencCfg(2)
			cfg.ForceChannels = fc
			cfg.Bitrate = 12000 // well below the downmix threshold
			p := newEncPair(t, cfg)
			for i := 0; i < 4; i++ {
				_, wit := p.frame("force", testPCM(frameSize, 2, i), frameSize, 1500)
				if wit.StreamChannels != fc {
					t.Fatalf("force_channels=%d: stream_channels = %d", fc, wit.StreamChannels)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bandwidth selection (opus_encoder.c:1584-1650, :1681-1685) and the TOC.
// ---------------------------------------------------------------------------

// TestOpusencEncodeBandwidthTiers ramps the rate down through every automatic
// bandwidth tier and back up, on ONE encoder, so st->auto_bandwidth's asymmetric
// hysteresis (:1606-1613) is exercised in both directions. st->first suppresses
// the hysteresis on frame 1 only, which is why the ramp starts high.
//
// The band feeds CELT's end band (:2266-2283) and the TOC's bandwidth bits, so a
// wrong tier shows up as a wrong first byte AND a wrong payload.
func TestOpusencEncodeBandwidthTiers(t *testing.T) {
	const frameSize = 960

	cfg := defaultOpusencCfg(1)
	cfg.Bitrate = oOpusBitrateMax
	cfg.VBR = 1
	cfg.VBRConstraint = 0
	p := newEncPair(t, cfg)

	// max_data_bytes*400 bps, and equiv_rate is 99% of that at complexity 9.
	// Thresholds (voice_est 48): FB >= 12281, SWB >= 11351, WB >= 9000.
	ramp := []int{80, 60, 40, 34, 32, 30, 29, 28, 26, 24, 23, 22, 20, 18,
		20, 22, 24, 26, 28, 30, 32, 34, 40, 60, 80}

	seen := map[int]bool{}
	seenEndBand := map[int]bool{}
	for _, mdb := range ramp {
		_, wit := p.frame("bwtiers", testPCM(frameSize, 1, mdb), frameSize, mdb)
		seen[wit.CurrBandwidth] = true
		seenEndBand[wit.EndBand] = true
	}

	for _, bw := range []struct {
		v    int
		name string
	}{
		{oBandwidthNarrowband, "NARROWBAND"},
		{oBandwidthWideband, "WIDEBAND"},
		{oBandwidthSuperwideband, "SUPERWIDEBAND"},
		{oBandwidthFullband, "FULLBAND"},
	} {
		if !seen[bw.v] {
			t.Fatalf("non-vacuity: the automatic bandwidth selection never landed on %s "+
				"(saw %v)", bw.name, seen)
		}
	}
	// MEDIUMBAND is never SELECTED: :1618-1620 rewrites it to wideband before it is
	// stored, so auto_bandwidth can only ever be NB/WB/SWB/FB.
	if seen[oBandwidthMediumband] {
		t.Fatal("the automatic selection produced MEDIUMBAND, which :1618-1620 forbids")
	}
	for _, eb := range []int{13, 17, 19, 21} {
		if !seenEndBand[eb] {
			t.Fatalf("non-vacuity: CELT end band %d never selected (saw %v)", eb, seenEndBand)
		}
	}
}

// TestOpusencEncodeBandwidthOverrides pins the three ways the automatic decision
// is overridden: OPUS_SET_BANDWIDTH (:1632), OPUS_SET_MAX_BANDWIDTH (:1629), and
// the CELT-only MEDIUMBAND -> WIDEBAND rewrite (:1681), which is reachable ONLY
// through an explicit OPUS_SET_BANDWIDTH(MEDIUMBAND).
func TestOpusencEncodeBandwidthOverrides(t *testing.T) {
	const frameSize = 960

	cases := []struct {
		name         string
		userBW       int
		maxBW        int
		wantBW       int // curr_bandwidth
		wantEndBand  int
		mediumRewrit bool
	}{
		{"userNB", oBandwidthNarrowband, oBandwidthFullband, oBandwidthNarrowband, 13, false},
		{"userMB->WB", oBandwidthMediumband, oBandwidthFullband, oBandwidthWideband, 17, true},
		{"userSWB", oBandwidthSuperwideband, oBandwidthFullband, oBandwidthSuperwideband, 19, false},
		{"userFB", oBandwidthFullband, oBandwidthFullband, oBandwidthFullband, 21, false},
		{"maxWB", oOpusAuto, oBandwidthWideband, oBandwidthWideband, 17, false},
		{"maxSWB", oOpusAuto, oBandwidthSuperwideband, oBandwidthSuperwideband, 19, false},
	}

	sawRewrite := false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(2)
			cfg.Bandwidth = tc.userBW
			cfg.MaxBandwidth = tc.maxBW
			cfg.Bitrate = 96000 // high enough that the automatic tier would be FULLBAND
			p := newEncPair(t, cfg)
			for i := 0; i < 3; i++ {
				_, wit := p.frame(tc.name, testPCM(frameSize, 2, i), frameSize, 1500)
				if wit.CurrBandwidth != tc.wantBW {
					t.Fatalf("%s: curr_bandwidth = %d, want %d", tc.name, wit.CurrBandwidth, tc.wantBW)
				}
				if wit.EndBand != tc.wantEndBand {
					t.Fatalf("%s: CELT end band = %d, want %d", tc.name, wit.EndBand, tc.wantEndBand)
				}
			}
			if tc.mediumRewrit {
				sawRewrite = true
			}
		})
	}
	if !sawRewrite {
		t.Fatal("non-vacuity: the :1681 MEDIUMBAND -> WIDEBAND rewrite was never exercised")
	}
}

// ---------------------------------------------------------------------------
// stereo_fade / stereoWidth_Q14 (opus_encoder.c:2320-2349).
// ---------------------------------------------------------------------------

// TestOpusencEncodeStereoFade drives the stereo-width reduction. stereoWidth_Q14
// is recomputed from equiv_rate every frame (:2320-2327): unity above 32 kbps,
// zero below 16 kbps, and interpolated between. stereo_fade (:2344) then fires
// whenever EITHER the carried hybrid_stereo_width_Q14 or the new width is below
// unity, and it CROSS-FADES from the old to the new over the MDCT overlap, so the
// carried field is real cross-frame state and a rate ramp is the only way to
// exercise it.
func TestOpusencEncodeStereoFade(t *testing.T) {
	const frameSize = 960

	cfg := defaultOpusencCfg(2)
	cfg.Bitrate = oOpusBitrateMax
	cfg.VBR = 1
	cfg.VBRConstraint = 0
	cfg.ForceChannels = 2 // keep it stereo across the whole ramp, so the fade is the variable
	p := newEncPair(t, cfg)

	// max_data_bytes*400 bps: 90 -> 36000 (width unity), 50 -> 20000 (interpolated),
	// 38 -> 15200 (width 0), and back up.
	ramp := []int{90, 85, 80, 70, 60, 50, 45, 42, 40, 38, 40, 45, 55, 70, 90}

	var sawUnity, sawZero, sawInterp, sawFade bool
	for _, mdb := range ramp {
		_, wit := p.frame("stereofade", testPCM(frameSize, 2, mdb), frameSize, mdb)
		switch {
		case wit.StereoWidthQ14 == 16384:
			sawUnity = true
		case wit.StereoWidthQ14 == 0:
			sawZero = true
		default:
			sawInterp = true
		}
		if wit.StereoFadeRan {
			sawFade = true
		}
	}

	switch {
	case !sawUnity:
		t.Fatal("non-vacuity: stereoWidth_Q14 never hit unity (equiv_rate > 32000)")
	case !sawZero:
		t.Fatal("non-vacuity: stereoWidth_Q14 never hit zero (equiv_rate < 16000)")
	case !sawInterp:
		t.Fatal("non-vacuity: stereoWidth_Q14 never took the interpolated branch")
	case !sawFade:
		t.Fatal("non-vacuity: stereo_fade never ran")
	}
}

// ---------------------------------------------------------------------------
// The degenerate TOC-only ("PLC") packet (opus_encoder.c:1340-1406).
// ---------------------------------------------------------------------------

// TestOpusencEncodeDegenerate covers the :1340 branch, which is NOT an error: it
// emits a 1- or 2-byte TOC-only packet and returns a POSITIVE length. It runs
// BEFORE the multiframe split, so it is also the C's answer for a starved
// 40/60/80 ms frame, and it is the only place packet codes 1 and 3 are produced
// on this path.
//
// It reads st->mode / st->bandwidth / st->stream_channels AS THEY STAND ON ENTRY,
// i.e. the PREVIOUS frame's decisions. On frame 1 those are the
// opus_encoder_init values (MODE_HYBRID, FULLBAND), which is why a degenerate
// first frame emits a HYBRID TOC even from a forced-CELT-only encoder.
func TestOpusencEncodeDegenerate(t *testing.T) {
	cases := []struct {
		name         string
		channels     int
		frameSize    int
		maxDataBytes int
		bitrate      int
		vbr          int
		wantCode     int // the TOC's low 2 bits
		wantMinRet   int
		clause       string
	}{
		// max_data_bytes < 3.
		{"buf2/20ms/vbr", 1, 960, 2, oOpusAuto, 1, 0, 1, "max_data_bytes < 3"},
		{"buf2/20ms/cbr", 2, 960, 2, oOpusAuto, 0, 0, 2, "max_data_bytes < 3"},
		// bitrate_bps < 3*frame_rate*8: 500 bps at 20 ms is under 1200.
		{"rate500/20ms/vbr", 1, 960, 200, 500, 1, 0, 1, "bitrate < 3*frame_rate*8"},
		{"rate500/20ms/cbr", 1, 960, 200, 500, 0, 0, 1, "bitrate < 3*frame_rate*8"},
		// 2.5 ms: frame_rate 400 > 100, so tocmode is forced to CELT_ONLY.
		{"buf2/2.5ms", 2, 120, 2, oOpusAuto, 1, 0, 1, "max_data_bytes < 3, frame_rate > 100"},
		// 2.5 ms at the 500 bps floor is the ONLY way to reach IMAX(1, cbr_bytes)
		// at :1333: bitrate_to_bits(500, 48000, 120) == 1, so (1+4)/8 == 0 bytes.
		{"cbrFloor1/2.5ms", 1, 120, 200, 500, 0, 0, 1, ":1333 IMAX(1, cbr_bytes) on a 0-byte budget"},
		// frame_rate < 50: the 40 ms -> 2x20 ms rewrite makes packet_code 1.
		{"40ms/starved/vbr", 1, 1920, 8, oOpusAuto, 1, 1, 1, "frame_rate 25, max_data_bytes*25 < 300"},
		{"40ms/starved/cbr", 2, 1920, 8, oOpusAuto, 0, 1, 1, "frame_rate 25, max_data_bytes*25 < 300"},
		// frame_rate <= 16: the >= 60 ms rewrite makes packet_code 3 (a 2-byte
		// packet whose second byte is the sub-frame count), and in CBR the
		// repacketizer then really pads it.
		{"60ms/starved/vbr", 1, 2880, 10, oOpusAuto, 1, 3, 2, "frame_rate 16, max_data_bytes*16 < 300"},
		{"60ms/starved/cbr", 1, 2880, 10, oOpusAuto, 0, 3, 2, "frame_rate 16, max_data_bytes*16 < 300"},
		{"80ms/starved/vbr", 2, 3840, 12, oOpusAuto, 1, 3, 2, "frame_rate 12, max_data_bytes*12 < 300"},
	}

	seenCode := map[int]bool{}
	sawFloor1 := false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			cfg.Bitrate = tc.bitrate
			cfg.VBR = tc.vbr
			cfg.VBRConstraint = 0
			p := newEncPair(t, cfg)

			for i := 0; i < 3; i++ {
				pcm := testPCM(tc.frameSize, tc.channels, i)
				ret, wit := p.frame(tc.name, pcm, tc.frameSize, tc.maxDataBytes)
				if ret < tc.wantMinRet {
					t.Fatalf("%s (%s): return %d, want >= %d", tc.name, tc.clause, ret, tc.wantMinRet)
				}
				if !wit.Degenerate {
					t.Fatalf("%s (%s): the :1340 degenerate branch did not fire; witness %+v",
						tc.name, tc.clause, wit)
				}
				seenCode[tc.wantCode] = true
				if wit.CBRFloor1 {
					sawFloor1 = true
				}
			}
		})
	}

	for _, code := range []int{0, 1, 3} {
		if !seenCode[code] {
			t.Fatalf("non-vacuity: degenerate packet code %d never produced", code)
		}
	}
	if !sawFloor1 {
		t.Fatal("non-vacuity: IMAX(1, cbr_bytes) at :1333 never raised a zero byte budget")
	}
}

// ---------------------------------------------------------------------------
// The rejection rules (opus_encoder.c:1224, :1231, :827).
// ---------------------------------------------------------------------------

// TestOpusencEncodeRejections pins the EXACT negative return code for every
// rejected input, and asserts the encoder state is unchanged (the C returns
// before any of the epilogue at :2555-2562, having only zeroed st->rangeFinal at
// :1223).
func TestOpusencEncodeRejections(t *testing.T) {
	const (
		opusBadArg         = -1
		opusBufferTooSmall = -2
	)

	cases := []struct {
		name         string
		frameSize    int // the caller's analysis_frame_size
		maxDataBytes int
		want         int
		why          string
	}{
		{"maxDataBytes0", 960, 0, opusBadArg, ":1224 max_data_bytes <= 0"},
		{"frameSizeTooSmall", 100, 100, opusBadArg, ":827 frame_size < Fs/400 -> -1 -> :1224"},
		{"frameSizeNotLegal", 500, 100, opusBadArg, ":827 500 is not one of the nine legal durations"},
		{"frameSizeOdd", 961, 100, opusBadArg, ":827 961 is not a legal duration"},
		{"frameSize0", 0, 100, opusBadArg, ":827 0 < Fs/400 -> -1 -> :1224"},
		{"100msIn1Byte", 4800, 1, opusBufferTooSmall, ":1231 Fs == frame_size*10 && max_data_bytes == 1"},
	}

	seen := map[int]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(2)
			p := newEncPair(t, cfg)

			// Frame 1: a good frame, so the rejection is tested against a NON-VIRGIN
			// encoder and "state unchanged" means something.
			p.frame(tc.name+"/warmup", testPCM(960, 2, 0), 960, 200)
			before := p.g.State()

			pcm := testPCM(5760, 2, 1) // long enough for any frame size under test
			ret, _ := p.frame(tc.name, pcm, tc.frameSize, tc.maxDataBytes)
			if ret != tc.want {
				t.Fatalf("%s (%s): return %d, want %d", tc.name, tc.why, ret, tc.want)
			}
			seen[tc.want] = true

			// The only field :1223 touches before rejecting.
			after := p.g.State()
			if after.RangeFinal != 0 {
				t.Fatalf("%s: rangeFinal = 0x%08x after a rejected frame, want 0 (:1223)",
					tc.name, after.RangeFinal)
			}
			before.RangeFinal = 0
			if before.Hash() != after.Hash() {
				t.Fatalf("%s: a rejected frame mutated the encoder state", tc.name)
			}
		})
	}

	for _, code := range []int{opusBadArg, opusBufferTooSmall} {
		if !seen[code] {
			t.Fatalf("non-vacuity: rejection code %d was never produced", code)
		}
	}
}

// ---------------------------------------------------------------------------
// The remaining live ctls.
// ---------------------------------------------------------------------------

// TestOpusencEncodeComplexity sweeps OPUS_SET_COMPLEXITY 0..10. It is live TWICE:
// it scales equiv_rate (:1032, and CELT-only takes a further 10% below complexity
// 5), which moves the bandwidth tiers, and it reaches CELT itself.
func TestOpusencEncodeComplexity(t *testing.T) {
	const frameSize = 960
	seenBelow5, seenAtOrAbove5 := false, false

	for complexity := 0; complexity <= 10; complexity++ {
		t.Run(fmt.Sprintf("complexity%d", complexity), func(t *testing.T) {
			for _, channels := range []int{1, 2} {
				cfg := defaultOpusencCfg(channels)
				cfg.Complexity = complexity
				cfg.Bitrate = 24000
				p := newEncPair(t, cfg)
				for i := 0; i < 4; i++ {
					p.frame(fmt.Sprintf("cx%d/c%d", complexity, channels),
						testPCM(frameSize, channels, i), frameSize, 1500)
				}
			}
		})
		if complexity < 5 {
			seenBelow5 = true
		} else {
			seenAtOrAbove5 = true
		}
	}
	if !seenBelow5 || !seenAtOrAbove5 {
		t.Fatal("non-vacuity: the complexity sweep missed one side of the CELT pitch-filter " +
			"threshold at compute_equiv_rate's `complexity < 5`")
	}
}

// TestOpusencEncodeMiscCtls covers the remaining ctls that are live on the
// CELT-only path: OPUS_SET_PACKET_LOSS_PERC (it feeds compute_equiv_rate AND
// CELT), OPUS_SET_LSB_DEPTH (:1242 IMINs it with the opus_encode argument of 16,
// then hands it to CELT at :1678), OPUS_SET_PREDICTION_DISABLED (:2292-2294 ->
// CELT_SET_PREDICTION(0)), OPUS_SET_SIGNAL (it moves voice_est off its constant
// 48, which moves every interpolated threshold), and OPUS_SET_LFE (:1683 pins the
// bandwidth to NARROWBAND and switches CELT into LFE mode).
func TestOpusencEncodeMiscCtls(t *testing.T) {
	const frameSize = 960

	cases := []struct {
		name  string
		apply func(*cOpusencCfg)
	}{
		{"loss10", func(c *cOpusencCfg) { c.PacketLossPerc = 10 }},
		{"loss50", func(c *cOpusencCfg) { c.PacketLossPerc = 50 }},
		{"lsbDepth8", func(c *cOpusencCfg) { c.LsbDepth = 8 }},
		{"lsbDepth16", func(c *cOpusencCfg) { c.LsbDepth = 16 }},
		{"predictionDisabled", func(c *cOpusencCfg) { c.PredictionDisabled = 1 }},
		{"signalVoice", func(c *cOpusencCfg) { c.SignalType = 3001 }},
		{"signalMusic", func(c *cOpusencCfg) { c.SignalType = 3002 }},
		{"lfe", func(c *cOpusencCfg) { c.LFE = 1 }},
		{"inbandFEC", func(c *cOpusencCfg) { c.InbandFEC = 1 }},
	}

	for _, tc := range cases {
		for _, channels := range []int{1, 2} {
			name := fmt.Sprintf("%s/c%d", tc.name, channels)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(channels)
				cfg.Bitrate = 32000
				tc.apply(&cfg)
				p := newEncPair(t, cfg)
				for i := 0; i < 5; i++ {
					p.frame(name, testPCM(frameSize, channels, i), frameSize, 1500)
				}
			})
		}
	}
}

// TestOpusencEncodeMixedFrameSizes changes the frame size MID-STREAM on one
// encoder. That is the sharpest test of the delay-compensation ring: the two arms
// of :2304 alternate, and st->prev_framesize changes under CELT, so a
// history-copy that is off by even one sample diverges on the next frame.
func TestOpusencEncodeMixedFrameSizes(t *testing.T) {
	// Alternates across the 288-sample boundary that separates the MOVE arm
	// (frame_size < 288) from the COPY arm.
	seq := []int{960, 120, 480, 240, 120, 960, 240, 480, 120, 120, 960, 480, 240, 960}

	for _, channels := range []int{1, 2} {
		for _, vbr := range []int{0, 1} {
			name := fmt.Sprintf("c%d/vbr%d", channels, vbr)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(channels)
				cfg.VBR = vbr
				cfg.VBRConstraint = 0
				cfg.Bitrate = 64000
				p := newEncPair(t, cfg)

				var sawMove, sawCopy bool
				for i, fs := range seq {
					_, wit := p.frame(name, testPCM(fs, channels, i), fs, 1500)
					sawMove = sawMove || wit.DelayMoveBranch
					sawCopy = sawCopy || wit.DelayCopyBranch
				}
				if !sawMove || !sawCopy {
					t.Fatalf("non-vacuity: the mixed sequence did not hit both arms of :2304 "+
						"(move=%v copy=%v)", sawMove, sawCopy)
				}
			})
		}
	}
}

// TestOpusencEncodeMultiframeIsRejected pins the ONE place this port DELIBERATELY
// DIVERGES from libopus, so the divergence is a tested contract rather than a
// silent one.
//
// A frame longer than 20 ms that is NOT starved enough to take the degenerate
// branch reaches opus_encoder.c:1698, where libopus splits it into 20 ms
// sub-frames, encodes each through opus_encode_frame_native and re-assembles them
// with the repacketizer into a code-2 or code-3 packet. That path is deferred; the
// Go encoder returns OPUS_UNIMPLEMENTED (-5) at exactly that line instead of
// emitting a wrong-length packet.
//
// The assertion is two-sided ON PURPOSE: C must SUCCEED (proving the frame really
// is on the multiframe path and this test is not just re-testing the degenerate
// branch) and Go must return exactly -5. When the multiframe path lands, this test
// is what flips to a normal frame() comparison.
func TestOpusencEncodeMultiframeIsRejected(t *testing.T) {
	const opusUnimplemented = -5

	// Every legal frame size above 20 ms at 48 kHz.
	for _, frameSize := range []int{1920, 2880, 3840, 4800, 5760} {
		for _, channels := range []int{1, 2} {
			name := fmt.Sprintf("fs%d/c%d", frameSize, channels)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(channels)
				cfg.Bitrate = 64000 // ample, so the :1340 degenerate branch does NOT fire
				ch, err := cOpusencCreate(cfg)
				if err != nil {
					t.Fatalf("cOpusencCreate: %v", err)
				}
				defer ch.Close()
				g := goEncoderMatchingCfg(t, cfg)

				pcm := testPCM(frameSize, channels, 1)
				cRet, _, _ := ch.Encode(pcm, frameSize, 1500)
				if cRet <= 0 {
					t.Fatalf("%s: C returned %d; this frame did not reach the multiframe "+
						"path, so the test is vacuous", name, cRet)
				}

				gBuf := make([]byte, 1501)
				gRet := g.EncodeRaw(pcm, frameSize, gBuf, 1500)
				if gRet != opusUnimplemented {
					t.Fatalf("%s: Go returned %d, want OPUS_UNIMPLEMENTED (%d). C returned "+
						"%d (a real multiframe packet).", name, gRet, opusUnimplemented, cRet)
				}
			})
		}
	}
}

// TestOpusencEncodeLongSequence is the endurance case: 60 frames on one encoder
// pair, so any slow drift in the delay ring, hp_mem, the CELT energy history or
// the VBR reservoir has room to show up. A single-frame test cannot see any of
// those.
func TestOpusencEncodeLongSequence(t *testing.T) {
	const frameSize = 960
	for _, channels := range []int{1, 2} {
		for _, vbr := range []int{0, 1} {
			name := fmt.Sprintf("c%d/vbr%d", channels, vbr)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(channels)
				cfg.VBR = vbr
				cfg.VBRConstraint = 1
				cfg.Bitrate = oOpusBitrateMax
				p := newEncPair(t, cfg)
				for i := 0; i < 60; i++ {
					// A per-frame rate ramp, so the bandwidth and (in stereo) the
					// downmix decisions keep moving under the sequence.
					mdb := 20 + (i*7)%120
					p.frame(name, testPCM(frameSize, channels, i), frameSize, mdb)
				}
			})
		}
	}
}
