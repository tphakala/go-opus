//go:build refc

package oracle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tphakala/go-opus/internal/packet"
	"github.com/tphakala/go-opus/opus"
)

// CP10: THE "INVISIBLE TO THE PACKET GATE" AUDIT.
//
// A bit-exact packet gate cannot see anything that is not in a packet. Every test
// in this repo that compares encoder output byte for byte, and every test that
// compares decoded PCM sample for sample, is structurally blind to a whole class of
// consumer-visible values: the ones the codec REPORTS rather than CODES. That blind
// spot has already shipped one real bug here (the encoder lookahead was 192 instead
// of 312, because the Fs/400 term was missing; it would have misaligned every Ogg
// stream by 120 samples and every packet in the gate would still have matched).
//
// So each such value gets its own differential assertion against the C. This file
// covers the ones the oggopus end-to-end alignment test does not:
//
//   - opus.PacketFrames  vs opus_packet_get_nb_frames   (src/opus_decoder.c:1273)
//   - opus.PacketDuration vs opus_packet_get_nb_samples (:1289), including the
//     internal packet.Samples across the whole Fs domain, because the "over 120 ms"
//     rejection is Fs-dependent and 48 kHz alone never reaches it;
//   - Decoder.LastPacketDuration vs OPUS_GET_LAST_PACKET_DURATION (:1167), which is
//     asserted NOWHERE else and is written on five distinct paths in
//     opus_decode_native that the PCM comparison cannot tell apart;
//   - Reset() on both the encoder and the decoder, asserted on PACKETS and PCM
//     rather than on state, and against the C's own OPUS_RESET_STATE.
//
// (The Ogg pre-skip, samples48k, the per-page granules, the final granule and the
// stream duration are covered end to end by oggopus/preskip_test.go; the encoder
// lookahead and the complexity zero value by opusenc_lookahead_test.go and
// opus_publicapi_enc_test.go. This file deliberately does not duplicate them.)

// ---------------------------------------------------------------------------
// opus.PacketFrames / opus.PacketDuration vs the C.
// ---------------------------------------------------------------------------

// synthPacket builds an n-byte packet whose first byte is toc and whose second, if
// n > 1, is b1; the rest is deterministic filler. Neither opus_packet_get_nb_frames
// nor opus_packet_get_nb_samples looks past byte 1 (the frame count lives in the
// TOC's low two bits and, for code 3, in the second byte), so the filler only has
// to exist. n == 0 returns an empty packet, the OPUS_BAD_ARG case.
func synthPacket(toc, b1 byte, n int) []byte {
	p := make([]byte, n)
	if n > 0 {
		p[0] = toc
	}
	if n > 1 {
		p[1] = b1
	}
	for i := 2; i < n; i++ {
		p[i] = byte(0x5A + i)
	}
	return p
}

// synthPackets enumerates the whole TOC domain: all 32 configurations x
// {mono,stereo} x the four frame-count codes, at several lengths, and for code 3
// (the only code that reads a second byte) the interesting frame counts and both
// flag bits.
//
// This is what "synthetic TOC bytes covering all configs" has to mean for these two
// functions. The counts are chosen for their boundaries, not for coverage theatre:
// 0 is a legal code-3 count that yields ZERO frames (and must not be mistaken for an
// error), 48 is exactly 120 ms at 2.5 ms per frame and must PASS, and 49 is the
// first count that must be REJECTED as over-long by opus_packet_get_nb_samples. The
// short lengths reach the truncation rejections: length 0 is OPUS_BAD_ARG, and a
// 1-byte code-3 packet is OPUS_INVALID_PACKET.
func synthPackets() [][]byte {
	out := make([][]byte, 0, 12288)
	out = append(out, nil, []byte{}) // len < 1: OPUS_BAD_ARG
	for toc := range 256 {
		if toc&0x3 != 3 {
			for _, n := range []int{1, 2, 3, 10} {
				out = append(out, synthPacket(byte(toc), 0, n))
			}
			continue
		}
		for _, count := range []int{0, 1, 2, 3, 6, 24, 47, 48, 49, 62, 63} {
			// 0x80 is the code-3 VBR flag, 0x40 the padding flag; neither changes the
			// frame count, and asserting that against the C is the point.
			for _, flags := range []int{0x00, 0x40, 0x80, 0xC0} {
				for _, n := range []int{1, 2, 3, 12} {
					out = append(out, synthPacket(byte(toc), byte(flags|count), n))
				}
			}
		}
	}
	return out
}

// rfcVectorPackets returns every packet of every RFC 8251 vector bitstream, i.e.
// the real-world packet corpus the decoder gate already runs on. The packet layer
// is exercised against all of it today, but it has NEVER been compared to the C
// functions; this is what makes that comparison. Returns nil when the vectors are
// not fetched.
func rfcVectorPackets(t *testing.T) [][]byte {
	t.Helper()
	if !rfcVectorsPresent() {
		return nil
	}
	var out [][]byte
	for _, name := range rfcVectorNames {
		raw, err := os.ReadFile(filepath.Join(rfcVectorsDir, "testvector"+name+".bit")) //nolint:gosec // a fixed, checked-in test path
		if err != nil {
			t.Fatalf("read vector %s: %v", name, err)
		}
		// opus_demo record framing: a 4-byte big-endian payload length, a 4-byte
		// big-endian encoder final range, then the packet.
		for off := 0; off+8 <= len(raw); {
			n := int(binary.BigEndian.Uint32(raw[off:]))
			off += 8
			if n == 0 || off+n > len(raw) {
				break
			}
			out = append(out, raw[off:off+n:off+n])
			off += n
		}
	}
	return out
}

// checkCountVsC asserts that a Go (value, error) pair reproduces one C return code
// exactly. The C packs both into an int: a non-negative count, or a negative OPUS_*
// code. The Go functions split them, so the ERROR MAPPING is as much a part of what
// is being compared as the count: an implementation that returned ErrInvalidPacket
// where the C returns OPUS_BAD_ARG would agree on every count and still be wrong.
//
// errBadArg / errInvalidPacket are passed in because the public opus package and the
// internal packet package carry independent (non-wrapping) sentinels for the same
// two conditions, and both are checked here.
func checkCountVsC(t *testing.T, where string, cRet, gVal int, gErr, errBadArg, errInvalidPacket error) {
	t.Helper()
	switch {
	case cRet == oOpusBadArg:
		if !errors.Is(gErr, errBadArg) {
			t.Fatalf("%s: C returned OPUS_BAD_ARG; Go returned (%d, %v), want %v", where, gVal, gErr, errBadArg)
		}
	case cRet == oOpusInvalidPacket:
		if !errors.Is(gErr, errInvalidPacket) {
			t.Fatalf("%s: C returned OPUS_INVALID_PACKET; Go returned (%d, %v), want %v",
				where, gVal, gErr, errInvalidPacket)
		}
	case cRet < 0:
		t.Fatalf("%s: the C returned an unexpected error code %d", where, cRet)
	default:
		if gErr != nil {
			t.Fatalf("%s: C returned %d; Go failed with %v", where, cRet, gErr)
		}
		if gVal != cRet {
			t.Fatalf("%s: Go returned %d, C returned %d", where, gVal, cRet)
		}
	}
}

// packetOutcomes tallies WHICH of the C's return classes a corpus actually reached.
// A differential over a corpus that never reaches a rejection proves nothing about
// the rejections, and the two functions under test are almost entirely rejection
// logic, so the sweep below asserts on this tally rather than trusting the corpus.
type packetOutcomes struct {
	ok          int // a non-negative count from both sides
	badArg      int // OPUS_BAD_ARG (len < 1)
	invalid     int // OPUS_INVALID_PACKET (truncated code 3)
	overLong    int // frames parsed fine, but the duration exceeds 120 ms at 48 kHz
	zeroFrames  int // a legal code-3 packet declaring ZERO frames
	rateVaries  int // the duration genuinely differs between 8 kHz and 48 kHz
	rateSplit   int // accepted at one legal sample rate and rejected at another
	packetsSeen int

	distinctToCs map[byte]bool
}

// assertPacketFuncsMatchC compares, for one packet, everything the two C packet
// inspection functions report against everything the Go ones do, and records which
// outcome class the packet landed in.
func assertPacketFuncsMatchC(t *testing.T, where string, pkt []byte, o *packetOutcomes) {
	t.Helper()

	o.packetsSeen++
	if len(pkt) > 0 {
		o.distinctToCs[pkt[0]] = true
	}

	cFrames := PacketGetNbFrames(pkt)
	frames, err := opus.PacketFrames(pkt)
	checkCountVsC(t, where+": PacketFrames", cFrames, frames, err,
		opus.ErrBadArg, opus.ErrInvalidPacket)

	// The public PacketDuration is defined at 48 kHz (opus.go:104), the rate RFC 7845
	// expresses every duration at.
	c48 := PacketGetNbSamples(pkt, 48000)
	dur, err := opus.PacketDuration(pkt)
	checkCountVsC(t, where+": PacketDuration", c48, dur, err,
		opus.ErrBadArg, opus.ErrInvalidPacket)

	switch {
	case c48 == oOpusBadArg:
		o.badArg++
	case c48 == oOpusInvalidPacket && cFrames == oOpusInvalidPacket:
		o.invalid++
	case c48 == oOpusInvalidPacket:
		o.overLong++ // the frame count parsed; the 120 ms ceiling is what rejected it
	default:
		o.ok++
		if cFrames == 0 {
			o.zeroFrames++
		}
	}

	// The same C function one level down, across the whole Fs domain, because the
	// public PacketDuration only ever asks for 48 kHz and a Go implementation that
	// IGNORED fs entirely would still satisfy it. The duration itself is strongly
	// Fs-dependent (it scales 6:1 from 8 kHz to 48 kHz), and rateVaries below is the
	// guard that says so.
	//
	// The 120 ms REJECTION, on the other hand, is provably rate-INVARIANT at the five
	// legal rates: opus_packet_get_samples_per_frame is Fs/400 << shift, Fs/50, Fs/100
	// or Fs*60/1000, every one of which divides exactly at 8/12/16/24/48 kHz, so
	// `samples*25 > Fs*3` reduces to an inequality in the frame count alone. rateSplit
	// records any counterexample, and asserting that it stays ZERO pins that property
	// of the C rather than assuming it.
	var rejected, accepted bool
	var d8, d48 int
	for _, fs := range []int{8000, 12000, 16000, 24000, 48000} {
		c := PacketGetNbSamples(pkt, fs)
		n, err := packet.Samples(pkt, fs)
		checkCountVsC(t, fmt.Sprintf("%s: packet.Samples(fs=%d)", where, fs),
			c, n, err, packet.ErrBadArg, packet.ErrInvalidPacket)
		switch {
		case c >= 0:
			accepted = true
		case cFrames >= 0:
			rejected = true // the frame count parsed; only the duration ceiling rejected it
		}
		if fs == 8000 {
			d8 = c
		} else if fs == 48000 {
			d48 = c
		}
	}
	if rejected && accepted {
		o.rateSplit++
	}
	if d8 >= 0 && d48 >= 0 && d8 != d48 {
		o.rateVaries++
	}
}

// TestPacketInspectionMatchesC is the differential for opus.PacketFrames and
// opus.PacketDuration. The packet layer is exercised against every RFC bitstream by
// the conformance gate, but conformance only proves the packets DECODE; it never
// compares these two REPORTED values against the C, and a decoder that ignored them
// entirely would still pass it. Here they are compared directly, over the whole
// synthetic TOC domain and over every packet of every RFC vector.
func TestPacketInspectionMatchesC(t *testing.T) {
	newOutcomes := func() *packetOutcomes {
		return &packetOutcomes{distinctToCs: map[byte]bool{}}
	}

	t.Run("synthetic-toc", func(t *testing.T) {
		o := newOutcomes()
		for i, pkt := range synthPackets() {
			assertPacketFuncsMatchC(t, fmt.Sprintf("synthetic %d %#x", i, pkt), pkt, o)
		}
		t.Logf("%d synthetic packets over %d distinct TOC bytes: ok=%d bad-arg=%d "+
			"truncated=%d over-120ms=%d zero-frame=%d rate-varying-duration=%d",
			o.packetsSeen, len(o.distinctToCs), o.ok, o.badArg, o.invalid, o.overLong,
			o.zeroFrames, o.rateVaries)

		// Non-vacuity. Agreement on a corpus that only ever walks the happy path says
		// nothing about these two functions, which are almost entirely rejection logic.
		// Every branch the C can take must actually have been taken:
		if len(o.distinctToCs) != 256 {
			t.Errorf("the synthetic corpus covers %d of the 256 TOC bytes", len(o.distinctToCs))
		}
		for _, c := range []struct {
			n    int
			what string
		}{
			{o.ok, "packets both sides accepted"},
			{o.badArg, "OPUS_BAD_ARG rejections (an empty packet)"},
			{o.invalid, "OPUS_INVALID_PACKET rejections (a truncated code-3 packet)"},
			{o.overLong, "over-120 ms rejections (the duration ceiling)"},
			{o.zeroFrames, "legal code-3 packets declaring ZERO frames"},
			{o.rateVaries, "packets whose duration differs between 8 kHz and 48 kHz " +
				"(without which an implementation that ignored fs would pass)"},
		} {
			if c.n == 0 {
				t.Errorf("non-vacuity: the corpus produced no %s, so that branch is untested", c.what)
			}
		}
		// The positive form of the rate-invariance argument above: no packet is legal
		// at one of the five rates and over-long at another. If a future rate ever
		// broke the exact proportionality, this would be the first thing to fail.
		if o.rateSplit != 0 {
			t.Errorf("%d packets are accepted at one legal sample rate and rejected at "+
				"another; opus_packet_get_samples_per_frame divides exactly at all five "+
				"rates, so the 120 ms ceiling must not depend on fs", o.rateSplit)
		}
	})

	t.Run("rfc-vectors", func(t *testing.T) {
		pkts := rfcVectorPackets(t)
		if len(pkts) == 0 {
			t.Skip("RFC vectors not fetched; run scripts/fetch-vectors.sh")
		}
		o := newOutcomes()
		for i, pkt := range pkts {
			assertPacketFuncsMatchC(t,
				fmt.Sprintf("rfc packet %d (toc %#02x, %d bytes)", i, pkt[0], len(pkt)), pkt, o)
		}
		t.Logf("%d real RFC vector packets over %d distinct TOC bytes: C and Go agree on "+
			"frames and duration at every rate (ok=%d)", o.packetsSeen, len(o.distinctToCs), o.ok)
		if o.ok == 0 {
			t.Error("no RFC packet was accepted, which cannot be right")
		}
	})
}

// ---------------------------------------------------------------------------
// Decoder.LastPacketDuration vs OPUS_GET_LAST_PACKET_DURATION.
// ---------------------------------------------------------------------------

// lpdStep is one entry in the LastPacketDuration script: a forced mode and a frame
// size. Combinations the C encoder rejects are skipped, so the script can name the
// full cross product without knowing which cells are legal.
type lpdStep struct {
	name      string
	mode      int
	bandwidth int
	frameSize int
}

// TestDecoderLastPacketDurationMatchesC pins Decoder.LastPacketDuration, which is
// asserted NOWHERE else in the repo. It is not a derived convenience: libopus writes
// st->last_packet_duration on five separate paths (opus_decoder.c:776 the multi-frame
// loop, :806 and :812 the FEC save-and-restore, :829 the PLC path, :869 the normal
// path), and the decoded PCM is identical whichever of them ran, so the existing
// bit-exact decoder differential is blind to a mistake in any of them.
//
// The script drives all three coding modes, every frame duration from 2.5 ms to
// 60 ms (which forces the multi-frame code-3 packets, and with them the :776 path),
// plus PLC and FEC.
func TestDecoderLastPacketDurationMatchesC(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			t.Parallel()

			var script []lpdStep
			for _, m := range []struct {
				name string
				mode int
				bw   int
			}{
				{"SILK", oModeSilkOnly, oBandwidthWideband},
				{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
				{"CELT", oModeCeltOnly, oBandwidthFullband},
			} {
				for _, n := range []int{120, 240, 480, 960, 1920, 2880} {
					script = append(script, lpdStep{m.name, m.mode, m.bw, n})
				}
			}

			// Enough PCM for the longest frame in the script, reused per step: the
			// value under test is a duration, not a function of the content.
			pcm := genPCM(1, 2880, channels)

			enc, err := NewOpusEncoder(48000, channels, 64000, 10, true)
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

			// A decoder that has decoded nothing reports 0 on both sides.
			if got, want := gDec.LastPacketDuration(), cDec.LastPacketDuration(); got != want {
				t.Fatalf("before any decode: LastPacketDuration Go %d, C %d", got, want)
			}

			// decodeBoth runs one packet (or a PLC / FEC request) through both decoders
			// with the same output-buffer frame size and compares the reported duration
			// against the C, along with the decode's own return value.
			decodeBoth := func(where string, pkt []byte, frameSize int, fec bool) {
				t.Helper()
				cOut, cerr := cDec.Decode(pkt, frameSize, fec)
				if cerr != nil {
					t.Fatalf("%s: C decode: %v", where, cerr)
				}
				gBuf := make([]int16, frameSize*channels)
				var n int
				var gerr error
				if fec {
					n, gerr = gDec.DecodeFEC(pkt, gBuf)
				} else {
					n, gerr = gDec.Decode(pkt, gBuf)
				}
				if gerr != nil {
					t.Fatalf("%s: Go decode: %v", where, gerr)
				}
				if got, want := n, len(cOut)/channels; got != want {
					t.Fatalf("%s: decoded %d samples/ch, C decoded %d", where, got, want)
				}
				cLPD, gLPD := cDec.LastPacketDuration(), gDec.LastPacketDuration()
				if gLPD != cLPD {
					t.Fatalf("%s: LastPacketDuration Go %d, C %d (OPUS_GET_LAST_PACKET_DURATION)",
						where, gLPD, cLPD)
				}
				// The reported duration must also BE the decode's own output length,
				// which is the property a caller actually relies on.
				if gLPD != n {
					t.Fatalf("%s: LastPacketDuration is %d but Decode returned %d samples/ch",
						where, gLPD, n)
				}
			}

			checked, skipped, multiFrame := 0, 0, 0
			for i, step := range script {
				where := fmt.Sprintf("%s/%d samples (step %d)", step.name, step.frameSize, i)
				pkt, err := enc.Encode(pcm, step.frameSize, step.mode, step.bandwidth)
				if err != nil {
					// Not every (mode, duration) pair is codable; the C says which.
					skipped++
					continue
				}
				if pkt == nil {
					continue // DTX
				}
				// Count the code-3 multi-frame packets. A 40 or 60 ms CELT frame makes
				// opus_encode split into 20 ms sub-frames and repacketize, which is the
				// ONLY way to reach the :776 assignment inside the multi-frame loop. The
				// claim that this script reaches that path is asserted below, not assumed.
				if nf, err := opus.PacketFrames(pkt); err == nil && nf > 1 {
					multiFrame++
				}

				// A generous buffer, so opus_decode's own clamp to the packet duration
				// (opus_decoder.c:911) is what decides the answer, on both sides.
				decodeBoth(where, pkt, 5760, false)
				checked++

				// FEC: the :806/:812 save-and-restore path. A CELT packet carries no
				// LBRR, so this falls through to PLC; both are worth pinning.
				decodeBoth(where+" fec", pkt, step.frameSize, true)
				checked++
			}

			// PLC (:829): the duration comes from the CALLER's buffer, not the packet,
			// which is exactly the kind of value a PCM comparison cannot check.
			for _, n := range []int{120, 240, 480, 960, 2880} {
				decodeBoth(fmt.Sprintf("PLC/%d samples", n), nil, n, false)
				checked++
			}

			// OPUS_RESET_STATE clears last_packet_duration (it lives inside the
			// OPUS_DECODER_RESET_START region, opus_decoder.c:80-87).
			if err := cDec.Reset(); err != nil {
				t.Fatalf("C decoder reset: %v", err)
			}
			gDec.Reset()
			if got, want := gDec.LastPacketDuration(), cDec.LastPacketDuration(); got != want || got != 0 {
				t.Fatalf("after Reset: LastPacketDuration Go %d, C %d (want 0 on both)", got, want)
			}

			if multiFrame == 0 {
				t.Error("non-vacuity: the script produced no multi-frame (code 3) packet, so " +
					"opus_decode_native's multi-frame path, the one that writes " +
					"last_packet_duration at opus_decoder.c:776, was never taken")
			}
			t.Logf("ch%d: %d LastPacketDuration comparisons match the C, over %d multi-frame "+
				"packets and every mode/duration/PLC/FEC path (%d mode/duration pairs the C "+
				"encoder rejects were skipped)", channels, checked, multiFrame, skipped)
		})
	}
}

// ---------------------------------------------------------------------------
// Reset(), asserted on packets and PCM rather than on state.
// ---------------------------------------------------------------------------

// TestEncoderResetPacketsMatchC asserts opus.Encoder.Reset against the C's
// OPUS_RESET_STATE, on the only evidence that matters to a caller: the PACKETS. A
// state comparison would prove that the two encoders agree about their own internals;
// this proves that a reset encoder ENCODES like a fresh one, and that it encodes like
// a reset C one.
//
// Three claims, and the third is what makes the first two non-vacuous:
//
//  1. after Reset, Go and C agree byte for byte, frame for frame (that is what
//     pubEncPair.frame checks on every call);
//  2. after Reset, the packets equal the ones a FRESH encoder produces from the same
//     PCM, which is the documented contract on opus.Encoder.Reset;
//  3. WITHOUT the reset, the same PCM must produce DIFFERENT packets. If it did not,
//     the encoder would carry no cross-frame state and claims 1 and 2 would hold for
//     a Reset that did nothing at all.
func TestEncoderResetPacketsMatchC(t *testing.T) {
	const frameSize, nFrames = 960, 6

	for _, channels := range []int{1, 2} {
		for _, rm := range []struct {
			name           string
			cbr, constrain bool
		}{
			{"vbr", false, false},
			{"cbr", true, false},
		} {
			t.Run(fmt.Sprintf("ch%d/%s", channels, rm.name), func(t *testing.T) {
				t.Parallel()
				cfg := opus.EncoderConfig{
					SampleRate:     48000,
					Channels:       channels,
					Bitrate:        64000,
					CBR:            rm.cbr,
					ConstrainedVBR: rm.constrain,
				}
				p := newPubEncPair(t, cfg)

				// Pass 1: the reference packets, from a fresh encoder pair.
				first := make([][]byte, nFrames)
				for i := range nFrames {
					first[i] = p.frame(fmt.Sprintf("pass1/frame%d", i),
						testPCM(frameSize, channels, i), frameSize)
				}

				// Pass 2, NO reset: both encoders now hold six frames of history, so the
				// same PCM cannot give the same packets. Go and C are still compared to
				// each other on every frame (that is p.frame's job), which incidentally
				// pins that they carry the SAME history.
				primed := make([][]byte, nFrames)
				for i := range nFrames {
					primed[i] = p.frame(fmt.Sprintf("pass2-primed/frame%d", i),
						testPCM(frameSize, channels, i), frameSize)
				}
				differs := false
				for i := range nFrames {
					if !bytes.Equal(first[i], primed[i]) {
						differs = true
						break
					}
				}
				if !differs {
					t.Fatal("non-vacuity: re-encoding the same PCM WITHOUT a reset produced " +
						"identical packets, so this test cannot tell whether Reset does anything")
				}

				// Pass 3, after Reset on both sides.
				if err := p.c.Reset(); err != nil {
					t.Fatalf("C OPUS_RESET_STATE: %v", err)
				}
				p.g.Reset()
				for i := range nFrames {
					got := p.frame(fmt.Sprintf("pass3-reset/frame%d", i),
						testPCM(frameSize, channels, i), frameSize)
					if !bytes.Equal(got, first[i]) {
						t.Fatalf("frame %d after Reset: %d bytes, want the %d-byte packet the "+
							"same encoder produced from the same PCM when it was fresh",
							i, len(got), len(first[i]))
					}
				}

				// Pass 4: an independently constructed encoder pair, i.e. "fresh" in the
				// strongest sense. A Reset that restored the wrong CONFIGURATION (rather
				// than the wrong state) would survive pass 3 and die here.
				fresh := newPubEncPair(t, cfg)
				for i := range nFrames {
					got := fresh.frame(fmt.Sprintf("pass4-fresh/frame%d", i),
						testPCM(frameSize, channels, i), frameSize)
					if !bytes.Equal(got, first[i]) {
						t.Fatalf("frame %d: a freshly constructed encoder disagrees with the "+
							"reset one (%d bytes vs %d)", i, len(got), len(first[i]))
					}
				}
			})
		}
	}
}

// TestDecoderResetPCMMatchesC is the decoder half of the same claim, and it is the
// half that was asserted nowhere at all: opus.Decoder.Reset is exercised by no test
// in the repo today. The evidence is the decoded PCM and the final range, compared
// against a C decoder given OPUS_RESET_STATE at the same point.
//
// The packet sequence deliberately crosses SILK<->hybrid<->CELT, because the state a
// reset has to clear is at its largest there: the CELT overlap-add and energy
// history, the SILK LPC/LTP and resampler memory, the mode/prev_mode transition
// machine and the redundancy crossfade all carry across packets.
func TestDecoderResetPCMMatchesC(t *testing.T) {
	script := []modeStep{
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"SILK", oModeSilkOnly, oBandwidthWideband},
		{"HYBRID", oModeHybrid, oBandwidthSuperwideband},
		{"CELT", oModeCeltOnly, oBandwidthFullband},
		{"SILK", oModeSilkOnly, oBandwidthWideband},
	}

	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			t.Parallel()
			const frameSize = 960

			// One fixed packet sequence, encoded once and replayed at every pass, so the
			// only thing that varies between passes is the decoder's own state.
			pcm := genPCM(len(script), frameSize, channels)
			enc, err := NewOpusEncoder(48000, channels, 64000, 10, false)
			if err != nil {
				t.Fatalf("NewOpusEncoder: %v", err)
			}
			defer enc.Close()
			pkts := make([][]byte, 0, len(script))
			for i, step := range script {
				in := pcm[i*frameSize*channels : (i+1)*frameSize*channels]
				pkt, err := enc.Encode(in, frameSize, step.mode, step.bandwidth)
				if err != nil {
					t.Fatalf("encode frame %d (%s): %v", i, step.name, err)
				}
				if pkt != nil {
					pkts = append(pkts, pkt)
				}
			}

			cDec, err := NewDecoder(48000, channels)
			if err != nil {
				t.Fatalf("C NewDecoder: %v", err)
			}
			defer cDec.Close()
			gDec, err := opus.NewDecoder(48000, channels)
			if err != nil {
				t.Fatalf("Go NewDecoder: %v", err)
			}

			// runPass decodes the whole sequence through both decoders, asserting C-vs-Go
			// agreement on every packet, and returns the Go output for cross-pass compares.
			runPass := func(pass string) ([][]int16, []uint32) {
				t.Helper()
				out := make([][]int16, len(pkts))
				ranges := make([]uint32, len(pkts))
				for i, pkt := range pkts {
					label := fmt.Sprintf("%s/packet%d", pass, i)
					cPCM, gPCM, cRng, gRng := decodePair(t, cDec, gDec, channels, pkt, false)
					assertPCMEqual(t, label, cPCM, gPCM)
					if cRng != gRng {
						t.Fatalf("%s: final range Go %#x, C %#x", label, gRng, cRng)
					}
					out[i] = slices.Clone(gPCM)
					ranges[i] = gRng
				}
				return out, ranges
			}

			first, firstRanges := runPass("pass1")

			// No reset: both decoders now hold the sequence's trailing state, so replaying
			// it cannot reproduce pass 1. (If it could, a no-op Reset would pass this test.)
			primed, _ := runPass("pass2-primed")
			differs := false
			for i := range first {
				if !slices.Equal(first[i], primed[i]) {
					differs = true
					break
				}
			}
			if !differs {
				t.Fatal("non-vacuity: replaying the same packets WITHOUT a reset reproduced " +
					"the same PCM, so this test cannot tell whether Reset does anything")
			}

			// Reset both, replay: the PCM and the final ranges must be the fresh ones.
			if err := cDec.Reset(); err != nil {
				t.Fatalf("C OPUS_RESET_STATE: %v", err)
			}
			gDec.Reset()
			after, afterRanges := runPass("pass3-reset")
			for i := range first {
				if !slices.Equal(first[i], after[i]) {
					t.Fatalf("packet %d after Reset: the decoded PCM differs from the "+
						"fresh-decoder PCM (%d samples vs %d)", i, len(after[i]), len(first[i]))
				}
				if afterRanges[i] != firstRanges[i] {
					t.Fatalf("packet %d after Reset: final range %#x, want %#x",
						i, afterRanges[i], firstRanges[i])
				}
			}
			t.Logf("ch%d: %d packets replay bit-identically after Reset, C and Go in lockstep",
				channels, len(pkts))
		})
	}
}
