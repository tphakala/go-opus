package oggopus

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

func TestConfigValidate(t *testing.T) {
	valid := Config{SampleRate: 48000, Channels: 2}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, r := range []int{8000, 12000, 16000, 24000, 48000} {
		c := Config{SampleRate: r, Channels: 1}
		if err := c.validate(); err != nil {
			t.Fatalf("rate %d rejected: %v", r, err)
		}
	}
	bad := []Config{
		{SampleRate: 44100, Channels: 2},
		{SampleRate: 0, Channels: 2},
		{SampleRate: 48000, Channels: 0},
		{SampleRate: 48000, Channels: 3},
		{SampleRate: 48000, Channels: 2, Bitrate: -1},
		{SampleRate: 48000, Channels: 2, Complexity: 11},
		{SampleRate: 48000, Channels: 2, Complexity: -1},
	}
	for i := range bad {
		if err := bad[i].validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %+v: got %v, want ErrInvalidConfig", bad[i], err)
		}
	}
}

func TestConfigVendorDefault(t *testing.T) {
	def := Config{}
	if got := def.vendorString(); got != "go-opus "+libVersion {
		t.Fatalf("default vendor = %q", got)
	}
	custom := Config{Vendor: "custom"}
	if got := custom.vendorString(); got != "custom" {
		t.Fatalf("override vendor = %q", got)
	}
}

// TestEncoderSeamWired confirms the codec seam is live: the PCM entry points do
// real work and no longer report a stub. It is the successor to TestEncoderSeam,
// which asserted the pre-codec errCodecNotWired behaviour.
func TestEncoderSeamWired(t *testing.T) {
	var buf bytes.Buffer
	e, err := NewEncoder(&buf, Config{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	// One full 20 ms stereo frame.
	if _, err := e.Write(make([]byte, 960*2*2)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("encoder produced no output")
	}
	// Close is idempotent.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Write after Close is rejected.
	if _, err := e.Write(make([]byte, 4)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: got %v, want ErrClosed", err)
	}

	if _, err := NewEncoder(&buf, Config{SampleRate: 44100, Channels: 2}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewEncoder bad config: got %v, want ErrInvalidConfig", err)
	}
	// DTX passes Config.validate but the codec does not implement it, so it must
	// surface as opus.ErrUnsupported rather than being silently dropped.
	if _, err := NewEncoder(&buf, Config{SampleRate: 48000, Channels: 1, DTX: true}); !errors.Is(err, opus.ErrUnsupported) {
		t.Fatalf("NewEncoder DTX: got %v, want opus.ErrUnsupported", err)
	}
}

// TestZeroValueEncoderDoesNotPanic pins the errUninitialized guard: a zero-value
// Encoder has no codec and no container, and must report that rather than panic
// or (worse) spin in write's frame loop with frameBytes == 0.
func TestZeroValueEncoderDoesNotPanic(t *testing.T) {
	var e Encoder
	if _, err := e.Write(make([]byte, 4)); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value Write: got %v, want errUninitialized", err)
	}
	if err := e.Close(); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value Close: got %v, want errUninitialized", err)
	}
	var d Decoder
	if _, err := d.Read(make([]byte, 4)); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value Read: got %v, want errUninitialized", err)
	}
	if _, err := d.WriteTo(io.Discard); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value WriteTo: got %v, want errUninitialized", err)
	}
}

func TestEncodeInterleavedWired(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{SampleRate: 48000, Channels: 1}
	if err := EncodeInterleaved(&buf, cfg, make([]byte, 960*2)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("EncodeInterleaved produced no output")
	}
	// Odd byte count is rejected before the codec.
	if err := EncodeInterleaved(io.Discard, cfg, make([]byte, 5)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("EncodeInterleaved odd length: got %v, want ErrInvalidConfig", err)
	}
}

// TestDecoderParsesHeaders confirms NewDecoder parses the headers and Info
// reports them. The PCM path is exercised end to end in roundtrip_test.go; here
// the packets are synthetic, so only the header surface is checked.
func TestDecoderParsesHeaders(t *testing.T) {
	var buf bytes.Buffer
	head := testHead(2, 312)
	head.inputSampleRate = 24000
	head.outputGain = -128
	writeContainer(t, &buf, head, opusTags{vendor: "go-opus", comments: []string{"X=1"}},
		synthPackets(5), 960, int64(5*960-312))

	d, err := NewDecoder(&buf)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	info := d.Info()
	if info.Channels != 2 || info.InputSampleRate != 24000 || info.PreSkip != 312 || info.OutputGain != -128 {
		t.Fatalf("Info mismatch: %+v", info)
	}
	// The packets are random bytes, not real opus: the decoder must reject them
	// cleanly rather than panic.
	if _, err := d.Read(make([]byte, 64)); err == nil {
		t.Fatalf("Read of synthetic garbage packets: got nil error, want a decode error")
	}
}

func TestDecoderRejectsGarbage(t *testing.T) {
	if _, err := NewDecoder(bytes.NewReader([]byte("not an ogg stream at all"))); err == nil {
		t.Fatalf("NewDecoder accepted garbage")
	}
	if _, err := NewDecoder(bytes.NewReader(nil)); err == nil {
		t.Fatalf("NewDecoder accepted empty stream")
	}
}
