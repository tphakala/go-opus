package opus

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

// genMSPCM builds nFrames*frameSize samples per channel of interleaved int16 test
// audio (a detuned sinusoid per channel), enough to make the encoder produce
// non-trivial packets.
func genMSPCM(nFrames, frameSize, channels int) []int16 {
	total := nFrames * frameSize
	out := make([]int16, total*channels)
	for n := range total {
		for c := range channels {
			f := 220.0 + 55.0*float64(c)
			v := 8000.0 * math.Sin(2*math.Pi*f*float64(n)/48000.0)
			out[n*channels+c] = int16(v)
		}
	}
	return out
}

// TestMultistreamDecoderBadArgs pins the constructor's argument validation.
func TestMultistreamDecoderBadArgs(t *testing.T) {
	cases := []struct {
		name                 string
		sampleRate, channels int
		streams, coupled     int
		mapping              []byte
	}{
		{"bad-sample-rate", 44100, 2, 1, 1, []byte{0, 1}},
		{"zero-channels", 48000, 0, 1, 0, []byte{}},
		{"too-many-channels", 48000, 256, 1, 0, make([]byte, 256)},
		{"zero-streams", 48000, 2, 0, 0, []byte{0, 1}},
		{"coupled-exceeds-streams", 48000, 2, 1, 2, []byte{0, 1}},
		{"negative-coupled", 48000, 2, 1, -1, []byte{0, 1}},
		{"streams-plus-coupled-overflow", 48000, 2, 200, 100, []byte{0, 1}},
		{"mapping-wrong-length", 48000, 2, 1, 1, []byte{0}},
		{"mapping-out-of-range", 48000, 2, 1, 1, []byte{0, 5}}, // maxChan=2, 5 invalid
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewMultistreamDecoder(tc.sampleRate, tc.channels, tc.streams, tc.coupled, tc.mapping)
			if !errors.Is(err, ErrBadArg) {
				t.Fatalf("want ErrBadArg, got %v", err)
			}
		})
	}
}

// TestMultistreamDecoderValidLayouts checks that legitimate layouts, including a
// muted channel (255), construct without error.
func TestMultistreamDecoderValidLayouts(t *testing.T) {
	cases := []struct {
		channels, streams, coupled int
		mapping                    []byte
	}{
		{2, 1, 1, []byte{0, 1}},
		{1, 1, 0, []byte{0}},
		{6, 4, 2, []byte{0, 4, 1, 2, 3, 5}},
		{3, 1, 1, []byte{0, 1, 255}}, // one muted channel
	}
	for _, tc := range cases {
		d, err := NewMultistreamDecoder(48000, tc.channels, tc.streams, tc.coupled, tc.mapping)
		if err != nil {
			t.Fatalf("ch%d: unexpected error: %v", tc.channels, err)
		}
		if d.Channels() != tc.channels || d.Streams() != tc.streams {
			t.Fatalf("ch%d: got channels=%d streams=%d", tc.channels, d.Channels(), d.Streams())
		}
	}
}

// TestMultistreamSingleStreamMatchesDecoder is an end-to-end, cgo-free check that
// a one-stream multistream decoder reproduces the single-stream Decoder exactly:
// a single-stream multistream packet is byte-identical to a plain Opus packet, so
// the two decoders must agree on PCM and final range packet for packet.
func TestMultistreamSingleStreamMatchesDecoder(t *testing.T) {
	const (
		frameSize = 960
		nFrames   = 8
	)
	cases := []struct {
		name     string
		channels int
		coupled  int
		mapping  []byte
	}{
		{"stereo", 2, 1, []byte{0, 1}},
		{"mono", 1, 0, []byte{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: tc.channels, Bitrate: 64000 * tc.channels})
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			dec, err := NewDecoder(48000, tc.channels)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			msDec, err := NewMultistreamDecoder(48000, tc.channels, 1, tc.coupled, tc.mapping)
			if err != nil {
				t.Fatalf("NewMultistreamDecoder: %v", err)
			}

			pcm := genMSPCM(nFrames, frameSize, tc.channels)
			encBuf := make([]byte, 4000)
			for i := range nFrames {
				frame := pcm[i*frameSize*tc.channels : (i+1)*frameSize*tc.channels]
				n, err := enc.Encode(frame, encBuf)
				if err != nil {
					t.Fatalf("frame %d: Encode: %v", i, err)
				}
				pkt := encBuf[:n]

				aBuf := make([]int16, frameSize*tc.channels)
				bBuf := make([]int16, frameSize*tc.channels)
				aN, err := dec.Decode(pkt, aBuf)
				if err != nil {
					t.Fatalf("frame %d: Decoder.Decode: %v", i, err)
				}
				bN, err := msDec.Decode(pkt, bBuf)
				if err != nil {
					t.Fatalf("frame %d: MultistreamDecoder.Decode: %v", i, err)
				}
				if aN != bN {
					t.Fatalf("frame %d: sample count Decoder=%d MS=%d", i, aN, bN)
				}
				for j := range aN * tc.channels {
					if aBuf[j] != bBuf[j] {
						t.Fatalf("frame %d: PCM mismatch at %d: Decoder=%d MS=%d", i, j, aBuf[j], bBuf[j])
					}
				}
				if dec.FinalRange() != msDec.FinalRange() {
					t.Fatalf("frame %d: final-range mismatch Decoder=0x%08x MS=0x%08x", i, dec.FinalRange(), msDec.FinalRange())
				}
				if msDec.LastPacketDuration() != bN {
					t.Fatalf("frame %d: LastPacketDuration=%d want %d", i, msDec.LastPacketDuration(), bN)
				}
			}
		})
	}
}

// TestMultistreamDecoderPLC checks that a nil/empty packet requests packet-loss
// concealment and returns a full frame without error.
func TestMultistreamDecoderPLC(t *testing.T) {
	const frameSize = 960
	msDec, err := NewMultistreamDecoder(48000, 2, 1, 1, []byte{0, 1})
	if err != nil {
		t.Fatalf("NewMultistreamDecoder: %v", err)
	}
	buf := make([]int16, frameSize*2)
	n, err := msDec.Decode(nil, buf)
	if err != nil {
		t.Fatalf("PLC decode: %v", err)
	}
	if n != frameSize {
		t.Fatalf("PLC returned %d samples, want %d", n, frameSize)
	}
}

// TestMultistreamDecoderBufferTooSmall checks that a packet whose duration exceeds
// the pcm buffer returns ErrBufferTooSmall before decoding.
func TestMultistreamDecoderBufferTooSmall(t *testing.T) {
	const frameSize = 960
	enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 128000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	msDec, err := NewMultistreamDecoder(48000, 2, 1, 1, []byte{0, 1})
	if err != nil {
		t.Fatalf("NewMultistreamDecoder: %v", err)
	}
	pcm := genMSPCM(1, frameSize, 2)
	encBuf := make([]byte, 4000)
	n, err := enc.Encode(pcm, encBuf)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// A 2-sample buffer (1 sample per channel) cannot hold a 960-sample frame.
	tiny := make([]int16, 2)
	if _, err := msDec.Decode(encBuf[:n], tiny); !errors.Is(err, ErrBufferTooSmall) {
		t.Fatalf("want ErrBufferTooSmall, got %v", err)
	}
}

// TestMultistreamDecoderFECMatchesDecoder pins the public DecodeFEC path: a
// one-stream multistream packet is a plain Opus packet, so DecodeFEC through the
// multistream decoder must reproduce the single-stream Decoder.DecodeFEC exactly
// (both take the same FEC-or-PLC branch). The public encoder does not emit in-band
// FEC, so these packets carry none and both decoders fall back to concealment for
// the requested duration; a real-FEC-recovery differential belongs in the refc
// oracle. This still covers the DecodeFEC method and its delegation to every
// sub-decoder byte-for-byte.
func TestMultistreamDecoderFECMatchesDecoder(t *testing.T) {
	const (
		frameSize = 960 // 20 ms, a multiple of 2.5 ms as DecodeFEC requires
		nFrames   = 6
	)
	enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 128000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	msDec, err := NewMultistreamDecoder(48000, 2, 1, 1, []byte{0, 1})
	if err != nil {
		t.Fatalf("NewMultistreamDecoder: %v", err)
	}

	pcm := genMSPCM(nFrames, frameSize, 2)
	encBuf := make([]byte, 4000)
	for i := range nFrames {
		frame := pcm[i*frameSize*2 : (i+1)*frameSize*2]
		n, err := enc.Encode(frame, encBuf)
		if err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
		pkt := encBuf[:n]

		aBuf := make([]int16, frameSize*2)
		bBuf := make([]int16, frameSize*2)
		aN, err := dec.DecodeFEC(pkt, aBuf)
		if err != nil {
			t.Fatalf("frame %d: Decoder.DecodeFEC: %v", i, err)
		}
		bN, err := msDec.DecodeFEC(pkt, bBuf)
		if err != nil {
			t.Fatalf("frame %d: MultistreamDecoder.DecodeFEC: %v", i, err)
		}
		if aN != bN {
			t.Fatalf("frame %d: sample count Decoder=%d MS=%d", i, aN, bN)
		}
		for j := range aN * 2 {
			if aBuf[j] != bBuf[j] {
				t.Fatalf("frame %d: PCM mismatch at %d: Decoder=%d MS=%d", i, j, aBuf[j], bBuf[j])
			}
		}
		if dec.FinalRange() != msDec.FinalRange() {
			t.Fatalf("frame %d: final-range mismatch Decoder=0x%08x MS=0x%08x", i, dec.FinalRange(), msDec.FinalRange())
		}
	}
}

// FuzzMultistreamDecode asserts the decoder never panics on hostile input, the
// documented robustness contract: arbitrary bytes must yield a value or an error,
// never a crash. It uses a one-stream stereo layout so the seeded real Opus packet
// is a valid multistream packet that reaches the decode core (decodeStream, the
// coupled scatter, buffer sizing), letting mutations explore near a real bitstream
// rather than only the early length/parse guards.
func FuzzMultistreamDecode(f *testing.F) {
	const channels = 2
	if enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: channels, Bitrate: 96000}); err == nil {
		seedBuf := make([]byte, 4000)
		if n, err := enc.Encode(genMSPCM(1, 960, channels), seedBuf); err == nil {
			f.Add(bytes.Clone(seedBuf[:n]))
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Add([]byte{0x0c, 0x01, 0x02, 0x03, 0x04})

	msDec, err := NewMultistreamDecoder(48000, channels, 1, 1, []byte{0, 1})
	if err != nil {
		f.Fatalf("NewMultistreamDecoder: %v", err)
	}
	buf := make([]int16, 5760*channels)
	f.Fuzz(func(t *testing.T, data []byte) {
		// Reset per input so each iteration is independent of prior packets:
		// testing.F requires the fuzz body not to depend on shared state, and a
		// crash must reproduce from its single saved corpus entry. This also
		// exercises Reset.
		msDec.Reset()
		// Must not panic. On success the returned per-channel count must fit the
		// buffer a caller would trust.
		n, err := msDec.Decode(data, buf)
		if err == nil && (n < 0 || n*channels > len(buf)) {
			t.Fatalf("Decode returned n=%d (channels=%d, buf=%d): out of range", n, channels, len(buf))
		}
	})
}
