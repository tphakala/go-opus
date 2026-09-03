//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/opusdec"
	"github.com/tphakala/go-opus/opus"
)

// This file extends the multistream decoder differential gate with the coverage
// legs the base surround/custom-mapping tests do not reach: exotic frame sizes,
// in-band FEC / LBRR recovery, DTX / silence packets, a decode mapping that mixes a
// coupled and a mono stream, and framing-level corruption parity. Every leg drives
// the REAL surround encoder to synthesize packets, then decodes them through BOTH
// opus_multistream_decode and the pure-Go opusdec.OpusMSDecoder in lockstep.

// decodeMSPairFEC is decodeMSPair for the FEC path: it decodes pkt with decode_fec=1
// through both decoders and asserts the recovered PCM and final range agree. Both are
// given a 120 ms frameSize (msMaxPerCh), as the single-stream FEC test does, so each
// reconstruction is concealment for the lead-in plus the tail LBRR frame; the
// differential is valid because both decoders use the identical frameSize.
func decodeMSPairFEC(t *testing.T, label string, cDec *CMSDecoder, gDec *opusdec.OpusMSDecoder, channels int, pkt []byte) {
	t.Helper()
	const sentinel int16 = 0x5aa5
	cBuf := make([]int16, msMaxPerCh*channels)
	gBuf := make([]int16, msMaxPerCh*channels)
	for i := range cBuf {
		cBuf[i] = sentinel
		gBuf[i] = sentinel
	}

	cN, err := cDec.DecodeFEC(pkt, cBuf, msMaxPerCh)
	if err != nil {
		t.Fatalf("%s: C DecodeFEC: %v", label, err)
	}
	cRng := cDec.FinalRange()

	gN, gerr := gDec.Decode(pkt, gBuf, msMaxPerCh, 1)
	if gerr != nil {
		t.Fatalf("%s: Go DecodeFEC: %v", label, gerr)
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

// TestMultistreamExoticFrameSizesDifferential drives the surround differential at
// every frame duration other than the 20 ms the base test uses (2.5, 5, 10, 40 and
// 60 ms), exercising the frame-size clamp and the multiframe buffer paths.
func TestMultistreamExoticFrameSizesDifferential(t *testing.T) {
	const (
		Fs      = 48000
		nFrames = 4
	)
	frameSizes := []int{120, 240, 480, 1920, 2880} // 2.5, 5, 10, 40, 60 ms at 48 kHz
	for _, channels := range []int{2, 4, 6} {
		for _, frameSize := range frameSizes {
			t.Run(fmt.Sprintf("ch%d/fs%d", channels, frameSize), func(t *testing.T) {
				pcm := genPCM(nFrames, frameSize, channels)
				layout, packets, err := MSSurroundEncodeSeq(1, channels, Fs, channels*64000, frameSize, pcm, nFrames)
				if err != nil {
					t.Fatalf("MSSurroundEncodeSeq: %v", err)
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
					decodeMSPair(t, fmt.Sprintf("ch%d/fs%d/pkt%d", channels, frameSize, i), cDec, gDec, channels, pkt)
				}
			})
		}
	}
}

// TestMultistreamFECDifferential encodes a family-1 stereo stream (one coupled
// stream) with in-band FEC enabled and the stream forced into SILK, which carries
// LBRR, then runs the drop-and-recover sequence through the multistream decode path
// on both decoders: decode packet 0, and for each later packet, DecodeFEC to
// reconstruct the previous frame before decoding it normally. Both decoders must
// agree bit-for-bit at every step. The pure-Go DecodeFEC test cannot enable FEC (the
// public encoder emits none) and so only exercises the PLC fallback; this leg drives
// the reference encoder with FEC on. The mode guard below confirms the packets are
// LBRR-capable (SILK/hybrid); it does not independently prove LBRR bytes were
// decoded, but the C-vs-Go parity is valid coverage of the FEC decode path either
// way.
//
// The single coupled stream is deliberate: the surround rate allocation in
// opus_multistream_surround_encoder drives per-stream mode and bandwidth from its
// masking model and overrides OPUS_SET_BANDWIDTH, so multichannel layouts do not
// reliably stay in SILK/hybrid and would carry no LBRR. The mode guard below fails
// the leg if even this configuration produced CELT-only packets, so a silent
// regression to plain concealment cannot pass unnoticed.
func TestMultistreamFECDifferential(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960 // 20 ms
		nFrames   = 24
		channels  = 2 // family-1 stereo: one coupled stream, reliably SILK at low rate
	)
	pcm := genPCM(nFrames, frameSize, channels)
	layout, packets, err := MSSurroundEncodeSeqOpts(1, channels, Fs, frameSize, pcm, nFrames, MSEncodeOpts{
		Bitrate:       channels * 8000,
		Complexity:    10,
		VBR:           0,
		InbandFEC:     true,
		PacketLossPct: 30,
		Bandwidth:     oBandwidthWideband,
		SignalType:    oSignalVoice,
	})
	if err != nil {
		t.Fatalf("MSSurroundEncodeSeqOpts: %v", err)
	}

	// Guard: LBRR only rides on SILK or hybrid frames. If the encoder produced
	// CELT-only packets the leg would silently degrade to plain concealment.
	lbrrCapable := 0
	for _, p := range packets {
		if len(p) == 0 {
			continue
		}
		mode, _, _, _ := opus.ParseTOC(p[0])
		if mode == opus.ModeSILKOnly || mode == opus.ModeHybrid {
			lbrrCapable++
		}
	}
	if lbrrCapable == 0 {
		t.Fatalf("no SILK/hybrid packets; FEC leg would not exercise LBRR")
	}
	t.Logf("%d/%d packets SILK/hybrid (LBRR-capable)", lbrrCapable, len(packets))

	cDec, err := NewCMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
	if err != nil {
		t.Fatalf("NewCMSDecoder: %v", err)
	}
	defer cDec.Destroy()
	gDec, err := opusdec.NewMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
	if err != nil {
		t.Fatalf("opusdec.NewMSDecoder: %v", err)
	}

	decodeMSPair(t, "fec-seed", cDec, gDec, channels, packets[0])
	for i := 1; i < len(packets); i++ {
		decodeMSPairFEC(t, fmt.Sprintf("fec-recover%d", i-1), cDec, gDec, channels, packets[i])
		decodeMSPair(t, fmt.Sprintf("post-fec%d", i), cDec, gDec, channels, packets[i])
	}
}

// TestMultistreamDTXDifferential encodes true silence with DTX enabled so the
// encoder emits tiny no-transmission packets, then checks C and Go conceal them
// identically. The guard fails the leg if no short packets appeared (DTX never
// engaged), so the leg cannot silently pass on ordinary coded packets.
func TestMultistreamDTXDifferential(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960 // 20 ms
		nFrames   = 12
	)
	for _, channels := range []int{2, 6} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			pcm := make([]int16, nFrames*frameSize*channels) // true silence
			layout, packets, err := MSSurroundEncodeSeqOpts(1, channels, Fs, frameSize, pcm, nFrames, MSEncodeOpts{
				Bitrate:    channels * 32000,
				Complexity: 10,
				VBR:        1,
				DTX:        true,
			})
			if err != nil {
				t.Fatalf("MSSurroundEncodeSeqOpts: %v", err)
			}

			minLen := 1 << 30
			dtxish := 0
			for _, p := range packets {
				if len(p) < minLen {
					minLen = len(p)
				}
				if len(p) <= 2*layout.Streams+2 {
					dtxish++
				}
			}
			t.Logf("ch%d: %d streams, min packet len %d, %d DTX-ish packets", channels, layout.Streams, minLen, dtxish)
			if dtxish == 0 {
				t.Fatalf("DTX produced no short packets (min len %d); leg would not exercise DTX", minLen)
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
				decodeMSPair(t, fmt.Sprintf("ch%d/dtx-pkt%d", channels, i), cDec, gDec, channels, pkt)
			}
		})
	}
}

// TestMultistreamMixedMappingDifferential decodes a source that carries more than
// one stream mixing a coupled and a mono stream through a custom mapping that
// duplicates the mono channel and mutes another, exercising the scatter over a mono
// stream that the stereo/mono base cases and the standard surround layouts do not.
func TestMultistreamMixedMappingDifferential(t *testing.T) {
	const (
		Fs          = 48000
		frameSize   = 960
		nFrames     = 5
		encChannels = 5 // 5.0 surround: more than one stream, at least one mono
	)
	pcm := genPCM(nFrames, frameSize, encChannels)
	layout, packets, err := MSSurroundEncodeSeq(1, encChannels, Fs, encChannels*64000, frameSize, pcm, nFrames)
	if err != nil {
		t.Fatalf("MSSurroundEncodeSeq: %v", err)
	}
	if layout.Streams <= layout.Coupled {
		t.Skipf("layout has no mono stream (streams=%d coupled=%d)", layout.Streams, layout.Coupled)
	}

	// Decode channel indices: coupled streams occupy 0..2*coupled-1, mono streams
	// 2*coupled..streams+coupled-1. Route coupled channels 0 and 1, the first mono
	// channel (duplicated), and a muted channel.
	monoIdx := 2 * layout.Coupled
	outMapping := []byte{0, 1, byte(monoIdx), byte(monoIdx), 255}
	maxChan := layout.Streams + layout.Coupled
	for i, m := range outMapping {
		if int(m) >= maxChan && m != 255 {
			t.Fatalf("outMapping[%d]=%d invalid for streams=%d coupled=%d", i, m, layout.Streams, layout.Coupled)
		}
	}
	outCh := len(outMapping)

	cDec, err := NewCMSDecoder(Fs, outCh, layout.Streams, layout.Coupled, outMapping)
	if err != nil {
		t.Fatalf("NewCMSDecoder: %v", err)
	}
	defer cDec.Destroy()
	gDec, err := opusdec.NewMSDecoder(Fs, outCh, layout.Streams, layout.Coupled, outMapping)
	if err != nil {
		t.Fatalf("opusdec.NewMSDecoder: %v", err)
	}
	for i, pkt := range packets {
		decodeMSPair(t, fmt.Sprintf("mixmap/pkt%d", i), cDec, gDec, outCh, pkt)
	}
}

// TestMultistreamCorruptionParity mutates the FRAMING layer of a multi-stream
// packet (tail truncations that shorten it below the streams' declared frame
// lengths) and asserts the
// C and Go decoders agree on the outcome: both reject it, or both accept and produce
// identical PCM. Framing corruption is used deliberately (not payload corruption),
// because a corrupted compressed payload can leave the reference decoder returning
// garbage with a success code where the strict Go bounds checks return an error,
// which is not a real divergence.
func TestMultistreamCorruptionParity(t *testing.T) {
	const (
		Fs        = 48000
		frameSize = 960
		nFrames   = 4
		channels  = 4
	)
	pcm := genPCM(nFrames, frameSize, channels)
	layout, packets, err := MSSurroundEncodeSeq(1, channels, Fs, channels*64000, frameSize, pcm, nFrames)
	if err != nil {
		t.Fatalf("MSSurroundEncodeSeq: %v", err)
	}
	if layout.Streams < 2 {
		t.Skipf("need more than one stream to exercise self-delimited framing (streams=%d)", layout.Streams)
	}

	base := packets[1]
	var trials [][]byte
	for _, cut := range []int{1, 2, 3, len(base) / 2} {
		if cut > 0 && len(base)-cut > 0 {
			trials = append(trials, base[:len(base)-cut])
		}
	}
	if len(trials) == 0 {
		t.Fatal("no corruption trials constructed")
	}

	rejected := 0
	for ci, cp := range trials {
		// Fresh decoders per trial: a rejected packet must not leave shared state that
		// perturbs the next trial's parity.
		cDec, err := NewCMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
		if err != nil {
			t.Fatalf("NewCMSDecoder: %v", err)
		}
		gDec, err := opusdec.NewMSDecoder(Fs, channels, layout.Streams, layout.Coupled, layout.Mapping)
		if err != nil {
			cDec.Destroy()
			t.Fatalf("opusdec.NewMSDecoder: %v", err)
		}

		cBuf := make([]int16, msMaxPerCh*channels)
		gBuf := make([]int16, msMaxPerCh*channels)
		cN, cErr := cDec.Decode(cp, cBuf, msMaxPerCh)
		gN, gErr := gDec.Decode(cp, gBuf, msMaxPerCh, 0)

		label := fmt.Sprintf("truncate-%d (len %d)", ci, len(cp))
		if (cErr == nil) != (gErr == nil) {
			t.Fatalf("%s: decode-success parity mismatch: C ok=%v (%v), Go ok=%v (%v)",
				label, cErr == nil, cErr, gErr == nil, gErr)
		}
		if cErr != nil {
			// Parity held above, so both rejected: count it as a real reject-path hit.
			rejected++
		}
		if cErr == nil && gErr == nil {
			if cN != gN {
				t.Fatalf("%s: sample-count mismatch C=%d Go=%d", label, cN, gN)
			}
			assertPCMEqual(t, label, cBuf[:cN*channels], gBuf[:gN*channels])
			if cDec.FinalRange() != gDec.FinalRange() {
				t.Fatalf("%s: final-range mismatch", label)
			}
		}
		cDec.Destroy()
	}

	// Positive control: at least one truncation must have exercised the reject path,
	// otherwise the leg would silently degrade to "two decoders agree on valid input"
	// if the packet shape ever changed.
	if rejected == 0 {
		t.Fatalf("no truncation trial was rejected; corruption leg exercised no error path")
	}
}
