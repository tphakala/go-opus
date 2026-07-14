// Package wav implements a minimal streaming RIFF/WAVE reader for 16-bit
// integer PCM, the one input format the encoder CLIs need.
//
// It is deliberately narrow. It reads the chunk headers, rejects anything that
// is not pcm_s16le with a clear error rather than reinterpreting the bytes, and
// then exposes the data chunk as a plain io.Reader so the caller can stream the
// samples through a fixed-size buffer. It never buffers the audio itself, and it
// never seeks, so it works on a pipe.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Format errors. Callers can match these with errors.Is.
var (
	// ErrNotRIFF is returned when the stream does not begin with a RIFF/WAVE header.
	ErrNotRIFF = errors.New("wav: not a RIFF/WAVE stream")
	// ErrUnsupported is returned for a well-formed WAVE whose sample format this
	// reader deliberately does not handle (anything but 16-bit integer PCM).
	ErrUnsupported = errors.New("wav: unsupported sample format")
	// ErrMalformed is returned when the chunk structure is self-inconsistent.
	ErrMalformed = errors.New("wav: malformed stream")
)

// WAVE format tags (the wFormatTag field of the fmt chunk).
const (
	formatPCM        = 0x0001
	formatIEEEFloat  = 0x0003
	formatExtensible = 0xFFFE
)

// bitsPerSample is the only sample width this reader accepts.
const bitsPerSample = 16

// maxChunkSkip bounds how many bytes we will skip past an unknown chunk. A
// declared chunk size is attacker-controlled in the general case; io.CopyN stops
// at EOF anyway, so this only guards against a silly loop on a crafted header.
const maxChunkSkip = 1 << 30

// Format describes the PCM data in a WAVE stream.
type Format struct {
	SampleRate int   // samples per second per channel
	Channels   int   // interleaved channel count
	DataBytes  int64 // declared size of the data chunk; -1 when unknown (streamed)
}

// Samples reports the number of samples per channel in the data chunk, or -1 if
// the data size was not declared.
func (f Format) Samples() int64 {
	if f.DataBytes < 0 || f.Channels == 0 {
		return -1
	}
	return f.DataBytes / int64(f.Channels*2)
}

// Duration reports the stream length in seconds, or -1 if it is not known.
func (f Format) Duration() float64 {
	n := f.Samples()
	if n < 0 || f.SampleRate == 0 {
		return -1
	}
	return float64(n) / float64(f.SampleRate)
}

// Reader streams the interleaved little-endian int16 samples of a WAVE data
// chunk. The bytes it yields are exactly the payload the oggopus encoder wants,
// so a caller can io.CopyBuffer straight from one to the other.
type Reader struct {
	// Format is the parsed fmt chunk, valid once NewReader returns.
	Format Format

	r    io.Reader // bounded to the data chunk when its size is declared
	read int64
}

// NewReader consumes the RIFF header and every chunk up to and including the
// fmt chunk, leaving r positioned at the first byte of audio. It fails if the
// stream is not a RIFF/WAVE, if the sample format is not 16-bit integer PCM, or
// if the channel count is zero.
func NewReader(r io.Reader) (*Reader, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, fmt.Errorf("%w: reading RIFF header: %w", ErrNotRIFF, err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%w: got %q/%q, want \"RIFF\"/\"WAVE\"", ErrNotRIFF, riff[0:4], riff[8:12])
	}

	w := &Reader{}
	haveFmt := false
	for {
		id, size, err := readChunkHeader(r)
		if err != nil {
			return nil, err
		}
		switch id {
		case "fmt ":
			if err := w.parseFmt(r, size); err != nil {
				return nil, err
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, fmt.Errorf("%w: data chunk precedes fmt chunk", ErrMalformed)
			}
			// A size of 0 or 0xFFFFFFFF marks a length the writer did not know
			// (a stream piped to a file); read to EOF in that case.
			if size == 0 || size == 0xFFFFFFFF {
				w.Format.DataBytes = -1
				w.r = r
			} else {
				w.Format.DataBytes = int64(size)
				w.r = io.LimitReader(r, int64(size))
			}
			return w, nil
		default:
			if err := skipChunk(r, size); err != nil {
				return nil, err
			}
		}
	}
}

// Read yields interleaved little-endian int16 PCM from the data chunk.
func (w *Reader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	w.read += int64(n)
	return n, err
}

// parseFmt reads a fmt chunk of the declared size and validates that it
// describes 16-bit integer PCM.
func (w *Reader) parseFmt(r io.Reader, size uint32) error {
	// 16 bytes is the PCM fmt chunk; extensible adds cbSize and a 22-byte tail.
	if size < 16 {
		return fmt.Errorf("%w: fmt chunk is %d bytes, want at least 16", ErrMalformed, size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("%w: reading fmt chunk: %w", ErrMalformed, err)
	}
	if size%2 == 1 { // consume the RIFF pad byte that follows an odd-sized chunk
		if _, err := io.CopyN(io.Discard, r, 1); err != nil {
			return fmt.Errorf("%w: skipping fmt pad byte: %w", ErrMalformed, err)
		}
	}

	format := binary.LittleEndian.Uint16(buf[0:2])
	channels := int(binary.LittleEndian.Uint16(buf[2:4]))
	rate := int(binary.LittleEndian.Uint32(buf[4:8]))
	bits := int(binary.LittleEndian.Uint16(buf[14:16]))

	// WAVE_FORMAT_EXTENSIBLE hides the real tag in the first two bytes of the
	// SubFormat GUID. Without this, a 32-bit float extensible file would look
	// like an unknown tag instead of the float file it is.
	if format == formatExtensible {
		if size < 40 {
			return fmt.Errorf("%w: extensible fmt chunk is %d bytes, want at least 40", ErrMalformed, size)
		}
		format = binary.LittleEndian.Uint16(buf[24:26])
	}

	switch format {
	case formatPCM:
		// The one format we handle; the width check below still applies.
	case formatIEEEFloat:
		return fmt.Errorf("%w: %d-bit IEEE float; convert to pcm_s16le first", ErrUnsupported, bits)
	default:
		return fmt.Errorf("%w: WAVE format tag 0x%04X; want 16-bit integer PCM (0x0001)", ErrUnsupported, format)
	}
	if bits != bitsPerSample {
		return fmt.Errorf("%w: %d-bit PCM; want %d-bit", ErrUnsupported, bits, bitsPerSample)
	}
	if channels < 1 {
		return fmt.Errorf("%w: %d channels", ErrMalformed, channels)
	}
	if rate < 1 {
		return fmt.Errorf("%w: sample rate %d", ErrMalformed, rate)
	}

	w.Format.Channels = channels
	w.Format.SampleRate = rate
	return nil
}

// readChunkHeader reads one 8-byte chunk header.
func readChunkHeader(r io.Reader) (id string, size uint32, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return "", 0, fmt.Errorf("%w: no data chunk", ErrMalformed)
		}
		return "", 0, fmt.Errorf("%w: reading chunk header: %w", ErrMalformed, err)
	}
	return string(hdr[0:4]), binary.LittleEndian.Uint32(hdr[4:8]), nil
}

// skipChunk discards a chunk body of the given size plus its RIFF pad byte. It
// uses io.CopyN so it works on a non-seekable reader.
func skipChunk(r io.Reader, size uint32) error {
	n := int64(size)
	if n%2 == 1 { // chunks are padded to an even length
		n++
	}
	if n > maxChunkSkip {
		return fmt.Errorf("%w: chunk declares %d bytes", ErrMalformed, size)
	}
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		return fmt.Errorf("%w: skipping %d-byte chunk: %w", ErrMalformed, size, err)
	}
	return nil
}
