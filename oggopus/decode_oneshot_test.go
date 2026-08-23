package oggopus

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// streamingDecode returns exactly what the streaming decoder yields for stream,
// the reference the one-shot must match (Opus is lossy but deterministic, so the
// same stream always decodes to the same bytes).
func streamingDecode(t *testing.T, stream []byte) []byte {
	t.Helper()
	d, err := NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, d); err != nil {
		t.Fatalf("streaming decode: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeInterleavedMatchesStreaming(t *testing.T) {
	cfg := Config{SampleRate: 48000, Channels: 2, Bitrate: 96000}
	stream := encodeOgg(t, cfg, tone(48000, cfg.Channels, 440, 48000))
	want := streamingDecode(t, stream)

	got, info, err := DecodeInterleaved(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("one-shot decode differs from streaming: got %d bytes, want %d", len(got), len(want))
	}
	if info.Channels != cfg.Channels || info.OutputSampleRate != OutputSampleRate {
		t.Errorf("info = %+v, want channels %d, rate %d", info, cfg.Channels, OutputSampleRate)
	}
}

func TestDecodeInterleavedLimit(t *testing.T) {
	cfg := Config{SampleRate: 48000, Channels: 2, Bitrate: 96000}
	stream := encodeOgg(t, cfg, tone(48000, cfg.Channels, 440, 48000))
	want := streamingDecode(t, stream)
	full := len(want)

	t.Run("below output fails", func(t *testing.T) {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(stream), full-1)
		if !errors.Is(err, ErrDecodeLimit) {
			t.Fatalf("err = %v, want ErrDecodeLimit", err)
		}
		if got != nil {
			t.Errorf("got %d bytes back on limit error, want nil", len(got))
		}
	})
	t.Run("exact output succeeds", func(t *testing.T) {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(stream), full)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit(exact): %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("exact-limit decode mismatch: got %d bytes, want %d", len(got), full)
		}
	})
	t.Run("non-positive is unbounded", func(t *testing.T) {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(stream), 0)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit(0): %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("unbounded decode mismatch: got %d bytes, want %d", len(got), full)
		}
	})
}

func TestDecodeInterleavedBadStream(t *testing.T) {
	got, _, err := DecodeInterleaved(bytes.NewReader([]byte("not an ogg opus stream at all")))
	if err == nil {
		t.Fatal("DecodeInterleaved on garbage: want error, got nil")
	}
	if errors.Is(err, ErrDecodeLimit) {
		t.Errorf("garbage input reported as ErrDecodeLimit: %v", err)
	}
	if got != nil {
		t.Errorf("got %d bytes back on error, want nil", len(got))
	}
}

// TestDecoderReset rebinds a decoder from one stream to another (including a
// different channel count) and checks the reset decode matches a fresh decode,
// and that a failed Reset leaves the decoder uninitialized rather than bound to
// the previous stream.
func TestDecoderReset(t *testing.T) {
	streamA := encodeOgg(t, Config{SampleRate: 48000, Channels: 1, Bitrate: 64000}, tone(24000, 1, 330, 48000))
	streamB := encodeOgg(t, Config{SampleRate: 48000, Channels: 2, Bitrate: 96000}, tone(24000, 2, 660, 48000))

	d, err := NewDecoder(bytes.NewReader(streamA))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("drain A: %v", err)
	}

	if err := d.Reset(bytes.NewReader(streamB)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d.Info().Channels != 2 {
		t.Errorf("after Reset Channels = %d, want 2", d.Info().Channels)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("decode B after Reset: %v", err)
	}
	if want := streamingDecode(t, streamB); !bytes.Equal(got.Bytes(), want) {
		t.Fatal("decode after Reset differs from a fresh decode of the same stream")
	}

	// A failed Reset must leave the decoder uninitialized, not bound to stream B.
	if err := d.Reset(nil); err == nil {
		t.Error("Reset(nil): want error, got nil")
	}
	if info := d.Info(); info != (Info{}) {
		t.Errorf("Info after a failed Reset = %+v, want zero (not the previous stream's metadata)", info)
	}
	if _, err := io.Copy(io.Discard, d); err == nil {
		t.Error("Read after a failed Reset should error, not resume the old stream")
	}
}
