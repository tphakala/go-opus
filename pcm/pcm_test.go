package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/tphakala/go-opus/oggopus"
	"github.com/tphakala/go-opus/pcm"
)

const (
	testRate     = 24000
	testChannels = 2
)

func validConfig() pcm.Config {
	return pcm.Config{SampleRate: testRate, Channels: testChannels, Bitrate: 64000}
}

// toneBytes returns nPerChannel interleaved little-endian int16 samples of a sine
// at hz, the byte form the public API consumes.
func toneBytes(nPerChannel, channels, hz, rate int) []byte {
	out := make([]byte, 0, nPerChannel*channels*2)
	for i := range nPerChannel {
		v := int16(19660 * math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(rate)))
		for range channels {
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

// decodePCM decodes a whole Ogg Opus stream through the pcm facade and returns the
// interleaved 48 kHz PCM bytes plus the parsed Info.
func decodePCM(t *testing.T, stream []byte) (samples []byte, info pcm.Info) {
	t.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("pcm.NewDecoder: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.WriteTo(&out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return out.Bytes(), d.Info()
}

// decodeOggopus decodes the same stream directly through oggopus, for the parity
// assertion that the facade transforms nothing.
func decodeOggopus(t *testing.T, stream []byte) []byte {
	t.Helper()
	d, err := oggopus.NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("oggopus.NewDecoder: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.WriteTo(&out); err != nil {
		t.Fatalf("oggopus WriteTo: %v", err)
	}
	return out.Bytes()
}

func TestNewEncoderRejectsNilWriter(t *testing.T) {
	if _, err := pcm.NewEncoder(nil, validConfig()); err == nil {
		t.Fatal("expected an error for a nil writer, got nil")
	}
}

func TestNewEncoderRejectsInvalidConfig(t *testing.T) {
	if _, err := pcm.NewEncoder(&bytes.Buffer{}, pcm.Config{}); !errors.Is(err, pcm.ErrInvalidConfig) {
		t.Fatalf("zero config: got %v, want ErrInvalidConfig", err)
	}
}

func TestNewDecoderRejectsNilReader(t *testing.T) {
	if _, err := pcm.NewDecoder(nil); err == nil {
		t.Fatal("expected an error for a nil reader, got nil")
	}
}

// TestRoundTrip streams a non-48 kHz tone through the facade encoder and decoder
// and checks the decoded length is the input rescaled to the 48 kHz output rate.
func TestRoundTrip(t *testing.T) {
	const nPerChannel = 5000 // not a whole number of 20 ms (480-sample) frames at 24 kHz
	in := toneBytes(nPerChannel, testChannels, 440, testRate)

	var buf bytes.Buffer
	e, err := pcm.NewEncoder(&buf, validConfig())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if _, err := e.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, info := decodePCM(t, buf.Bytes())

	if info.Channels != testChannels {
		t.Fatalf("Info.Channels = %d, want %d", info.Channels, testChannels)
	}
	wantPerChannel := nPerChannel * pcm.OutputSampleRate / testRate
	wantBytes := wantPerChannel * testChannels * 2
	if len(out) != wantBytes {
		t.Fatalf("decoded %d bytes, want %d (%d samples/ch at %d Hz rescaled to %d Hz, %d channels)",
			len(out), wantBytes, nPerChannel, testRate, pcm.OutputSampleRate, testChannels)
	}
}

// TestEncodeInterleavedOneShot exercises the one-shot helper and its input
// validation.
func TestEncodeInterleavedOneShot(t *testing.T) {
	const nPerChannel = 4800
	in := toneBytes(nPerChannel, testChannels, 660, testRate)

	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, validConfig(), in); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	out, _ := decodePCM(t, buf.Bytes())
	if want := nPerChannel * pcm.OutputSampleRate / testRate * testChannels * 2; len(out) != want {
		t.Fatalf("decoded %d bytes, want %d", len(out), want)
	}

	// A buffer that is not a whole number of samples is rejected before any write.
	var sink bytes.Buffer
	if err := pcm.EncodeInterleaved(&sink, validConfig(), []byte{1, 2, 3}); !errors.Is(err, pcm.ErrInvalidConfig) {
		t.Fatalf("odd-length pcm: got %v, want ErrInvalidConfig", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("EncodeInterleaved wrote %d bytes before rejecting a bad length", sink.Len())
	}
}

// TestInfoOutputSampleRate pins the #21 fix through the facade: the output rate is
// the fixed 48 kHz, distinct from the informational input rate.
func TestInfoOutputSampleRate(t *testing.T) {
	const rate = 16000
	in := toneBytes(4000, 1, 440, rate)

	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, pcm.Config{SampleRate: rate, Channels: 1, Bitrate: 32000}, in); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	_, info := decodePCM(t, buf.Bytes())

	if pcm.OutputSampleRate != 48000 {
		t.Fatalf("pcm.OutputSampleRate = %d, want 48000", pcm.OutputSampleRate)
	}
	if info.OutputSampleRate != pcm.OutputSampleRate {
		t.Fatalf("Info.OutputSampleRate = %d, want %d", info.OutputSampleRate, pcm.OutputSampleRate)
	}
	if info.InputSampleRate != rate {
		t.Fatalf("Info.InputSampleRate = %d, want %d", info.InputSampleRate, rate)
	}
}

// Compile-time proof that the facade types ARE the oggopus types (aliases, not
// wrappers). If any of these were converted to a concrete wrapper struct, this
// block would stop compiling.
var (
	_ *oggopus.Encoder = (*pcm.Encoder)(nil)
	_ *oggopus.Decoder = (*pcm.Decoder)(nil)
	_ oggopus.Config   = pcm.Config{}
	_ oggopus.Info     = pcm.Info{}
)

// TestParityWithOggopus proves the facade neither reshapes the API nor transforms
// any bytes: the error sentinels and constant are the same values, and PCM decoded
// through the facade is byte-identical to PCM decoded through oggopus, for both the
// one-shot and the streaming encode paths. Raw stream bytes are NOT compared
// because each stream carries a random Ogg serial number (as TestComplexityZeroMeansDefault
// documents); decoded-PCM equality plus the alias identity above is the strongest
// honest assertion.
func TestParityWithOggopus(t *testing.T) {
	// The re-exported sentinels are the same values, so errors.Is matches an error
	// from either package against either sentinel.
	if !errors.Is(pcm.ErrInvalidConfig, oggopus.ErrInvalidConfig) {
		t.Fatal("pcm.ErrInvalidConfig does not match oggopus.ErrInvalidConfig")
	}
	if !errors.Is(pcm.ErrClosed, oggopus.ErrClosed) {
		t.Fatal("pcm.ErrClosed does not match oggopus.ErrClosed")
	}
	if pcm.OutputSampleRate != oggopus.OutputSampleRate {
		t.Fatal("pcm.OutputSampleRate is not oggopus.OutputSampleRate")
	}

	cfg := validConfig()
	in := toneBytes(4800, testChannels, 523, testRate)

	// One-shot: facade vs oggopus.
	var viaPCM, viaOgg bytes.Buffer
	if err := pcm.EncodeInterleaved(&viaPCM, cfg, in); err != nil {
		t.Fatalf("pcm.EncodeInterleaved: %v", err)
	}
	if err := oggopus.EncodeInterleaved(&viaOgg, cfg, in); err != nil {
		t.Fatalf("oggopus.EncodeInterleaved: %v", err)
	}
	// The facade decoder and the oggopus decoder read the facade-produced stream
	// identically (the aliases share one implementation).
	pcmDecoded, _ := decodePCM(t, viaPCM.Bytes())
	if oggDecoded := decodeOggopus(t, viaPCM.Bytes()); !bytes.Equal(pcmDecoded, oggDecoded) {
		t.Fatal("facade and oggopus decoders disagree on the facade-produced stream")
	}
	// The two encoders are deterministic, so their streams (serial aside) decode to
	// identical PCM.
	if oggStreamDecoded := decodeOggopus(t, viaOgg.Bytes()); !bytes.Equal(pcmDecoded, oggStreamDecoded) {
		t.Fatal("facade one-shot and oggopus one-shot decode to different PCM")
	}

	// Streaming: facade NewEncoder -> Write -> Close must match the one-shot path.
	var viaStream bytes.Buffer
	e, err := pcm.NewEncoder(&viaStream, cfg)
	if err != nil {
		t.Fatalf("pcm.NewEncoder: %v", err)
	}
	if _, err := e.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if streamDecoded, _ := decodePCM(t, viaStream.Bytes()); !bytes.Equal(pcmDecoded, streamDecoded) {
		t.Fatal("facade streaming encode decodes differently from the facade one-shot encode")
	}
}
