//go:build refc

package oggopus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/tphakala/go-opus/internal/reftest/oracle"
)

// TestDecodeFamily1SurroundAgainstLibopus is the end-to-end family-1 surround
// differential: the reference surround encoder synthesizes real multistream packet
// sequences for each standard channel count, they are muxed into an Ogg family-1
// stream, decoded through oggopus.Decoder, and compared byte-for-byte against a
// direct reference multistream decode of the same packets. This proves the whole
// container decode path (family-1 OpusHead parse, newFrameDecoder routing to the
// multistream decoder, and the pre-skip drop) agrees with the reference codec across
// real surround layouts, beyond the go-only wiring the default round-trip test
// covers. End-trim is zero here for the same reason as the go-only round-trip test.
func TestDecodeFamily1SurroundAgainstLibopus(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960 // 20 ms
		nFrames   = 8
		preSkip   = 312
	)
	// 3..8 channels are the family-1 surround layouts that carry a real mapping
	// table with more than one stream; 1 and 2 channels reduce to a single stream
	// and are covered by the single-stream container tests.
	for channels := 3; channels <= 8; channels++ {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			pcm := genInterleavedPCM(nFrames*frameSize, channels)
			layout, packets, err := oracle.MSSurroundEncodeSeq(1, channels, Fs, channels*64000, frameSize, pcm, nFrames)
			if err != nil {
				t.Fatalf("MSSurroundEncodeSeq: %v", err)
			}
			if layout.Streams < 1 || layout.Coupled < 0 || layout.Coupled > layout.Streams {
				t.Fatalf("implausible layout: streams=%d coupled=%d", layout.Streams, layout.Coupled)
			}

			head := opusHead{
				version:         opusHeadVersion,
				channels:        byte(channels),
				preSkip:         preSkip,
				inputSampleRate: 48000,
				mappingFamily:   1,
				streamCount:     byte(layout.Streams),
				coupledCount:    byte(layout.Coupled),
				channelMapping:  layout.Mapping,
			}
			var buf bytes.Buffer
			writeContainer(t, &buf, head, opusTags{vendor: "go-opus"}, packets, frameSize, int64(nFrames*frameSize-preSkip))

			dec, err := NewDecoder(&buf)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if got := dec.Info().Channels; got != channels {
				t.Fatalf("Info().Channels = %d want %d", got, channels)
			}
			got, err := io.ReadAll(dec)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}

			want := referenceCMSDecode(t, Fs, channels, layout, frameSize, packets, preSkip)
			if len(want) == 0 {
				t.Fatal("reference produced no PCM; the test would be vacuous")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("family-1 container decode mismatch vs reference: got %d bytes, want %d bytes, first diff at byte %d",
					len(got), len(want), firstDiffByte(got, want))
			}
		})
	}
}

// referenceCMSDecode decodes packets through the reference multistream decoder and
// returns the interleaved little-endian int16 PCM with the leading preSkip samples
// per channel dropped, exactly what the container decoder must reproduce.
func referenceCMSDecode(t *testing.T, fs, channels int, layout oracle.MSLayout, frameSize int, packets [][]byte, preSkip int) []byte {
	t.Helper()
	cDec, err := oracle.NewCMSDecoder(fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
	if err != nil {
		t.Fatalf("NewCMSDecoder: %v", err)
	}
	defer cDec.Destroy()

	buf := make([]int16, frameSize*channels)
	var all []int16
	for i, p := range packets {
		n, err := cDec.Decode(p, buf, frameSize)
		if err != nil {
			t.Fatalf("reference decode packet %d: %v", i, err)
		}
		all = append(all, buf[:n*channels]...)
	}
	if preSkip*channels > len(all) {
		t.Fatalf("pre-skip %d samples exceeds decoded %d", preSkip, len(all)/channels)
	}
	all = all[preSkip*channels:]
	out := make([]byte, 0, len(all)*2)
	for _, s := range all {
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}
