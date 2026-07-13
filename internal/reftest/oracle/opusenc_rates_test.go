//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// CP9 unit RATES. The pure-integer rate / decision / framing half of
// opus_encode_native (opus_encoder.c:1182): no PCM enters any function under test.
//
// It is checked on TWO surfaces, and both are needed:
//
//  1. The flat wrappers over the file statics (gen_toc, user_bitrate_to_bitrate,
//     compute_equiv_rate, frame_size_select) let each function be swept over its
//     whole domain, including the arguments the frozen encoder never produces.
//  2. The oracle's FIELD-LEVEL STATE DUMP lets the whole chain be checked in situ,
//     across multi-frame sequences, against the real C encoder: stream_channels,
//     mode, bandwidth, auto_bandwidth, first and bitrate_bps are exactly the
//     fields this unit computes, so a real opus_encode run pins them without this
//     unit having to implement one line of the PCM half. That is what makes the
//     stereo/mono and bandwidth HYSTERESIS testable at all: it is cross-frame
//     state.
//
// The sequences below are driven by varying max_data_bytes PER FRAME with the
// user bitrate pinned to OPUS_BITRATE_MAX, because the oracle handle applies its
// ctls once at create time. user_bitrate_to_bitrate (:733) then makes
// st->bitrate_bps exactly bits_to_bitrate(max_data_bytes*8, Fs, frame_size), i.e.
// max_data_bytes*400 at 48 kHz / 20 ms, which is a clean per-frame rate knob.

// rateStateFields compares only the fields THIS unit computes. hp_mem,
// delay_buffer and rangeFinal belong to the PCM half and are deliberately not
// compared here; compareState in opusenc_test.go is the full-state gate.
func rateStateFields(t *testing.T, where string, got opusenc.State, want cOpusencState) {
	t.Helper()
	for _, f := range []struct {
		name     string
		got, exp int64
	}{
		{"stream_channels", int64(got.StreamChannels), int64(want.StreamChannels)},
		{"mode", int64(got.Mode), int64(want.Mode)},
		{"bandwidth", int64(got.Bandwidth), int64(want.Bandwidth)},
		{"auto_bandwidth", int64(got.AutoBandwidth), int64(want.AutoBandwidth)},
		{"first", int64(got.First), int64(want.First)},
		{"bitrate_bps", int64(got.BitrateBps), int64(want.BitrateBps)},
		{"prev_mode", int64(got.PrevMode), int64(want.PrevMode)},
		{"prev_channels", int64(got.PrevChannels), int64(want.PrevChannels)},
		{"prev_framesize", int64(got.PrevFramesize), int64(want.PrevFramesize)},
	} {
		if f.got != f.exp {
			t.Fatalf("%s: field %s: Go = %d, C = %d", where, f.name, f.got, f.exp)
		}
	}
}

// ---------------------------------------------------------------------------
// The flat statics.
// ---------------------------------------------------------------------------

// TestOpusencUserBitrateToBitrateMatchesC sweeps user_bitrate_to_bitrate
// (opus_encoder.c:733) over every sample rate, both channel counts, both
// sentinels, and byte budgets from 1 to the :1221 cap of 1276*6.
//
// frameSize == 0 is included on purpose: it is the `if(!frame_size)
// frame_size=st->Fs/400;` guard at :736, the only divide-by-zero shield in the
// function.
func TestOpusencUserBitrateToBitrateMatchesC(t *testing.T) {
	sampleRates := []int32{8000, 12000, 16000, 24000, 48000}
	userBitrates := []int32{
		opusenc.OpusAuto,       // 60*Fs/frame_size + Fs*channels
		opusenc.OpusBitrateMax, // 1500000
		500, 2400, 6000, 9000, 12000, 16000, 17000, 19000, 24000, 32000,
		64000, 96000, 128000, 256000, 510000, 600000, 1000000, 1500000,
	}
	maxDataBytesSet := []int{1, 2, 3, 8, 12, 20, 41, 47, 160, 161, 500, 1275, 1276, 1277, 1500, 7656}

	sawAuto, sawMax, sawClampedByBuffer, sawClampedByUser := 0, 0, 0, 0

	for _, fs := range sampleRates {
		// The nine legal durations at this Fs, plus 0 (the :736 guard).
		frameSizes := []int{
			0,
			int(fs / 400), int(fs / 200), int(fs / 100), int(fs / 50),
			int(fs / 25), int(3 * fs / 50), int(4 * fs / 50), int(5 * fs / 50), int(6 * fs / 50),
		}
		for _, ch := range []int{1, 2} {
			for _, ub := range userBitrates {
				for _, fsz := range frameSizes {
					for _, mdb := range maxDataBytesSet {
						got := opusenc.UserBitrateToBitrate(fs, ch, ub, fsz, mdb)
						want := cOpusencUserBitrateToBitrate(fs, ch, ub, fsz, mdb)
						if got != want {
							t.Fatalf("user_bitrate_to_bitrate(Fs=%d, ch=%d, user=%d, frame_size=%d, max_bytes=%d): Go = %d, C = %d",
								fs, ch, ub, fsz, mdb, got, want)
						}
						switch ub {
						case opusenc.OpusAuto:
							sawAuto++
						case opusenc.OpusBitrateMax:
							sawMax++
						}
						// Which of the two IMIN arms won?
						eff := fsz
						if eff == 0 {
							eff = int(fs / 400)
						}
						maxBitrate := int32(mdb) * 8 * (6 * fs / int32(eff)) / 6
						if got == maxBitrate {
							sawClampedByBuffer++
						} else {
							sawClampedByUser++
						}
					}
				}
			}
		}
	}
	// NON-VACUITY: both sentinels and both arms of the IMIN must have been taken.
	if sawAuto == 0 || sawMax == 0 || sawClampedByBuffer == 0 || sawClampedByUser == 0 {
		t.Fatalf("vacuous sweep: auto=%d max=%d buffer-clamped=%d user-clamped=%d",
			sawAuto, sawMax, sawClampedByBuffer, sawClampedByUser)
	}
}

// TestOpusencComputeEquivRateMatchesC sweeps compute_equiv_rate
// (opus_encoder.c:1027) over every branch: the small-frame overhead subtraction
// (frame_rate > 50), the CBR penalty, the complexity scaling, and all four mode
// arms (SILK, hybrid, CELT-only, and the mode == 0 "not known yet" arm that the
// two calls at :1410 and :1458 actually use).
func TestOpusencComputeEquivRateMatchesC(t *testing.T) {
	bitrates := []int32{0, 500, 2400, 6000, 9000, 11000, 12000, 16400, 17000, 18400,
		19000, 24000, 32000, 64000, 128000, 510000, 1500000}
	frameRates := []int{400, 200, 100, 50, 25, 16, 12, 10, 8}
	modes := []int{0, opusenc.ModeSilkOnly, opusenc.ModeHybrid, opusenc.ModeCeltOnly}
	losses := []int{0, 1, 2, 5, 10, 25, 50, 100}

	branches := map[string]int{}

	for _, br := range bitrates {
		for _, ch := range []int{1, 2} {
			for _, fr := range frameRates {
				for _, vbr := range []int{0, 1} {
					for _, mode := range modes {
						for cx := 0; cx <= 10; cx++ {
							for _, loss := range losses {
								got := opusenc.ComputeEquivRate(br, ch, fr, vbr, mode, cx, loss)
								want := cOpusencComputeEquivRate(br, ch, fr, vbr, mode, cx, loss)
								if got != want {
									t.Fatalf("compute_equiv_rate(br=%d, ch=%d, fr=%d, vbr=%d, mode=%d, cx=%d, loss=%d): Go = %d, C = %d",
										br, ch, fr, vbr, mode, cx, loss, got, want)
								}
								if fr > 50 {
									branches["overhead"]++
								}
								if vbr == 0 {
									branches["cbr-penalty"]++
								}
								switch mode {
								case opusenc.ModeSilkOnly, opusenc.ModeHybrid:
									branches["silk-arm"]++
									if cx < 2 {
										branches["silk-nsq"]++
									}
									if loss > 0 {
										branches["silk-loss"]++
									}
								case opusenc.ModeCeltOnly:
									branches["celt-arm"]++
									if cx < 5 {
										branches["celt-nopitch"]++
									}
								default:
									branches["unknown-arm"]++
									if loss > 0 {
										branches["unknown-loss"]++
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for _, b := range []string{"overhead", "cbr-penalty", "silk-arm", "silk-nsq", "silk-loss",
		"celt-arm", "celt-nopitch", "unknown-arm", "unknown-loss"} {
		if branches[b] == 0 {
			t.Fatalf("vacuous sweep: compute_equiv_rate branch %q was never taken", b)
		}
	}
}

// TestOpusencFrameSizeSelectMatchesC sweeps frame_size_select
// (opus_encoder.c:827), the REJECTION RULE that turns into OPUS_BAD_ARG at :1224.
// It must reject:
//   - anything below Fs/400 (2.5 ms),
//   - a variable_duration that is neither OPUS_FRAMESIZE_ARG nor in
//     OPUS_FRAMESIZE_2_5_MS..OPUS_FRAMESIZE_120_MS,
//   - a ctl-selected duration LONGER than the caller's sample count,
//   - any sample count that is not exactly one of the nine legal Opus durations,
//   - sub-10 ms frames under OPUS_APPLICATION_RESTRICTED_SILK.
func TestOpusencFrameSizeSelectMatchesC(t *testing.T) {
	const appRestrictedSILK = 2052 // OPUS_APPLICATION_RESTRICTED_SILK

	apps := []int{opusenc.ApplicationVOIP, opusenc.ApplicationAudio,
		opusenc.ApplicationRestrictedLowdelay, appRestrictedSILK}
	durations := []int{
		opusenc.FramesizeArg,
		opusenc.Framesize2_5Ms, opusenc.Framesize5Ms, opusenc.Framesize10Ms,
		opusenc.Framesize20Ms, opusenc.Framesize40Ms, opusenc.Framesize60Ms,
		opusenc.Framesize80Ms, opusenc.Framesize100Ms, opusenc.Framesize120Ms,
		4999, 5010, 0, -1000, // invalid: the `else return -1` at :839
	}
	sampleRates := []int32{8000, 12000, 16000, 24000, 48000}

	accepted, rejected := 0, 0
	rejectedTooShort, rejectedBadDuration, rejectedTooLong, rejectedIllegal, rejectedSilk := 0, 0, 0, 0, 0

	for _, fs := range sampleRates {
		// Every legal duration at this Fs, plus a pile of illegal sample counts.
		frameSizes := []int32{
			0, 1, 2, fs/400 - 1, fs / 400, fs/400 + 1,
			fs / 200, fs / 100, fs/100 - 1, fs / 50, fs/50 + 1,
			fs / 25, 3 * fs / 50, 4 * fs / 50, 5 * fs / 50, 6 * fs / 50, 7 * fs / 50,
			333, 999, 1000, 4321, 6000,
		}
		for _, app := range apps {
			for _, vd := range durations {
				for _, fsz := range frameSizes {
					got := opusenc.FrameSizeSelect(app, fsz, vd, fs)
					want := cOpusencFrameSizeSelect(app, fsz, vd, fs)
					if got != want {
						t.Fatalf("frame_size_select(app=%d, frame_size=%d, variable_duration=%d, Fs=%d): Go = %d, C = %d",
							app, fsz, vd, fs, got, want)
					}
					if got > 0 {
						accepted++
						continue
					}
					rejected++
					// Attribute the rejection so each rule is proven live.
					switch {
					case fsz < fs/400:
						rejectedTooShort++
					case vd != opusenc.FramesizeArg &&
						(vd < opusenc.Framesize2_5Ms || vd > opusenc.Framesize120Ms):
						rejectedBadDuration++
					case app == appRestrictedSILK && legalOpusFrame(fsz, fs) && fsz < fs/100 &&
						vd == opusenc.FramesizeArg:
						rejectedSilk++
					case vd != opusenc.FramesizeArg && ctlFrameSize(vd, fs) > fsz:
						rejectedTooLong++
					default:
						rejectedIllegal++
					}
				}
			}
		}
	}
	if accepted == 0 {
		t.Fatal("vacuous sweep: frame_size_select never accepted anything")
	}
	for name, n := range map[string]int{
		"below Fs/400":            rejectedTooShort,
		"bad variable_duration":   rejectedBadDuration,
		"ctl duration > buffer":   rejectedTooLong,
		"not a legal duration":    rejectedIllegal,
		"RESTRICTED_SILK < 10 ms": rejectedSilk,
	} {
		if n == 0 {
			t.Fatalf("vacuous sweep: frame_size_select rejection rule %q was never exercised", name)
		}
	}
	if rejected == 0 {
		t.Fatal("vacuous sweep: frame_size_select never rejected anything")
	}

	// SPOT CHECKS at 48 kHz, the frozen rate: exactly the nine legal sizes pass
	// with OPUS_FRAMESIZE_ARG, and nothing else does.
	legal := map[int32]bool{120: true, 240: true, 480: true, 960: true, 1920: true,
		2880: true, 3840: true, 4800: true, 5760: true}
	for n := int32(0); n <= 6000; n++ {
		want := int32(-1)
		if legal[n] {
			want = n
		}
		if got := opusenc.FrameSizeSelect(opusenc.ApplicationAudio, n, opusenc.FramesizeArg, 48000); got != want {
			t.Fatalf("frame_size_select(AUDIO, %d, ARG, 48000) = %d, want %d", n, got, want)
		}
	}
}

// legalOpusFrame mirrors the nine divisibility tests at :843-846.
func legalOpusFrame(n, fs int32) bool {
	return 400*n == fs || 200*n == fs || 100*n == fs || 50*n == fs || 25*n == fs ||
		50*n == 3*fs || 50*n == 4*fs || 50*n == 5*fs || 50*n == 6*fs
}

// ctlFrameSize mirrors :834-838: the sample count OPUS_SET_EXPERT_FRAME_DURATION
// asks for.
func ctlFrameSize(vd int, fs int32) int32 {
	if vd <= opusenc.Framesize40Ms {
		return (fs / 400) << (vd - opusenc.Framesize2_5Ms)
	}
	return int32(vd-opusenc.Framesize2_5Ms-2) * fs / 50
}

// TestOpusencGenTOCMatchesC sweeps gen_toc (opus_encoder.c:330) over the domain
// the encoder can actually reach in each mode. The domain matters: gen_toc's SILK
// and hybrid arms shift (bandwidth - X) left, and the callers guarantee X is a
// lower bound (the degenerate path clamps bw per mode at :1394-1399, and the main
// path's mode/bandwidth pair is made consistent at :1691-1695), so a negative
// shift never happens in C and must not be manufactured here either.
func TestOpusencGenTOCMatchesC(t *testing.T) {
	type dom struct {
		mode       int
		framerates []int
		bandwidths []int
	}
	// CELT-only is the frozen path: every bandwidth, every frame rate, and the
	// `if (tmp < 0) tmp = 0;` clamp at :344 (NB is BELOW mediumband).
	doms := []dom{
		{opusenc.ModeCeltOnly,
			[]int{400, 200, 100, 50, 25, 16, 12, 10, 8},
			[]int{opusenc.BandwidthNarrowband, opusenc.BandwidthMediumband,
				opusenc.BandwidthWideband, opusenc.BandwidthSuperwideband,
				opusenc.BandwidthFullband}},
		// SILK: bw <= WB (guaranteed at :1395), frame rate <= 100 (period >= 2).
		{opusenc.ModeSilkOnly,
			[]int{100, 50, 25, 16},
			[]int{opusenc.BandwidthNarrowband, opusenc.BandwidthMediumband,
				opusenc.BandwidthWideband}},
		// Hybrid: bw >= SWB (guaranteed at :1399), frame rate 100 or 50.
		{opusenc.ModeHybrid,
			[]int{100, 50},
			[]int{opusenc.BandwidthSuperwideband, opusenc.BandwidthFullband}},
	}

	sawStereoBit, sawNBClamp := false, false
	for _, d := range doms {
		for _, fr := range d.framerates {
			for _, bw := range d.bandwidths {
				for _, ch := range []int{1, 2} {
					got := opusenc.GenTOC(d.mode, fr, bw, ch)
					want := cOpusencGenTOC(d.mode, fr, bw, ch)
					if got != want {
						t.Fatalf("gen_toc(mode=%d, framerate=%d, bandwidth=%d, channels=%d): Go = 0x%02x, C = 0x%02x",
							d.mode, fr, bw, ch, got, want)
					}
					if ch == 2 && got&0x04 != 0 {
						sawStereoBit = true
					}
					if d.mode == opusenc.ModeCeltOnly && bw == opusenc.BandwidthNarrowband {
						sawNBClamp = true
					}
				}
			}
		}
	}
	if !sawStereoBit || !sawNBClamp {
		t.Fatalf("vacuous sweep: stereo bit seen=%v, CELT NB tmp<0 clamp seen=%v", sawStereoBit, sawNBClamp)
	}

	// THE POINT OF gen_toc's channels ARGUMENT: it is st->stream_channels at the
	// :2551 call site, so a stereo encoder downmixed to mono must emit a MONO TOC.
	mono := opusenc.GenTOC(opusenc.ModeCeltOnly, 50, opusenc.BandwidthFullband, 1)
	stereo := opusenc.GenTOC(opusenc.ModeCeltOnly, 50, opusenc.BandwidthFullband, 2)
	if mono&0x04 != 0 || stereo&0x04 == 0 || mono == stereo {
		t.Fatalf("gen_toc stereo bit: mono = 0x%02x, stereo = 0x%02x", mono, stereo)
	}
}

// ---------------------------------------------------------------------------
// The decision chain, against the C encoder's field-level state dump.
// ---------------------------------------------------------------------------

// rateSeqCase is one multi-frame sequence. maxDataBytes varies per frame, which
// (with the bitrate pinned to OPUS_BITRATE_MAX) varies st->bitrate_bps per frame
// and therefore walks the decisions across their thresholds.
type rateSeqCase struct {
	name         string
	channels     int
	frameSize    int
	maxDataBytes []int
	// Overrides on top of defaultOpusencCfg. Bitrate defaults to
	// OPUS_BITRATE_MAX so max_data_bytes is the rate knob.
	bitrate       int
	vbr           int
	vbrConstraint int
	complexity    int
	loss          int
	signalType    int
	forceChannels int
	maxBandwidth  int
	userBandwidth int
	// Expectations, one per frame, asserted on the C. They are what makes the
	// sweep NON-VACUOUS: without them a sequence that never actually crossed a
	// threshold would still "pass" by agreeing with C on a constant.
	wantStreamChannels []int32
	wantBandwidth      []int32
}

func (c rateSeqCase) cfg() cOpusencCfg {
	cfg := defaultOpusencCfg(c.channels)
	cfg.Bitrate = c.bitrate
	cfg.VBR = c.vbr
	cfg.VBRConstraint = c.vbrConstraint
	cfg.Complexity = c.complexity
	cfg.PacketLossPerc = c.loss
	cfg.SignalType = c.signalType
	cfg.ForceChannels = c.forceChannels
	cfg.MaxBandwidth = c.maxBandwidth
	cfg.Bandwidth = c.userBandwidth
	return cfg
}

// TestOpusencRateDecisionsMatchC is the main gate for this unit. For each frame of
// each sequence it runs the C encoder (which exercises the WHOLE of
// opus_encode_native) and the Go decision chain ALONE, and requires them to land
// on the same stream_channels / mode / bandwidth / auto_bandwidth / first /
// bitrate_bps / prev_*.
func TestOpusencRateDecisionsMatchC(t *testing.T) {
	base := func(c rateSeqCase) rateSeqCase {
		c.bitrate = oOpusBitrateMax
		c.vbr = 1
		c.vbrConstraint = 1
		c.complexity = 9
		c.signalType = oOpusAuto
		c.forceChannels = oOpusAuto
		c.maxBandwidth = oBandwidthFullband
		c.userBandwidth = oOpusAuto
		return c
	}
	const (
		nb  = int32(opusenc.BandwidthNarrowband)
		wb  = int32(opusenc.BandwidthWideband)
		swb = int32(opusenc.BandwidthSuperwideband)
		fb  = int32(opusenc.BandwidthFullband)
	)

	cases := []rateSeqCase{
		// ------------------------------------------------------------------
		// THE STEREO <-> MONO HYSTERESIS, BOTH DIRECTIONS (:1440-1448).
		// equiv_rate = bitrate*99/100 here (20 ms, VBR, complexity 9, no loss),
		// and bitrate = max_data_bytes*400. voice_est is 48, so the memoryless
		// threshold is 17000 + ((48*48*2000)>>14) = 17281, and the hysteresis
		// makes it 16281 while stereo and 18281 while mono.
		//   mdb=200 -> 80000 bps, equiv 79200            -> stereo
		//   mdb=41  -> 16400 bps, equiv 16236 <= 16281   -> DOWN to mono
		//   mdb=46  -> 18400 bps, equiv 18216 <= 18281   -> HOLDS mono (memoryless
		//              would have said stereo: 18216 > 17281)
		//   mdb=47  -> 18800 bps, equiv 18612 >  18281   -> UP to stereo
		//   mdb=42  -> 16800 bps, equiv 16632 >  16281   -> HOLDS stereo (memoryless
		//              would have said mono: 16632 < 17281)
		//   mdb=41  -> 16400 bps                         -> DOWN to mono again
		base(rateSeqCase{
			name: "stereo-mono-hysteresis/both-directions", channels: 2, frameSize: 960,
			maxDataBytes:       []int{200, 41, 46, 47, 42, 41},
			wantStreamChannels: []int32{2, 1, 1, 2, 2, 1},
		}),
		// ------------------------------------------------------------------
		// THE BANDWIDTH TIERS AND THEIR HYSTERESIS (:1598-1616). Mono, so
		// stream_channels is pinned and only the bandwidth moves. voice_est 48
		// gives thresholds FB 12281 (+-2000), SWB 11351 (+-1000), WB 9000 (+-700).
		//   mdb=200 -> equiv 79200                  -> FB   (first frame, no hysteresis)
		//   mdb=31  -> equiv 12276 >= 12281-2000    -> HOLDS FB (memoryless: SWB)
		//   mdb=25  -> equiv  9900 <  10281, < 10351, >= 8300 -> falls to WB
		//   mdb=31  -> equiv 12276 <  12281+2000, < 11351+1000 -> HOLDS WB (memoryless: FB)
		//   mdb=33  -> equiv 13068 >= 11351+1000    -> UP to SWB
		//   mdb=40  -> equiv 15840 >= 12281+2000    -> UP to FB
		//   mdb=20  -> equiv  7920 <  8300          -> all the way DOWN to NB
		//   mdb=200 -> equiv 79200                  -> back UP to FB
		base(rateSeqCase{
			name: "bandwidth-tiers/mono/hysteresis", channels: 1, frameSize: 960,
			maxDataBytes:       []int{200, 31, 25, 31, 33, 40, 20, 200},
			wantStreamChannels: []int32{1, 1, 1, 1, 1, 1, 1, 1},
			wantBandwidth:      []int32{fb, fb, wb, wb, swb, fb, nb, fb},
		}),
		// The same walk on a STEREO encoder, which selects the stereo threshold
		// tables at :1590-1592 (identical values in v1.6.1, but a different branch)
		// and moves stream_channels underneath the bandwidth decision.
		base(rateSeqCase{
			name: "bandwidth-tiers/stereo", channels: 2, frameSize: 960,
			maxDataBytes: []int{200, 60, 40, 33, 25, 20, 200},
		}),
		// ------------------------------------------------------------------
		// OPUS_SIGNAL_VOICE / OPUS_SIGNAL_MUSIC drive voice_est to 127 / 0, which
		// moves BOTH the stereo threshold and the whole bandwidth table. This is
		// the only way to prove ComputeVoiceEst's expression rather than its
		// frozen constant 48.
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "voice-est/voice", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 48, 44, 40, 35, 30, 25},
			})
			c.signalType = 3001 // OPUS_SIGNAL_VOICE -> voice_est 127
			return c
		}(),
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "voice-est/music", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 48, 44, 40, 35, 30, 25},
			})
			c.signalType = 3002 // OPUS_SIGNAL_MUSIC -> voice_est 0
			return c
		}(),
		// ------------------------------------------------------------------
		// COMPLEXITY < 5 makes compute_equiv_rate's CELT arm subtract 10% at the
		// :1571 call (mode == MODE_CELT_ONLY) but NOT at :1410 / :1458 (mode == 0).
		// Passing the wrong mode at any of the three call sites diverges here.
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "complexity-0/celt-arm", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 45, 42, 35, 30, 25, 20},
			})
			c.complexity = 0
			return c
		}(),
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "complexity-4/celt-arm-boundary", channels: 1, frameSize: 960,
				maxDataBytes: []int{200, 35, 32, 28, 24, 20},
			})
			c.complexity = 4
			return c
		}(),
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "complexity-10", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 45, 42, 35, 25},
			})
			c.complexity = 10
			return c
		}(),
		// ------------------------------------------------------------------
		// PACKET LOSS is subtracted only on the mode == 0 arm (:1054) and NOT on
		// the CELT arm, so the :1410/:1458 equiv_rate (used by the stereo decision)
		// and the :1571 one (used by the bandwidth decision) genuinely differ.
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "packet-loss-25", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 50, 45, 40, 33, 28, 22},
			})
			c.loss = 25
			return c
		}(),
		// ------------------------------------------------------------------
		// CBR: compute_equiv_rate subtracts equiv/12 (:1035), AND the byte budget
		// requantizes st->bitrate_bps at :1331 before any decision reads it.
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "cbr/rate-sweep", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 50, 45, 42, 38, 30, 24},
			})
			c.vbr = 0
			c.vbrConstraint = 0
			return c
		}(),
		// ------------------------------------------------------------------
		// LM 0..3. frame_rate > 50 turns on the (40*channels+20)*(frame_rate-50)
		// overhead subtraction in compute_equiv_rate, which is a big correction at
		// 2.5 ms (350 * 100 = 35000 bps for stereo).
		base(rateSeqCase{
			name: "lm0/2.5ms/stereo", channels: 2, frameSize: 120,
			maxDataBytes: []int{100, 40, 25, 18, 12, 60},
		}),
		base(rateSeqCase{
			name: "lm1/5ms/stereo", channels: 2, frameSize: 240,
			maxDataBytes: []int{120, 40, 25, 18, 12, 60},
		}),
		base(rateSeqCase{
			name: "lm2/10ms/mono", channels: 1, frameSize: 480,
			maxDataBytes: []int{150, 40, 25, 18, 12, 80},
		}),
		base(rateSeqCase{
			name: "lm3/20ms/mono", channels: 1, frameSize: 960,
			maxDataBytes: []int{200, 60, 40, 30, 24, 100},
		}),
		// ------------------------------------------------------------------
		// The overrides on top of the automatic selection (:1629-1633).
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "max-bandwidth/wideband-clamp", channels: 2, frameSize: 960,
				maxDataBytes: []int{200, 100, 40, 25, 200},
			})
			c.maxBandwidth = 1103 // OPUS_BANDWIDTH_WIDEBAND
			return c
		}(),
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "user-bandwidth/forced-narrowband", channels: 1, frameSize: 960,
				maxDataBytes: []int{200, 100, 40, 200},
			})
			c.userBandwidth = 1101 // OPUS_BANDWIDTH_NARROWBAND
			return c
		}(),
		func() rateSeqCase {
			// OPUS_SET_BANDWIDTH(MEDIUMBAND) is the ONLY way to reach the
			// `mode == MODE_CELT_ONLY && bandwidth == MEDIUMBAND -> WIDEBAND`
			// fixup at :1680-1682: the automatic selection can never pick
			// mediumband (it is rewritten at :1618-1620 BEFORE being stored).
			c := base(rateSeqCase{
				name: "user-bandwidth/mediumband-to-wideband-fixup", channels: 1, frameSize: 960,
				maxDataBytes:  []int{200, 100, 40},
				wantBandwidth: []int32{wb, wb, wb},
			})
			c.userBandwidth = 1102 // OPUS_BANDWIDTH_MEDIUMBAND
			return c
		}(),
		// ------------------------------------------------------------------
		// OPUS_SET_FORCE_CHANNELS overrides the rate decision entirely (:1428-1431)
		// AND flips the bandwidth table selection at :1590 (force_channels == 1
		// selects the MONO tables even on a stereo encoder).
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "force-channels/mono-on-stereo-encoder", channels: 2, frameSize: 960,
				maxDataBytes:       []int{200, 40, 200},
				wantStreamChannels: []int32{1, 1, 1},
			})
			c.forceChannels = 1
			return c
		}(),
		func() rateSeqCase {
			c := base(rateSeqCase{
				name: "force-channels/stereo-below-threshold", channels: 2, frameSize: 960,
				maxDataBytes:       []int{200, 20, 200},
				wantStreamChannels: []int32{2, 2, 2},
			})
			c.forceChannels = 2
			return c
		}(),
	}

	seenBandwidth := map[int32]bool{}
	seenStreamChannels := map[int32]bool{}
	seenStereoToMono, seenMonoToStereo := false, false
	seenBandwidthUp, seenBandwidthDown := false, false

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg()
			ce, err := cOpusencCreate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ce.Close()
			gs := goEncoderMatchingCfg(t, cfg)

			prevCh, prevBw := int32(0), int32(0)
			for f, mdb := range tc.maxDataBytes {
				pcm := testPCM(tc.frameSize, tc.channels, f)

				ret, pkt, cState := ce.Encode(pcm, tc.frameSize, mdb)
				if ret <= 0 {
					t.Fatalf("frame %d (max_data_bytes=%d): C opus_encode returned %d, "+
						"want a positive length (this sequence must not hit the degenerate branch)",
						f, mdb, ret)
				}

				// The Go decision chain, alone. No PCM, no CELT.
				maxDataBytes, argErr := gs.CheckEncodeArgs(tc.frameSize, mdb)
				if argErr != nil {
					t.Fatalf("frame %d: Go CheckEncodeArgs rejected (%v) what C accepted", f, argErr)
				}
				d, ok := gs.DecideRates(tc.frameSize, maxDataBytes)
				if !ok {
					t.Fatalf("frame %d: DecideRates needs the unported OPUS_AUTO mode decision", f)
				}
				if d.Degenerate {
					t.Fatalf("frame %d: Go says degenerate but C returned a %d-byte packet", f, ret)
				}
				gs.AdvanceDecisionState(tc.frameSize)

				where := fmt.Sprintf("%s: frame %d (max_data_bytes=%d)", tc.name, f, mdb)
				rateStateFields(t, where, gs.State(), cState)

				// curr_bandwidth is what CELT's end band and the TOC are built
				// from; it is st->bandwidth after the :1681-1685 fixups.
				if int32(d.CurrBandwidth) != cState.Bandwidth {
					t.Fatalf("%s: curr_bandwidth: Go = %d, C st->bandwidth = %d",
						where, d.CurrBandwidth, cState.Bandwidth)
				}
				// THE TOC BYTE, IN SITU. Rebuilding the C packet's first byte out
				// of this unit's decisions is the strongest available check that
				// gen_toc's mode / framerate / bandwidth / STREAM_CHANNELS
				// arguments are all right, and it fails loudly if the stereo bit is
				// taken from st->channels instead of st->stream_channels.
				gotTOC := opusenc.GenTOC(opusenc.ModeCeltOnly, d.FrameRate,
					d.CurrBandwidth, int(gs.State().StreamChannels))
				if gotTOC != pkt[0] {
					t.Fatalf("%s: TOC: Go gen_toc(CELT_ONLY, %d, %d, %d) = 0x%02x, "+
						"C packet[0] = 0x%02x", where, d.FrameRate, d.CurrBandwidth,
						gs.State().StreamChannels, gotTOC, pkt[0])
				}

				// Per-frame expectations (non-vacuity: prove the sweep really
				// crossed the thresholds it claims to).
				if tc.wantStreamChannels != nil && cState.StreamChannels != tc.wantStreamChannels[f] {
					t.Fatalf("%s: C stream_channels = %d, the case predicted %d: "+
						"the sequence does not exercise the transition it documents",
						where, cState.StreamChannels, tc.wantStreamChannels[f])
				}
				if tc.wantBandwidth != nil && cState.Bandwidth != tc.wantBandwidth[f] {
					t.Fatalf("%s: C bandwidth = %d, the case predicted %d: "+
						"the sequence does not exercise the tier it documents",
						where, cState.Bandwidth, tc.wantBandwidth[f])
				}

				seenBandwidth[cState.Bandwidth] = true
				seenStreamChannels[cState.StreamChannels] = true
				if f > 0 {
					if prevCh == 2 && cState.StreamChannels == 1 {
						seenStereoToMono = true
					}
					if prevCh == 1 && cState.StreamChannels == 2 {
						seenMonoToStereo = true
					}
					if cState.AutoBandwidth > prevBw {
						seenBandwidthUp = true
					}
					if cState.AutoBandwidth < prevBw {
						seenBandwidthDown = true
					}
				}
				prevCh = cState.StreamChannels
				prevBw = cState.AutoBandwidth
			}
		})
	}

	// NON-VACUITY over the whole battery.
	if !seenStereoToMono || !seenMonoToStereo {
		t.Errorf("vacuous battery: stereo->mono seen = %v, mono->stereo seen = %v; "+
			"BOTH directions of the :1440-1448 hysteresis must be exercised",
			seenStereoToMono, seenMonoToStereo)
	}
	if !seenBandwidthUp || !seenBandwidthDown {
		t.Errorf("vacuous battery: auto_bandwidth never went %s",
			map[bool]string{true: "down", false: "up"}[seenBandwidthUp])
	}
	for _, bw := range []int32{nb, wb, swb, fb} {
		if !seenBandwidth[bw] {
			t.Errorf("vacuous battery: bandwidth tier %d was never selected", bw)
		}
	}
	for _, ch := range []int32{1, 2} {
		if !seenStreamChannels[ch] {
			t.Errorf("vacuous battery: stream_channels never took the value %d", ch)
		}
	}
}

// ---------------------------------------------------------------------------
// The CBR byte-budget derivation (opus_encoder.c:1327-1334).
// ---------------------------------------------------------------------------

// TestOpusencCBRBudgetMatchesC pins the derivation the whole CBR packet size rests
// on. Each case asserts, against the REAL C encoder:
//
//   - the packet length (which IS cbr_bytes, because the CBR path leaves CELT's
//     bitrate at OPUS_BITRATE_MAX and ec_enc_done therefore fills the whole
//     storage window),
//   - the WRITTEN-BACK st->bitrate_bps, which is what every later decision sees.
//
// The +4 before the /8 is round-to-nearest-byte, so the boundary cases below (a
// bitrate that lands on bits % 8 == 3 versus == 4) differ by a whole byte of
// packet and 400 bps of requantized rate.
func TestOpusencCBRBudgetMatchesC(t *testing.T) {
	cases := []struct {
		name         string
		channels     int
		frameSize    int
		bitrate      int
		maxDataBytes int
		// Expectations, asserted against C. They pin the DERIVATION, not just
		// Go/C agreement.
		wantCBRBytes   int
		wantBitrateBps int32
		wantDegenerate bool
		// wantPadding says the packet is longer than the 1276-byte code-0 limit,
		// so opus_packet_pad at :2646 does real code-3 padding rather than
		// short-circuiting.
		wantPadding bool
	}{
		// bitrate_to_bits(64150, 48000, 960) = 64150*6/300 = 1283 bits.
		// (1283+4)/8 = 160 bytes. Without the +4: 1283/8 = 160 too. Same answer,
		// so this is the CONTROL for the boundary case below it.
		{"round/below-boundary", 1, 960, 64150, 1500, 160, 64000, false, false},
		// bitrate_to_bits(64200, 48000, 960) = 1284 bits. (1284+4)/8 = 161, but
		// 1284/8 = 160. THE +4 IS WHAT MAKES THIS 161. And the write-back turns a
		// request for 64200 into 64400 bps, which every later decision then sees.
		{"round/on-boundary", 1, 960, 64200, 1500, 161, 64400, false, false},
		{"round/just-past-boundary", 1, 960, 64249, 1500, 161, 64400, false, false},
		// 2.5 ms: bitrate_to_bits(64000, 48000, 120) = 64000*6/2400 = 160 bits.
		// (160+4)/8 = 20 bytes; the write-back is 20*8*2400/6 = 64000.
		{"lm0/exact", 1, 120, 64000, 1500, 20, 64000, false, false},
		// cbr_bytes CLAMPED BY max_data_bytes: 600000 bps at 20 ms wants 1500
		// bytes, but the buffer only has 800, so cbr_bytes = 800 and the rate is
		// requantized DOWN to 800*8*50 = 320000.
		{"clamped-by-max-data-bytes", 2, 960, 600000, 800, 800, 320000, false, false},
		// cbr_bytes > 1276: the ONLY configuration in which opus_packet_pad does
		// real work (:2646). 600000 bps at 20 ms = 12000 bits -> 1500 bytes.
		{"cbr-bytes-over-1276/real-padding", 2, 960, 600000, 4000, 1500, 600000, false, true},
		// IMAX(1, cbr_bytes): at 2.5 ms, 500 bps is 1 bit per frame, so
		// (1+4)/8 == 0 bytes. max_data_bytes becomes 1, st->bitrate_bps becomes 0,
		// and :1340 (max_data_bytes < 3) immediately sends it to the degenerate
		// branch, which pads back out to the 1 byte the IMAX guaranteed.
		{"imax-1-cbr-bytes/degenerate", 1, 120, 500, 1500, 0, 0, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			cfg.Bitrate = tc.bitrate
			cfg.VBR = 0
			cfg.VBRConstraint = 0

			ce, err := cOpusencCreate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ce.Close()
			gs := goEncoderMatchingCfg(t, cfg)

			pcm := testPCM(tc.frameSize, tc.channels, 0)
			ret, pkt, cState := ce.Encode(pcm, tc.frameSize, tc.maxDataBytes)
			if ret <= 0 {
				t.Fatalf("C opus_encode returned %d", ret)
			}

			// --- The C's own numbers, pinned. If these drift, the Go comparison
			// below would be comparing against the wrong thing.
			if cState.BitrateBps != tc.wantBitrateBps {
				t.Fatalf("C st->bitrate_bps after the write-back = %d, want %d "+
					"(opus_encoder.c:1331)", cState.BitrateBps, tc.wantBitrateBps)
			}
			wantLen := tc.wantCBRBytes
			if wantLen < 1 {
				wantLen = 1 // IMAX(1, cbr_bytes)
			}
			if len(pkt) != wantLen {
				t.Fatalf("C packet is %d bytes, want cbr_bytes = %d: the CBR budget "+
					"does not become the packet size", len(pkt), wantLen)
			}
			if tc.wantPadding {
				if tc.wantCBRBytes <= 1276 {
					t.Fatalf("case claims real padding but cbr_bytes = %d <= 1276, so "+
						"opus_packet_pad short-circuits (repacketizer.c:343)", tc.wantCBRBytes)
				}
				// A code-3 padded packet has the frame-count bits set in the TOC.
				if pkt[0]&0x03 != 3 {
					t.Fatalf("C packet TOC = 0x%02x: expected code 3 (real padding) at %d bytes",
						pkt[0], len(pkt))
				}
			} else if len(pkt) > 1 && pkt[0]&0x03 != 0 {
				t.Fatalf("C packet TOC = 0x%02x: expected code 0 (no padding)", pkt[0])
			}

			// --- The Go chain.
			maxDataBytes, argErr := gs.CheckEncodeArgs(tc.frameSize, tc.maxDataBytes)
			if argErr != nil {
				t.Fatalf("Go CheckEncodeArgs rejected (%v) what C accepted", argErr)
			}
			d, ok := gs.DecideRates(tc.frameSize, maxDataBytes)
			if !ok {
				t.Fatal("DecideRates needs the unported OPUS_AUTO mode decision")
			}
			if d.CBRBytes != tc.wantCBRBytes {
				t.Fatalf("Go cbr_bytes = %d, want %d", d.CBRBytes, tc.wantCBRBytes)
			}
			if got := gs.State().BitrateBps; got != cState.BitrateBps {
				t.Fatalf("Go st->bitrate_bps = %d, C = %d", got, cState.BitrateBps)
			}
			if d.MaxDataBytes != wantLen {
				t.Fatalf("Go max_data_bytes after IMAX(1, cbr_bytes) = %d, want %d",
					d.MaxDataBytes, wantLen)
			}
			if d.Degenerate != tc.wantDegenerate {
				t.Fatalf("Go degenerate = %v, want %v", d.Degenerate, tc.wantDegenerate)
			}
			if !d.Degenerate {
				gs.AdvanceDecisionState(tc.frameSize)
				rateStateFields(t, tc.name, gs.State(), cState)
			}
		})
	}
}

// TestOpusencCBRRoundingBoundaryMatchesC brute-forces every bitrate in a window
// straddling the +4 rounding boundary and requires the Go budget to agree with the
// C encoder's ACTUAL PACKET LENGTH at each one. Removing the +4 makes this fail on
// exactly the bitrates whose bit count is congruent to 4, 5, 6 or 7 mod 8.
func TestOpusencCBRRoundingBoundaryMatchesC(t *testing.T) {
	const (
		channels  = 1
		frameSize = 960 // 20 ms: bits = bitrate/50
	)
	sawShort, sawLong := 0, 0

	for bitrate := 63900; bitrate <= 64600; bitrate += 25 {
		cfg := defaultOpusencCfg(channels)
		cfg.Bitrate = bitrate
		cfg.VBR = 0
		cfg.VBRConstraint = 0

		ce, err := cOpusencCreate(cfg)
		if err != nil {
			t.Fatal(err)
		}
		gs := goEncoderMatchingCfg(t, cfg)

		ret, _, cState := ce.Encode(testPCM(frameSize, channels, 0), frameSize, 1500)
		ce.Close()
		if ret <= 0 {
			t.Fatalf("bitrate %d: C opus_encode returned %d", bitrate, ret)
		}

		maxDataBytes, argErr := gs.CheckEncodeArgs(frameSize, 1500)
		if argErr != nil {
			t.Fatalf("bitrate %d: Go CheckEncodeArgs: %v", bitrate, argErr)
		}
		d, ok := gs.DecideRates(frameSize, maxDataBytes)
		if !ok {
			t.Fatalf("bitrate %d: DecideRates needs the unported OPUS_AUTO mode decision", bitrate)
		}
		if d.CBRBytes != ret {
			t.Fatalf("bitrate %d: Go cbr_bytes = %d, but the C packet is %d bytes "+
				"(the +4 round-to-nearest-byte at opus_encoder.c:1330)", bitrate, d.CBRBytes, ret)
		}
		if got := gs.State().BitrateBps; got != cState.BitrateBps {
			t.Fatalf("bitrate %d: Go bitrate_bps after write-back = %d, C = %d",
				bitrate, got, cState.BitrateBps)
		}

		// NON-VACUITY: the window must straddle the boundary, i.e. contain
		// bitrates that round DOWN (bits%8 < 4) and bitrates that round UP.
		bits := bitrate / 50
		if bits%8 < 4 {
			sawShort++
		} else {
			sawLong++
		}
	}
	if sawShort == 0 || sawLong == 0 {
		t.Fatalf("vacuous sweep: rounded-down = %d, rounded-up = %d; the window does not "+
			"straddle the +4 boundary", sawShort, sawLong)
	}
}

// ---------------------------------------------------------------------------
// The degenerate TOC-only packet (opus_encoder.c:1340-1406).
// ---------------------------------------------------------------------------

// TestOpusencDegeneratePacketMatchesC pins the degenerate branch BYTE FOR BYTE.
// It is NOT an error path: it returns a positive length and a real, decodable
// TOC-only packet telling the decoder to run PLC.
//
// The branch runs BEFORE this frame's stereo/mono, mode and bandwidth decisions,
// so it reads the PREVIOUS frame's st->mode / st->bandwidth / st->stream_channels.
// On a fresh encoder those are the opus_encoder_init values (MODE_HYBRID,
// FULLBAND, channels), which is why a starved FIRST frame emits a HYBRID TOC even
// from a forced-CELT-only encoder. Getting that wrong is invisible in the state
// dump and visible only in the packet, which is why the packet is compared here.
func TestOpusencDegeneratePacketMatchesC(t *testing.T) {
	cases := []struct {
		name         string
		channels     int
		frameSize    int
		bitrate      int
		vbr          int
		maxDataBytes int
		// wantPacketCode pins WHICH of the degenerate sub-branches ran.
		wantPacketCode int
		wantLen        int
	}{
		// max_data_bytes < 3 (:1340). 20 ms, so frame_rate 50: no reframing,
		// packet_code 0, one byte.
		{"vbr/20ms/2-bytes", 1, 960, oOpusAuto, 1, 2, 0, 1},
		{"vbr/20ms/2-bytes/stereo", 2, 960, oOpusAuto, 1, 2, 0, 1},
		// st->bitrate_bps < 3*frame_rate*8: 20 ms needs 1200 bps. OPUS_BITRATE_MAX
		// with a 2-byte buffer gives 800 bps... which also trips max_data_bytes < 3,
		// so use a bigger buffer and a tiny bitrate instead. 1000 bps < 1200.
		{"vbr/20ms/bitrate-under-3-bytes-per-frame", 1, 960, 1000, 1, 100, 0, 1},
		// frame_rate < 50 && max_data_bytes*frame_rate < 300: 40 ms (frame_rate 25),
		// 8 bytes -> 200 < 300. tocmode is HYBRID (frame 1), so :1364 reframes
		// 40 ms into 2 x 20 ms and sets packet_code 1.
		{"vbr/40ms/reframed-to-2x20ms", 1, 1920, oOpusAuto, 1, 8, 1, 1},
		{"vbr/40ms/reframed-to-2x20ms/stereo", 2, 1920, oOpusAuto, 1, 8, 1, 1},
		// 60 ms (frame_rate 16) with room for 2 bytes of TOC: :1375 takes the
		// num_multiframes branch, packet_code 3, and data[1] = 50/16 = 3.
		{"vbr/60ms/code-3-multiframe", 1, 2880, oOpusAuto, 1, 10, 3, 2},
		{"vbr/60ms/code-3-multiframe/stereo", 2, 2880, oOpusAuto, 1, 10, 3, 2},
		// out_data_bytes == 1 forces the SILK-only TOC arm at :1370 even for a
		// 60 ms CELT encoder, because a code-3 packet needs two bytes.
		{"vbr/60ms/1-byte-forces-silk-toc", 1, 2880, oOpusAuto, 1, 1, 0, 1},
		// 120 ms (frame_rate 10): the (tocmode==SILK && frame_rate!=10) test is
		// FALSE here, so the num_multiframes arm runs with 50/10 = 5.
		{"vbr/120ms/code-3-multiframe", 1, 5760, oOpusAuto, 1, 12, 3, 2},
		// CBR: the degenerate packet is PADDED out to max_data_bytes (:1403).
		// 2.5 ms at 500 bps gives cbr_bytes 0 -> max_data_bytes 1 -> pad(1,1),
		// which short-circuits.
		{"cbr/2.5ms/imax-1", 1, 120, 500, 0, 1500, 0, 1},
		// CBR with a 2-byte budget: bitrate_bps is clamped to 800 by the buffer,
		// cbr_bytes = (16+4)/8 = 2, max_data_bytes = 2 < 3 -> degenerate.
		//
		// THE PACKET CODE IS 3, NOT 0. The degenerate branch builds a 1-byte code-0
		// TOC, then :1403 calls opus_packet_pad(data, 1, 2). len(1) != new_len(2), so
		// the repacketizer.c:343 short-circuit does NOT fire and the packet is
		// genuinely RE-FRAMED into a code-3 packet with a padding byte. This is the
		// only degenerate case in which opus_packet_pad does real work, and the only
		// place a code-0 TOC turns into a code-3 one after gen_toc has already run.
		{"cbr/20ms/padded-to-2/reframed-code-3", 1, 960, oOpusBitrateMax, 0, 2, 3, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			cfg.Bitrate = tc.bitrate
			cfg.VBR = tc.vbr
			cfg.VBRConstraint = 0

			ce, err := cOpusencCreate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ce.Close()
			gs := goEncoderMatchingCfg(t, cfg)

			pcm := testPCM(tc.frameSize, tc.channels, 0)
			ret, pkt, cState := ce.Encode(pcm, tc.frameSize, tc.maxDataBytes)
			if ret <= 0 {
				t.Fatalf("C opus_encode returned %d, but the degenerate branch returns a "+
					"POSITIVE length (it is not an error)", ret)
			}
			if ret != tc.wantLen {
				t.Fatalf("C returned %d bytes, the case predicted %d", ret, tc.wantLen)
			}
			if got := int(pkt[0] & 0x03); got != tc.wantPacketCode {
				t.Fatalf("C TOC 0x%02x has packet code %d, the case predicted %d: "+
					"the wrong degenerate sub-branch ran", pkt[0], got, tc.wantPacketCode)
			}

			// --- Go.
			maxDataBytes, argErr := gs.CheckEncodeArgs(tc.frameSize, tc.maxDataBytes)
			if argErr != nil {
				t.Fatalf("Go CheckEncodeArgs rejected (%v) what C accepted", argErr)
			}
			d, ok := gs.DecideRates(tc.frameSize, maxDataBytes)
			if !ok {
				t.Fatal("DecideRates needs the unported OPUS_AUTO mode decision")
			}
			if !d.Degenerate {
				t.Fatalf("Go did not take the degenerate branch, but C emitted a %d-byte "+
					"TOC-only packet", ret)
			}

			// The C's data buffer is out_data_bytes long; give Go the same room.
			// (max_data_bytes after IMAX(1, cbr_bytes) can only ever exceed
			// out_data_bytes by the ret <= 2 bump, and ret == 2 implies
			// out_data_bytes >= 2 by the :1370 test.)
			bufLen := tc.maxDataBytes
			if bufLen < 2 {
				bufLen = 2
			}
			goBuf := make([]byte, bufLen)
			goRet := gs.EncodeDegenerate(goBuf, tc.maxDataBytes, d.MaxDataBytes, d.FrameRate)
			if goRet != ret {
				t.Fatalf("Go EncodeDegenerate returned %d, C returned %d", goRet, ret)
			}
			if !bytes.Equal(goBuf[:goRet], pkt) {
				for i := range pkt {
					if goBuf[i] != pkt[i] {
						t.Fatalf("degenerate packet byte %d: Go = 0x%02x, C = 0x%02x (whole packet Go=%x C=%x)",
							i, goBuf[i], pkt[i], goBuf[:goRet], pkt)
					}
				}
			}

			// The degenerate branch returns at :1406, BEFORE the :2555-2562
			// epilogue, so st->first and the prev_* fields must be untouched.
			if cState.First != 1 {
				t.Fatalf("C st->first = %d after a degenerate frame, want 1: the branch "+
					"must return before the :2562 epilogue", cState.First)
			}
			rateStateFields(t, tc.name+"/state", gs.State(), cState)
		})
	}
}

// ---------------------------------------------------------------------------
// The rejection rules (opus_encoder.c:1224-1235, :827).
// ---------------------------------------------------------------------------

// TestOpusencRejectionRulesMatchC pins the EXACT error code for each way a call
// can be rejected, against the C encoder. A degenerate packet is deliberately
// included as the control: it looks like a failure and is not one.
func TestOpusencRejectionRulesMatchC(t *testing.T) {
	const (
		opusOK             = 0
		opusBadArg         = -1
		opusBufferTooSmall = -2
	)
	cases := []struct {
		name         string
		channels     int
		frameSize    int // as passed to opus_encode (analysis_frame_size)
		maxDataBytes int
		wantCode     int // negative, or opusOK meaning "returns a positive length"
		why          string
	}{
		{"frame-size/not-a-legal-duration", 1, 999, 100, opusBadArg,
			"frame_size_select rejects 999 (:843), and -1 trips frame_size <= 0 at :1224"},
		{"frame-size/below-2.5ms", 1, 100, 100, opusBadArg,
			"frame_size < Fs/400 = 120 (:829)"},
		{"frame-size/zero", 1, 0, 100, opusBadArg, "frame_size <= 0 (:1224)"},
		{"frame-size/7-times-20ms", 1, 6720, 500, opusBadArg,
			"140 ms is not one of the nine legal durations (:843)"},
		{"max-data-bytes/zero", 1, 960, 0, opusBadArg, "max_data_bytes <= 0 (:1224)"},
		{"100ms-in-1-byte", 1, 4800, 1, opusBufferTooSmall,
			"max_data_bytes == 1 && Fs == frame_size*10 (:1231)"},
		{"100ms-in-1-byte/stereo", 2, 4800, 1, opusBufferTooSmall,
			"same rule, stereo"},
		// CONTROL: 60 ms in 1 byte is NOT rejected. Fs != frame_size*10, so :1231
		// does not fire and the degenerate branch emits a 1-byte SILK TOC.
		{"60ms-in-1-byte/degenerate-not-an-error", 1, 2880, 1, opusOK,
			"Fs != frame_size*10, so :1231 does not apply; :1340 emits a TOC-only packet"},
		// CONTROL: a normal frame.
		{"normal/20ms", 1, 960, 200, opusOK, "nothing is wrong with this call"},
	}

	sawBadArg, sawBufferTooSmall, sawOK := 0, 0, 0

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			ce, err := cOpusencCreate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ce.Close()
			gs := goEncoderMatchingCfg(t, cfg)

			n := tc.frameSize
			if n <= 0 {
				n = 1
			}
			ret, _, _ := ce.Encode(testPCM(n, tc.channels, 0), tc.frameSize, tc.maxDataBytes)

			switch tc.wantCode {
			case opusOK:
				if ret <= 0 {
					t.Fatalf("C opus_encode returned %d, want a positive length (%s)", ret, tc.why)
				}
				sawOK++
			default:
				if ret != tc.wantCode {
					t.Fatalf("C opus_encode returned %d, want %d (%s)", ret, tc.wantCode, tc.why)
				}
				if ret == opusBadArg {
					sawBadArg++
				} else {
					sawBufferTooSmall++
				}
			}

			// --- Go: opus_encode's own frame_size_select, then the :1224-1235 rules.
			goFrameSize := opusenc.FrameSizeSelect(cfg.Application, int32(tc.frameSize),
				cfg.ExpertFrameDuration, int32(cfg.Fs))
			_, argErr := gs.CheckEncodeArgs(int(goFrameSize), tc.maxDataBytes)

			var goCode int
			switch argErr {
			case nil:
				goCode = opusOK
			case opusenc.ErrBadArg:
				goCode = opusBadArg
			case opusenc.ErrBufferTooSmall:
				goCode = opusBufferTooSmall
			default:
				t.Fatalf("unexpected Go error %v", argErr)
			}
			if goCode != tc.wantCode {
				t.Fatalf("Go rejection code = %d, C = %d (%s)", goCode, tc.wantCode, tc.why)
			}
		})
	}
	if sawBadArg == 0 || sawBufferTooSmall == 0 || sawOK == 0 {
		t.Fatalf("vacuous battery: OPUS_BAD_ARG = %d, OPUS_BUFFER_TOO_SMALL = %d, accepted = %d",
			sawBadArg, sawBufferTooSmall, sawOK)
	}
}

// TestOpusencVoiceEstMatchesC pins ComputeVoiceEst (opus_encoder.c:1413-1424) over
// its whole domain, including the voice_ratio arm that DISABLE_FLOAT_API makes
// unreachable in this build (st->voice_ratio is re-pinned to -1 at :1308). There
// is no flat wrapper for voice_est, so the expected values are the C expression
// evaluated by hand; the frozen configuration's value (48) is separately pinned by
// the state-dump battery above, which would move if the expression were wrong.
func TestOpusencVoiceEstMatchesC(t *testing.T) {
	const (
		sigAuto  = opusenc.OpusAuto
		sigVoice = opusenc.SignalVoice
		sigMusic = opusenc.SignalMusic
	)
	cases := []struct {
		signalType, voiceRatio, application, want int
	}{
		// OPUS_SIGNAL_* wins over everything (:1414-1417).
		{sigVoice, -1, opusenc.ApplicationAudio, 127},
		{sigVoice, 50, opusenc.ApplicationVOIP, 127},
		{sigMusic, -1, opusenc.ApplicationAudio, 0},
		{sigMusic, 100, opusenc.ApplicationVOIP, 0},
		// voice_ratio >= 0: Q7 rescale of a 0..100 percentage, capped at 115 for
		// AUDIO (:1418-1422). 100*327>>8 == 127.
		{sigAuto, 0, opusenc.ApplicationAudio, 0},
		{sigAuto, 50, opusenc.ApplicationAudio, 63},   // 50*327>>8 = 16350>>8 = 63
		{sigAuto, 90, opusenc.ApplicationAudio, 114},  // 29430>>8 = 114
		{sigAuto, 91, opusenc.ApplicationAudio, 115},  // 29757>>8 = 116 -> capped
		{sigAuto, 100, opusenc.ApplicationAudio, 115}, // 127 -> capped
		{sigAuto, 100, opusenc.ApplicationVOIP, 127},  // no cap outside AUDIO
		{sigAuto, 100, opusenc.ApplicationRestrictedLowdelay, 127},
		// voice_ratio < 0: the application decides (:1423-1424).
		{sigAuto, -1, opusenc.ApplicationVOIP, 115},
		{sigAuto, -1, opusenc.ApplicationAudio, 48}, // THE FROZEN VALUE
		{sigAuto, -1, opusenc.ApplicationRestrictedLowdelay, 48},
	}
	for _, tc := range cases {
		got := opusenc.ComputeVoiceEst(tc.signalType, tc.voiceRatio, tc.application)
		if got != tc.want {
			t.Errorf("ComputeVoiceEst(signal=%d, voice_ratio=%d, app=%d) = %d, want %d",
				tc.signalType, tc.voiceRatio, tc.application, got, tc.want)
		}
	}
	// The constant the brief calls out: 48, and it must come from the expression,
	// not from a hard-coded return.
	if got := opusenc.ComputeVoiceEst(opusenc.OpusAuto, -1, opusenc.ApplicationAudio); got != 48 {
		t.Fatalf("frozen-config voice_est = %d, want 48", got)
	}
	if opusenc.ComputeVoiceEst(opusenc.SignalVoice, -1, opusenc.ApplicationAudio) == 48 {
		t.Fatal("ComputeVoiceEst returns 48 unconditionally: the expression is hard-coded")
	}
}
