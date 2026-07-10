// Command goopusdec is an opus_demo-style decode tool used for conformance runs.
//
// It reads an opus_demo .bit file (a sequence of records, each a 4-byte
// big-endian payload length, a 4-byte big-endian encoder final range, then that
// many packet bytes), decodes every packet through the public opus.Decoder, and
// writes the interleaved little-endian 16-bit PCM to an output file. With
// -verify it also checks each packet's decoder final range against the range
// recorded in the bitstream, the bit-exactness check the RFC conformance suite
// relies on.
//
// Phase 2 decodes CELT-only streams; it is extended to full RFC 6716 in phase 3.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/tphakala/go-opus/opus"
)

// maxPacketSamples is 120 ms at 48 kHz, the largest per-channel duration one
// Opus packet can carry (RFC 6716 section 3.2.5).
const maxPacketSamples = 5760

func main() {
	rate := flag.Int("rate", 48000, "output sample rate in Hz (8000, 12000, 16000, 24000, or 48000)")
	channels := flag.Int("channels", 2, "output channel count (1 or 2)")
	verify := flag.Bool("verify", false, "check each packet's decoder final range against the bitstream")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] input.bit output.pcm\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Decode an opus_demo .bit file to interleaved little-endian 16-bit PCM.")
		fmt.Fprintln(os.Stderr, "\nflags:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(flag.Arg(0), flag.Arg(1), *rate, *channels, *verify); err != nil {
		fmt.Fprintf(os.Stderr, "goopusdec: %v\n", err)
		os.Exit(1)
	}
}

// run decodes inPath to outPath at the given rate and channel count. When verify
// is set it reports any packet whose decoder final range disagrees with the
// bitstream and fails if there is at least one mismatch.
func run(inPath, outPath string, rate, channels int, verify bool) (err error) {
	bitData, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	dec, err := opus.NewDecoder(rate, channels)
	if err != nil {
		return fmt.Errorf("new decoder: %w", err)
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

	w := bufio.NewWriter(out)
	pcm := make([]int16, maxPacketSamples*channels)
	sampleBytes := make([]byte, maxPacketSamples*channels*2)

	var packets, samples, lost, mismatches int
	off := 0
	for off+8 <= len(bitData) {
		length := int(binary.BigEndian.Uint32(bitData[off:]))
		wantRange := binary.BigEndian.Uint32(bitData[off+4:])
		off += 8

		if length == 0 { // opus_demo lost-frame marker: no packet to decode.
			lost++
			continue
		}
		if off+length > len(bitData) {
			return fmt.Errorf("truncated record at offset %d: want %d bytes, have %d", off, length, len(bitData)-off)
		}
		payload := bitData[off : off+length]
		off += length

		n, derr := dec.Decode(payload, pcm)
		if derr != nil {
			return fmt.Errorf("packet %d (%d bytes): %w", packets, length, derr)
		}
		packets++
		samples += n

		if verify {
			if got := dec.FinalRange(); got != wantRange {
				mismatches++
				fmt.Fprintf(os.Stderr, "range mismatch on packet %d: want %d, got %d\n", packets-1, wantRange, got)
			}
		}

		frame := pcm[:n*channels]
		for i, s := range frame {
			binary.LittleEndian.PutUint16(sampleBytes[2*i:], uint16(s))
		}
		if _, werr := w.Write(sampleBytes[:len(frame)*2]); werr != nil {
			return fmt.Errorf("write pcm: %w", werr)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}

	fmt.Fprintf(os.Stderr, "decoded %d packets (%d samples/channel, %d lost markers skipped)\n", packets, samples, lost)
	if verify {
		if mismatches > 0 {
			return fmt.Errorf("%d/%d packets failed the final-range check", mismatches, packets)
		}
		fmt.Fprintf(os.Stderr, "final-range: all %d packets match\n", packets)
	}
	return nil
}
