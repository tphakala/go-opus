// Command wav2opus encodes a 16-bit PCM WAV file to an Ogg Opus file through the
// public oggopus encoder.
//
// It exists mainly so there is a go-opus subject to time against opusenc and
// ffmpeg in scripts/bench-encoders.sh. It streams: the WAV samples move through a
// fixed-size buffer into the encoder, so peak memory does not track the input
// length and a throughput measurement measures the codec rather than our own
// buffering.
//
// go-opus's encoder is CELT-only, so every packet it writes is a CELT packet
// regardless of bitrate. libopus in its default mode would pick SILK, CELT, or
// hybrid per frame; see scripts/bench-encoders.sh for how the comparison is
// mode-matched.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tphakala/go-opus/internal/wav"
	"github.com/tphakala/go-opus/oggopus"
)

// copyBufSize is the fixed staging buffer between the WAV reader and the
// encoder. At 48 kHz stereo it is about 170 ms of audio, comfortably more than
// the encoder's 20 ms frame, and it does not grow with the input.
const copyBufSize = 64 << 10

// maxChannels is the widest stream Opus codes as a single (mono or stereo)
// stream, which is all oggopus exposes.
const maxChannels = 2

func main() {
	bitrate := flag.Int("bitrate", 96000, "target bitrate in bits per second")
	complexity := flag.Int("complexity", 10, "encoder complexity, 0..10 (0 selects the library default)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] input.wav output.opus\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Encode a 16-bit PCM WAV file to Ogg Opus (CELT-only).")
		fmt.Fprintln(os.Stderr, "\nflags:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(flag.Arg(0), flag.Arg(1), *bitrate, *complexity); err != nil {
		fmt.Fprintf(os.Stderr, "wav2opus: %v\n", err)
		os.Exit(1)
	}
}

// run encodes inPath to outPath at the given bitrate and complexity.
func run(inPath, outPath string, bitrate, complexity int) (err error) {
	in, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer func() {
		if cerr := in.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close input: %w", cerr)
		}
	}()

	src, err := wav.NewReader(bufio.NewReaderSize(in, copyBufSize))
	if err != nil {
		return fmt.Errorf("read %s: %w", inPath, err)
	}
	if src.Format.Channels > maxChannels {
		return fmt.Errorf("%d-channel input: oggopus encodes mono or stereo only", src.Format.Channels)
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close output: %w", cerr)
		}
	}()

	w := bufio.NewWriterSize(out, copyBufSize)
	enc, err := oggopus.NewEncoder(w, oggopus.Config{
		SampleRate: src.Format.SampleRate,
		Channels:   src.Format.Channels,
		Bitrate:    bitrate,
		Complexity: complexity,
	})
	if err != nil {
		return fmt.Errorf("new encoder: %w", err)
	}

	// Stream through one fixed buffer. io.CopyBuffer is explicit about the
	// staging buffer, and neither side implements ReadFrom/WriteTo, so this is
	// the whole memory cost of the audio path.
	n, err := io.CopyBuffer(enc, src, make([]byte, copyBufSize))
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("finish stream: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}

	report(inPath, outPath, src.Format, n, out)
	return nil
}

// report prints a one-line summary of what was encoded. It is written to stderr
// so it cannot contaminate an Ogg stream on stdout if this ever grows a "-" sink.
func report(inPath, outPath string, f wav.Format, pcmBytes int64, out *os.File) {
	samples := pcmBytes / int64(f.Channels*2)
	secs := float64(samples) / float64(f.SampleRate)

	var outBytes int64 = -1
	if st, err := out.Stat(); err == nil {
		outBytes = st.Size()
	}

	fmt.Fprintf(os.Stderr, "%s: %d Hz, %d ch, %d samples/channel (%.2f s)\n",
		inPath, f.SampleRate, f.Channels, samples, secs)
	if outBytes >= 0 && secs > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d bytes (%.1f kbit/s)\n",
			outPath, outBytes, float64(outBytes)*8/secs/1000)
	}
}
