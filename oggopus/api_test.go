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

// TestVendorStringDerivesFromOpusVersion pins the OpusTags vendor string to
// opus.Version. The two were independent "0.1.0-dev" literals (config.go admitted
// the duplication in its own comment), and a version string is precisely the kind of
// value NO bit-exactness gate can check: OpusTags vendor text is free-form, so a
// container claiming go-opus 0.1.0-dev while the codec inside it is 0.4.0 produces a
// perfectly valid, perfectly wrong file. libVersion now derives from opus.Version at
// compile time, so drift is impossible; what remains testable, and what this pins, is
// that the derivation is actually WIRED (a re-introduced literal that happened to
// match today would still pass an equality check against the string, but not against
// the constant) and that the "go-opus <version>" shape is what lands in the tags.
func TestVendorStringDerivesFromOpusVersion(t *testing.T) {
	if libVersion != opus.Version {
		t.Fatalf("libVersion = %q, opus.Version = %q: the oggopus vendor version has drifted "+
			"from the codec's; libVersion must be defined as opus.Version, not restated",
			libVersion, opus.Version)
	}
	if opus.Version == "" {
		t.Fatal("opus.Version is empty")
	}
	want := "go-opus " + opus.Version
	if got := (&Config{}).vendorString(); got != want {
		t.Fatalf("default vendor string = %q, want %q", got, want)
	}

	// And the string really is what reaches the bytes: encode a stream and find it in
	// the OpusTags header page. A constant that is right but unused would pass every
	// check above.
	var buf bytes.Buffer
	if err := EncodeInterleaved(&buf, Config{SampleRate: 48000, Channels: 1}, make([]byte, 960*2)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Fatalf("the encoded stream does not carry the vendor string %q", want)
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
	// OutputSampleRate is the fixed 48 kHz decode rate, distinct from the
	// informational 24000 Hz input rate; a consumer must not read one for the other.
	if info.OutputSampleRate != OutputSampleRate {
		t.Fatalf("OutputSampleRate = %d, want %d", info.OutputSampleRate, OutputSampleRate)
	}
	if info.OutputSampleRate == info.InputSampleRate {
		t.Fatalf("OutputSampleRate must differ from the 24000 Hz InputSampleRate; both = %d", info.OutputSampleRate)
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

// TestNilSinkAndSourceRejected pins that the public constructors reject a nil
// io.Writer or io.Reader with an error rather than panicking, the contract the
// pcm facade (and the go-flac/go-aac siblings) rely on. Config validation still
// takes precedence over the nil-writer check, so a bad config reports
// ErrInvalidConfig even when the writer is also nil.
func TestNilSinkAndSourceRejected(t *testing.T) {
	valid := Config{SampleRate: 48000, Channels: 1}

	if _, err := NewEncoder(nil, valid); err == nil {
		t.Fatal("NewEncoder(nil, valid): got nil error, want a nil-writer error")
	}
	if err := (&Encoder{}).Reset(nil, valid); err == nil {
		t.Fatal("Encoder.Reset(nil, valid): got nil error, want a nil-writer error")
	}
	if err := EncodeInterleaved(nil, valid, make([]byte, 4)); err == nil {
		t.Fatal("EncodeInterleaved(nil, valid, ...): got nil error, want a nil-writer error")
	}
	if _, err := NewDecoder(nil); err == nil {
		t.Fatal("NewDecoder(nil): got nil error, want a nil-reader error")
	}

	// A bad config is reported before the nil-writer check on both entry points.
	if _, err := NewEncoder(nil, Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewEncoder(nil, bad cfg): got %v, want ErrInvalidConfig", err)
	}
	if err := EncodeInterleaved(nil, Config{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("EncodeInterleaved(nil, bad cfg): got %v, want ErrInvalidConfig", err)
	}
}
