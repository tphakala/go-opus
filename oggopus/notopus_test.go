package oggopus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// oggStream writes one Ogg page per packet: the first page carries flagBOS, the
// last flagEOS, all share one serial, and sequence numbers run 0..n-1.
func oggStream(t *testing.T, pkts ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i, p := range pkts {
		var flags headerType
		if i == 0 {
			flags |= flagBOS
		}
		if i == len(pkts)-1 {
			flags |= flagEOS
		}
		if err := writePage(&buf, flags, 0, 0x1234, uint32(i), lacing(len(p)), p); err != nil {
			t.Fatalf("writePage %d: %v", i, err)
		}
	}
	return buf.Bytes()
}

// vorbisIdentHeader builds a Vorbis identification header: packet type 0x01,
// "vorbis", version uint32 0, channels 1, sample rate uint32 44100, three int32
// bitrates (max, nominal, min), blocksize byte 0xB8, framing byte 0x01.
func vorbisIdentHeader() []byte {
	b := []byte{0x01}
	b = append(b, "vorbis"...)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 44100)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 128000)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 0xB8, 0x01)
	if len(b) != 30 {
		panic("vorbis identification header is not 30 bytes")
	}
	return b
}

// vorbisCommentHeader builds a Vorbis comment header: packet type 0x03,
// "vorbis", a length-prefixed vendor string, a zero comment count, framing bit.
func vorbisCommentHeader() []byte {
	b := []byte{0x03}
	b = append(b, "vorbis"...)
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = append(b, "test"...)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return append(b, 0x01)
}

// oggFLACMappingHeader builds the Ogg FLAC first packet: 0x7F, "FLAC", major 1,
// minor 0, header-packet count uint16 big-endian, "fLaC", a STREAMINFO metadata
// block header (last-block flag clear, type 0, 24-bit big-endian length 34), and
// 34 STREAMINFO bytes (block sizes 4096, 44100 Hz, 1 channel, 16 bits).
func oggFLACMappingHeader() []byte {
	b := []byte{0x7F}
	b = append(b, "FLAC"...)
	b = append(b, 1, 0)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = append(b, "fLaC"...)
	b = append(b, 0x00, 0x00, 0x00, 34)
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:2], 4096)
	binary.BigEndian.PutUint16(si[2:4], 4096)
	// 20-bit sample rate, 3-bit channels-1, 5-bit bits-1, 36-bit total samples.
	packed := uint64(44100)<<44 | uint64(0)<<41 | uint64(15)<<36
	binary.BigEndian.PutUint64(si[10:18], packed)
	b = append(b, si...)
	if len(b) != 51 {
		panic("Ogg FLAC mapping header is not 51 bytes")
	}
	return b
}

func TestNotOpusFirstPacketWrapsErrNotOpus(t *testing.T) {
	vorbis := vorbisIdentHeader()
	tests := []struct {
		name   string
		stream []byte
	}{
		{"ogg vorbis", oggStream(t, vorbis, vorbisCommentHeader())},
		{"ogg flac", oggStream(t, oggFLACMappingHeader())},
		{"short foreign under 8 bytes", oggStream(t, []byte("\x01vorbis"))},
		{"short foreign 8 to 18 bytes", oggStream(t, vorbis[:12])},
		{"empty first packet", oggStream(t, nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDecoder(bytes.NewReader(tc.stream)); !errors.Is(err, ErrNotOpus) {
				t.Fatalf("NewDecoder err = %v, want ErrNotOpus", err)
			}
			if _, _, err := DecodeInterleaved(bytes.NewReader(tc.stream)); !errors.Is(err, ErrNotOpus) {
				t.Fatalf("DecodeInterleaved err = %v, want ErrNotOpus", err)
			}
			if _, _, err := DecodeInterleavedLimit(bytes.NewReader(tc.stream), 0); !errors.Is(err, ErrNotOpus) {
				t.Fatalf("DecodeInterleavedLimit err = %v, want ErrNotOpus", err)
			}

			var valid bytes.Buffer
			writeContainer(t, &valid, testHead(2, 312), opusTags{vendor: "go-opus"},
				synthPackets(2), 960, int64(2*960-312))
			d, err := NewDecoder(&valid)
			if err != nil {
				t.Fatalf("NewDecoder(valid): %v", err)
			}
			if err := d.Reset(bytes.NewReader(tc.stream)); !errors.Is(err, ErrNotOpus) {
				t.Fatalf("Reset err = %v, want ErrNotOpus", err)
			}
			if got := d.Info(); got != (Info{}) {
				t.Fatalf("Info after failed Reset = %+v, want zero", got)
			}
		})
	}
}

func TestCorruptOpusIsNotErrNotOpus(t *testing.T) {
	full := testHead(2, 312).marshal()
	mod := func(f func(b []byte)) []byte {
		b := bytes.Clone(full)
		f(b)
		return b
	}

	f1 := testHead(2, 312)
	f1.mappingFamily = 1
	f1.streamCount, f1.coupledCount = 1, 1
	f1.channelMapping = []byte{0, 1}
	f1Full := f1.marshal()

	var noBOS bytes.Buffer
	if err := writePage(&noBOS, 0, 0, 0x1234, 0, lacing(len(full)), full); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		stream []byte
	}{
		{"OpusHead truncated to 10 bytes", oggStream(t, full[:10])},
		{"bad major version", oggStream(t, mod(func(b []byte) { b[8] = 0x10 }))},
		{"zero channels", oggStream(t, mod(func(b []byte) { b[9] = 0 }))},
		{"family 0 with 3 channels", oggStream(t, mod(func(b []byte) { b[9] = 3 }))},
		{"family 1 truncated mapping table", oggStream(t, f1Full[:len(f1Full)-1])},
		{"OpusTags bad magic", oggStream(t, full, vorbisCommentHeader())},
		{"OpusTags missing", oggStream(t, full)},
		{"not ogg", []byte("not an ogg stream at all")},
		{"empty input", nil},
		{"valid OpusHead without BOS", noBOS.Bytes()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDecoder(bytes.NewReader(tc.stream))
			if err == nil {
				t.Fatal("NewDecoder err = nil, want error")
			}
			if errors.Is(err, ErrNotOpus) {
				t.Fatalf("NewDecoder err = %v, must not wrap ErrNotOpus", err)
			}
		})
	}
}
