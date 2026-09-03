//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/opusdec"
)

// This file is the multistream decoder differential gate. The REAL C surround
// encoder (mapping family 1) synthesizes a multistream packet sequence for each
// standard channel count; every packet is then decoded through BOTH the C
// opus_multistream_decode and the pure-Go opusdec.OpusMSDecoder in lockstep, and
// the int16 PCM must match byte for byte and the per-packet final range (the XOR
// of the streams' range states) must be equal. A second test replays the same
// encoded streams through a CUSTOM decoder mapping (duplicate output channels and
// muted 255 channels) to exercise the channel scatter that the standard layouts
// do not, and a PLC packet is injected to check concealment agrees too.

const msMaxPerCh = 5760 // 120 ms at 48 kHz, the decoder's clamp ceiling

// decodeMSPair decodes one packet (nil for PLC) through the C and Go multistream
// decoders and asserts bit-identical PCM and equal final range.
func decodeMSPair(t *testing.T, label string, cDec *CMSDecoder, gDec *opusdec.OpusMSDecoder, channels int, pkt []byte) {
	t.Helper()

	// Pre-fill both output buffers with a distinctive sentinel so a muted channel
	// that a decoder fails to zero (mapping value 255) is caught as a mismatch
	// rather than hidden by Go's zero-initialized make. The present channels are
	// fully overwritten, so only the muted lanes can still carry the sentinel.
	const sentinel int16 = 0x5aa5
	cBuf := make([]int16, msMaxPerCh*channels)
	gBuf := make([]int16, msMaxPerCh*channels)
	for i := range cBuf {
		cBuf[i] = sentinel
		gBuf[i] = sentinel
	}

	cN, err := cDec.Decode(pkt, cBuf, msMaxPerCh)
	if err != nil {
		t.Fatalf("%s: C decode: %v", label, err)
	}
	cRng := cDec.FinalRange()

	gN, gerr := gDec.Decode(pkt, gBuf, msMaxPerCh, 0)
	if gerr != nil {
		t.Fatalf("%s: Go decode: %v", label, gerr)
	}
	gRng := gDec.FinalRange()

	if cN != gN {
		t.Fatalf("%s: sample-count mismatch C=%d Go=%d", label, cN, gN)
	}
	assertPCMEqual(t, label, cBuf[:cN*channels], gBuf[:gN*channels])
	if cRng != gRng {
		t.Fatalf("%s: final-range mismatch C=0x%08x Go=0x%08x", label, cRng, gRng)
	}
}

// TestMultistreamSurroundDifferential drives every standard family-1 surround
// layout (1..8 channels) end to end: encode a sequence with the C surround
// encoder, then decode each packet through the C and Go multistream decoders and
// assert they agree bit-for-bit. A PLC packet is injected mid-sequence.
func TestMultistreamSurroundDifferential(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960 // 20 ms
		nFrames   = 6
	)
	for channels := 1; channels <= 8; channels++ {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			pcm := genPCM(nFrames, frameSize, channels)
			layout, packets, err := MSSurroundEncodeSeq(1, channels, Fs, channels*64000, frameSize, pcm, nFrames)
			if err != nil {
				t.Fatalf("MSSurroundEncodeSeq: %v", err)
			}
			if layout.Streams < 1 || layout.Coupled < 0 || layout.Coupled > layout.Streams {
				t.Fatalf("implausible layout: streams=%d coupled=%d", layout.Streams, layout.Coupled)
			}

			cDec, err := NewCMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
			if err != nil {
				t.Fatalf("NewCMSDecoder: %v", err)
			}
			defer cDec.Destroy()
			gDec, err := opusdec.NewMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
			if err != nil {
				t.Fatalf("opusdec.NewMSDecoder: %v", err)
			}

			for i, pkt := range packets {
				decodeMSPair(t, fmt.Sprintf("ch%d/pkt%d", channels, i), cDec, gDec, channels, pkt)
				// Inject a PLC (lost) packet after the third frame; both decoders
				// must conceal identically before resuming on real packets.
				if i == 2 {
					decodeMSPair(t, fmt.Sprintf("ch%d/plc", channels), cDec, gDec, channels, nil)
				}
			}
		})
	}
}

// TestMultistreamCustomMappingDifferential encodes a small layout, then decodes
// the same packets through a CUSTOM decoder mapping that routes one decoded
// channel to several output channels and leaves others muted (255). This is the
// scatter path (get_left/right/mono_channel's prev walk and the muted-channel
// zeroing) that the standard surround layouts do not exercise.
func TestMultistreamCustomMappingDifferential(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960
		nFrames   = 5
	)
	cases := []struct {
		name        string
		encChannels int
		// outChannels and mapping define the decode-side layout replayed over the
		// encoded streams; mapping entries must be < streams+coupled or == 255.
		outMapping []byte
	}{
		// Stereo source (streams=1, coupled=1, stream channels {0,1}): duplicate L/R
		// and mute two extra channels.
		{"stereo-dup-mute", 2, []byte{0, 1, 1, 0, 255, 255}},
		// Mono source (streams=1, coupled=0, stream channel {0}): duplicate the mono
		// channel and interleave a muted channel.
		{"mono-dup-mute", 1, []byte{0, 255, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := genPCM(nFrames, frameSize, tc.encChannels)
			layout, packets, err := MSSurroundEncodeSeq(1, tc.encChannels, Fs, tc.encChannels*64000, frameSize, pcm, nFrames)
			if err != nil {
				t.Fatalf("MSSurroundEncodeSeq: %v", err)
			}

			// Guard the fixture: every custom mapping entry must be a valid stream
			// channel for the layout we actually got, or muted.
			maxChan := layout.Streams + layout.Coupled
			for i, m := range tc.outMapping {
				if int(m) >= maxChan && m != 255 {
					t.Fatalf("fixture mapping[%d]=%d invalid for streams=%d coupled=%d", i, m, layout.Streams, layout.Coupled)
				}
			}

			outCh := len(tc.outMapping)
			cDec, err := NewCMSDecoder(Fs, outCh, layout.Streams, layout.Coupled, tc.outMapping)
			if err != nil {
				t.Fatalf("NewCMSDecoder: %v", err)
			}
			defer cDec.Destroy()
			gDec, err := opusdec.NewMSDecoder(Fs, outCh, layout.Streams, layout.Coupled, tc.outMapping)
			if err != nil {
				t.Fatalf("opusdec.NewMSDecoder: %v", err)
			}

			for i, pkt := range packets {
				decodeMSPair(t, fmt.Sprintf("%s/pkt%d", tc.name, i), cDec, gDec, outCh, pkt)
			}
		})
	}
}
