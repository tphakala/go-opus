package oggopus

import (
	"bytes"
	"errors"
	"io"
	"testing"
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

// TestEncoderSeam confirms the container-only build: NewEncoder validates config
// and succeeds, but the PCM entry points surface errCodecNotWired until the
// phase-4 encoder is wired.
func TestEncoderSeam(t *testing.T) {
	var buf bytes.Buffer
	e, err := NewEncoder(&buf, Config{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if _, err := e.Write(make([]byte, 4)); !errors.Is(err, errCodecNotWired) {
		t.Fatalf("Write: got %v, want errCodecNotWired", err)
	}
	if err := e.Close(); !errors.Is(err, errCodecNotWired) {
		t.Fatalf("Close: got %v, want errCodecNotWired", err)
	}

	if _, err := NewEncoder(&buf, Config{SampleRate: 44100, Channels: 2}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewEncoder bad config: got %v, want ErrInvalidConfig", err)
	}
}

func TestEncodeInterleavedSeam(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{SampleRate: 48000, Channels: 1}
	// Valid config and whole-sample buffer: reaches the seam and reports it.
	if err := EncodeInterleaved(&buf, cfg, make([]byte, 960*2)); !errors.Is(err, errCodecNotWired) {
		t.Fatalf("EncodeInterleaved: got %v, want errCodecNotWired", err)
	}
	// Odd byte count is rejected before the seam.
	if err := EncodeInterleaved(&buf, cfg, make([]byte, 5)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("EncodeInterleaved odd length: got %v, want ErrInvalidConfig", err)
	}
}

// TestDecoderParsesHeaders confirms the container-only decoder does real work:
// NewDecoder parses the headers and Info reports them, while the PCM output path
// surfaces errCodecNotWired.
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
	if _, err := d.Read(make([]byte, 64)); !errors.Is(err, errCodecNotWired) {
		t.Fatalf("Read: got %v, want errCodecNotWired", err)
	}
	if _, err := d.WriteTo(io.Discard); !errors.Is(err, errCodecNotWired) {
		t.Fatalf("WriteTo: got %v, want errCodecNotWired", err)
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
