//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

// CP10: THE PUBLIC ENCODER GATE.
//
// internal/opusenc is already pinned against the C, frame for frame, at every rate
// (opusenc_encode_test.go, opusenc_samplerate_test.go). What those tests do NOT
// cover is the public opus.Encoder, which is where a real caller enters: it is the
// thing that chooses the application, forces the mode, resolves the zero values and
// hands the frame size down. A wrapper that sets one ctl differently from the gated
// configuration produces a perfectly valid Opus stream that is simply NOT the one
// libopus would have produced, and no test below this file would say so.
//
// So this file re-runs the differential ONE LEVEL UP: opus.NewEncoder(cfg) against
// a C encoder configured the way that cfg is DOCUMENTED to configure it. cCfgFor is
// the executable statement of that contract.

// cCfgFor is the C-side mirror of what opus.NewEncoder is documented to do with an
// EncoderConfig. Every line of it is a claim about the public constructor:
//
//   - OPUS_APPLICATION_AUDIO, and never VOIP or RESTRICTED_LOWDELAY (the
//     application decides whether the delay-compensation ring exists at all);
//   - OPUS_SET_FORCE_MODE(MODE_CELT_ONLY), the v1 scope;
//   - Bitrate 0 -> OPUS_AUTO;
//   - Complexity 0 -> 10, which is NOT the libopus default of 9;
//   - CBR -> OPUS_SET_VBR(0), ConstrainedVBR -> OPUS_SET_VBR_CONSTRAINT;
//   - every other ctl left at its opus_encoder_init default.
//
// If the public constructor and this function ever disagree, the sweep below fails
// on the first packet.
func cCfgFor(cfg opus.EncoderConfig) cOpusencCfg {
	c := defaultOpusencCfg(cfg.Channels)
	c.Fs = cfg.SampleRate
	c.Application = oAppAudio
	c.ForceMode = oModeCeltOnly

	c.Bitrate = oOpusAuto
	if cfg.Bitrate > 0 {
		c.Bitrate = cfg.Bitrate
	}

	c.Complexity = cfg.Complexity
	if cfg.Complexity == 0 {
		c.Complexity = 10 // the go-opus default; libopus' own is 9
	}

	c.VBR = 1
	if cfg.CBR {
		c.VBR = 0
	}
	c.VBRConstraint = 0
	if cfg.ConstrainedVBR {
		c.VBRConstraint = 1
	}
	return c
}

// pubEncPair drives one public Go encoder and one C encoder configured through
// cCfgFor, and asserts total agreement on every frame: the packet bytes, the packet
// length, and OPUS_GET_FINAL_RANGE.
type pubEncPair struct {
	t   *testing.T
	c   *cOpusencHandle
	g   *opus.Encoder
	cfg opus.EncoderConfig
	buf []byte
}

func newPubEncPair(t *testing.T, cfg opus.EncoderConfig) *pubEncPair {
	t.Helper()
	ch, err := cOpusencCreate(cCfgFor(cfg))
	if err != nil {
		t.Fatalf("cOpusencCreate: %v", err)
	}
	t.Cleanup(ch.Close)
	g, err := opus.NewEncoder(cfg)
	if err != nil {
		t.Fatalf("opus.NewEncoder(%+v): %v", cfg, err)
	}
	return &pubEncPair{t: t, c: ch, g: g, cfg: cfg, buf: make([]byte, 1500)}
}

// frame encodes one frame on both encoders and compares everything observable.
func (p *pubEncPair) frame(where string, pcm []int16, frameSize int) []byte {
	p.t.Helper()

	// The C is handed the SAME byte budget the Go encoder gets, because
	// max_data_bytes is an input to the rate chain (opus_encoder.c:745), not just an
	// output bound.
	cRet, cPkt, cState := p.c.Encode(pcm, frameSize, len(p.buf))
	gRet, gErr := p.g.Encode(pcm, p.buf)

	if cRet < 0 {
		p.t.Fatalf("%s: the C encoder failed with %d; the Go encoder returned (%d, %v)",
			where, cRet, gRet, gErr)
	}
	if gErr != nil {
		p.t.Fatalf("%s: Go Encode: %v (C returned %d bytes)", where, gErr, cRet)
	}
	if gRet != cRet {
		p.t.Fatalf("%s: packet length: Go %d, C %d", where, gRet, cRet)
	}
	gPkt := p.buf[:gRet]
	if !bytes.Equal(gPkt, cPkt) {
		for i := range gPkt {
			if gPkt[i] != cPkt[i] {
				p.t.Fatalf("%s: packet byte %d of %d: Go %#02x, C %#02x",
					where, i, gRet, gPkt[i], cPkt[i])
			}
		}
	}
	if got, want := p.g.FinalRange(), cState.RangeFinal; got != want {
		p.t.Fatalf("%s: FinalRange: Go %#x, C %#x", where, got, want)
	}
	return bytes.Clone(gPkt)
}

// TestOpusPublicEncoderMatchesC IS THE CP10 PUBLIC-API GATE: every rate the API
// accepts x mono/stereo x the four frame durations it codes x {CBR, VBR,
// constrained VBR} x {automatic, 16, 64, 128 kbps} x complexity {0 (the default), 1,
// 5, 10}, eight frames each, byte for byte against libopus.
//
// The complexity-0 row is the one that cannot be moved: it is the only place the
// zero-value -> 10 mapping is checked against the C rather than against another Go
// encoder.
func TestOpusPublicEncoderMatchesC(t *testing.T) {
	rateModes := []struct {
		name           string
		cbr, constrain bool
	}{
		{"vbr", false, false},
		{"cvbr", false, true},
		{"cbr", true, false},
	}

	for _, fs := range encoderRates {
		for _, channels := range []int{1, 2} {
			for _, frameSize := range frameSizesAt(fs) {
				for _, rm := range rateModes {
					for _, bitrate := range []int{0, 16000, 64000, 128000} {
						for _, complexity := range []int{0, 1, 5, 10} {
							name := fmt.Sprintf("fs%d/c%d/n%d/%s/%dbps/cx%d",
								fs, channels, frameSize, rm.name, bitrate, complexity)
							t.Run(name, func(t *testing.T) {
								t.Parallel()
								cfg := opus.EncoderConfig{
									SampleRate:     fs,
									Channels:       channels,
									Bitrate:        bitrate,
									CBR:            rm.cbr,
									ConstrainedVBR: rm.constrain,
									Complexity:     complexity,
								}
								p := newPubEncPair(t, cfg)
								for i := 0; i < 8; i++ {
									pcm := testPCM(frameSize, channels, i)
									if i == 5 || i == 6 {
										pcm = zerosPCM(frameSize, channels) // CELT's silence path
									}
									p.frame(name, pcm, frameSize)
								}
							})
						}
					}
				}
			}
		}
	}
}

// TestOpusPublicEncoderComplexityZeroIsTenVsC is the same claim as the pure-Go
// TestEncoderComplexityZeroValueIsTen, made against the C instead of against
// another Go encoder, and it is the sharper of the two: it fixes WHICH C encoder
// the zero value is supposed to equal.
//
// The non-vacuity guard is the second half. Complexity 9 (the LIBOPUS default, i.e.
// exactly the value the zero value would silently fall back to if opus.NewEncoder
// ever stopped issuing OPUS_SET_COMPLEXITY) must produce DIFFERENT bytes at this
// configuration. It only does at some configurations: 9 and 10 differ solely through
// compute_equiv_rate's (90+complexity)/100 (opus_encoder.c), a 1% shift in the
// equivalent rate, and CELT's own thresholds top out at complexity >= 8. 24 kbps
// stereo 20 ms at 48 kHz is a point where the 1% crosses a decision boundary.
func TestOpusPublicEncoderComplexityZeroIsTenVsC(t *testing.T) {
	cfg := opus.EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 24000} // Complexity: 0
	const frameSize, nFrames = 960, 8

	// Go zero value == C complexity 10, byte for byte.
	if got := cCfgFor(cfg).Complexity; got != 10 {
		t.Fatalf("cCfgFor: complexity %d, want 10", got)
	}
	p := newPubEncPair(t, cfg)
	want := make([][]byte, nFrames)
	for i := 0; i < nFrames; i++ {
		want[i] = p.frame("cx0-vs-C10", testPCM(frameSize, 2, i), frameSize)
	}

	// Non-vacuity: a C encoder at complexity 9 must NOT agree, or the equality above
	// proves nothing about the mapping.
	c9 := cCfgFor(cfg)
	c9.Complexity = 9
	h, err := cOpusencCreate(c9)
	if err != nil {
		t.Fatalf("cOpusencCreate(complexity 9): %v", err)
	}
	defer h.Close()

	differs := false
	for i := 0; i < nFrames; i++ {
		ret, pkt, _ := h.Encode(testPCM(frameSize, 2, i), frameSize, 1500)
		if ret <= 0 {
			t.Fatalf("C encode at complexity 9: %d", ret)
		}
		if !bytes.Equal(pkt, want[i]) {
			differs = true
		}
	}
	if !differs {
		t.Fatal("non-vacuity: the C encoder at complexity 9 produced the same packets as the " +
			"public encoder's zero value over 8 frames, so this test cannot tell 10 from 9 " +
			"(pick a configuration where compute_equiv_rate's 1% shift crosses a boundary)")
	}
}

// TestOpusPublicEncoderLookaheadMatchesC pins opus.Encoder.Lookahead against the C
// OPUS_GET_LOOKAHEAD ctl at every rate, and with it the 48 kHz pre-skip the Ogg
// container writes into OpusHead. NOTHING IN A PACKET DEPENDS ON THIS VALUE, so the
// sweep above is completely blind to it; it has already been wrong once in this repo
// (192 instead of 312, the Fs/400 term missing), and the only thing that catches
// that is an assertion like this one.
func TestOpusPublicEncoderLookaheadMatchesC(t *testing.T) {
	for _, fs := range encoderRates {
		for _, channels := range []int{1, 2} {
			e, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: fs, Channels: channels})
			if err != nil {
				t.Fatalf("opus.NewEncoder(fs=%d, ch=%d): %v", fs, channels, err)
			}
			want := int(cTopencLookahead(int32(fs), channels, oAppAudio))
			if got := e.Lookahead(); got != want {
				t.Errorf("fs=%d ch=%d: Lookahead() = %d, want %d (C OPUS_GET_LOOKAHEAD)",
					fs, channels, got, want)
			}
			// RFC 7845 section 5.1: the pre-skip is expressed at 48 kHz whatever the
			// coding rate. Both of the C's terms (Fs/400 and Fs/250) are proportional
			// to Fs and divide exactly, so this is 312 at every legal rate.
			if got := want * 48000 / fs; got != 312 {
				t.Errorf("fs=%d: the 48 kHz pre-skip derived from the C lookahead is %d, want 312",
					fs, got)
			}
		}
	}
}
