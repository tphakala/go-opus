//go:build refc

package oracle

import (
	"bytes"
	"math"
	"reflect"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/opusenc"
	"github.com/tphakala/go-opus/internal/packet"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// CP9-prep gate. These tests do NOT yet drive opus_encode_native end to end (that
// is CP9 proper); they pin the SHARED SEAMS that the CP9 driver is built on, so a
// break shows up here rather than as a mysterious packet divergence later:
//
//  1. the oracle handle triple (create / encode / dump-state / destroy) really
//     runs the C opus_encode and returns a usable field-level state dump;
//  2. internal/opusenc's NewEncoder / Reset reproduce opus_encoder_init and
//     OPUS_RESET_STATE field for field, in the canonical order;
//  3. the flat wrappers over the opus_encoder.c statics agree with their Go
//     counterparts where a Go counterpart already exists (bits_to_bitrate /
//     bitrate_to_bits);
//  4. celt.EncodeWithEC (the external-coder seam) is byte- and state-identical to
//     celt.Encode on this path, which is the equivalence the CBR budget derivation
//     relies on;
//  5. internal/packet.Pad matches opus_packet_pad, including the len == new_len
//     short-circuit that leaves a CBR packet untouched.

// testPCM is a deterministic, non-degenerate signal: a swept tone plus a little
// wideband content, so transient/prefilter/bandwidth decisions are actually
// exercised rather than sitting on a silent-frame fast path.
func testPCM(n, channels int, seed int) []int16 {
	pcm := make([]int16, n*channels)
	phase := float64(seed) * 0.37
	for i := 0; i < n; i++ {
		t := float64(i) + float64(seed)*float64(n)
		v := 0.45*math.Sin(2*math.Pi*(440.0+0.05*t)*t/48000.0+phase) +
			0.18*math.Sin(2*math.Pi*3130.0*t/48000.0) +
			0.07*math.Sin(2*math.Pi*11050.0*t/48000.0)
		s := int16(v * 22000)
		for c := 0; c < channels; c++ {
			// Decorrelate the channels a little so the stereo paths are non-trivial.
			if c == 1 {
				s = int16(v*19000) - int16(v*v*3000)
			}
			pcm[i*channels+c] = s
		}
	}
	return pcm
}

// goEncoderMatchingCfg builds a pure-Go opusenc.Encoder configured exactly like
// the given oracle cfg.
func goEncoderMatchingCfg(t *testing.T, cfg cOpusencCfg) *opusenc.Encoder {
	t.Helper()
	st := opusenc.NewEncoder(int32(cfg.Fs), cfg.Channels, cfg.Application)
	if st == nil {
		t.Fatalf("opusenc.NewEncoder(%d, %d, %d) returned nil", cfg.Fs, cfg.Channels, cfg.Application)
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("SetForceMode", st.SetForceMode(cfg.ForceMode))
	must("SetBitrate", st.SetBitrate(int32(cfg.Bitrate)))
	must("SetComplexity", st.SetComplexity(cfg.Complexity))
	must("SetVBR", st.SetVBR(cfg.VBR))
	must("SetVBRConstraint", st.SetVBRConstraint(cfg.VBRConstraint))
	must("SetForceChannels", st.SetForceChannels(cfg.ForceChannels))
	must("SetMaxBandwidth", st.SetMaxBandwidth(cfg.MaxBandwidth))
	must("SetBandwidth", st.SetBandwidth(cfg.Bandwidth))
	must("SetSignal", st.SetSignal(cfg.SignalType))
	must("SetLsbDepth", st.SetLsbDepth(cfg.LsbDepth))
	must("SetDTX", st.SetDTX(cfg.DTX))
	must("SetInbandFEC", st.SetInbandFEC(cfg.InbandFEC))
	must("SetPacketLossPerc", st.SetPacketLossPerc(cfg.PacketLossPerc))
	must("SetPredictionDisabled", st.SetPredictionDisabled(cfg.PredictionDisabled))
	must("SetLFE", st.SetLFE(cfg.LFE))
	must("SetExpertFrameDuration", st.SetExpertFrameDuration(cfg.ExpertFrameDuration))
	return st
}

// compareState asserts the Go state equals the C dump, field by field, in the
// canonical order. It reports the FIRST differing field by name, which is the
// whole point of a field-level dump over a hash.
func compareState(t *testing.T, where string, got opusenc.State, want cOpusencState) {
	t.Helper()
	type field struct {
		name     string
		got, exp int64
	}
	fields := []field{
		{"stream_channels", int64(got.StreamChannels), int64(want.StreamChannels)},
		{"hybrid_stereo_width_Q14", int64(got.HybridStereoWidthQ14), int64(want.HybridStereoWidthQ14)},
		{"variable_HP_smth2_Q15", int64(got.VariableHPSmth2Q15), int64(want.VariableHPSmth2Q15)},
		{"prev_HB_gain", int64(got.PrevHBGain), int64(want.PrevHBGain)},
		{"hp_mem[0]", int64(got.HPMem[0]), int64(want.HPMem[0])},
		{"hp_mem[1]", int64(got.HPMem[1]), int64(want.HPMem[1])},
		{"hp_mem[2]", int64(got.HPMem[2]), int64(want.HPMem[2])},
		{"hp_mem[3]", int64(got.HPMem[3]), int64(want.HPMem[3])},
		{"mode", int64(got.Mode), int64(want.Mode)},
		{"prev_mode", int64(got.PrevMode), int64(want.PrevMode)},
		{"prev_channels", int64(got.PrevChannels), int64(want.PrevChannels)},
		{"prev_framesize", int64(got.PrevFramesize), int64(want.PrevFramesize)},
		{"bandwidth", int64(got.Bandwidth), int64(want.Bandwidth)},
		{"auto_bandwidth", int64(got.AutoBandwidth), int64(want.AutoBandwidth)},
		{"first", int64(got.First), int64(want.First)},
		{"bitrate_bps", int64(got.BitrateBps), int64(want.BitrateBps)},
		{"rangeFinal", int64(got.RangeFinal), int64(want.RangeFinal)},
	}
	for _, f := range fields {
		if f.got != f.exp {
			t.Errorf("%s: field %s: got %d, want %d (C)", where, f.name, f.got, f.exp)
		}
	}
	if len(got.DelayBuffer) != len(want.DelayBuffer) {
		t.Fatalf("%s: delay_buffer length: got %d, want %d (C)",
			where, len(got.DelayBuffer), len(want.DelayBuffer))
	}
	for i := range got.DelayBuffer {
		if got.DelayBuffer[i] != want.DelayBuffer[i] {
			t.Fatalf("%s: delay_buffer[%d]: got %d, want %d (C)",
				where, i, got.DelayBuffer[i], want.DelayBuffer[i])
		}
	}
}

// TestOpusencInitMatchesC pins opus_encoder_init: the pure-Go NewEncoder plus the
// full ctl set must land on exactly the C encoder's cross-frame state, in the
// canonical field order. This is the contract the CP9 driver is written against.
func TestOpusencInitMatchesC(t *testing.T) {
	for _, channels := range []int{1, 2} {
		cfg := defaultOpusencCfg(channels)
		ce, err := cOpusencCreate(cfg)
		if err != nil {
			t.Fatalf("channels=%d: %v", channels, err)
		}
		defer ce.Close()

		cs := ce.State()

		// NON-VACUITY: pin the C init values themselves, so this test cannot decay
		// into "two all-zero structs are equal" if the shim ever stops reading the
		// live struct. Every one of these is a specific line of opus_encoder_init.
		for _, chk := range []struct {
			name     string
			got, exp int32
		}{
			{"stream_channels", cs.StreamChannels, int32(channels)},       // :246
			{"hybrid_stereo_width_Q14", cs.HybridStereoWidthQ14, 1 << 14}, // :315
			{"prev_HB_gain (Q15ONE)", cs.PrevHBGain, 32767},               // :316
			{"mode (MODE_HYBRID)", cs.Mode, 1001},                         // :319
			{"bandwidth (FULLBAND)", cs.Bandwidth, 1105},                  // :320
			{"first", cs.First, 1},                                        // :318
			{"bitrate_bps", cs.BitrateBps, 3000 + 48000*int32(channels)},  // :302
		} {
			if chk.got != chk.exp {
				t.Fatalf("channels=%d: C init %s = %d, want %d: the oracle state dump is wrong, "+
					"so comparing Go against it would be meaningless",
					channels, chk.name, chk.got, chk.exp)
			}
		}
		// variable_HP_smth2_Q15 = silk_LSHIFT(silk_lin2log(60), 8) (:317). Only its
		// non-zero-ness is asserted here; the exact value is what the Go comparison
		// below pins, and it is the reason silk_lin2log has to be right.
		if cs.VariableHPSmth2Q15 == 0 {
			t.Fatalf("channels=%d: C variable_HP_smth2_Q15 = 0, want silk_lin2log(60)<<8", channels)
		}

		gs := goEncoderMatchingCfg(t, cfg)
		compareState(t, "after init", gs.State(), cs)

		// The delay ring geometry the whole port depends on (48 kHz AUDIO).
		if want := 480 * channels; len(gs.State().DelayBuffer) != want {
			t.Errorf("channels=%d: delay_buffer len = %d, want encoder_buffer*channels = %d",
				channels, len(gs.State().DelayBuffer), want)
		}
		if got := gs.DelayCompensation(); got != 192 {
			t.Errorf("channels=%d: delay_compensation = %d, want Fs/250 = 192", channels, got)
		}
	}
}

// TestOpusencResetMatchesC pins OPUS_RESET_STATE. It matters that the C is driven
// through real frames FIRST: a reset that merely re-ran init would pass on a
// virgin encoder and fail here.
func TestOpusencResetMatchesC(t *testing.T) {
	const channels = 2
	cfg := defaultOpusencCfg(channels)
	cfg.Bitrate = 96000
	cfg.VBR = 0

	ce, err := cOpusencCreate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ce.Close()

	// Dirty the C state with a few real frames.
	for f := 0; f < 4; f++ {
		ret, _, _ := ce.Encode(testPCM(960, channels, f), 960, 1500)
		if ret < 0 {
			t.Fatalf("frame %d: C opus_encode returned %d", f, ret)
		}
	}
	dirty := ce.State()
	if dirty.First != 0 {
		t.Fatalf("C st->first = %d after 4 frames, want 0 (nothing was encoded?)", dirty.First)
	}

	if err := ce.Reset(); err != nil {
		t.Fatal(err)
	}
	cReset := ce.State()

	// bitrate_bps lives ABOVE OPUS_ENCODER_RESET_START (opus_encoder.c:95 vs :111),
	// so OPUS_RESET_STATE must NOT clear it. Assert that rather than assume it.
	if cReset.BitrateBps != dirty.BitrateBps {
		t.Errorf("C reset cleared bitrate_bps (%d -> %d); it is above OPUS_ENCODER_RESET_START and must survive",
			dirty.BitrateBps, cReset.BitrateBps)
	}
	if cReset.First != 1 || cReset.Mode != 1001 /* MODE_HYBRID */ {
		t.Errorf("C reset: first=%d mode=%d, want 1 / 1001", cReset.First, cReset.Mode)
	}

	// The Go side must reach the same place. Give it the same bitrate_bps the C
	// encode loop computed, since Go has not run opus_encode_native yet (CP9).
	gs := goEncoderMatchingCfg(t, cfg)
	gs.Reset()
	got := gs.State()
	got.BitrateBps = cReset.BitrateBps // not part of the reset region; see above
	compareState(t, "after OPUS_RESET_STATE", got, cReset)
}

// TestOpusencHandleTripleRuns is the smoke test for the oracle itself: the shim's
// create / encode / dump-state / destroy triple must actually drive C opus_encode
// and hand back a real packet, a real rangeFinal, and a delay_buffer that has been
// filled. If this is vacuous, every CP9 comparison downstream is vacuous too.
func TestOpusencHandleTripleRuns(t *testing.T) {
	const channels = 2
	cfg := defaultOpusencCfg(channels)
	cfg.Bitrate = 128000

	ce, err := cOpusencCreate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ce.Close()

	pre := ce.State()
	if !allZero(pre.DelayBuffer) {
		t.Fatalf("delay_buffer is not zero before the first frame")
	}

	ret, pkt, st := ce.Encode(testPCM(960, channels, 0), 960, 1500)
	if ret <= 0 {
		t.Fatalf("C opus_encode returned %d, want a positive packet length", ret)
	}
	if len(pkt) != ret {
		t.Fatalf("packet length %d != return value %d", len(pkt), ret)
	}
	// TOC byte for CELT-only / 20 ms: gen_toc sets 0x80 and period=3 (<<3).
	if pkt[0]&0x80 == 0 {
		t.Errorf("TOC 0x%02x: bit 7 clear, so this is not a CELT-only packet", pkt[0])
	}
	if st.RangeFinal == 0 {
		t.Errorf("rangeFinal is 0 after a real frame")
	}
	if allZero(st.DelayBuffer) {
		t.Errorf("delay_buffer still zero after a frame: the state dump is not reading the live struct")
	}
	if st.First != 0 {
		t.Errorf("st->first = %d after one frame, want 0", st.First)
	}
	if st.PrevFramesize != 960 {
		t.Errorf("st->prev_framesize = %d, want 960", st.PrevFramesize)
	}
	if st.Mode != 1002 /* MODE_CELT_ONLY */ {
		t.Errorf("st->mode = %d, want MODE_CELT_ONLY (1002): FORCE_MODE did not take", st.Mode)
	}
}

func allZero(s []int16) bool {
	for _, v := range s {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestOpusencGenTOCReachesC exercises the gen_toc static wrapper across the frame
// sizes and bandwidths CP9 sweeps, so the wrapper itself is known to be wired up.
func TestOpusencGenTOCReachesC(t *testing.T) {
	// 48 kHz frame rates for LM 0..3: 2.5/5/10/20 ms.
	for _, fr := range []int{400, 200, 100, 50} {
		for _, bw := range []int{oBandwidthNarrowband, oBandwidthMediumband,
			oBandwidthWideband, oBandwidthSuperwideband, oBandwidthFullband} {
			for _, ch := range []int{1, 2} {
				toc := cOpusencGenTOC(oModeCeltOnly, fr, bw, ch)
				if toc&0x80 == 0 {
					t.Errorf("gen_toc(CELT_ONLY, %d, %d, %d) = 0x%02x: bit 7 must be set",
						fr, bw, ch, toc)
				}
				wantStereo := byte(0)
				if ch == 2 {
					wantStereo = 1 << 2
				}
				if toc&(1<<2) != wantStereo {
					t.Errorf("gen_toc(..., channels=%d) = 0x%02x: stereo bit wrong", ch, toc)
				}
			}
		}
	}
	// NB and MB both clamp to tmp=0 (the `if (tmp < 0) tmp = 0;` at :344).
	if a, b := cOpusencGenTOC(oModeCeltOnly, 50, oBandwidthNarrowband, 1),
		cOpusencGenTOC(oModeCeltOnly, 50, oBandwidthMediumband, 1); a != b {
		t.Errorf("gen_toc NB (0x%02x) != MB (0x%02x): the tmp<0 clamp at :344 is not being hit", a, b)
	}
}

// TestBitrateHelpersMatchC pins the exported celt.BitsToBitrate / BitrateToBits
// seam against the C static inlines they are transliterated from. The CBR
// byte-budget derivation (opus_encoder.c:1330-1333) is built out of this pair, and
// the two integer divisions must truncate in the C's order.
func TestBitrateHelpersMatchC(t *testing.T) {
	const fs = 48000
	frameSizes := []int32{120, 240, 480, 960}
	bitrates := []int32{500, 2400, 6000, 9000, 12000, 16000, 17000, 19000, 24000,
		32000, 48000, 64000, 96000, 128000, 256000, 510000, 1000000, 1500000}

	for _, fsz := range frameSizes {
		for _, br := range bitrates {
			if got, want := celt.BitrateToBits(br, fs, fsz), cOpusencBitrateToBits(br, fs, fsz); got != want {
				t.Errorf("BitrateToBits(%d, %d, %d) = %d, C = %d", br, fs, fsz, got, want)
			}
		}
		// bits_to_bitrate over the byte budgets the CBR path actually produces,
		// including the 1276 boundary where opus_packet_pad starts doing real work.
		for _, bytesN := range []int32{1, 2, 3, 8, 100, 250, 1275, 1276, 1277, 2000, 7656} {
			bits := bytesN * 8
			if got, want := celt.BitsToBitrate(bits, fs, fsz), cOpusencBitsToBitrate(bits, fs, fsz); got != want {
				t.Errorf("BitsToBitrate(%d, %d, %d) = %d, C = %d", bits, fs, fsz, got, want)
			}
		}
	}
}

// TestCeltEncodeWithECMatchesEncode pins the celt.EncodeWithEC seam. The CP9 brief
// argues the external-coder call shape (enc != NULL, compressed == NULL) is
// NUMERICALLY EQUIVALENT to the enc == NULL shape on this path, because a fresh
// coder has ec_tell()==1 and nbFilledBytes==0, tell0_frac is read only in the
// hybrid branch, and the CBR clamp that would ec_enc_shrink needs
// bitrate != OPUS_BITRATE_MAX. This test does not TRUST that argument, it CHECKS
// it: over a multi-frame sequence the two shapes must produce identical packets
// AND identical cross-frame CELT state.
func TestCeltEncodeWithECMatchesEncode(t *testing.T) {
	cases := []struct {
		name       string
		channels   int
		frameSize  int
		nbBytes    int
		vbr        int
		bitrate    int32
		signalling int
		// wantShrink asserts the :1917-1920 CBR clamp actually LOWERED
		// nbCompressedBytes, i.e. the ec_enc_shrink that only exists on the
		// external-coder path really ran. Without this the shrink branch could be
		// dead and the test would still pass, which is exactly the vacuity trap.
		wantShrink bool
	}{
		{"mono/20ms/vbr", 1, 960, 250, 1, 64000, 0, false},
		{"stereo/20ms/vbr", 2, 960, 250, 1, 96000, 0, false},
		{"stereo/20ms/cbr-bitratemax", 2, 960, 120, 0, -1 /* OPUS_BITRATE_MAX */, 0, false},
		{"mono/10ms/vbr", 1, 480, 120, 1, 48000, 0, false},
		{"mono/5ms/vbr", 1, 240, 60, 1, 48000, 0, false},
		{"mono/2.5ms/vbr", 1, 120, 40, 1, 48000, 0, false},
		// A real CBR bitrate (bitrate != OPUS_BITRATE_MAX, vbr == 0) is the ONLY
		// configuration in which the :1917 clamp runs at all, and therefore the only
		// one where the external-coder path takes a branch (ec_enc_shrink) that the
		// enc==NULL path does not. signalling=1 additionally exercises the
		// `-!!st->signalling` term. Neither is reachable from opus_encoder (it pins
		// CELT's bitrate to OPUS_BITRATE_MAX), but the seam must still be right, and
		// these two cases are what make this test non-vacuous.
		{"mono/20ms/cbr-signalling", 1, 960, 250, 0, 64000, 1, true},
		{"mono/20ms/cbr-nosignalling", 1, 960, 250, 0, 64000, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mk := func() *celt.Encoder {
				st := celt.NewEncoder(48000, tc.channels)
				st.SetSignalling(tc.signalling)
				st.SetVBR(tc.vbr)
				st.SetComplexity(9)
				if !st.SetBitrate(tc.bitrate) {
					t.Fatalf("SetBitrate(%d) rejected", tc.bitrate)
				}
				return st
			}
			a, b := mk(), mk()

			for f := 0; f < 6; f++ {
				pcm := testPCM(tc.frameSize, tc.channels, f)

				// Shape 1: enc == NULL.
				bufA := make([]byte, tc.nbBytes)
				retA := a.Encode(pcm, tc.frameSize, bufA, tc.nbBytes)

				// Shape 2: external coder, compressed == NULL.
				bufB := make([]byte, tc.nbBytes)
				var ec rangecoding.Encoder
				ec.Init(bufB)
				retB := b.EncodeWithEC(pcm, tc.frameSize, &ec, tc.nbBytes)

				if retA != retB {
					t.Fatalf("frame %d: Encode = %d, EncodeWithEC = %d", f, retA, retB)
				}
				if retA < 0 {
					t.Fatalf("frame %d: both returned error %d", f, retA)
				}
				if tc.wantShrink && retA >= tc.nbBytes {
					t.Fatalf("frame %d: ret = %d with nbCompressedBytes = %d: the :1917 CBR clamp "+
						"did not fire, so the ec_enc_shrink branch this case exists to cover is dead",
						f, retA, tc.nbBytes)
				}
				if !bytes.Equal(bufA[:retA], bufB[:retB]) {
					for i := range bufA[:retA] {
						if bufA[i] != bufB[i] {
							t.Fatalf("frame %d: payload byte %d: Encode=0x%02x EncodeWithEC=0x%02x",
								f, i, bufA[i], bufB[i])
						}
					}
				}
				if !reflect.DeepEqual(a.State(), b.State()) {
					t.Fatalf("frame %d: CELT cross-frame state diverged between the two call shapes", f)
				}
			}
		})
	}
}

// TestPacketPadMatchesC pins the internal/packet.Pad seam against opus_packet_pad,
// which the CBR path calls on every frame (opus_encoder.c:2646). Both the
// short-circuit (len == new_len: touch nothing) and the real code-3 padding are
// covered, plus the rejection cases.
func TestPacketPadMatchesC(t *testing.T) {
	// Real packets, straight from the C encoder, so the padding runs over genuine
	// CELT-only framing rather than a synthetic TOC.
	const channels = 2
	cfg := defaultOpusencCfg(channels)
	cfg.Bitrate = 64000
	ce, err := cOpusencCreate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ce.Close()

	var pkts [][]byte
	for f := 0; f < 4; f++ {
		ret, pkt, _ := ce.Encode(testPCM(960, channels, f), 960, 1500)
		if ret <= 0 {
			t.Fatalf("frame %d: C opus_encode returned %d", f, ret)
		}
		pkts = append(pkts, pkt)
	}

	for i, pkt := range pkts {
		for _, grow := range []int{0, 1, 2, 3, 17, 400} {
			newLen := len(pkt) + grow

			cBuf, cRet := cOpusencPacketPad(pkt, len(pkt), newLen)

			goBuf := make([]byte, newLen)
			copy(goBuf, pkt)
			goErr := packet.Pad(goBuf, len(pkt), newLen)

			if (cRet == 0) != (goErr == nil) {
				t.Fatalf("pkt %d grow %d: C ret=%d, Go err=%v", i, grow, cRet, goErr)
			}
			if cRet != 0 {
				continue
			}
			if !bytes.Equal(goBuf, cBuf) {
				for j := range goBuf {
					if goBuf[j] != cBuf[j] {
						t.Fatalf("pkt %d grow %d: byte %d: Go=0x%02x C=0x%02x (len %d -> %d)",
							i, grow, j, goBuf[j], cBuf[j], len(pkt), newLen)
					}
				}
			}
			// grow == 0 is the repacketizer.c:343 short-circuit: the packet must come
			// back byte-for-byte unchanged, NOT re-framed.
			if grow == 0 && !bytes.Equal(goBuf, pkt) {
				t.Fatalf("pkt %d: Pad(len == new_len) re-framed the packet; it must be a no-op", i)
			}
		}
	}

	// Rejection rules (repacketizer.c:341-346).
	if err := packet.Pad(nil, 0, 0); err == nil {
		t.Errorf("Pad(len=0) accepted; C returns OPUS_BAD_ARG")
	}
	shrink := make([]byte, len(pkts[0]))
	copy(shrink, pkts[0])
	if err := packet.Pad(shrink, len(pkts[0]), len(pkts[0])-1); err == nil {
		t.Errorf("Pad(len > new_len) accepted; C returns OPUS_BAD_ARG")
	}
}
