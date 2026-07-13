//go:build refc

package oracle

import (
	"math"
	"testing"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// CP9 unit "PCM": the differential gate for the PCM-domain front end of
// opus_encode_frame_native (internal/opusenc/pcm.go) against the C oracle.
//
// Three layers, each bit-exact:
//
//  1. TestOpusencDCRejectMatchesC       - dc_reject (opus_encoder.c:479) leaf,
//     multi-frame with hp_mem threaded.
//  2. TestOpusencStereoFadeMatchesC     - stereo_fade (:548) leaf.
//  3. TestOpusencPCMFrontEndMatchesC    - the WHOLE front end (history copy,
//     variable_HP_smth2_Q15, dc_reject, the delay-compensation ring, the
//     stereoWidth_Q14 derivation and the stereo_fade crossfade) run in LOCKSTEP
//     with the real C opus_encode over multi-frame sequences, compared against the
//     C encoder's field-level state dump every frame.
//
// (3) is what actually pins the ring. delay_buffer, hp_mem and
// hybrid_stereo_width_Q14 are all cross-frame state, so a single-frame check is
// worthless: an ordering bug (storing POST-fade history instead of PRE-fade, or
// taking the head of delay_buffer instead of its tail) reproduces frame 1 exactly
// and only diverges from frame 2 on.

// ---------------------------------------------------------------------------
// Signals.
// ---------------------------------------------------------------------------

// pcmSignal builds a deterministic, non-degenerate frame of interleaved int16.
// kind selects the character of the signal; seed advances the phase so a sequence
// of frames is continuous rather than a repeat of one frame.
//
//	"tone"   swept tone plus harmonics, decorrelated between L and R
//	"dc"     a large DC offset with a small ripple: the case dc_reject exists for
//	"clip"   alternating full-scale +32767 / -32768, to drive SATURATE, the
//	         SHL32(x,14) headroom and stereo_fade's int16 narrowing to their limits
//	"wide"   L and R in antiphase at full amplitude: maximal side signal, so
//	         stereo_fade has the largest possible effect
//	"quiet"  near-silence with a 1-LSB dither (no digital-silence fast path, but
//	         nothing for the ring to hide behind either)
func pcmSignal(kind string, n, channels, seed int) []int16 {
	pcm := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		t := float64(seed*n + i)
		var l, r float64
		switch kind {
		case "tone":
			l = 0.62*math.Sin(2*math.Pi*(300.0+0.03*t)*t/48000.0) +
				0.21*math.Sin(2*math.Pi*4700.0*t/48000.0)
			r = 0.55*math.Sin(2*math.Pi*(300.0+0.03*t)*t/48000.0+0.9) +
				0.17*math.Sin(2*math.Pi*9100.0*t/48000.0)
		case "dc":
			l = 0.80 + 0.04*math.Sin(2*math.Pi*120.0*t/48000.0)
			r = -0.75 + 0.05*math.Sin(2*math.Pi*90.0*t/48000.0)
		case "clip":
			// Exact rail values, not scaled floats.
			for c := 0; c < channels; c++ {
				if (i+c+seed)&1 == 0 {
					pcm[i*channels+c] = 32767
				} else {
					pcm[i*channels+c] = -32768
				}
			}
			continue
		case "wide":
			v := math.Sin(2 * math.Pi * (220.0 + 0.11*t) * t / 48000.0)
			l = 0.98 * v
			r = -0.98 * v
		case "quiet":
			l = 0.00004 * math.Sin(2*math.Pi*1000.0*t/48000.0)
			r = 0.00003 * math.Cos(2*math.Pi*1500.0*t/48000.0)
		default:
			panic("pcmSignal: unknown kind " + kind)
		}
		pcm[i*channels] = clampToInt16(l * 32767)
		if channels == 2 {
			pcm[i*channels+1] = clampToInt16(r * 32767)
		}
	}
	return pcm
}

func clampToInt16(v float64) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}

// ---------------------------------------------------------------------------
// 1. dc_reject.
// ---------------------------------------------------------------------------

// TestOpusencDCRejectMatchesC drives dc_reject (opus_encoder.c:479, FIXED_POINT)
// against the C for multi-frame sequences with hp_mem threaded across frames.
//
// hp_mem[1] and hp_mem[3] must stay ZERO throughout: dc_reject strides by 2*c and
// touches only [0] and [2]. The odd slots belong to hp_cutoff / silk_biquad, which
// is the OPUS_APPLICATION_VOIP path and is not ported. This is asserted, not
// assumed, because "tidying" the indexing to c instead of 2*c is exactly the kind
// of change that would silently pass a single-channel test.
func TestOpusencDCRejectMatchesC(t *testing.T) {
	const fs = 48000
	kinds := []string{"tone", "dc", "clip", "wide", "quiet"}

	var (
		frames      int
		sawNonZeroM int // hp_mem[0] actually moved off 0
		sawFiltered int // the filter actually changed a sample
	)

	for _, channels := range []int{1, 2} {
		for _, frameSize := range []int{120, 240, 480, 960} {
			for _, kind := range kinds {
				t.Run("", func(t *testing.T) {
					var goMem [4]int32
					cMem := [4]int32{}
					for f := 0; f < 6; f++ {
						in := pcmSignal(kind, frameSize, channels, f)

						goOut := make([]int16, frameSize*channels)
						// cutoff 3 is what the Opus caller hard-codes at :2008.
						opusenc.DCReject(in, 3, goOut, &goMem, frameSize, channels, fs)

						cOut, newCMem := cOpusencDCReject(in, 3, cMem, frameSize, channels, fs)
						cMem = newCMem

						frames++
						for i := range goOut {
							if goOut[i] != cOut[i] {
								t.Fatalf("c=%d lm=%d kind=%s frame=%d: out[%d] = %d, want %d (C)",
									channels, frameSize, kind, f, i, goOut[i], cOut[i])
							}
							if goOut[i] != in[i] {
								sawFiltered++
							}
						}
						if goMem != cMem {
							t.Fatalf("c=%d lm=%d kind=%s frame=%d: hp_mem = %v, want %v (C)",
								channels, frameSize, kind, f, goMem, cMem)
						}
						if goMem[1] != 0 || goMem[3] != 0 {
							t.Fatalf("c=%d lm=%d kind=%s frame=%d: dc_reject touched the ODD hp_mem "+
								"slots (%v); it strides by 2*c and must not", channels, frameSize, kind, f, goMem)
						}
						if goMem[0] != 0 {
							sawNonZeroM++
						}
					}
				})
			}
		}
	}

	// Non-vacuity.
	if frames != 2*4*5*6 {
		t.Fatalf("non-vacuity: ran %d frames, expected %d", frames, 2*4*5*6)
	}
	if sawNonZeroM == 0 {
		t.Fatal("non-vacuity: hp_mem[0] never moved off zero; the filter state is not being exercised")
	}
	if sawFiltered == 0 {
		t.Fatal("non-vacuity: dc_reject never changed a sample; it is behaving as a copy")
	}
}

// ---------------------------------------------------------------------------
// 2. stereo_fade.
// ---------------------------------------------------------------------------

// TestOpusencStereoFadeMatchesC drives stereo_fade (opus_encoder.c:548) against
// the C over the exact g1/g2 values the :2333-2340 mapping can produce: the Q14
// stereoWidth is either 16384, which maps to Q15ONE (32767, NOT 32768, which does
// not fit), or is shifted left by one.
func TestOpusencStereoFadeMatchesC(t *testing.T) {
	const (
		fs       = 48000
		channels = 2
	)

	// The Q14 stereoWidth values, mapped exactly as :2337-2338 does.
	q14 := []int16{0, 1, 2048, 8192, 12288, 16383, 16384}
	mapQ14 := func(v int16) int16 {
		if v == 16384 {
			return 32767 // Q15ONE
		}
		return int16(uint16(v) << 1) // SHL16(v, 1)
	}

	enc := opusenc.NewEncoder(fs, channels, opusenc.ApplicationAudio)
	if enc == nil {
		t.Fatal("opusenc.NewEncoder returned nil")
	}
	window := enc.CELT().Window()
	overlap := enc.CELT().Overlap()
	if overlap != 120 || len(window) != 120 {
		t.Fatalf("celt mode geometry: overlap=%d len(window)=%d, want 120/120", overlap, len(window))
	}

	var cases, changed int

	for _, frameSize := range []int{120, 240, 480, 960} {
		for _, kind := range []string{"tone", "wide", "clip", "dc"} {
			for _, g1q := range q14 {
				for _, g2q := range q14 {
					in := pcmSignal(kind, frameSize, channels, 3)
					g1 := mapQ14(g1q)
					g2 := mapQ14(g2q)

					goOut := make([]int16, len(in))
					copy(goOut, in)
					opusenc.StereoFade(in, goOut, g1, g2, overlap, frameSize, channels, window, fs)

					cOut := cOpusencStereoFade(in, g1, g2, frameSize, channels, fs)

					cases++
					for i := range goOut {
						if goOut[i] != cOut[i] {
							t.Fatalf("lm=%d kind=%s g1=%d g2=%d: out[%d] = %d, want %d (C)",
								frameSize, kind, g1, g2, i, goOut[i], cOut[i])
						}
						if goOut[i] != in[i] {
							changed++
						}
					}
				}
			}
		}
	}

	if cases != 4*4*len(q14)*len(q14) {
		t.Fatalf("non-vacuity: ran %d cases, expected %d", cases, 4*4*len(q14)*len(q14))
	}
	if changed == 0 {
		t.Fatal("non-vacuity: stereo_fade never changed a sample")
	}
}

// ---------------------------------------------------------------------------
// 3. The whole PCM front end, in lockstep with the C encoder.
// ---------------------------------------------------------------------------

// hbGainQ15One is HB_gain in the frozen config. It is assigned Q15ONE at
// opus_encoder.c:2044 and only moved off it inside `if (st->mode != MODE_CELT_ONLY)`
// (:2061), which is unreachable under OPUS_SET_FORCE_MODE(MODE_CELT_ONLY). So
// gain_fade (:2316) never fires and is not ported; the st->prev_HB_gain = HB_gain
// assignment (:2319) is still live and IS compared.
const hbGainQ15One int16 = 32767

type pcmFrontCase struct {
	name       string
	channels   int
	frameSize  int
	bitrate    int
	vbr        int
	complexity int
	kind       string
}

// TestOpusencPCMFrontEndMatchesC is the CP9 PCM gate.
//
// It runs the real C opus_encode frame by frame and, for each frame, runs the
// pure-Go front end on the SAME input, then compares every field of the C
// encoder's state that this unit owns:
//
//	hp_mem[0..3]            (dc_reject)
//	delay_buffer[0..N)      (the delay-compensation ring)
//	variable_HP_smth2_Q15   (the smoother at :1974-1978)
//	prev_HB_gain            (:2319)
//	hybrid_stereo_width_Q14 (the stereo_fade crossfade carried across frames)
//
// The two per-frame decisions that belong to the ENCODER DRIVER, not to this unit
// (st->mode at :1528-1530 and st->stream_channels at :1428-1456), plus equiv_rate
// (:1573), are read back off the C encoder's own dump and injected, so a driver
// bug cannot masquerade as a front-end pass or vice versa.
//
// The sweep MUST cover LM 0..3: the delay-buffer update at :2304-2312 takes the
// MOVE branch when frame_size < 288 (LM0=120, LM1=240) and the COPY branch
// otherwise (LM2=480, LM3=960). A sweep that misses either branch is vacuous, and
// the counters below fail the test if it does.
func TestOpusencPCMFrontEndMatchesC(t *testing.T) {
	cases := []pcmFrontCase{
		// LM 0..3, mono and stereo, at a bitrate high enough that the stereo width
		// is not reduced (equiv_rate > 32000) -> stereo_fade must NOT fire.
		{"mono-lm0-96k", 1, 120, 96000, 1, 9, "tone"},
		{"mono-lm1-96k", 1, 240, 96000, 1, 9, "dc"},
		{"mono-lm2-96k", 1, 480, 96000, 1, 9, "clip"},
		{"mono-lm3-96k", 1, 960, 96000, 1, 9, "quiet"},
		{"stereo-lm0-96k", 2, 120, 96000, 1, 9, "tone"},
		{"stereo-lm1-96k", 2, 240, 96000, 1, 9, "wide"},
		{"stereo-lm2-96k", 2, 480, 96000, 1, 9, "clip"},
		{"stereo-lm3-96k", 2, 960, 96000, 1, 9, "dc"},

		// Low bitrate: equiv_rate <= 32000 -> stereoWidth_Q14 < 16384 -> stereo_fade
		// FIRES for the stereo cases, and keeps firing on later frames because
		// hybrid_stereo_width_Q14 stays below 1<<14. LM0/LM1 also push equiv_rate
		// below 16000 (the per-frame overhead term at compute_equiv_rate:1033
		// dominates at 400/200 Hz frame rates), which pins stereoWidth_Q14 to 0.
		{"stereo-lm0-24k-fade", 2, 120, 24000, 1, 9, "wide"},
		{"stereo-lm1-24k-fade", 2, 240, 24000, 1, 9, "tone"},
		{"stereo-lm2-24k-fade", 2, 480, 24000, 1, 9, "wide"},
		{"stereo-lm3-24k-fade", 2, 960, 24000, 1, 9, "clip"},
		{"stereo-lm3-20k-fade", 2, 960, 20000, 1, 9, "wide"},
		{"mono-lm3-24k", 1, 960, 24000, 1, 9, "wide"},

		// CBR: st->bitrate_bps is REQUANTIZED at :1330-1333 before equiv_rate is
		// computed, so the whole stereoWidth chain sees the quantized rate.
		{"stereo-lm3-cbr-32k", 2, 960, 32000, 0, 9, "tone"},
		{"stereo-lm2-cbr-48k", 2, 480, 48000, 0, 9, "wide"},
		{"mono-lm1-cbr-64k", 1, 240, 64000, 0, 9, "clip"},

		// Complexity 0 and 10 move equiv_rate (compute_equiv_rate:1037 and the
		// CELT complexity<5 penalty at :1048), which can flip stereo_fade on or off.
		{"stereo-lm3-40k-c0", 2, 960, 40000, 1, 0, "wide"},
		{"stereo-lm3-33k-c10", 2, 960, 33000, 1, 10, "wide"},
		{"stereo-lm3-34k-c0", 2, 960, 34000, 1, 0, "tone"},
	}

	const (
		frames       = 10
		maxDataBytes = 1276
	)

	var (
		moveBranch, copyBranch int
		fadeFired, fadeNotHit  int
		monoCases, stereoCases int
		lmSeen                 = map[int]int{}
		preFadeOrderingMatters int
		hpMemMoved             int
		delayNonZero           int
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultOpusencCfg(tc.channels)
			cfg.Bitrate = tc.bitrate
			cfg.VBR = tc.vbr
			cfg.Complexity = tc.complexity

			ch, err := cOpusencCreate(cfg)
			if err != nil {
				t.Fatalf("cOpusencCreate: %v", err)
			}
			defer ch.Close()

			gst := goEncoderMatchingCfg(t, cfg)
			// The ring depth is delay_compensation (Fs/250 = 192), NOT Lookahead, which
			// also carries the Fs/400 CELT overlap term (312 at 48 kHz).
			totalBuffer := gst.DelayCompensation()
			if totalBuffer != 192 {
				t.Fatalf("total_buffer = %d, want 192 (the whole ring depends on it)", totalBuffer)
			}

			frameRate := cfg.Fs / tc.frameSize

			for f := 0; f < frames; f++ {
				pcm := pcmSignal(tc.kind, tc.frameSize, tc.channels, f)

				// Snapshot the pre-frame crossfade state so the test can predict
				// whether stereo_fade is about to fire (a non-vacuity counter, not a
				// substitute for the comparison).
				prevHybrid := gst.State().HybridStereoWidthQ14

				// --- C ---
				cIn := make([]int16, len(pcm))
				copy(cIn, pcm)
				ret, _, cst := ch.Encode(cIn, tc.frameSize, maxDataBytes)
				if ret < 0 {
					t.Fatalf("frame %d: C opus_encode returned %d", f, ret)
				}
				// The degenerate PLC branch (:1340) returns BEFORE
				// opus_encode_frame_native, so it would leave hp_mem and delay_buffer
				// untouched and make this lockstep meaningless. Assert it did not fire.
				if cst.BitrateBps < int32(24*frameRate) {
					t.Fatalf("frame %d: bitrate_bps=%d < 3*frame_rate*8=%d, so the C took the "+
						"degenerate PLC branch (:1340) and never ran the PCM front end; "+
						"this case cannot validate the ring", f, cst.BitrateBps, 24*frameRate)
				}
				if cst.Mode != int32(opusenc.ModeCeltOnly) {
					t.Fatalf("frame %d: C mode = %d, want MODE_CELT_ONLY (%d)",
						f, cst.Mode, opusenc.ModeCeltOnly)
				}

				// equiv_rate as computed at :1573, from the values the C encoder
				// itself landed on: st->bitrate_bps (post-CBR-requantization) and
				// st->stream_channels (post-downmix decision).
				equivRate := cOpusencComputeEquivRate(cst.BitrateBps, int(cst.StreamChannels),
					frameRate, cfg.VBR, int(cst.Mode), cfg.Complexity, cfg.PacketLossPerc)

				// --- Go ---
				gst.SetPCMFrameContext(int(cst.Mode), int(cst.StreamChannels))
				pcmBuf := gst.PCMFront(pcm, tc.frameSize, totalBuffer)
				gst.UpdateDelayBuffer(pcmBuf, tc.frameSize, totalBuffer)
				gst.PCMFades(pcmBuf, tc.frameSize, equivRate, hbGainQ15One)
				gs := gst.State()

				// --- compare the fields this unit owns ---
				for i := 0; i < 4; i++ {
					if gs.HPMem[i] != cst.HPMem[i] {
						t.Fatalf("frame %d: hp_mem[%d] = %d, want %d (C)",
							f, i, gs.HPMem[i], cst.HPMem[i])
					}
				}
				if gs.VariableHPSmth2Q15 != cst.VariableHPSmth2Q15 {
					t.Fatalf("frame %d: variable_HP_smth2_Q15 = %d, want %d (C)",
						f, gs.VariableHPSmth2Q15, cst.VariableHPSmth2Q15)
				}
				if gs.PrevHBGain != cst.PrevHBGain {
					t.Fatalf("frame %d: prev_HB_gain = %d, want %d (C)",
						f, gs.PrevHBGain, cst.PrevHBGain)
				}
				if gs.HybridStereoWidthQ14 != cst.HybridStereoWidthQ14 {
					t.Fatalf("frame %d: hybrid_stereo_width_Q14 = %d, want %d (C)",
						f, gs.HybridStereoWidthQ14, cst.HybridStereoWidthQ14)
				}
				if len(gs.DelayBuffer) != len(cst.DelayBuffer) {
					t.Fatalf("frame %d: len(delay_buffer) = %d, want %d (C)",
						f, len(gs.DelayBuffer), len(cst.DelayBuffer))
				}
				for i := range gs.DelayBuffer {
					if gs.DelayBuffer[i] != cst.DelayBuffer[i] {
						t.Fatalf("frame %d: delay_buffer[%d] = %d, want %d (C)  "+
							"(delay_buffer is stored PRE-fade, from pcm_buf, at :2304-2312)",
							f, i, gs.DelayBuffer[i], cst.DelayBuffer[i])
					}
					if gs.DelayBuffer[i] != 0 {
						delayNonZero++
					}
				}
				if gs.HPMem[0] != 0 {
					hpMemMoved++
				}

				// --- non-vacuity bookkeeping ---
				lmSeen[tc.frameSize]++
				if tc.channels == 1 {
					monoCases++
				} else {
					stereoCases++
				}
				if tc.channels*(480-(tc.frameSize+totalBuffer)) > 0 {
					moveBranch++
				} else {
					copyBranch++
				}

				// Did stereo_fade fire? Reproduce the :2329-2331 guard.
				stereoWidth := stereoWidthQ14FromEquivRate(equivRate)
				fired := tc.channels == 2 && (prevHybrid < (1<<14) || stereoWidth < (1<<14))
				if fired {
					fadeFired++
					// On the COPY branch the stored history is exactly the TAIL of
					// pcm_buf. If it still matches the POST-fade pcm_buf, then
					// storing after the fade would have passed too, and this frame
					// proves nothing about the :2313-2314 ordering. Count the frames
					// where it genuinely does.
					if tc.frameSize+totalBuffer >= 480 {
						tail := pcmBuf[(tc.frameSize+totalBuffer-480)*tc.channels:]
						for i := range gs.DelayBuffer {
							if gs.DelayBuffer[i] != tail[i] {
								preFadeOrderingMatters++
								break
							}
						}
					}
				} else {
					fadeNotHit++
				}
			}
		})
	}

	// ---- non-vacuity guards. Any zero here means a branch was never tested. ----
	if moveBranch == 0 {
		t.Error("non-vacuity: the MOVE branch of the delay-buffer update (:2306-2309, " +
			"frame_size < 288) was never taken")
	}
	if copyBranch == 0 {
		t.Error("non-vacuity: the COPY branch of the delay-buffer update (:2311, " +
			"frame_size >= 288) was never taken")
	}
	if fadeFired == 0 {
		t.Error("non-vacuity: stereo_fade never fired")
	}
	if fadeNotHit == 0 {
		t.Error("non-vacuity: stereo_fade always fired; the not-firing path is untested")
	}
	if preFadeOrderingMatters == 0 {
		t.Error("non-vacuity: no frame where the PRE-fade delay_buffer differs from the " +
			"POST-fade pcm_buf, so the :2313-2314 ordering is not actually pinned")
	}
	if monoCases == 0 || stereoCases == 0 {
		t.Errorf("non-vacuity: mono frames=%d stereo frames=%d, need both", monoCases, stereoCases)
	}
	for _, lm := range []int{120, 240, 480, 960} {
		if lmSeen[lm] == 0 {
			t.Errorf("non-vacuity: frame_size %d (LM %d) never exercised", lm, lmOf(lm))
		}
	}
	if hpMemMoved == 0 {
		t.Error("non-vacuity: hp_mem[0] never moved off zero")
	}
	if delayNonZero == 0 {
		t.Error("non-vacuity: delay_buffer stayed all-zero; the ring is not carrying anything")
	}

	t.Logf("coverage: move=%d copy=%d fade_fired=%d fade_not_fired=%d "+
		"pre_fade_ordering_pinned=%d mono=%d stereo=%d lm={120:%d 240:%d 480:%d 960:%d}",
		moveBranch, copyBranch, fadeFired, fadeNotHit, preFadeOrderingMatters,
		monoCases, stereoCases, lmSeen[120], lmSeen[240], lmSeen[480], lmSeen[960])
}

// stereoWidthQ14FromEquivRate mirrors opus_encoder.c:2320-2328. It exists only to
// predict, in the TEST, whether stereo_fade is about to fire, so the non-vacuity
// counters are honest. The port's own copy lives in opusenc.PCMFades and is what
// the differential actually checks.
func stereoWidthQ14FromEquivRate(equivRate int32) int32 {
	switch {
	case equivRate > 32000:
		return 16384
	case equivRate < 16000:
		return 0
	default:
		return 16384 - 2048*(32000-equivRate)/(equivRate-14000)
	}
}

func lmOf(frameSize int) int {
	switch frameSize {
	case 120:
		return 0
	case 240:
		return 1
	case 480:
		return 2
	default:
		return 3
	}
}
