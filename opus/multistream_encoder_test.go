package opus

import (
	"bytes"
	"errors"
	"testing"
)

// TestMultistreamEncoderBadArgs pins the constructor's argument validation,
// including the encoder-only rules (streams+coupled must not exceed channels, and
// every stream must be referenced by a channel) that the decoder does not have.
func TestMultistreamEncoderBadArgs(t *testing.T) {
	cases := []struct {
		name string
		cfg  MultistreamEncoderConfig
	}{
		{"bad-sample-rate", MultistreamEncoderConfig{SampleRate: 44100, Channels: 2, Streams: 1, CoupledStreams: 1, Mapping: []byte{0, 1}}},
		{"zero-channels", MultistreamEncoderConfig{SampleRate: 48000, Channels: 0, Streams: 1, Mapping: []byte{}}},
		{"too-many-channels", MultistreamEncoderConfig{SampleRate: 48000, Channels: 256, Streams: 1, Mapping: make([]byte, 256)}},
		{"zero-streams", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 0, Mapping: []byte{0, 1}}},
		{"coupled-exceeds-streams", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: 2, Mapping: []byte{0, 1}}},
		{"negative-coupled", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: -1, Mapping: []byte{0, 1}}},
		{"streams-plus-coupled-overflow", MultistreamEncoderConfig{SampleRate: 48000, Channels: 255, Streams: 200, CoupledStreams: 100, Mapping: make([]byte, 255)}},
		{"streams-plus-coupled-exceeds-channels", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 2, CoupledStreams: 1, Mapping: []byte{0, 1}}},
		{"mapping-wrong-length", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: 1, Mapping: []byte{0}}},
		{"mapping-out-of-range", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: 1, Mapping: []byte{0, 5}}},
		{"unreferenced-stream", MultistreamEncoderConfig{SampleRate: 48000, Channels: 2, Streams: 2, CoupledStreams: 0, Mapping: []byte{0, 0}}},
		{"negative-bitrate", MultistreamEncoderConfig{SampleRate: 48000, Channels: 1, Streams: 1, Mapping: []byte{0}, Bitrate: -1}},
		{"bad-complexity", MultistreamEncoderConfig{SampleRate: 48000, Channels: 1, Streams: 1, Mapping: []byte{0}, Complexity: 11}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMultistreamEncoder(tc.cfg); !errors.Is(err, ErrBadArg) {
				t.Fatalf("want ErrBadArg, got %v", err)
			}
		})
	}
}

// TestMultistreamEncoderValidLayouts checks that legitimate family-0/255 layouts
// construct, expose the layout accessors, and return a defensive mapping copy.
func TestMultistreamEncoderValidLayouts(t *testing.T) {
	cases := []struct {
		channels, streams, coupled int
		mapping                    []byte
	}{
		{1, 1, 0, []byte{0}},
		{2, 1, 1, []byte{0, 1}},
		{2, 2, 0, []byte{0, 1}},
		{6, 4, 2, []byte{0, 1, 2, 3, 4, 5}},
		{8, 8, 0, []byte{0, 1, 2, 3, 4, 5, 6, 7}},
	}
	for _, tc := range cases {
		e, err := NewMultistreamEncoder(MultistreamEncoderConfig{
			SampleRate: 48000, Channels: tc.channels, Streams: tc.streams,
			CoupledStreams: tc.coupled, Mapping: tc.mapping,
		})
		if err != nil {
			t.Fatalf("ch%d: unexpected error: %v", tc.channels, err)
		}
		if e.Channels() != tc.channels || e.Streams() != tc.streams || e.CoupledStreams() != tc.coupled {
			t.Fatalf("ch%d: got channels=%d streams=%d coupled=%d", tc.channels, e.Channels(), e.Streams(), e.CoupledStreams())
		}
		got := e.Mapping()
		if !bytes.Equal(got, tc.mapping) {
			t.Fatalf("ch%d: got mapping=%v want %v", tc.channels, got, tc.mapping)
		}
		got[0] ^= 0xff
		if bytes.Equal(e.Mapping(), got) {
			t.Fatalf("ch%d: Mapping() is not a defensive copy", tc.channels)
		}
	}
}

// TestMultistreamEncoderRoundTrip is an end-to-end, cgo-free bit-exactness check:
// it encodes with MultistreamEncoder and decodes with MultistreamDecoder over the
// same layout, and asserts that on every frame the encoder's final range equals the
// decoder's. Equal final range is the range-coder handshake, so it proves the
// encoder emits packets a conformant decoder consumes to the identical range state.
func TestMultistreamEncoderRoundTrip(t *testing.T) {
	const (
		frameSize = 960
		nFrames   = 6
	)
	cases := []struct {
		name                       string
		channels, streams, coupled int
		mapping                    []byte
	}{
		{"mono", 1, 1, 0, []byte{0}},
		{"stereo", 2, 1, 1, []byte{0, 1}},
		{"two-mono", 2, 2, 0, []byte{0, 1}},
		{"5.1", 6, 4, 2, []byte{0, 4, 1, 2, 3, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := NewMultistreamEncoder(MultistreamEncoderConfig{
				SampleRate: 48000, Channels: tc.channels, Streams: tc.streams,
				CoupledStreams: tc.coupled, Mapping: tc.mapping, Bitrate: 64000 * tc.channels,
			})
			if err != nil {
				t.Fatalf("NewMultistreamEncoder: %v", err)
			}
			dec, err := NewMultistreamDecoder(48000, tc.channels, tc.streams, tc.coupled, tc.mapping)
			if err != nil {
				t.Fatalf("NewMultistreamDecoder: %v", err)
			}

			pcm := genMSPCM(nFrames, frameSize, tc.channels)
			encBuf := make([]byte, tc.streams*1400)
			decBuf := make([]int16, frameSize*tc.channels)
			for i := range nFrames {
				frame := pcm[i*frameSize*tc.channels : (i+1)*frameSize*tc.channels]
				n, err := enc.Encode(frame, encBuf)
				if err != nil {
					t.Fatalf("frame %d: Encode: %v", i, err)
				}
				got, err := dec.Decode(encBuf[:n], decBuf)
				if err != nil {
					t.Fatalf("frame %d: Decode: %v", i, err)
				}
				if got != frameSize {
					t.Fatalf("frame %d: decoded %d samples, want %d", i, got, frameSize)
				}
				if er, dr := enc.FinalRange(), dec.FinalRange(); er != dr {
					t.Fatalf("frame %d: final range mismatch: enc=%08x dec=%08x", i, er, dr)
				}
			}
		})
	}
}

// TestMultistreamEncoderReset pins Reset: after encoding frames (which advance the
// per-stream cross-frame state), Reset must restore the initial state so re-encoding
// the same input reproduces the same packets byte for byte. If Reset were a no-op the
// second pass would diverge, because the carried state changes the coded bits.
func TestMultistreamEncoderReset(t *testing.T) {
	const (
		frameSize = 960
		nFrames   = 4
	)
	enc, err := NewMultistreamEncoder(MultistreamEncoderConfig{
		SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: 1, Mapping: []byte{0, 1}, Bitrate: 96000,
	})
	if err != nil {
		t.Fatalf("NewMultistreamEncoder: %v", err)
	}
	pcm := genMSPCM(nFrames, frameSize, 2)
	buf := make([]byte, 4000)

	first := make([][]byte, nFrames)
	for i := range nFrames {
		frame := pcm[i*frameSize*2 : (i+1)*frameSize*2]
		n, err := enc.Encode(frame, buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		first[i] = bytes.Clone(buf[:n])
	}
	enc.Reset()
	for i := range nFrames {
		frame := pcm[i*frameSize*2 : (i+1)*frameSize*2]
		n, err := enc.Encode(frame, buf)
		if err != nil {
			t.Fatalf("post-reset frame %d: %v", i, err)
		}
		if !bytes.Equal(buf[:n], first[i]) {
			t.Fatalf("post-reset frame %d differs from the pre-reset packet: Reset did not restore initial state", i)
		}
	}
}

// TestMultistreamEncoderZeroAlloc locks the documented 0-allocations-per-frame
// steady state: after a warmup Encode grows the demux scratch, subsequent Encode
// calls must allocate nothing. It guards against a future edit reintroducing a
// per-frame allocation (a per-stream slice, a closure), which no other test catches.
func TestMultistreamEncoderZeroAlloc(t *testing.T) {
	const frameSize = 960
	enc, err := NewMultistreamEncoder(MultistreamEncoderConfig{
		SampleRate: 48000, Channels: 4, Streams: 2, CoupledStreams: 2, Mapping: []byte{0, 1, 2, 3}, Bitrate: 128000,
	})
	if err != nil {
		t.Fatalf("NewMultistreamEncoder: %v", err)
	}
	pcm := genMSPCM(1, frameSize, 4)
	buf := make([]byte, 8000)
	if _, err := enc.Encode(pcm, buf); err != nil { // warm the lazy-grown buffer
		t.Fatalf("warmup Encode: %v", err)
	}
	allocs := testing.AllocsPerRun(50, func() {
		if _, err := enc.Encode(pcm, buf); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("steady-state Encode allocates %v per call, want 0", allocs)
	}
}

// TestMultistreamEncoderEncodeErrors pins the Encode-time argument checks.
func TestMultistreamEncoderEncodeErrors(t *testing.T) {
	enc, err := NewMultistreamEncoder(MultistreamEncoderConfig{
		SampleRate: 48000, Channels: 2, Streams: 1, CoupledStreams: 1, Mapping: []byte{0, 1},
	})
	if err != nil {
		t.Fatalf("NewMultistreamEncoder: %v", err)
	}
	buf := make([]byte, 4000)

	// Wrong pcm length (not a multiple of the channel count).
	if _, err := enc.Encode(make([]int16, 961), buf); !errors.Is(err, ErrBadArg) {
		t.Fatalf("odd pcm length: want ErrBadArg, got %v", err)
	}
	// Not an Opus frame duration (15 ms = 720 samples per channel at 48 kHz).
	if _, err := enc.Encode(make([]int16, 720*2), buf); !errors.Is(err, ErrBadArg) {
		t.Fatalf("non-frame-duration: want ErrBadArg, got %v", err)
	}
	// Empty output buffer.
	if _, err := enc.Encode(genMSPCM(1, 960, 2), nil); !errors.Is(err, ErrBufferTooSmall) {
		t.Fatalf("empty buf: want ErrBufferTooSmall, got %v", err)
	}
}
