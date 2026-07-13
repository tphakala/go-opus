package opus

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
)

// This is the PUBLIC-API test for the encoder: it runs with no cgo and no build
// tag, so it is part of the default test job. It does NOT try to prove
// bit-exactness (that is the differential gate's job, against the C oracle under
// -tags refc). It proves the things the differential gate CANNOT see, because they
// never reach a packet byte:
//
//   - the accept/reject domain of NewEncoder, which is the contract that the
//     encoder never accepts a configuration it cannot code;
//   - the frame-duration domain, including the > 20 ms rejection;
//   - that Complexity's ZERO VALUE really is 10 and not the C default of 9;
//   - Lookahead, and the 48 kHz pre-skip a container derives from it;
//   - Reset's semantics, asserted on PACKETS rather than on state;
//   - that what Encode returns is a packet this package's own parser accepts and
//     this package's own Decoder decodes.

// encTestPCM is a deterministic, non-degenerate signal: a swept tone plus two
// fixed partials, so the transient, prefilter and bandwidth decisions are actually
// exercised rather than sitting on the silent-frame fast path. The channels are
// decorrelated so the stereo decisions are non-trivial. fs scales the sweep so the
// signal occupies the same fraction of the band at every rate.
func encTestPCM(n, channels, fs, seed int) []int16 {
	pcm := make([]int16, n*channels)
	phase := float64(seed) * 0.37
	for i := range n {
		t := float64(i) + float64(seed)*float64(n)
		f := float64(fs)
		v := 0.45*math.Sin(2*math.Pi*(0.0092*f+0.05*t)*t/f+phase) +
			0.18*math.Sin(2*math.Pi*0.065*f*t/f) +
			0.07*math.Sin(2*math.Pi*0.23*f*t/f)
		for c := range channels {
			s := int16(v * 22000)
			if c == 1 {
				s = int16(v*19000) - int16(v*v*3000)
			}
			pcm[i*channels+c] = s
		}
	}
	return pcm
}

// encodeSeq encodes nFrames frames of encTestPCM through one Encoder and returns
// the packets and the final range after each one. It is the workhorse of the
// tests below that compare two configurations.
func encodeSeq(t *testing.T, cfg EncoderConfig, frameSize, nFrames int) (pkts [][]byte, ranges []uint32) {
	t.Helper()
	e, err := NewEncoder(cfg)
	if err != nil {
		t.Fatalf("NewEncoder(%+v): %v", cfg, err)
	}
	pkts = make([][]byte, nFrames)
	ranges = make([]uint32, nFrames)
	buf := make([]byte, 1500)
	for i := range nFrames {
		pcm := encTestPCM(frameSize, cfg.Channels, cfg.SampleRate, i)
		n, err := e.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("Encode frame %d: %v", i, err)
		}
		if n <= 0 {
			t.Fatalf("Encode frame %d returned %d bytes", i, n)
		}
		pkts[i] = bytes.Clone(buf[:n])
		ranges[i] = e.FinalRange()
	}
	return pkts, ranges
}

// encRates are the five sample rates the encoder is bit-exact at, and therefore
// the five NewEncoder must accept.
var encRates = []int{8000, 12000, 16000, 24000, 48000}

func TestNewEncoderSampleRateDomain(t *testing.T) {
	for _, fs := range encRates {
		for _, ch := range []int{1, 2} {
			e, err := NewEncoder(EncoderConfig{SampleRate: fs, Channels: ch})
			if err != nil {
				t.Errorf("NewEncoder(fs=%d, ch=%d): unexpected error %v", fs, ch, err)
				continue
			}
			if e == nil {
				t.Errorf("NewEncoder(fs=%d, ch=%d): nil encoder, nil error", fs, ch)
			}
		}
	}

	// Everything else is REJECTED, not resampled and not silently accepted. 44100
	// and 32000 are the traps: they are the rates a caller is most likely to have,
	// and libopus does not code them either.
	for _, fs := range []int{0, -48000, 1, 4000, 11025, 22050, 32000, 44100, 47999, 48001, 96000} {
		e, err := NewEncoder(EncoderConfig{SampleRate: fs, Channels: 1})
		if !errors.Is(err, ErrBadArg) {
			t.Errorf("NewEncoder(fs=%d): error %v, want ErrBadArg", fs, err)
		}
		if e != nil {
			t.Errorf("NewEncoder(fs=%d): non-nil encoder on error", fs)
		}
	}
}

func TestNewEncoderChannelDomain(t *testing.T) {
	for _, ch := range []int{-1, 0, 3, 8} {
		_, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: ch})
		if !errors.Is(err, ErrBadArg) {
			t.Errorf("NewEncoder(channels=%d): error %v, want ErrBadArg", ch, err)
		}
	}
}

func TestNewEncoderComplexityDomain(t *testing.T) {
	// 0 (the default) through 10 are accepted.
	for c := 0; c <= 10; c++ {
		if _, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 1, Complexity: c}); err != nil {
			t.Errorf("NewEncoder(complexity=%d): unexpected error %v", c, err)
		}
	}
	for _, c := range []int{-1, 11, 100} {
		_, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 1, Complexity: c})
		if !errors.Is(err, ErrBadArg) {
			t.Errorf("NewEncoder(complexity=%d): error %v, want ErrBadArg", c, err)
		}
	}
}

// TestEncoderComplexityZeroValueIsTen is the assertion the packet gate cannot make
// for us. EncoderConfig.Complexity's zero value is documented as "the default,
// which is 10"; libopus' own default is 9 (opus_encoder.c:270), so if NewEncoder
// ever stopped issuing an explicit OPUS_SET_COMPLEXITY the zero value would
// silently become 9 and every packet would change with no test to say so.
//
// The proof is differential IN GO: complexity 0 must produce the same bytes as
// complexity 10, and it must NOT produce the same bytes as 9 or as 1. The last two
// are the non-vacuity guards: without them, a build in which complexity did
// nothing at all would pass the first assertion.
//
// THE CONFIGURATION IS CHOSEN, NOT ARBITRARY. Complexity 9 and 10 differ only
// through compute_equiv_rate's equiv = equiv*(90+complexity)/100
// (opus_encoder.c:248), a 1% shift in the equivalent rate; inside CELT the highest
// complexity threshold is >= 8 (bands.c's theta RDO), so 8, 9 and 10 take
// identical CELT branches. That 1% is only visible where it crosses a decision
// boundary in the rate chain, which it does NOT at, say, 64 kbps stereo: 9 and 10
// are byte-identical there. 24 kbps stereo 20 ms at 48 kHz is a point where it
// does. Move this configuration and the non-vacuity guard below can go quiet.
func TestEncoderComplexityZeroValueIsTen(t *testing.T) {
	const frameSize, nFrames = 960, 8
	base := EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 24000}

	cfgWith := func(c int) EncoderConfig {
		cfg := base
		cfg.Complexity = c
		return cfg
	}

	zero, zeroRanges := encodeSeq(t, cfgWith(0), frameSize, nFrames)
	ten, tenRanges := encodeSeq(t, cfgWith(10), frameSize, nFrames)
	for i := range zero {
		if !bytes.Equal(zero[i], ten[i]) {
			t.Fatalf("frame %d: Complexity 0 packet (%d bytes) != Complexity 10 packet (%d bytes); "+
				"the zero value must map to the default, 10", i, len(zero[i]), len(ten[i]))
		}
		if zeroRanges[i] != tenRanges[i] {
			t.Fatalf("frame %d: Complexity 0 final range %#x != Complexity 10 final range %#x",
				i, zeroRanges[i], tenRanges[i])
		}
	}

	// Non-vacuity: complexity has to be OBSERVABLE, or the equality above is empty.
	// 9 is libopus' default, and it is the value the zero value would land on if the
	// explicit ctl were dropped, so it is the one that matters most.
	for _, c := range []int{1, 9} {
		other, _ := encodeSeq(t, cfgWith(c), frameSize, nFrames)
		differs := false
		for i := range zero {
			if !bytes.Equal(zero[i], other[i]) {
				differs = true
				break
			}
		}
		if !differs {
			t.Fatalf("non-vacuity: Complexity %d produced the same packets as Complexity 0 over "+
				"%d frames, so this test cannot tell 0 from 10", c, nFrames)
		}
	}
}

func TestNewEncoderBitrateDomain(t *testing.T) {
	// The zero value is automatic (OPUS_AUTO), and it encodes.
	if _, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatalf("NewEncoder with Bitrate 0 (automatic): %v", err)
	}
	// A negative bitrate is a mistake, not a sentinel: OPUS_AUTO is spelled 0 here.
	for _, br := range []int{-1, -1000} {
		_, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: br})
		if !errors.Is(err, ErrBadArg) {
			t.Errorf("NewEncoder(bitrate=%d): error %v, want ErrBadArg", br, err)
		}
	}
	// Out-of-range positive bitrates are CLAMPED, exactly as OPUS_SET_BITRATE
	// clamps them, and still encode.
	for _, br := range []int{1, 500, 6000, 510000, 2_000_000} {
		cfg := EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: br}
		e, err := NewEncoder(cfg)
		if err != nil {
			t.Fatalf("NewEncoder(bitrate=%d): %v", br, err)
		}
		buf := make([]byte, 4000)
		n, err := e.Encode(encTestPCM(960, 2, 48000, 0), buf)
		if err != nil {
			t.Fatalf("Encode at bitrate %d: %v", br, err)
		}
		if n <= 0 {
			t.Fatalf("Encode at bitrate %d returned %d bytes", br, n)
		}
	}
}

// TestNewEncoderRejectsDTX pins the deferral. DTX is NOT implemented; the config
// field exists because the design names it, and asking for it must fail loudly
// rather than produce a stream that is silently not using DTX.
func TestNewEncoderRejectsDTX(t *testing.T) {
	_, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 1, DTX: true})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewEncoder(DTX=true): error %v, want ErrUnsupported", err)
	}
	// ErrUnsupported is its own sentinel and is deliberately NOT ErrBadArg: the
	// configuration is legal Opus, this build just cannot do it.
	if errors.Is(err, ErrBadArg) {
		t.Errorf("NewEncoder(DTX=true): error also matches ErrBadArg; the two must stay distinct")
	}
}

// TestEncodeFrameDurationDomain sweeps the whole frame-size domain at every rate:
// the four durations this release codes, the five longer ones Opus defines and
// this release refuses, and lengths that are not Opus durations at all.
func TestEncodeFrameDurationDomain(t *testing.T) {
	for _, fs := range encRates {
		for _, ch := range []int{1, 2} {
			t.Run(fmt.Sprintf("fs%d/c%d", fs, ch), func(t *testing.T) {
				e, err := NewEncoder(EncoderConfig{SampleRate: fs, Channels: ch, Bitrate: 64000})
				if err != nil {
					t.Fatalf("NewEncoder: %v", err)
				}
				buf := make([]byte, 1500)

				// 2.5, 5, 10 and 20 ms all code.
				for _, n := range []int{fs / 400, fs / 200, fs / 100, fs / 50} {
					got, err := e.Encode(encTestPCM(n, ch, fs, 0), buf)
					if err != nil {
						t.Errorf("Encode(%d samples/channel): %v", n, err)
						continue
					}
					if got <= 0 {
						t.Errorf("Encode(%d samples/channel) returned %d bytes", n, got)
					}
				}

				// 40, 60, 80, 100 and 120 ms are legal Opus durations that this
				// release does not code. They must be REFUSED, not mis-coded.
				for _, n := range []int{fs / 25, 3 * fs / 50, 4 * fs / 50, 5 * fs / 50, 6 * fs / 50} {
					_, err := e.Encode(encTestPCM(n, ch, fs, 0), buf)
					if !errors.Is(err, ErrUnsupported) {
						t.Errorf("Encode(%d samples/channel, > 20 ms): error %v, want ErrUnsupported",
							n, err)
					}
					if errors.Is(err, ErrBadArg) {
						t.Errorf("Encode(%d samples/channel): error matches ErrBadArg; a legal-but-"+
							"unimplemented duration is ErrUnsupported, not ErrBadArg", n)
					}
				}

				// Not an Opus frame duration at any rate.
				for _, n := range []int{1, 7, fs/400 - 1, fs/400 + 1, fs/50 + 1, 3 * fs / 100} {
					_, err := e.Encode(encTestPCM(n, ch, fs, 0), buf)
					if !errors.Is(err, ErrBadArg) {
						t.Errorf("Encode(%d samples/channel): error %v, want ErrBadArg", n, err)
					}
				}

				// An empty frame, and (for stereo) a length that is not a whole
				// number of frames.
				if _, err := e.Encode(nil, buf); !errors.Is(err, ErrBadArg) {
					t.Errorf("Encode(nil pcm): error %v, want ErrBadArg", err)
				}
				if ch == 2 {
					odd := make([]int16, fs/50*ch-1)
					if _, err := e.Encode(odd, buf); !errors.Is(err, ErrBadArg) {
						t.Errorf("Encode(len(pcm) not a multiple of channels): error %v, want ErrBadArg",
							err)
					}
				}
			})
		}
	}
}

// TestEncodeBufferTooSmall pins the ONE fatal buffer condition. Everything else is
// adaptive: libopus lowers the bitrate to fit the buffer it is given, so a small
// buffer costs quality and is not an error. A buffer with no room at all cannot
// hold even a TOC byte, and that is ErrBufferTooSmall.
func TestEncodeBufferTooSmall(t *testing.T) {
	e, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 96000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	pcm := encTestPCM(960, 2, 48000, 0)

	for _, buf := range [][]byte{nil, {}, make([]byte, 0, 64)} {
		if _, err := e.Encode(pcm, buf); !errors.Is(err, ErrBufferTooSmall) {
			t.Errorf("Encode into a zero-length buffer: error %v, want ErrBufferTooSmall", err)
		}
	}

	// The adaptive path: a buffer far too small for 96 kbps still produces a
	// packet, and the encoder never writes past it.
	for _, size := range []int{1, 2, 8, 40} {
		buf := make([]byte, size+8)
		for i := range buf {
			buf[i] = 0xAA
		}
		n, err := e.Encode(pcm, buf[:size])
		if err != nil {
			t.Fatalf("Encode into a %d-byte buffer: %v", size, err)
		}
		if n <= 0 || n > size {
			t.Fatalf("Encode into a %d-byte buffer returned %d bytes", size, n)
		}
		for i := size; i < len(buf); i++ {
			if buf[i] != 0xAA {
				t.Fatalf("Encode into a %d-byte buffer wrote past the end (byte %d)", size, i)
			}
		}
	}
}

// TestEncoderReset asserts Reset on PACKETS, not on state: a reset Encoder fed the
// same PCM must emit the same bytes and the same final ranges as a fresh one. The
// non-vacuity guard is the second half: WITHOUT the reset, the same PCM must
// produce DIFFERENT packets, which is what proves the encoder carries cross-frame
// state at all and that the first assertion is not trivially true.
func TestEncoderReset(t *testing.T) {
	const frameSize, nFrames = 960, 5
	cfg := EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 64000}

	e, err := NewEncoder(cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	buf := make([]byte, 1500)
	run := func() ([][]byte, []uint32) {
		t.Helper()
		pkts := make([][]byte, nFrames)
		ranges := make([]uint32, nFrames)
		for i := range nFrames {
			n, err := e.Encode(encTestPCM(frameSize, cfg.Channels, cfg.SampleRate, i), buf)
			if err != nil {
				t.Fatalf("Encode frame %d: %v", i, err)
			}
			pkts[i] = bytes.Clone(buf[:n])
			ranges[i] = e.FinalRange()
		}
		return pkts, ranges
	}

	first, firstRanges := run()

	// No reset: the encoder is now primed with five frames of history, so the same
	// input cannot give the same output.
	again, _ := run()
	same := true
	for i := range first {
		if !bytes.Equal(first[i], again[i]) {
			same = false
			break
		}
	}
	if same {
		t.Fatal("non-vacuity: re-encoding the same PCM without Reset gave identical packets, so " +
			"this test cannot tell whether Reset does anything")
	}

	e.Reset()
	after, afterRanges := run()
	for i := range first {
		if !bytes.Equal(first[i], after[i]) {
			t.Fatalf("frame %d after Reset: packet differs from the fresh-encoder packet "+
				"(%d bytes vs %d)", i, len(after[i]), len(first[i]))
		}
		if firstRanges[i] != afterRanges[i] {
			t.Fatalf("frame %d after Reset: final range %#x, want %#x",
				i, afterRanges[i], firstRanges[i])
		}
	}

	// Reset keeps the CONFIGURATION (OPUS_RESET_STATE clears only the reset
	// region), so a fresh encoder built from the same config agrees with it.
	fresh, freshRanges := encodeSeq(t, cfg, frameSize, nFrames)
	for i := range first {
		if !bytes.Equal(fresh[i], first[i]) || freshRanges[i] != firstRanges[i] {
			t.Fatalf("frame %d: a fresh encoder disagrees with the reset one", i)
		}
	}
}

// TestEncodeLookahead pins OPUS_GET_LOOKAHEAD at every rate, and with it the 48 kHz
// pre-skip a container derives from it. The value never reaches a packet byte, so
// the differential packet gate is blind to it; getting it wrong (192 instead of
// 312, which is exactly the bug this assertion exists to prevent) misaligns every
// decoded stream by 120 samples.
func TestEncodeLookahead(t *testing.T) {
	for _, fs := range encRates {
		for _, ch := range []int{1, 2} {
			e, err := NewEncoder(EncoderConfig{SampleRate: fs, Channels: ch})
			if err != nil {
				t.Fatalf("NewEncoder(fs=%d): %v", fs, err)
			}
			// Fs/400 (the CELT MDCT overlap) + Fs/250 (the delay-compensation ring).
			want := fs/400 + fs/250
			if got := e.Lookahead(); got != want {
				t.Errorf("fs=%d: Lookahead() = %d, want %d (Fs/400 + Fs/250)", fs, got, want)
			}
			// RFC 7845 defines the pre-skip at 48 kHz whatever the coding rate. Both
			// terms are proportional to Fs and divide exactly, so the answer is 312
			// at every rate.
			if got := e.Lookahead() * 48000 / fs; got != 312 {
				t.Errorf("fs=%d: pre-skip at 48 kHz = %d, want 312", fs, got)
			}
		}
	}
}

// TestEncodePacketParses closes the loop inside this package: what Encode returns
// must be a packet this package's own ParseTOC / PacketFrames / PacketDuration
// accept, and that its own Decoder decodes to the frame length that went in.
func TestEncodePacketParses(t *testing.T) {
	for _, fs := range encRates {
		for _, ch := range []int{1, 2} {
			for _, n := range []int{fs / 400, fs / 200, fs / 100, fs / 50} {
				name := fmt.Sprintf("fs%d/c%d/n%d", fs, ch, n)
				t.Run(name, func(t *testing.T) {
					cfg := EncoderConfig{SampleRate: fs, Channels: ch, Bitrate: 96000}
					e, err := NewEncoder(cfg)
					if err != nil {
						t.Fatalf("NewEncoder: %v", err)
					}
					dec, err := NewDecoder(fs, ch)
					if err != nil {
						t.Fatalf("NewDecoder: %v", err)
					}
					buf := make([]byte, 1500)
					out := make([]int16, n*ch)

					for i := range 3 {
						size, err := e.Encode(encTestPCM(n, ch, fs, i), buf)
						if err != nil {
							t.Fatalf("Encode frame %d: %v", i, err)
						}
						pkt := buf[:size]

						mode, _, dur, tocChannels := ParseTOC(pkt[0])
						if mode != ModeCELTOnly {
							t.Fatalf("frame %d: TOC mode = %v, want ModeCELTOnly (v1 forces it)", i, mode)
						}
						// A stereo encoder may code a mono stream when the rate is
						// too low; it may never code MORE channels than it was given.
						if tocChannels < 1 || tocChannels > ch {
							t.Fatalf("frame %d: TOC channels = %d, want 1..%d", i, tocChannels, ch)
						}
						// PacketDuration is in 48 kHz samples, whatever the coding rate.
						want48k := n * 48000 / fs
						if got, err := PacketDuration(pkt); err != nil || got != want48k {
							t.Fatalf("frame %d: PacketDuration = %d, %v; want %d, nil", i, got, err, want48k)
						}
						if got, err := PacketFrames(pkt); err != nil || got != 1 {
							t.Fatalf("frame %d: PacketFrames = %d, %v; want 1, nil", i, got, err)
						}
						// FrameDuration is in MICROSECONDS (FrameDuration2500us == 2500).
						wantUs := n * 1_000_000 / fs
						if int(dur) != wantUs {
							t.Fatalf("frame %d: TOC frame duration = %d us, want %d us",
								i, int(dur), wantUs)
						}

						got, err := dec.Decode(pkt, out)
						if err != nil {
							t.Fatalf("frame %d: decoding our own packet: %v", i, err)
						}
						if got != n {
							t.Fatalf("frame %d: decoded %d samples/channel, want %d", i, got, n)
						}
						if dec.LastPacketDuration() != n {
							t.Fatalf("frame %d: LastPacketDuration = %d, want %d",
								i, dec.LastPacketDuration(), n)
						}
					}
				})
			}
		}
	}
}

// TestEncodeRateModes checks that the three rate modes the config can express all
// encode, and that hard CBR really is constant: every 20 ms packet at 64 kbps is
// the same size. VBR is not asserted to VARY (a pathological signal need not make
// it vary), only to encode; the differential gate is what proves the VBR bytes are
// right.
func TestEncodeRateModes(t *testing.T) {
	const frameSize, nFrames = 960, 8
	for _, mode := range []struct {
		name           string
		cbr, constrain bool
	}{
		{"vbr", false, false},
		{"cvbr", false, true},
		{"cbr", true, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := EncoderConfig{
				SampleRate: 48000, Channels: 2, Bitrate: 64000,
				CBR: mode.cbr, ConstrainedVBR: mode.constrain,
			}
			pkts, _ := encodeSeq(t, cfg, frameSize, nFrames)
			if !mode.cbr {
				return
			}
			// 64000 bps * 20 ms = 1280 bits = 160 bytes, TOC included.
			const wantBytes = 64000 / 50 / 8
			for i, p := range pkts {
				if len(p) != wantBytes {
					t.Errorf("CBR frame %d: %d bytes, want %d", i, len(p), wantBytes)
				}
			}
		})
	}
}
