package pcm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// rampPCM builds n samples per channel of deterministic int16 PCM as bytes.
func rampPCM(n, channels int) []byte {
	b := make([]byte, n*channels*2)
	for i := range n * channels {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16((i%800-400)*64)))
	}
	return b
}

// TestDecodeInterleavedForwarders checks the pcm-layer one-shot decode, its
// limit forwarding and ErrDecodeLimit re-export, and that the aliased Decoder
// exposes Reset.
func TestDecodeInterleavedForwarders(t *testing.T) {
	cfg := Config{SampleRate: 48000, Channels: 1, Bitrate: 64000}
	var stream bytes.Buffer
	if err := EncodeInterleaved(&stream, cfg, rampPCM(48000, cfg.Channels)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}

	got, info, err := DecodeInterleaved(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if len(got) == 0 || info.Channels != cfg.Channels {
		t.Fatalf("got %d bytes, info = %+v", len(got), info)
	}

	if _, _, err := DecodeInterleavedLimit(bytes.NewReader(stream.Bytes()), 16); !errors.Is(err, ErrDecodeLimit) {
		t.Fatalf("DecodeInterleavedLimit(16): err = %v, want ErrDecodeLimit", err)
	}
	if DefaultMaxDecodedBytes <= 0 {
		t.Errorf("DefaultMaxDecodedBytes = %d, want positive", DefaultMaxDecodedBytes)
	}

	// The aliased Decoder exposes Reset (defined on oggopus.Decoder).
	d, err := NewDecoder(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if err := d.Reset(bytes.NewReader(stream.Bytes())); err != nil {
		t.Fatalf("Decoder.Reset: %v", err)
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode after Reset: %v", err)
	}
}
