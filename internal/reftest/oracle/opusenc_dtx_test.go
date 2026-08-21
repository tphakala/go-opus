//go:build refc

package oracle

import (
	"fmt"
	"testing"
)

// DTX GATE. Generalized discontinuous transmission (OPUS_SET_DTX) against the C
// libopus encoder, through the same paired-encoder harness the CP9 gate uses.
// Every frame goes through encPair.frame(), which asserts the return value, the
// packet length, every packet byte, st->rangeFinal, and the cross-frame state
// FIELD BY FIELD (now including nb_no_activity_ms_Q1 and peak_signal_energy) plus
// the State hash. So byte-for-byte and state-for-state agreement is the gate;
// the loops below add NON-VACUITY guards proving the DTX branches actually fired.
//
// What DTX does in the frozen forced-CELT-only config (see internal/opusenc/dtx.go):
// the decision guard reduces to `use_dtx && is_silence`, so a frame is dropped to
// a 1-byte TOC-only packet only on EXACT digital silence, and only after a
// ~200 ms onset (NB_SPEECH_FRAMES_BEFORE_DTX = 10 frames of 20 ms). A run is
// capped at MAX_CONSECUTIVE_DTX (20) frames, after which one full frame is forced
// and the counter re-arms.

// dtxCfg is the frozen CP9 config with DTX enabled.
func dtxCfg(channels int) cOpusencCfg {
	cfg := defaultOpusencCfg(channels)
	cfg.DTX = 1
	return cfg
}

// TestOpusencDTXSilenceOnsetAndCap drives a long run of exact digital silence and
// proves, bit-exactly against C, the full DTX life cycle: full frames before the
// onset, 1-byte packets after it, a forced full frame at the consecutive-DTX cap,
// and re-onset afterwards.
func TestOpusencDTXSilenceOnsetAndCap(t *testing.T) {
	for _, ch := range []int{1, 2} {
		ch := ch
		name := "mono"
		if ch == 2 {
			name = "stereo"
		}
		t.Run(name, func(t *testing.T) {
			cfg := dtxCfg(ch)
			p := newEncPair(t, cfg)
			frameSize := cfg.Fs / 50 // 20 ms
			const frames = 40        // > 800 ms: onset (frame 10), cap (frame 30), re-onset
			silence := genSilence(frameSize*frames, ch)

			short, full := 0, 0
			firstShort, fullAfterShort := -1, -1
			for i := 0; i < frames; i++ {
				off := i * frameSize * ch
				ret, _ := p.frame("dtx-silence", silence[off:off+frameSize*ch], frameSize, 1500)
				switch {
				case ret == 1:
					short++
					if firstShort < 0 {
						firstShort = i
					}
				default:
					full++
					if firstShort >= 0 && fullAfterShort < 0 {
						fullAfterShort = i
					}
				}
			}

			// Non-vacuity: DTX must have fired, but not before the onset, and a full
			// frame must reappear after a DTX run (the consecutive-DTX cap re-arm).
			if short == 0 {
				t.Fatalf("no 1-byte DTX packet across %d silent frames", frames)
			}
			if firstShort < 10 {
				t.Errorf("first DTX packet at frame %d, want >= 10 (200 ms onset)", firstShort)
			}
			if fullAfterShort < 0 {
				t.Errorf("no full frame after a DTX run; the consecutive-DTX cap (%d) never forced one",
					maxConsecutiveDTXFrames)
			}
		})
	}
}

// TestOpusencDTXTransitions feeds silence -> signal -> silence. The signal block
// resets the no-activity counter, so DTX must stop during it and re-onset only
// after another ~200 ms of the trailing silence. The per-frame gate proves the
// counter (nb_no_activity_ms_Q1) tracks C exactly through the resets.
func TestOpusencDTXTransitions(t *testing.T) {
	cfg := dtxCfg(1)
	p := newEncPair(t, cfg)
	frameSize := cfg.Fs / 50

	// block encodes a run and returns how many frames were DTX'd and the index of
	// the FIRST DTX frame within the block (-1 if none).
	block := func(pcm []int16, label string) (short, firstShort int) {
		firstShort = -1
		frames := len(pcm) / frameSize
		for i := 0; i < frames; i++ {
			off := i * frameSize
			ret, _ := p.frame(label, pcm[off:off+frameSize], frameSize, 1500)
			if ret == 1 {
				short++
				if firstShort < 0 {
					firstShort = i
				}
			}
		}
		return short, firstShort
	}

	// 20 silent frames (400 ms): onset then a DTX run.
	s1, _ := block(genSilence(frameSize*20, 1), "silence-1")
	// 15 frames of audible noise: no DTX, and the counter resets to 0.
	sig, _ := block(genNoise(frameSize*15, 1, 0x1234), "signal")
	// 20 more silent frames: DTX must re-onset FROM SCRATCH.
	_, firstS2 := block(genSilence(frameSize*20, 1), "silence-2")

	if s1 == 0 {
		t.Errorf("first silence block produced no DTX packets")
	}
	if sig != 0 {
		t.Errorf("audible signal produced %d DTX packets, want 0", sig)
	}
	// A first DTX index >= 10 in silence-2 proves the counter actually RESET during
	// the signal block: had it not, DTX would resume almost immediately. s2==0 (no
	// re-arm) is caught by the same check (firstS2 stays -1).
	if firstS2 < 10 {
		t.Errorf("trailing silence re-onset at frame %d, want >= 10 (counter did not reset to 0 during signal)", firstS2)
	}
}

// TestOpusencDTXNearSilenceDoesNotFire proves DTX fires only on EXACT digital
// silence: -80 dBFS noise and a pure-DC offset are both non-silent, so no DTX
// packet may appear, while the per-frame gate still holds peak_signal_energy
// bit-exact against C across the whole run (the defense-in-depth the state-hash
// inclusion buys).
func TestOpusencDTXNearSilenceDoesNotFire(t *testing.T) {
	cfg := dtxCfg(1)
	frameSize := cfg.Fs / 50
	const frames = 40

	cases := []struct {
		name string
		pcm  []int16
	}{
		{"nearsilence-80dbfs", gainPCM(genNoise(frameSize*frames, 1, 0x5EED), 3.28/9000)},
		{"dc", offsetPCM(genSilence(frameSize*frames, 1), 8000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newEncPair(t, cfg)
			for i := 0; i < frames; i++ {
				off := i * frameSize
				ret, _ := p.frame(tc.name, tc.pcm[off:off+frameSize], frameSize, 1500)
				if ret == 1 {
					t.Fatalf("frame %d: non-silent %s produced a 1-byte DTX packet", i, tc.name)
				}
			}
		})
	}
}

// TestOpusencDTXCBRSilence exercises DTX under hard CBR. The DTX decision returns
// its 1-byte packet BEFORE opus_packet_pad, so a DTX'd frame is 1 byte even under
// CBR, while its neighbours pad to the CBR budget. The gate proves the exact
// interleaving against C.
func TestOpusencDTXCBRSilence(t *testing.T) {
	cfg := dtxCfg(1)
	cfg.VBR = 0 // hard CBR
	cfg.VBRConstraint = 0
	cfg.Bitrate = 32000
	p := newEncPair(t, cfg)
	frameSize := cfg.Fs / 50
	const frames = 40

	silence := genSilence(frameSize*frames, 1)
	short := 0
	for i := 0; i < frames; i++ {
		off := i * frameSize
		ret, _ := p.frame("dtx-cbr", silence[off:off+frameSize], frameSize, 1500)
		if ret == 1 {
			short++
		}
	}
	if short == 0 {
		t.Fatalf("no 1-byte DTX packet under CBR across %d silent frames", frames)
	}
}

// TestOpusencDTXMultiframeVBR runs the multiframe path (40/60/120 ms, split into
// 20 ms sub-frames) over long silence under VBR. DTX is decided PER sub-frame, so
// the per-frame gate proves the sub-frame is_silence recomputation and the
// repacketizer assembly of DTX'd sub-frames match C byte for byte.
func TestOpusencDTXMultiframeVBR(t *testing.T) {
	for _, ms := range []int{40, 60, 120} {
		ms := ms
		t.Run(fmt.Sprintf("%dms", ms), func(t *testing.T) {
			cfg := dtxCfg(1)
			p := newEncPair(t, cfg)
			frameSize := (cfg.Fs / 1000) * ms // e.g. 60 ms -> 2880 at 48 kHz
			const calls = 20
			silence := genSilence(frameSize*calls, 1)
			firstLen, minLen := -1, 1<<30
			for i := 0; i < calls; i++ {
				off := i * frameSize
				ret, _ := p.frame("dtx-mf-vbr", silence[off:off+frameSize], frameSize, 8000)
				if firstLen < 0 {
					firstLen = ret // call 0 is pre-onset: every sub-frame is a full packet
				}
				if ret < minLen {
					minLen = ret
				}
			}
			// Non-vacuity: once every sub-frame of a call is DTX'd the packet collapses
			// to the repacketized 1-byte sub-frames, strictly smaller than the pre-onset
			// packet. Without this a shared-config bug that disabled DTX on BOTH encoders
			// would compare equal and pass while proving nothing.
			if minLen >= firstLen {
				t.Fatalf("no DTX collapse across %d silent %d ms calls: min=%d, pre-onset=%d",
					calls, ms, minLen, firstLen)
			}
		})
	}
}

// TestOpusencDTXMultiframeCBRPadding pins the pad = !use_vbr && (dtx_count !=
// nb_frames) rule (opus_encoder.c:1831): under CBR a multiframe packet whose EVERY
// sub-frame was DTX'd is left UNPADDED (tiny), while a padded packet fills the CBR
// budget. The gate holds every byte against C; the size collapse from the largest
// to the smallest packet is the non-vacuity proof that the all-DTX branch ran.
func TestOpusencDTXMultiframeCBRPadding(t *testing.T) {
	cfg := dtxCfg(1)
	cfg.VBR = 0
	cfg.VBRConstraint = 0
	cfg.Bitrate = 48000
	p := newEncPair(t, cfg)
	frameSize := (cfg.Fs / 1000) * 60 // 60 ms -> 3 sub-frames
	const calls = 30
	silence := genSilence(frameSize*calls, 1)

	minLen, maxLen := 1<<30, 0
	for i := 0; i < calls; i++ {
		off := i * frameSize
		ret, _ := p.frame("dtx-mf-cbr", silence[off:off+frameSize], frameSize, 8000)
		if ret < minLen {
			minLen = ret
		}
		if ret > maxLen {
			maxLen = ret
		}
	}
	// A padded CBR 60 ms packet at 48 kbps is ~360 bytes; a fully-DTX one is a
	// handful. Require a large collapse so the all-DTX (unpadded) branch is proven
	// to have fired, not just a couple of DTX'd sub-frames inside a padded packet.
	if maxLen < 100 {
		t.Fatalf("largest CBR multiframe packet was %d bytes, expected a padded (~360 B) packet", maxLen)
	}
	if minLen > 20 {
		t.Fatalf("smallest CBR multiframe packet was %d bytes; an all-DTX packet should be unpadded (< 20 B)", minLen)
	}
}

// maxConsecutiveDTXFrames mirrors internal/opusenc's unexported maxConsecutiveDTX
// (libopus MAX_CONSECUTIVE_DTX) for the non-vacuity message above; kept local so
// the test does not import an unexported constant.
const maxConsecutiveDTXFrames = 20
