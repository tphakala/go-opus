//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/opusenc"
)

// CP10-0: THE SAMPLE-RATE GATE.
//
// Until CP10 the encoder was 48 kHz ONLY and nothing said so: celt.NewEncoder
// hard-coded st->upsample = 1, while opusenc.NewEncoder happily ACCEPTED 8, 12, 16
// and 24 kHz. The CELT LM search (celt_encoder.c:1837, frame_size *= st->upsample)
// then failed to match any LM and returned OPUS_BAD_ARG, so a non-48k encoder was a
// configuration that could be built but never used. This file is what makes the
// other four rates real, and what keeps them real.
//
// THE WHOLE OF THE RATE is one integer. celt_encoder_init (celt_encoder.c:239)
// builds the SAME 48 kHz / 960 mode at every sampling_rate and threads the rate in
// through exactly one field, st->upsample = resampling_factor(Fs) (:255). From
// there:
//
//	celt_preemphasis (:583-589)  Nu = N/upsample, and the input is ZERO-STUFFED:
//	                             inp[i*upsample] = pcm[i], the rest left at 0.
//	compute_mdcts (:544-552)     the surviving bins are SCALED by upsample and
//	                             everything above bound = B*N/upsample is CLEARED.
//	celt_encode_with_ec (:1837)  frame_size *= upsample BEFORE the LM search, so
//	                             every downstream quantity (N, vbr_rate, the
//	                             allocation) is in 48 kHz samples.
//
// So an upsample that is off by one does not degrade the packet, it changes the
// frame length CELT thinks it is coding. That is why this sweep is a byte-for-byte
// comparison and not a quality check.
//
// The Opus layer above it is already rate-parameterized (encoder_buffer = Fs/100,
// delay_compensation = Fs/250, the frame-size domain Fs/400..Fs/50, the Nyquist
// bandwidth clamps at opus_encoder.c:1643-1650), and rates.go's decision chain is
// pinned against the C at all five rates by opusenc_rates_test.go. What was NEVER
// exercised end to end is an actual ENCODE below 48 kHz. It is now.

// encoderRates are the five rates opus_encoder_create accepts. 96 kHz is
// ENABLE_QEXT-only and outside the frozen config.
var encoderRates = []int{8000, 12000, 16000, 24000, 48000}

// frameSizesAt returns the four frame sizes phase 4 supports at fs, i.e. the
// 2.5 / 5 / 10 / 20 ms durations, which are Fs/400, Fs/200, Fs/100 and Fs/50. They
// map onto LM 0..3 after CELT's frame_size *= upsample, so each one is 120, 240,
// 480 or 960 samples of the 48 kHz mode at EVERY rate.
func frameSizesAt(fs int) []int {
	return []int{fs / 400, fs / 200, fs / 100, fs / 50}
}

// maxBandwidthAt is the highest bandwidth opus_encoder.c:1643-1650 will allow at
// fs, AFTER the CELT-only MEDIUMBAND -> WIDEBAND rewrite at :1681. This is the
// consequence of the rate that is visible in the TOC byte, so it doubles as the
// non-vacuity guard: a sweep at 8 kHz that never lands on NARROWBAND is not really
// encoding at 8 kHz.
//
// 12 kHz is the interesting one: the Nyquist clamp pins it to MEDIUMBAND, which
// CELT does not support, so :1681 rewrites it to WIDEBAND. A 12 kHz stream is
// therefore coded with the WIDEBAND end band (17) even though its Nyquist is 6 kHz;
// the bins above it are the ones compute_mdcts already zeroed. That is libopus'
// behaviour, and the byte comparison is what proves the port reproduces it rather
// than "fixing" it.
func maxBandwidthAt(fs int) (bandwidth, endBand int) {
	switch fs {
	case 8000:
		return oBandwidthNarrowband, 13
	case 12000:
		return oBandwidthWideband, 17 // MEDIUMBAND, rewritten by :1681
	case 16000:
		return oBandwidthWideband, 17
	case 24000:
		return oBandwidthSuperwideband, 19
	default:
		return oBandwidthFullband, 21
	}
}

// TestCeltResamplingFactorMatchesC pins celt.ResamplingFactor against
// resampling_factor (celt/celt.c:62) over every rate the encoder can be built at
// AND over rates it cannot, because the 0 return is what makes celt.NewEncoder
// reject an unsupported rate instead of dividing by it in celt_preemphasis.
func TestCeltResamplingFactorMatchesC(t *testing.T) {
	rates := []int32{
		// The five legal ones.
		8000, 12000, 16000, 24000, 48000,
		// Rates the C returns 0 for: the celt_assert(0) is compiled out in the
		// frozen config (no ENABLE_ASSERTIONS), so 0 is a value that really can
		// come back.
		0, 1, 4000, 11025, 22050, 32000, 44100, 64000, 96000, 192000, -48000,
	}
	sawZero := false
	for _, r := range rates {
		got := int32(celt.ResamplingFactor(r))
		want := cCeltResamplingFactor(r)
		if got != want {
			t.Errorf("ResamplingFactor(%d) = %d, want %d (C)", r, got, want)
		}
		if want == 0 {
			sawZero = true
		}
	}
	if !sawZero {
		t.Fatal("non-vacuity: no unsupported rate was tested, so the 0 return is unproven")
	}
}

// The matching "celt.NewEncoder rejects a rate resampling_factor cannot handle"
// invariant needs no C and lives in internal/celt (TestNewEncoderRateDomain), so it
// runs in the default test job and not only under -tags refc.

// TestOpusencNewEncoderRateDomain pins the ACCEPT/REJECT boundary of the Go
// constructor against opus_encoder_create's. THE API MUST NEVER ACCEPT A SAMPLE
// RATE IT CANNOT ENCODE: this is the test that says so, and the sweep below is what
// proves "accepts" means "encodes bit-exactly".
func TestOpusencNewEncoderRateDomain(t *testing.T) {
	rates := []int{8000, 12000, 16000, 24000, 48000, 0, 11025, 22050, 32000, 44100, 96000}
	sawAccept, sawReject := false, false
	for _, fs := range rates {
		t.Run(fmt.Sprintf("fs%d", fs), func(t *testing.T) {
			cfg := defaultOpusencCfg(2)
			cfg.Fs = fs
			ch, cErr := cOpusencCreate(cfg)
			if ch != nil {
				defer ch.Close()
			}
			cOK := cErr == nil
			gOK := opusenc.NewEncoder(int32(fs), 2, opusenc.ApplicationAudio) != nil
			if gOK != cOK {
				t.Fatalf("fs=%d: Go accepted = %v, C accepted = %v (%v)", fs, gOK, cOK, cErr)
			}
			if cOK {
				sawAccept = true
			} else {
				sawReject = true
			}
		})
	}
	if !sawAccept || !sawReject {
		t.Fatal("non-vacuity: the rate domain test saw only one verdict")
	}
}

// TestOpusencEncodeSampleRateSweep IS THE CP10-0 GATE. Every rate x mono/stereo x
// LM 0..3 x {CBR, VBR, constrained VBR} x four bitrates, eight frames each, on a
// live encoder pair: every packet byte, the return value, st->rangeFinal and the
// whole cross-frame state must match the C.
//
// The 48 kHz row is kept as the CONTROL. It duplicates TestOpusencEncodeSweep, and
// that is the point: it proves this harness reproduces the known-good rate through
// exactly the code path the other four rates take, so a failure at 8 kHz is a
// failure of the rate and not of the test.
func TestOpusencEncodeSampleRateSweep(t *testing.T) {
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

	// Per-rate non-vacuity. Each rate has to reach all four LMs, both arms of the
	// delay-ring update (:2304), both stream-channel counts, and its own bandwidth
	// ceiling; otherwise the sweep is not really exercising that rate.
	type cover struct {
		lm                 [4]bool
		moveArm, copyArm   bool
		mono, stereo       bool
		maxBW, maxEnd      int
		sawSilence, sawPkt bool
	}
	seen := map[int]*cover{}
	for _, fs := range encoderRates {
		seen[fs] = &cover{}
	}

	for _, fs := range encoderRates {
		cov := seen[fs]
		for _, channels := range []int{1, 2} {
			for lm, frameSize := range frameSizesAt(fs) {
				for _, rm := range rateModes {
					for _, bitrate := range []int{16000, 32000, 64000, 128000} {
						name := fmt.Sprintf("fs%d/c%d/n%d/%s/%dbps",
							fs, channels, frameSize, rm.name, bitrate)
						t.Run(name, func(t *testing.T) {
							cfg := defaultOpusencCfg(channels)
							cfg.Fs = fs
							cfg.VBR = rm.vbr
							cfg.VBRConstraint = rm.vbrConstraint
							cfg.Bitrate = bitrate
							p := newEncPair(t, cfg)

							for i := 0; i < 8; i++ {
								pcm := testPCM(frameSize, channels, i)
								if i == 5 || i == 6 {
									pcm = zerosPCM(frameSize, channels) // CELT's silence path
									cov.sawSilence = true
								}
								ret, wit := p.frame(name, pcm, frameSize, 1500)
								if ret <= 0 {
									t.Fatalf("%s: unexpected non-positive return %d", name, ret)
								}
								cov.sawPkt = true
								cov.lm[lm] = true
								cov.moveArm = cov.moveArm || wit.DelayMoveBranch
								cov.copyArm = cov.copyArm || wit.DelayCopyBranch
								if wit.StreamChannels == 1 {
									cov.mono = true
								} else {
									cov.stereo = true
								}
								if wit.CurrBandwidth > cov.maxBW {
									cov.maxBW = wit.CurrBandwidth
								}
								if wit.EndBand > cov.maxEnd {
									cov.maxEnd = wit.EndBand
								}
							}
						})
					}
				}
			}
		}
	}

	for _, fs := range encoderRates {
		cov := seen[fs]
		if !cov.sawPkt {
			t.Fatalf("fs=%d: no packet was ever coded", fs)
		}
		for lm, ok := range cov.lm {
			if !ok {
				t.Errorf("fs=%d: non-vacuity: LM %d (frame size %d) never encoded",
					fs, lm, (fs/400)<<lm)
			}
		}
		// encoder_buffer = Fs/100 and total_buffer = Fs/250, so the :2304 MOVE arm
		// needs frame_size < Fs*3/500 (LM0/LM1) and the COPY arm frame_size >= it
		// (LM2/LM3). The split falls in the same place at every rate.
		if !cov.moveArm {
			t.Errorf("fs=%d: non-vacuity: the :2304 delay-buffer MOVE branch never fired "+
				"(needs frame_size < %d)", fs, fs*3/500)
		}
		if !cov.copyArm {
			t.Errorf("fs=%d: non-vacuity: the :2304 delay-buffer COPY branch never fired", fs)
		}
		if !cov.mono {
			t.Errorf("fs=%d: non-vacuity: no mono stream was coded", fs)
		}
		if !cov.stereo {
			t.Errorf("fs=%d: non-vacuity: no stereo stream was coded", fs)
		}
		if !cov.sawSilence {
			t.Errorf("fs=%d: non-vacuity: no digitally silent frame was coded", fs)
		}
		// THE RATE IS VISIBLE IN THE TOC. If the Nyquist clamp at :1643-1650 were not
		// applied, an 8 kHz stream at 128 kbps would come out FULLBAND. This asserts
		// the ceiling was both REACHED (the sweep is rich enough) and NOT EXCEEDED.
		wantBW, wantEnd := maxBandwidthAt(fs)
		if cov.maxBW != wantBW {
			t.Errorf("fs=%d: highest curr_bandwidth seen = %d, want %d "+
				"(the opus_encoder.c:1643-1650 Nyquist clamp)", fs, cov.maxBW, wantBW)
		}
		if cov.maxEnd != wantEnd {
			t.Errorf("fs=%d: highest CELT end band seen = %d, want %d", fs, cov.maxEnd, wantEnd)
		}
	}
}

// TestOpusencEncodeSampleRateMixedFrameSizes changes the frame size MID-STREAM at
// every rate. It is the sharpest test of the delay-compensation ring below 48 kHz,
// where encoder_buffer (Fs/100) and total_buffer (Fs/250) are small: at 8 kHz they
// are 80 and 32 samples, so an off-by-one in the history copy that 48 kHz's
// 480/192 would absorb for a frame or two shows up immediately.
func TestOpusencEncodeSampleRateMixedFrameSizes(t *testing.T) {
	for _, fs := range encoderRates {
		n := frameSizesAt(fs) // LM 0..3
		// Alternates across the MOVE/COPY boundary (frame_size == Fs*3/500).
		seq := []int{n[3], n[0], n[2], n[1], n[0], n[3], n[1], n[2], n[0], n[0], n[3], n[2], n[1], n[3]}

		for _, channels := range []int{1, 2} {
			for _, vbr := range []int{0, 1} {
				name := fmt.Sprintf("fs%d/c%d/vbr%d", fs, channels, vbr)
				t.Run(name, func(t *testing.T) {
					cfg := defaultOpusencCfg(channels)
					cfg.Fs = fs
					cfg.VBR = vbr
					cfg.VBRConstraint = 0
					cfg.Bitrate = 64000
					p := newEncPair(t, cfg)

					var sawMove, sawCopy bool
					for i, frameSize := range seq {
						_, wit := p.frame(name, testPCM(frameSize, channels, i), frameSize, 1500)
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
}

// TestOpusencEncodeSampleRateFrameDomain pins the FRAME-SIZE DOMAIN at every rate.
// The legal durations scale with Fs (Fs/400 .. Fs/50 for phase 4), so a sample
// count that is a perfectly good 20 ms frame at 48 kHz is an ILLEGAL frame size at
// 8 kHz, and frame_size_select (:827) must reject it with exactly the code the C
// returns. This is the rejection surface the public API's validation sits on.
//
// It also pins the > 20 ms frames, where this port DELIBERATELY diverges: libopus
// splits them with the repacketizer (:1698) and go-opus returns OPUS_UNIMPLEMENTED.
// The guard is `frame_size > st->Fs/50`, so it has to move with the rate too.
func TestOpusencEncodeSampleRateFrameDomain(t *testing.T) {
	const (
		opusBadArg        = -1
		opusUnimplemented = -5
	)

	var sawBadArg, sawUnimplemented, sawOK bool

	for _, fs := range encoderRates {
		// Every legal phase-4 duration, plus the illegal neighbours.
		type tc struct {
			frameSize int
			why       string
		}
		cases := []tc{
			{fs / 400, "2.5 ms"},
			{fs / 200, "5 ms"},
			{fs / 100, "10 ms"},
			{fs / 50, "20 ms"},
			{fs/400 - 1, "below Fs/400: :827 rejects"},
			{fs/50 + 1, "not a legal duration"},
			{fs/100 + 1, "not a legal duration"},
			{fs/50 - 1, "not a legal duration"},
			{960, "a 48 kHz 20 ms frame, offered at every rate"},
			{480, "a 48 kHz 10 ms frame, offered at every rate"},
			{120, "a 48 kHz 2.5 ms frame, offered at every rate"},
		}
		// The multiframe durations (40/60/80/100/120 ms), which C codes and this
		// port rejects at :1698.
		for _, mult := range []int{2, 3, 4, 5, 6} {
			cases = append(cases, tc{mult * fs / 50, fmt.Sprintf("%d x 20 ms: multiframe", mult)})
		}

		done := map[int]bool{}
		for _, c := range cases {
			if c.frameSize <= 0 || done[c.frameSize] {
				continue
			}
			done[c.frameSize] = true

			name := fmt.Sprintf("fs%d/n%d", fs, c.frameSize)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(2)
				cfg.Fs = fs
				cfg.Bitrate = 64000 // ample, so a long frame is NOT degenerate
				ch, err := cOpusencCreate(cfg)
				if err != nil {
					t.Fatalf("cOpusencCreate: %v", err)
				}
				defer ch.Close()
				g := goEncoderMatchingCfg(t, cfg)

				pcm := testPCM(c.frameSize, 2, 3)
				cRet, cPkt, _ := ch.Encode(pcm, c.frameSize, 1500)

				gBuf := make([]byte, 1501)
				gRet := g.EncodeRaw(pcm, c.frameSize, gBuf, 1500)

				if gRet != cRet {
					// The ONE deliberate divergence: a frame LONGER than 20 ms at
					// this rate that is not starved enough to be degenerate reaches
					// the multiframe split at :1698, which C codes and this port
					// defers. Anything else is a real failure.
					if c.frameSize > fs/50 && cRet > 0 && gRet == opusUnimplemented {
						sawUnimplemented = true
						return
					}
					t.Fatalf("%s (%s): return value: Go = %d, C = %d", name, c.why, gRet, cRet)
				}
				switch {
				case cRet == opusBadArg:
					sawBadArg = true
				case cRet > 0:
					sawOK = true
					for i := range cPkt {
						if gBuf[i] != cPkt[i] {
							t.Fatalf("%s (%s): packet byte %d of %d: Go = 0x%02x, C = 0x%02x",
								name, c.why, i, len(cPkt), gBuf[i], cPkt[i])
						}
					}
				}
			})
		}
	}

	switch {
	case !sawOK:
		t.Fatal("non-vacuity: no legal frame size was ever coded")
	case !sawBadArg:
		t.Fatal("non-vacuity: no illegal frame size was ever REJECTED with OPUS_BAD_ARG")
	case !sawUnimplemented:
		t.Fatal("non-vacuity: the deferred multiframe path was never reached")
	}
}

// TestOpusencEncodeSampleRateComplexity sweeps complexity 0..10 at every rate, on
// the shortest and longest frame. Complexity moves the CELT analysis (the pitch
// filter below 5, the TF/spreading search) AND equiv_rate, and CELT is running the
// upsampled signal, so this is where an upsample-dependent branch in the analysis
// would show up.
func TestOpusencEncodeSampleRateComplexity(t *testing.T) {
	for _, fs := range encoderRates {
		if fs == 48000 {
			continue // already covered by TestOpusencEncodeComplexity
		}
		for _, complexity := range []int{0, 3, 5, 8, 10} {
			for _, channels := range []int{1, 2} {
				for _, frameSize := range []int{fs / 400, fs / 50} {
					name := fmt.Sprintf("fs%d/cx%d/c%d/n%d", fs, complexity, channels, frameSize)
					t.Run(name, func(t *testing.T) {
						cfg := defaultOpusencCfg(channels)
						cfg.Fs = fs
						cfg.Complexity = complexity
						cfg.Bitrate = 24000
						p := newEncPair(t, cfg)
						for i := 0; i < 6; i++ {
							p.frame(name, testPCM(frameSize, channels, i), frameSize, 1500)
						}
					})
				}
			}
		}
	}
}

// TestOpusencEncodeSampleRateLongSequence is the endurance case below 48 kHz: 60
// frames on one encoder pair with the rate ramping under it, so any slow drift in
// the delay ring, hp_mem, the CELT energy history or the VBR reservoir has room to
// show up at a rate where the buffers are short.
func TestOpusencEncodeSampleRateLongSequence(t *testing.T) {
	for _, fs := range encoderRates {
		if fs == 48000 {
			continue // already covered by TestOpusencEncodeLongSequence
		}
		frameSize := fs / 50 // 20 ms
		for _, channels := range []int{1, 2} {
			name := fmt.Sprintf("fs%d/c%d", fs, channels)
			t.Run(name, func(t *testing.T) {
				cfg := defaultOpusencCfg(channels)
				cfg.Fs = fs
				cfg.VBR = 1
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
