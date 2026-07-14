package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// buildWAV assembles a RIFF/WAVE stream around the given fmt chunk body and PCM
// payload, optionally injecting an extra chunk before the data chunk (the LIST
// chunk ffmpeg writes, which a reader must skip rather than choke on).
func buildWAV(fmtBody, pcm []byte, extraChunks ...[]byte) []byte {
	var body bytes.Buffer
	body.WriteString("WAVE")

	body.WriteString("fmt ")
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(fmtBody)))
	body.Write(fmtBody)

	for _, c := range extraChunks {
		body.Write(c)
	}

	body.WriteString("data")
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(pcm)))
	body.Write(pcm)

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// pcmFmt is a standard 16-byte PCM fmt chunk body.
func pcmFmt(format uint16, channels, rate, bits int) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:2], format)
	binary.LittleEndian.PutUint16(b[2:4], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:8], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:12], uint32(rate*channels*bits/8)) // byte rate
	binary.LittleEndian.PutUint16(b[12:14], uint16(channels*bits/8))     // block align
	binary.LittleEndian.PutUint16(b[14:16], uint16(bits))
	return b
}

// extensibleFmt is a 40-byte WAVE_FORMAT_EXTENSIBLE fmt body whose SubFormat GUID
// begins with the given real format tag.
func extensibleFmt(subFormat uint16, channels, rate, bits int) []byte {
	b := make([]byte, 40)
	copy(b, pcmFmt(formatExtensible, channels, rate, bits))
	binary.LittleEndian.PutUint16(b[16:18], 22) // cbSize
	binary.LittleEndian.PutUint16(b[24:26], subFormat)
	return b
}

// listChunk is a small filler chunk with an odd body, so the pad byte is exercised.
func listChunk() []byte {
	var c bytes.Buffer
	c.WriteString("LIST")
	body := []byte("INFOxyz") // 7 bytes: odd, so a pad byte must follow
	_ = binary.Write(&c, binary.LittleEndian, uint32(len(body)))
	c.Write(body)
	c.WriteByte(0) // RIFF pad
	return c.Bytes()
}

func TestNewReaderParsesPCM16(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0} // 2 stereo frames
	raw := buildWAV(pcmFmt(formatPCM, 2, 48000, 16), pcm)

	r, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.Format.SampleRate != 48000 || r.Format.Channels != 2 {
		t.Errorf("format = %d Hz/%d ch, want 48000/2", r.Format.SampleRate, r.Format.Channels)
	}
	if got := r.Format.Samples(); got != 2 {
		t.Errorf("Samples() = %d, want 2", got)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Errorf("payload = %v, want %v", got, pcm)
	}
}

// The data chunk must be bounded by its declared size: trailing chunks after the
// audio must not be handed to the encoder as if they were samples.
func TestReaderStopsAtDeclaredDataSize(t *testing.T) {
	pcm := []byte{1, 0, 2, 0}
	raw := buildWAV(pcmFmt(formatPCM, 1, 48000, 16), pcm)
	raw = append(raw, []byte("LIST\x04\x00\x00\x00junk")...)

	r, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Errorf("payload = %v, want %v (trailing chunk leaked into the audio)", got, pcm)
	}
}

func TestNewReaderSkipsChunksBeforeData(t *testing.T) {
	pcm := []byte{9, 0, 8, 0}
	raw := buildWAV(pcmFmt(formatPCM, 1, 24000, 16), pcm, listChunk())

	r, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Errorf("payload = %v, want %v", got, pcm)
	}
}

// Anything that is not 16-bit integer PCM must be rejected with a clear error
// rather than reinterpreted as samples: silently treating float32 bytes as int16
// is exactly the kind of mangling this reader exists to prevent.
func TestNewReaderRejectsUnsupportedFormats(t *testing.T) {
	tests := []struct {
		name    string
		fmtBody []byte
		want    error
	}{
		{"ieee float32", pcmFmt(formatIEEEFloat, 2, 48000, 32), ErrUnsupported},
		{"pcm 24-bit", pcmFmt(formatPCM, 2, 48000, 24), ErrUnsupported},
		{"pcm 8-bit", pcmFmt(formatPCM, 1, 48000, 8), ErrUnsupported},
		{"a-law", pcmFmt(0x0006, 1, 8000, 8), ErrUnsupported},
		{"extensible float32", extensibleFmt(formatIEEEFloat, 2, 48000, 32), ErrUnsupported},
		{"extensible pcm16", extensibleFmt(formatPCM, 2, 48000, 16), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildWAV(tc.fmtBody, []byte{0, 0, 0, 0})
			_, err := NewReader(bytes.NewReader(raw))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("NewReader: unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewReader error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewReaderRejectsNonRIFF(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00"),
		[]byte("short"),
		{},
	} {
		if _, err := NewReader(bytes.NewReader(raw)); !errors.Is(err, ErrNotRIFF) {
			t.Errorf("NewReader(%q) error = %v, want ErrNotRIFF", raw, err)
		}
	}
}

func TestNewReaderRejectsMissingDataChunk(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("WAVE")
	body.WriteString("fmt ")
	fmtBody := pcmFmt(formatPCM, 2, 48000, 16)
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(fmtBody)))
	body.Write(fmtBody)

	var raw bytes.Buffer
	raw.WriteString("RIFF")
	_ = binary.Write(&raw, binary.LittleEndian, uint32(body.Len()))
	raw.Write(body.Bytes())

	if _, err := NewReader(bytes.NewReader(raw.Bytes())); !errors.Is(err, ErrMalformed) {
		t.Errorf("NewReader error = %v, want ErrMalformed", err)
	}
}

// A streamed WAVE (piped to a file) leaves the data size at 0 or 0xFFFFFFFF; the
// reader must then read to EOF instead of stopping immediately.
func TestNewReaderUndeclaredDataSizeReadsToEOF(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0}
	for _, size := range []uint32{0, 0xFFFFFFFF} {
		var body bytes.Buffer
		body.WriteString("WAVE")
		body.WriteString("fmt ")
		fmtBody := pcmFmt(formatPCM, 1, 48000, 16)
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(fmtBody)))
		body.Write(fmtBody)
		body.WriteString("data")
		_ = binary.Write(&body, binary.LittleEndian, size)
		body.Write(pcm)

		var raw bytes.Buffer
		raw.WriteString("RIFF")
		_ = binary.Write(&raw, binary.LittleEndian, uint32(body.Len()))
		raw.Write(body.Bytes())

		r, err := NewReader(bytes.NewReader(raw.Bytes()))
		if err != nil {
			t.Fatalf("size %#x: NewReader: %v", size, err)
		}
		if r.Format.DataBytes != -1 {
			t.Errorf("size %#x: DataBytes = %d, want -1 (unknown)", size, r.Format.DataBytes)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("size %#x: ReadAll: %v", size, err)
		}
		if !bytes.Equal(got, pcm) {
			t.Errorf("size %#x: payload = %v, want %v", size, got, pcm)
		}
	}
}
