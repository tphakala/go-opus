//go:build refc

package oracle

import (
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

// This SEQUENCE-based differential test pins the pure-Go top-level Opus decoder
// (opus.Decoder over internal/opusdec: the src/opus_decoder.c mode-transition and
// redundancy state machine) to the pinned libopus C decoder (opus_decode). The
// top-level decoder is a cross-frame / cross-packet state machine (mode
// transitions, the CELT redundancy frames at SILK<->CELT boundaries, hybrid
// SILK-low + CELT-high, and the FEC/LBRR paths), so the RFC vectors alone do not
// force every transition. Here the REAL C Opus encoder synthesizes packet
// SEQUENCES with a scripted per-frame mode (SILK<->hybrid<->CELT, which makes
// opus_encode emit the redundancy frames) and optional in-band FEC/LBRR, and each
// packet is decoded through BOTH the C opus_decode and the Go opus.Decoder IN
// LOCKSTEP, asserting after every packet that (a) the int16 PCM is bit-identical
// and (b) the per-packet final range matches.

// modeStep is one entry in an encoder force-mode script.
type modeStep struct {
	name      string
	mode      int
	bandwidth int
}

// genPCM builds nFrames*frameSize samples per channel of deterministic, lively
// interleaved int16 content (a few detuned sinusoids plus a little LCG noise) so
// the forced SILK / hybrid / CELT encodes are meaningful rather than silence.
func genPCM(nFrames, frameSize, channels int) []int16 {
	total := nFrames * frameSize
	out := make([]int16, total*channels)
	var lcg uint32 = 0x12345678
	for n := 0; n < total; n++ {
		lcg = lcg*1664525 + 1013904223
		noise := int32(int16(lcg>>16)) / 40
		// Cheap integer sinusoid mix via a rotating phase table.
		s0 := sineTab[(n*7)&255]
		s1 := sineTab[(n*13)&255]
		s2 := sineTab[(n*3)&255]
		v := (int32(s0)*3+int32(s1)*2+int32(s2)*2)/8 + noise
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		for c := 0; c < channels; c++ {
			// Slightly decorrelate the second channel so stereo coding does work.
			cv := v
			if c == 1 {
				cv = (v*7)/8 + int32(sineTab[(n*5+64)&255])/4
				if cv > 32767 {
					cv = 32767
				} else if cv < -32768 {
					cv = -32768
				}
			}
			out[n*channels+c] = int16(cv)
		}
	}
	return out
}

// sineTab is a 256-entry sine table scaled to +-6000.
var sineTab = func() [256]int16 {
	var t [256]int16
	// A crude fixed-point sine via the parabola approximation, ample for test audio.
	for i := 0; i < 256; i++ {
		// map i -> [-pi, pi) as x in [-128,128)
		x := i - 128
		// parabolic sine: y = (4/128^2)*x*(128-|x|) style, scaled
		ax := x
		if ax < 0 {
			ax = -ax
		}
		y := (x * (128 - ax))
		t[i] = int16(y * 6000 / (128 * 64))
	}
	return t
}()

// decodePair decodes one packet through the C and Go decoders and returns both
// interleaved int16 outputs and both final ranges. decodeFEC selects the
// decode_fec path; a nil pkt requests PLC.
func decodePair(t *testing.T, cDec *Decoder, gDec *opus.Decoder, channels int, pkt []byte, decodeFEC bool) (cPCM, gPCM []int16, cRng, gRng uint32) {
	t.Helper()
	const maxPerCh = 5760

	cOut, err := cDec.Decode(pkt, maxPerCh, decodeFEC)
	if err != nil {
		t.Fatalf("C decode: %v", err)
	}
	cRng = cDec.FinalRange()

	gBuf := make([]int16, maxPerCh*channels)
	var n int
	var gerr error
	if decodeFEC {
		n, gerr = gDec.DecodeFEC(pkt, gBuf)
	} else {
		n, gerr = gDec.Decode(pkt, gBuf)
	}
	if gerr != nil {
		t.Fatalf("Go decode: %v", gerr)
	}
	gPCM = gBuf[:n*channels]
	gRng = gDec.FinalRange()
	return cOut, gPCM, cRng, gRng
}

// assertPCMEqual fails with a pinpointed message on the first differing sample.
func assertPCMEqual(t *testing.T, label string, c, g []int16) {
	t.Helper()
	if len(c) != len(g) {
		t.Fatalf("%s: sample-count mismatch C=%d Go=%d", label, len(c), len(g))
	}
	for i := range c {
		if c[i] != g[i] {
			t.Fatalf("%s: PCM mismatch at sample %d: C=%d Go=%d", label, i, c[i], g[i])
		}
	}
}

// TestOpusDecodeTransitionsDifferential forces a SILK<->hybrid<->CELT mode script
// through the C encoder (which emits the redundancy frames at the boundaries) and
// checks the C and Go decoders agree bit-for-bit on every packet, for mono and
// stereo.
func TestOpusDecodeTransitionsDifferential(t *testing.T) {
	// A script that visits every ordered pair of modes at least once, so both
	// transition directions and both redundancy kinds (celt_to_silk and
	// silk_to_celt) are exercised.
	script := []modeStep{
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
	}

	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			const frameSize = 960 // 20 ms at 48 kHz
			// Repeat the script a few times to cover repeated transitions.
			reps := 4
			pcm := genPCM(len(script)*reps, frameSize, channels)

			enc, err := NewOpusEncoder(48000, channels, 64000, 10, false)
			if err != nil {
				t.Fatalf("NewOpusEncoder: %v", err)
			}
			defer enc.Close()
			cDec, err := NewDecoder(48000, channels)
			if err != nil {
				t.Fatalf("C NewDecoder: %v", err)
			}
			defer cDec.Close()
			gDec, err := opus.NewDecoder(48000, channels)
			if err != nil {
				t.Fatalf("Go NewDecoder: %v", err)
			}

			transitions, rangeChecks, prev := 0, 0, ""
			for r := 0; r < reps; r++ {
				for s, step := range script {
					frameIdx := r*len(script) + s
					in := pcm[frameIdx*frameSize*channels : (frameIdx+1)*frameSize*channels]
					pkt, err := enc.Encode(in, frameSize, step.mode, step.bandwidth)
					if err != nil {
						t.Fatalf("encode frame %d (%s): %v", frameIdx, step.name, err)
					}
					if pkt == nil {
						continue // DTX (not expected here)
					}
					gotMode, _, _, _ := opus.ParseTOC(pkt[0])
					if prev != "" && gotMode.String() != prev {
						transitions++
					}
					prev = gotMode.String()

					label := fmt.Sprintf("frame %d req=%s got=%s", frameIdx, step.name, gotMode)
					cPCM, gPCM, cRng, gRng := decodePair(t, cDec, gDec, channels, pkt, false)
					assertPCMEqual(t, label, cPCM, gPCM)
					if cRng != gRng {
						t.Fatalf("%s: final-range mismatch C=%d Go=%d", label, cRng, gRng)
					}
					rangeChecks++
				}
			}
			t.Logf("ch%d: %d packets decoded bit-identical (PCM+range), %d mode transitions exercised",
				channels, rangeChecks, transitions)
			if transitions < 6 {
				t.Errorf("expected several mode transitions, saw %d", transitions)
			}
		})
	}
}

// TestOpusDecodeFECDifferential encodes SILK/hybrid frames with in-band FEC/LBRR
// and checks (a) the normal decode of every FEC-carrying packet agrees C-vs-Go
// bit-for-bit (which validates the LBRR-skip path and the redundancy parse), and
// (b) a decode_fec recovery of a "lost" frame from the next packet agrees C-vs-Go.
func TestOpusDecodeFECDifferential(t *testing.T) {
	const channels = 1
	const frameSize = 960 // 20 ms

	// Force wideband SILK so every frame is a SILK payload that carries LBRR.
	script := make([]modeStep, 24)
	for i := range script {
		script[i] = modeStep{"SILK", oModeSilkOnly, oBandwidthWideband}
	}
	pcm := genPCM(len(script), frameSize, channels)

	enc, err := NewOpusEncoder(48000, channels, 24000, 10, true)
	if err != nil {
		t.Fatalf("NewOpusEncoder: %v", err)
	}
	defer enc.Close()

	packets := make([][]byte, 0, len(script))
	for i, step := range script {
		in := pcm[i*frameSize*channels : (i+1)*frameSize*channels]
		pkt, err := enc.Encode(in, frameSize, step.mode, step.bandwidth)
		if err != nil {
			t.Fatalf("encode frame %d: %v", i, err)
		}
		if pkt != nil {
			packets = append(packets, pkt)
		}
	}
	if len(packets) < 4 {
		t.Fatalf("expected several FEC packets, got %d", len(packets))
	}

	// (a) Normal decode of the whole sequence, C vs Go per packet.
	t.Run("normal", func(t *testing.T) {
		cDec, err := NewDecoder(48000, channels)
		if err != nil {
			t.Fatalf("C NewDecoder: %v", err)
		}
		defer cDec.Close()
		gDec, err := opus.NewDecoder(48000, channels)
		if err != nil {
			t.Fatalf("Go NewDecoder: %v", err)
		}
		for i, pkt := range packets {
			cPCM, gPCM, cRng, gRng := decodePair(t, cDec, gDec, channels, pkt, false)
			assertPCMEqual(t, fmt.Sprintf("normal packet %d", i), cPCM, gPCM)
			if cRng != gRng {
				t.Fatalf("normal packet %d: final-range mismatch C=%d Go=%d", i, cRng, gRng)
			}
		}
		t.Logf("normal: %d FEC-carrying packets decoded bit-identical", len(packets))
	})

	// (b) FEC recovery: both decoders run the identical op sequence
	//   decode(pkt[0]); for i>=1: decodeFEC(pkt[i]) then decode(pkt[i]).
	// The decodeFEC call reconstructs frame i-1 from pkt[i]'s LBRR data; both
	// decoders must produce the same recovered PCM and range at every step.
	t.Run("fec-recovery", func(t *testing.T) {
		cDec, err := NewDecoder(48000, channels)
		if err != nil {
			t.Fatalf("C NewDecoder: %v", err)
		}
		defer cDec.Close()
		gDec, err := opus.NewDecoder(48000, channels)
		if err != nil {
			t.Fatalf("Go NewDecoder: %v", err)
		}

		cPCM, gPCM, cRng, gRng := decodePair(t, cDec, gDec, channels, packets[0], false)
		assertPCMEqual(t, "fec seed packet 0", cPCM, gPCM)
		if cRng != gRng {
			t.Fatalf("fec seed packet 0: range C=%d Go=%d", cRng, gRng)
		}

		recoveries := 0
		for i := 1; i < len(packets); i++ {
			// Recover frame i-1 from packet i via FEC/LBRR.
			cR, gR, cRr, gRr := decodePair(t, cDec, gDec, channels, packets[i], true)
			assertPCMEqual(t, fmt.Sprintf("fec-recover frame %d", i-1), cR, gR)
			if cRr != gRr {
				t.Fatalf("fec-recover frame %d: range C=%d Go=%d", i-1, cRr, gRr)
			}
			recoveries++
			// Then decode packet i normally.
			cN, gN, cRn, gRn := decodePair(t, cDec, gDec, channels, packets[i], false)
			assertPCMEqual(t, fmt.Sprintf("post-fec packet %d", i), cN, gN)
			if cRn != gRn {
				t.Fatalf("post-fec packet %d: range C=%d Go=%d", i, cRn, gRn)
			}
		}
		t.Logf("fec-recovery: %d FEC reconstructions matched bit-identical (PCM+range)", recoveries)
	})
}
