//go:build refc

package oracle

// Sequence-based differential test for the pure-Go SILK packet-loss concealment and
// comfort-noise generation (internal/silk plc.go + cng.go, wired into decode_frame.go)
// against the pinned libopus C (silk/PLC.c + silk/CNG.c + the loss branch of
// silk/decode_frame.c), driven by real SILK bitstreams the C SILK encoder produces.
//
// SILK concealment carries cross-frame state of its own (sPLC: LTP taps, pitch lag,
// bandwidth-expanded LPC, random seed/scale, concealed-frame energy; sCNG: smoothed
// background LSFs/gain, excitation buffer, synth state, random seed), on top of the
// core synthesis state. Per hard-parts.md 5 a single-frame test cannot validate it, so
// this test encodes multi-second PCM, then decodes each packet with BOTH the C and Go
// decoders in lockstep while marking CHOSEN packets LOST on both sides (whole-packet
// loss keeps the per-packet range coder in sync). After EVERY frame (concealed AND
// recovery) it asserts the decoded int16 output is bit-identical and a hash of the
// persistent decoder state INCLUDING sPLC and sCNG matches, so a divergence is caught
// on the frame it first appears. Coverage spans NB/MB/WB, 10/20/40/60 ms, isolated
// single losses and bursts of 2-3 packets (the energy fade over consecutive losses),
// voiced (LTP-extrapolation PLC), unvoiced and inactive (CNG) signals, and the
// glue-frames recovery. TestSilkPLCSequenceMutationDetected proves the assertions are
// not vacuous.
//
// The PCM generators (scVoiced, scNoise, scSilence, scSweep, scMixed) and the small
// print helpers (firstOutputDiff, sprintDiff, itoa) plus the Go decoder setup
// (newSilkcoreGoDec) are shared with silkcore_test.go (same package); only the
// loss-aware driver, the extended state hash and the loss patterns are new here.

import (
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// silkplcGoHash reproduces the C oracle_silkplc_state_hash FNV-1a over the Go decoder
// state, in the identical canonical field order (the silkcore synthesis state, then
// the full sPLC and sCNG sub-state), so the two hashes compare directly.
func silkplcGoHash(d *silk.DecoderState) uint64 {
	h := uint64(14695981039346656037)
	add := func(v int64) {
		x := uint64(v)
		for b := 0; b < 8; b++ {
			h ^= (x >> (8 * b)) & 0xff
			h *= 1099511628211
		}
	}

	for i := range d.SLPCQ14Buf {
		add(int64(d.SLPCQ14Buf[i]))
	}
	for i := range d.OutBuf {
		add(int64(d.OutBuf[i]))
	}
	for i := range d.ExcQ14 {
		add(int64(d.ExcQ14[i]))
	}
	for i := range d.PrevNLSFQ15 {
		add(int64(d.PrevNLSFQ15[i]))
	}

	add(int64(d.LagPrev))
	add(int64(d.LastGainIndex))
	add(int64(d.PrevGainQ16))
	add(int64(d.PrevSignalType))
	add(int64(d.LossCnt))
	add(int64(d.FirstFrameAfterReset))
	add(int64(d.ECPrevSignalType))
	add(int64(d.ECPrevLagIndex))

	ix := &d.Indices
	add(int64(ix.SignalType))
	add(int64(ix.QuantOffsetType))
	for i := range ix.GainsIndices {
		add(int64(ix.GainsIndices[i]))
	}
	for i := range ix.LTPIndex {
		add(int64(ix.LTPIndex[i]))
	}
	for i := range ix.NLSFIndices {
		add(int64(ix.NLSFIndices[i]))
	}
	add(int64(ix.LagIndex))
	add(int64(ix.ContourIndex))
	add(int64(ix.NLSFInterpCoefQ2))
	add(int64(ix.PERIndex))
	add(int64(ix.LTPScaleIndex))
	add(int64(ix.Seed))

	// sPLC
	p := &d.SPLC
	add(int64(p.PitchLQ8))
	for i := range p.LTPCoefQ14 {
		add(int64(p.LTPCoefQ14[i]))
	}
	for i := range p.PrevLPCQ12 {
		add(int64(p.PrevLPCQ12[i]))
	}
	add(int64(p.LastFrameLost))
	add(int64(p.RandSeed))
	add(int64(p.RandScaleQ14))
	add(int64(p.ConcEnergy))
	add(int64(p.ConcEnergyShift))
	add(int64(p.PrevLTPScaleQ14))
	add(int64(p.PrevGainQ16[0]))
	add(int64(p.PrevGainQ16[1]))
	add(int64(p.FsKHz))
	add(int64(p.NbSubfr))
	add(int64(p.SubfrLength))

	// sCNG
	c := &d.SCNG
	for i := range c.CNGExcBufQ14 {
		add(int64(c.CNGExcBufQ14[i]))
	}
	for i := range c.CNGSmthNLSFQ15 {
		add(int64(c.CNGSmthNLSFQ15[i]))
	}
	for i := range c.CNGSynthState {
		add(int64(c.CNGSynthState[i]))
	}
	add(int64(c.CNGSmthGainQ16))
	add(int64(c.RandSeed))
	add(int64(c.FsKHz))

	return h
}

// goDecodePlcPacket replays the mono silk_Decode driver over one packet with the
// pure-Go decoder. When lost is true the packet is concealed (FLAG_PACKET_LOST, no
// header parse, no range decoding) for each of its frames; otherwise the VAD + LBRR
// header is parsed and each frame decoded normally. It returns one silkplcFrame per
// SILK frame.
func goDecodePlcPacket(t *testing.T, d *silk.DecoderState, buf []byte, nFramesPerPacket int, lost bool) []silkplcFrame {
	t.Helper()
	d.NFramesDecoded = 0
	var dec rangecoding.Decoder
	if lost {
		dec.Init([]byte{0, 0}) // dummy; the loss branch never consults it
	} else {
		dec.Init(buf)
		for i := 0; i < nFramesPerPacket; i++ {
			d.VADFlags[i] = dec.DecBitLogp(1)
		}
		if lbrr := dec.DecBitLogp(1); lbrr != 0 {
			t.Fatalf("unexpected LBRR flag set (FEC disabled)")
		}
	}

	frames := make([]silkplcFrame, nFramesPerPacket)
	pOut := make([]int16, d.FrameLength)
	for n := 0; n < nFramesPerPacket; n++ {
		condCoding := 0 // CODE_INDEPENDENTLY
		if d.NFramesDecoded > 0 {
			condCoding = 2 // CODE_CONDITIONALLY
		}
		lostFlag := 0
		if lost {
			lostFlag = 1 // FLAG_PACKET_LOST
		}
		silk.DecodeFrame(d, &dec, pOut, lostFlag, condCoding)
		d.NFramesDecoded++

		out := make([]int16, d.FrameLength)
		copy(out, pOut[:d.FrameLength])
		rng := uint32(0)
		tell := 0
		if !lost {
			rng = dec.Rng()
			tell = dec.Tell()
		}
		frames[n] = silkplcFrame{
			out:                  out,
			signalType:           int(d.Indices.SignalType),
			quantOffsetType:      int(d.Indices.QuantOffsetType),
			stateHash:            silkplcGoHash(d),
			rng:                  rng,
			tell:                 tell,
			prevGainQ16:          d.PrevGainQ16,
			lagPrev:              d.LagPrev,
			lastGainIndex:        int(d.LastGainIndex),
			prevSignalType:       d.PrevSignalType,
			firstFrameAfterReset: d.FirstFrameAfterReset,
			lossCnt:              d.LossCnt,
			plcLastFrameLost:     d.SPLC.LastFrameLost,
			plcRandSeed:          d.SPLC.RandSeed,
			cngRandSeed:          d.SCNG.RandSeed,
			lost:                 lost,
		}
	}
	return frames
}

// plcFramesEqual reports whether a C-decoded frame and a Go-decoded frame agree on the
// decoded samples and the observable end/persistent state. rng/tell are only compared
// for received frames (they are zeroed for concealed frames, whose range coder is not
// consulted).
func plcFramesEqual(c, g silkplcFrame) bool {
	if len(c.out) != len(g.out) {
		return false
	}
	for i := range c.out {
		if c.out[i] != g.out[i] {
			return false
		}
	}
	return c.stateHash == g.stateHash && c.rng == g.rng && c.tell == g.tell &&
		c.signalType == g.signalType && c.quantOffsetType == g.quantOffsetType &&
		c.prevGainQ16 == g.prevGainQ16 && c.lagPrev == g.lagPrev &&
		c.lastGainIndex == g.lastGainIndex && c.prevSignalType == g.prevSignalType &&
		c.firstFrameAfterReset == g.firstFrameAfterReset && c.lossCnt == g.lossCnt &&
		c.plcLastFrameLost == g.plcLastFrameLost && c.plcRandSeed == g.plcRandSeed &&
		c.cngRandSeed == g.cngRandSeed
}

// plcLossPattern marks whole packets lost: packet 2 is an isolated single loss,
// packets 5-6 a burst of two, packets 9-11 a burst of three. Packets 0-1 stay good so
// the synthesis state is populated before the first loss, and a good packet follows
// each loss so the recovery (glue-frames) path is exercised.
func plcLossPattern(nPackets int) map[int]bool {
	lost := map[int]bool{}
	mark := func(idxs ...int) {
		for _, i := range idxs {
			if i >= 0 && i < nPackets {
				lost[i] = true
			}
		}
	}
	mark(2)
	mark(5, 6)
	mark(9, 10, 11)
	return lost
}

// TestSilkPLCSequenceMatchesC is the primary deliverable: across every internal rate,
// frame length and signal type, the C SILK encoder produces real bitstreams and the
// pure-Go decoder matches the C bit-for-bit on every decoded sample and on the
// per-frame persistent-state hash (including sPLC/sCNG), over multi-second sequences
// with chosen packets marked lost, on both the concealed frames and the recovery
// frames.
func TestSilkPLCSequenceMatchesC(t *testing.T) {
	type genSpec struct {
		name string
		gen  func(fsHz, n int) []int16
	}
	gens := []genSpec{
		{"voiced", scVoiced},
		{"noise", scNoise(0x51CC0)},
		{"silence", scSilence},
		{"sweep", scSweep},
		{"mixed", scMixed},
	}
	fsRates := []int{8, 12, 16}
	payloads := []int{10, 20, 40, 60}
	const durationMs = 1500

	// Coverage flags: assert the interesting branches actually ran.
	var (
		frames                          int
		concealedFrames, recoveryFrames int
		maxLossCnt                      int
		rateSeen                        = map[int]bool{}
		nb2Seen, nb4Seen                bool
		voicedConceal                   bool
		unvoicedConceal                 bool
		inactiveConceal                 bool
	)

	for _, fs := range fsRates {
		for _, payload := range payloads {
			for _, gs := range gens {
				bitrate := (fs / 8) * 12000 // ~12k (NB) .. 24k (WB)
				ctx := newSilkplcCtx(fs, payload, bitrate, 5, 0)
				if ctx == nil {
					t.Fatalf("newSilkplcCtx(%d,%d) failed", fs, payload)
				}

				nPackets := durationMs / payload
				samplesPerPacket := payload * fs // payload_ms * fs_kHz
				pcm := gs.gen(fs*1000, nPackets*samplesPerPacket)
				lossPattern := plcLossPattern(nPackets)

				goDec := newSilkcoreGoDec(fs, ctx.nFramesPerPacket, ctx.nbSubfr)
				prevPacketLost := false

				for p := 0; p < nPackets; p++ {
					chunk := pcm[p*samplesPerPacket : (p+1)*samplesPerPacket]
					buf := ctx.encode(chunk)
					if buf == nil {
						continue // DTX / empty packet (useDTX is off, so rare)
					}
					if got := ctx.internalRate(); got != fs*1000 {
						t.Fatalf("fs=%d: encoder chose internal rate %d, want %d", fs, got, fs*1000)
					}

					lost := lossPattern[p]
					var cFrames, goFrames []silkplcFrame
					if lost {
						cFrames = ctx.decodePacket(nil, true)
						goFrames = goDecodePlcPacket(t, goDec, nil, ctx.nFramesPerPacket, true)
					} else {
						cFrames = ctx.decodePacket(buf, false)
						goFrames = goDecodePlcPacket(t, goDec, buf, ctx.nFramesPerPacket, false)
					}
					if len(cFrames) != len(goFrames) {
						t.Fatalf("fs=%d payload=%d gen=%s pkt=%d: frame count C=%d Go=%d",
							fs, payload, gs.name, p, len(cFrames), len(goFrames))
					}

					for f := range cFrames {
						c, g := cFrames[f], goFrames[f]
						if !plcFramesEqual(c, g) {
							t.Fatalf("MISMATCH fs=%d payload=%d gen=%s pkt=%d frame=%d (global %d) lost=%v:\n"+
								"  sig C=%d G=%d  hash C=%016x G=%016x  rng C=%08x G=%08x  tell C=%d G=%d\n"+
								"  lossCnt C=%d G=%d  prevSig C=%d G=%d  plcSeed C=%d G=%d  cngSeed C=%d G=%d\n"+
								"  prevGain C=%d G=%d  lagPrev C=%d G=%d  lastFrameLost C=%d G=%d  firstDiff=%s",
								fs, payload, gs.name, p, f, frames, lost,
								c.signalType, g.signalType, c.stateHash, g.stateHash, c.rng, g.rng, c.tell, g.tell,
								c.lossCnt, g.lossCnt, c.prevSignalType, g.prevSignalType, c.plcRandSeed, g.plcRandSeed,
								c.cngRandSeed, g.cngRandSeed, c.prevGainQ16, g.prevGainQ16, c.lagPrev, g.lagPrev,
								c.plcLastFrameLost, g.plcLastFrameLost, firstOutputDiff(c.out, g.out))
						}

						// Coverage bookkeeping (drive off the matched result).
						frames++
						rateSeen[fs] = true
						if ctx.nbSubfr == 2 {
							nb2Seen = true
						} else {
							nb4Seen = true
						}
						if g.lost {
							concealedFrames++
							if g.lossCnt > maxLossCnt {
								maxLossCnt = g.lossCnt
							}
							// prevSignalType selects the concealment flavor.
							switch g.prevSignalType {
							case 2:
								voicedConceal = true
							case 1:
								unvoicedConceal = true
							case 0:
								inactiveConceal = true
							}
						} else if prevPacketLost && f == 0 {
							recoveryFrames++
						}
					}
					prevPacketLost = lost
				}
				ctx.close()
			}
		}
	}

	// Non-vacuity: assert the branches we care about were exercised.
	if frames < 1000 {
		t.Fatalf("too few frames decoded (%d); sequence coverage is suspect", frames)
	}
	if concealedFrames < 100 {
		t.Errorf("too few concealed frames (%d)", concealedFrames)
	}
	if recoveryFrames < 20 {
		t.Errorf("too few recovery frames (%d); glue-frames path under-covered", recoveryFrames)
	}
	if maxLossCnt < 2 {
		t.Errorf("burst loss never drove lossCnt past the fade saturation (max %d)", maxLossCnt)
	}
	for _, fs := range fsRates {
		if !rateSeen[fs] {
			t.Errorf("rate %d kHz never exercised", fs)
		}
	}
	if !nb2Seen {
		t.Errorf("10 ms (nb_subfr=2) frames never exercised")
	}
	if !nb4Seen {
		t.Errorf("20/40/60 ms (nb_subfr=4) frames never exercised")
	}
	if !voicedConceal {
		t.Errorf("voiced (LTP-extrapolation) concealment never exercised")
	}
	if !unvoicedConceal {
		t.Errorf("unvoiced concealment never exercised")
	}
	if !inactiveConceal {
		t.Errorf("inactive (CNG) concealment never exercised")
	}
	t.Logf("decoded %d frames (%d concealed, %d recovery, maxLossCnt=%d) bit-exact across "+
		"NB/MB/WB, 10/20/40/60 ms, voiced/unvoiced/inactive with isolated + burst loss",
		frames, concealedFrames, recoveryFrames, maxLossCnt)
}

// TestSilkPLCSequenceMutationDetected proves the differential comparison is not
// vacuous: after concealing a real lost frame with both C and Go (which must agree), a
// deliberate one-bit perturbation of the Go concealed output, and separately of the Go
// state hash (which now covers sPLC/sCNG), is detected by plcFramesEqual.
func TestSilkPLCSequenceMutationDetected(t *testing.T) {
	ctx := newSilkplcCtx(16, 20, 24000, 5, 0)
	if ctx == nil {
		t.Fatal("newSilkplcCtx failed")
	}
	defer ctx.close()
	goDec := newSilkcoreGoDec(16, ctx.nFramesPerPacket, ctx.nbSubfr)

	samplesPerPacket := 20 * 16
	pcm := scVoiced(16000, 8*samplesPerPacket)

	var c, g silkplcFrame
	got := false
	// Decode a few good packets to populate state, then lose one and capture the
	// concealed frame from both decoders.
	for p := 0; p < 8; p++ {
		buf := ctx.encode(pcm[p*samplesPerPacket : (p+1)*samplesPerPacket])
		if buf == nil {
			continue
		}
		lost := p == 4
		if lost {
			cFrames := ctx.decodePacket(nil, true)
			goFrames := goDecodePlcPacket(t, goDec, nil, ctx.nFramesPerPacket, true)
			c, g = cFrames[len(cFrames)-1], goFrames[len(goFrames)-1]
			got = true
		} else {
			ctx.decodePacket(buf, false)
			goDecodePlcPacket(t, goDec, buf, ctx.nFramesPerPacket, false)
		}
	}
	if !got {
		t.Fatal("no concealed frame produced")
	}
	if !g.lost {
		t.Fatal("captured frame was not a concealed frame")
	}
	if !plcFramesEqual(c, g) {
		t.Fatal("baseline C/Go concealed frame unexpectedly differ")
	}
	if len(g.out) == 0 {
		t.Fatal("empty concealed output frame")
	}

	// Mutate one concealed output sample: must now be detected.
	mOut := g
	mOut.out = append([]int16(nil), g.out...)
	mOut.out[0] ^= 1
	if plcFramesEqual(c, mOut) {
		t.Fatal("mutation of a concealed output sample was NOT detected")
	}

	// Mutate the persistent-state hash (covers sPLC/sCNG): must now be detected.
	mHash := g
	mHash.stateHash ^= 1
	if plcFramesEqual(c, mHash) {
		t.Fatal("mutation of the state hash was NOT detected")
	}
}
