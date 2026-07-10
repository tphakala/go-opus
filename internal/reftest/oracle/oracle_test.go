//go:build refc

package oracle

import (
	"math"
	"strings"
	"testing"
)

// TestBuildConfig asserts (and dumps) that the oracle really is the frozen
// FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64 configuration. shim.c also
// enforces this at compile time; this surfaces it to CI output as well.
func TestBuildConfig(t *testing.T) {
	cfg := GetBuildConfig()
	t.Logf("oracle: %s", VersionString())
	t.Logf("build config: FIXED_POINT=%v DISABLE_FLOAT_API=%v OPUS_FAST_INT64=%v "+
		"CUSTOM_MODES=%v ENABLE_QEXT=%v arch_int64=%v",
		cfg.FixedPoint, cfg.DisableFloatAPI, cfg.FastInt64,
		cfg.CustomModes, cfg.EnableQEXT, cfg.ArchInt64)

	if !cfg.FixedPoint {
		t.Error("oracle is not FIXED_POINT")
	}
	if !cfg.DisableFloatAPI {
		t.Error("oracle does not have DISABLE_FLOAT_API")
	}
	if !cfg.FastInt64 {
		t.Error("oracle does not have OPUS_FAST_INT64 (hard-parts.md section 4)")
	}
	if !cfg.ArchInt64 {
		t.Error("oracle opus_int64 is not 8 bytes")
	}
	if cfg.CustomModes {
		t.Error("oracle must not define CUSTOM_MODES")
	}
	if cfg.EnableQEXT {
		t.Error("oracle must not define ENABLE_QEXT")
	}

	// libopus embeds "-fixed"/"-float" in its version string precisely so callers
	// can tell the arithmetic path at runtime (celt/celt.c). It is a second,
	// independent confirmation that we linked the fixed-point build. The version
	// number itself is "unknown" because PACKAGE_VERSION is not injected; the
	// authoritative pin is the libopus submodule commit recorded in README.md.
	if v := VersionString(); !strings.Contains(v, "-fixed") || strings.Contains(v, "-float") {
		t.Errorf("oracle version %q does not self-report a fixed-point build", v)
	}
}

// sineFrames builds n frames of frameSize-sample 48 kHz mono int16 sine tone.
func sineFrames(n, frameSize int, freq, amp float64) [][]int16 {
	const sampleRate = 48000.0
	frames := make([][]int16, n)
	phase := 0.0
	step := 2 * math.Pi * freq / sampleRate
	for f := range frames {
		buf := make([]int16, frameSize)
		for i := range buf {
			buf[i] = int16(amp * math.Sin(phase))
			phase += step
		}
		frames[f] = buf
	}
	return frames
}

// TestEncodeDecodeRoundTrip is the phase-0 end-to-end smoke test: a short
// CELT-only 48 kHz mono sequence encodes, decodes back, and the encoder and
// decoder final ranges agree on every frame. Proves the oracle is callable and
// self-consistent end to end.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	const (
		sampleRate = 48000
		channels   = 1
		bitrate    = 64000
		complexity = 5
		frameSize  = 960 // 20 ms at 48 kHz
		nFrames    = 5
	)

	enc, err := NewCELTEncoder(sampleRate, channels, bitrate, complexity)
	if err != nil {
		t.Fatalf("NewCELTEncoder: %v", err)
	}
	defer enc.Close()

	dec, err := NewDecoder(sampleRate, channels)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()

	frames := sineFrames(nFrames, frameSize, 440, 8000)
	for i, pcm := range frames {
		pkt, err := enc.Encode(pcm, frameSize)
		if err != nil {
			t.Fatalf("frame %d: encode: %v", i, err)
		}
		if len(pkt) < 1 || len(pkt) > 1275 {
			t.Fatalf("frame %d: implausible packet length %d", i, len(pkt))
		}
		encRange := enc.FinalRange()

		out, err := dec.Decode(pkt, frameSize, false)
		if err != nil {
			t.Fatalf("frame %d: decode: %v", i, err)
		}
		if len(out) != frameSize*channels {
			t.Fatalf("frame %d: decoded %d samples, want %d", i, len(out), frameSize*channels)
		}
		decRange := dec.FinalRange()

		// The final-range agreement is the primary differential check: encoder
		// and decoder must arrive at the same range coder state for the packet.
		if encRange != decRange {
			t.Errorf("frame %d: final range mismatch enc=%d dec=%d", i, encRange, decRange)
		}
		t.Logf("frame %d: %d bytes, finalRange=%d, stateHash=%#x",
			i, len(pkt), encRange, enc.StateHash())
	}
}

// TestSilenceEncodes checks the silence special case encodes and decodes without
// error (silence takes the 2-byte DTX-like path in the encoder).
func TestSilenceEncodes(t *testing.T) {
	const frameSize = 960
	enc, err := NewCELTEncoder(48000, 1, 64000, 5)
	if err != nil {
		t.Fatalf("NewCELTEncoder: %v", err)
	}
	defer enc.Close()
	dec, err := NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()

	silence := make([]int16, frameSize)
	pkt, err := enc.Encode(silence, frameSize)
	if err != nil {
		t.Fatalf("encode silence: %v", err)
	}
	out, err := dec.Decode(pkt, frameSize, false)
	if err != nil {
		t.Fatalf("decode silence: %v", err)
	}
	if len(out) != frameSize {
		t.Fatalf("decoded %d samples, want %d", len(out), frameSize)
	}
	t.Logf("silence frame: %d bytes, finalRange=%d", len(pkt), enc.FinalRange())
}

// TestStateHashEvolves confirms the persistent-state hash tap is wired and that
// the encoder's cross-frame state actually changes as a sequence is encoded
// (the property sequence tests rely on to localize divergence to a frame).
func TestStateHashEvolves(t *testing.T) {
	const frameSize = 960
	enc, err := NewCELTEncoder(48000, 1, 64000, 5)
	if err != nil {
		t.Fatalf("NewCELTEncoder: %v", err)
	}
	defer enc.Close()

	frames := sineFrames(4, frameSize, 660, 6000)
	var hashes []uint64
	for i, pcm := range frames {
		if _, err := enc.Encode(pcm, frameSize); err != nil {
			t.Fatalf("frame %d: encode: %v", i, err)
		}
		hashes = append(hashes, enc.StateHash())
	}
	if hashes[0] == 0 {
		t.Fatal("state hash returned 0; tap not wired")
	}
	// State must advance across frames for at least one step, otherwise the tap
	// is not observing the cross-frame state it claims to.
	allEqual := true
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Errorf("state hash never changed across frames: %#x", hashes)
	}
	t.Logf("state hashes: %#x", hashes)
}
