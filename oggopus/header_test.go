package oggopus

import (
	"bytes"
	"testing"
)

func TestOpusHeadGoldenBytes(t *testing.T) {
	h := opusHead{
		version:         1,
		channels:        1,
		preSkip:         312,
		inputSampleRate: 48000,
		outputGain:      0,
		mappingFamily:   0,
	}
	want := []byte{
		0x4f, 0x70, 0x75, 0x73, 0x48, 0x65, 0x61, 0x64, // "OpusHead"
		0x01,       // version 1
		0x01,       // channels 1
		0x38, 0x01, // pre-skip 312
		0x80, 0xbb, 0x00, 0x00, // input rate 48000
		0x00, 0x00, // gain 0
		0x00, // family 0
	}
	if got := h.marshal(); !bytes.Equal(got, want) {
		t.Fatalf("OpusHead marshal\n got %x\nwant %x", got, want)
	}
}

func TestOpusTagsGoldenBytes(t *testing.T) {
	tg := opusTags{vendor: "go-opus"}
	want := []byte{
		0x4f, 0x70, 0x75, 0x73, 0x54, 0x61, 0x67, 0x73, // "OpusTags"
		0x07, 0x00, 0x00, 0x00, // vendor length 7
		0x67, 0x6f, 0x2d, 0x6f, 0x70, 0x75, 0x73, // "go-opus"
		0x00, 0x00, 0x00, 0x00, // comment count 0
	}
	if got := tg.marshal(); !bytes.Equal(got, want) {
		t.Fatalf("OpusTags marshal\n got %x\nwant %x", got, want)
	}
}

func TestOpusHeadRoundTrip(t *testing.T) {
	cases := []opusHead{
		{version: 1, channels: 1, preSkip: 312, inputSampleRate: 48000, outputGain: 0, mappingFamily: 0},
		{version: 1, channels: 2, preSkip: 0, inputSampleRate: 16000, outputGain: -256, mappingFamily: 0},
		{version: 1, channels: 2, preSkip: 65535, inputSampleRate: 0, outputGain: 12345, mappingFamily: 0},
	}
	for _, want := range cases {
		got, err := parseOpusHead(want.marshal())
		if err != nil {
			t.Fatalf("parseOpusHead(%+v): %v", want, err)
		}
		if got.version != want.version || got.channels != want.channels ||
			got.preSkip != want.preSkip || got.inputSampleRate != want.inputSampleRate ||
			got.outputGain != want.outputGain || got.mappingFamily != want.mappingFamily {
			t.Fatalf("OpusHead round-trip\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestOpusTagsRoundTrip(t *testing.T) {
	want := opusTags{
		vendor:   "go-opus 0.1.0-dev",
		comments: []string{"ENCODER=go-opus", "TITLE=hello", "ARTIST=BirdNET-Go"},
	}
	got, err := parseOpusTags(want.marshal())
	if err != nil {
		t.Fatalf("parseOpusTags: %v", err)
	}
	if got.vendor != want.vendor {
		t.Fatalf("vendor got %q want %q", got.vendor, want.vendor)
	}
	if len(got.comments) != len(want.comments) {
		t.Fatalf("comment count got %d want %d", len(got.comments), len(want.comments))
	}
	for i := range want.comments {
		if got.comments[i] != want.comments[i] {
			t.Fatalf("comment %d got %q want %q", i, got.comments[i], want.comments[i])
		}
	}
}

func TestParseOpusHeadRejectsBad(t *testing.T) {
	full := testHead(2, 312).marshal()
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"truncated", full[:10]},
		{"bad magic", append([]byte("XpusHead"), full[8:]...)},
		{"zero channels", func() []byte { b := bytes.Clone(full); b[9] = 0; return b }()},
		{"family0 too many channels", func() []byte { b := bytes.Clone(full); b[9] = 3; return b }()},
		{"bad major version", func() []byte { b := bytes.Clone(full); b[8] = 0x10; return b }()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOpusHead(tc.in); err == nil {
				t.Fatalf("parseOpusHead(%s) = nil error, want error", tc.name)
			}
		})
	}
}

func TestParseOpusTagsRejectsBad(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"bad magic", []byte("XpusTags\x00\x00\x00\x00")},
		{"vendor overruns", []byte("OpusTags\xff\xff\xff\x7f")},
		{"missing comment count", func() []byte {
			return append([]byte("OpusTags"), 0x03, 0, 0, 0, 'a', 'b', 'c')
		}()},
		// OpusTags + empty vendor (len 0) + comment count 1 + a huge comment length.
		{"comment overruns", []byte("OpusTags\x00\x00\x00\x00\x01\x00\x00\x00\xff\xff\xff\x7f")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOpusTags(tc.in); err == nil {
				t.Fatalf("parseOpusTags(%s) = nil error, want error", tc.name)
			}
		})
	}
}

// TestParseOpusHeadFamily1 checks the parser accepts a mapping-family header and
// retains the mapping table, so third-party streams do not choke the reader.
func TestParseOpusHeadFamily1(t *testing.T) {
	b := []byte{
		'O', 'p', 'u', 's', 'H', 'e', 'a', 'd',
		1,    // version
		2,    // channels
		0, 0, // pre-skip
		0x80, 0xbb, 0, 0, // rate 48000
		0, 0, // gain
		1,    // family 1
		1,    // stream count
		1,    // coupled count
		0, 1, // channel mapping (2 bytes)
	}
	h, err := parseOpusHead(b)
	if err != nil {
		t.Fatalf("family 1 parse: %v", err)
	}
	if h.mappingFamily != 1 || h.streamCount != 1 || h.coupledCount != 1 || len(h.channelMapping) != 2 {
		t.Fatalf("family 1 fields wrong: %+v", h)
	}
}
