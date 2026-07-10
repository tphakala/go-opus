//go:build refc

package oracle

// End-to-end sequence differential test for the pure-Go top-level SILK decoder
// (internal/silk dec_api.go silk.Decoder.Decode, tying together the per-channel frame
// decode, stereo un-mixing, the resampler, and the PLC/CNG and FEC/LBRR paths) against
// the pinned libopus C (silk/dec_API.c silk_Decode), driven by real SILK bitstreams the
// C SILK encoder (silk/enc_API.c silk_Encode) produces from generated PCM.
//
// SILK is a cross-frame / cross-packet state machine (per-channel LPC/LTP history, the
// stereo delay buffers, the resampler filter memory, and the PLC/CNG sub-states), so per
// hard-parts.md 5 only multi-second SEQUENCES with per-frame state hashes validate it.
// Each test drives BOTH the C silk_Decode and the Go silk.Decoder.Decode over identical
// packets/flags in lockstep the way src/opus_decoder.c opus_decode_frame does, asserting
// after EVERY SILK frame that the decoded int16 API-rate PCM is bit-identical and a hash
// of the FULL persistent decoder state (both channel states incl. resampler + PLC + CNG,
// the stereo state, and the super-struct scalars) matches, so a divergence is caught on
// the frame it first appears.
//
// Coverage:
//   - TestSilkDecAPISequenceMatchesC: mono + stereo, internal NB/MB/WB (8/12/16 kHz),
//     API rates 8/12/16/24/48 kHz (every resampler cell: copy, up, fractional-up and the
//     three down-sampling pairs), 10/20/40/60 ms packets, several signal types.
//   - TestSilkDecAPIFECAndPLCMatchesC: crafted loss sequences that conceal a lost packet
//     with PLC (FLAG_PACKET_LOST) and recover a lost packet from the next packet's LBRR
//     redundant frame (FLAG_DECODE_LBRR), mono and stereo, resample and copy.
//   - TestSilkDecAPIDTXMatchesC: DTX sequences whose silent gaps drive the loss/CNG path.
//   - TestSilkDecAPIMutationDetected proves the assertions are not vacuous.

import (
	"math"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// silkdecapiGoHash reproduces the C oracle_silkdecapi_state_hash FNV-1a over the whole
// Go silk.Decoder, in the identical canonical field order, so the two hashes compare
// directly. Each value is folded as its int64 little-endian byte sequence.
func silkdecapiGoHash(d *silk.Decoder) uint64 {
	h := uint64(14695981039346656037)
	add := func(v int64) {
		x := uint64(v)
		for b := 0; b < 8; b++ {
			h ^= (x >> (8 * b)) & 0xff
			h *= 1099511628211
		}
	}

	hashResampler := func(S *silk.ResamplerState) {
		for i := range S.SIIR {
			add(int64(S.SIIR[i]))
		}
		// SFIR is stored canonically (int32 view for down_FIR, sign-extended int16 for
		// IIR_FIR), matching the C read-through-view canonicalization.
		for i := range S.SFIR {
			add(int64(S.SFIR[i]))
		}
		for i := range S.DelayBuf {
			add(int64(S.DelayBuf[i]))
		}
		add(int64(S.ResamplerFunction))
		add(int64(S.BatchSize))
		add(int64(S.InvRatioQ16))
		add(int64(S.FIROrder))
		add(int64(S.FIRFracs))
		add(int64(S.FsInKHz))
		add(int64(S.FsOutKHz))
		add(int64(S.InputDelay))
	}

	hashChannel := func(c *silk.DecoderState) {
		add(int64(c.PrevGainQ16))
		for i := range c.ExcQ14 {
			add(int64(c.ExcQ14[i]))
		}
		for i := range c.SLPCQ14Buf {
			add(int64(c.SLPCQ14Buf[i]))
		}
		for i := range c.OutBuf {
			add(int64(c.OutBuf[i]))
		}
		add(int64(c.LagPrev))
		add(int64(c.LastGainIndex))
		add(int64(c.FsKHz))
		add(int64(c.FsAPIhz))
		add(int64(c.NbSubfr))
		add(int64(c.FrameLength))
		add(int64(c.SubfrLength))
		add(int64(c.LtpMemLength))
		add(int64(c.LPCOrder))
		for i := range c.PrevNLSFQ15 {
			add(int64(c.PrevNLSFQ15[i]))
		}
		add(int64(c.FirstFrameAfterReset))
		add(int64(c.NFramesDecoded))
		add(int64(c.NFramesPerPacket))
		add(int64(c.ECPrevSignalType))
		add(int64(c.ECPrevLagIndex))
		for i := range c.VADFlags {
			add(int64(c.VADFlags[i]))
		}
		add(int64(c.LBRRFlag))
		for i := range c.LBRRFlags {
			add(int64(c.LBRRFlags[i]))
		}

		hashResampler(&c.ResamplerState)

		ix := &c.Indices
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

		cng := &c.SCNG
		for i := range cng.CNGExcBufQ14 {
			add(int64(cng.CNGExcBufQ14[i]))
		}
		for i := range cng.CNGSmthNLSFQ15 {
			add(int64(cng.CNGSmthNLSFQ15[i]))
		}
		for i := range cng.CNGSynthState {
			add(int64(cng.CNGSynthState[i]))
		}
		add(int64(cng.CNGSmthGainQ16))
		add(int64(cng.RandSeed))
		add(int64(cng.FsKHz))

		add(int64(c.LossCnt))
		add(int64(c.PrevSignalType))

		plc := &c.SPLC
		add(int64(plc.PitchLQ8))
		for i := range plc.LTPCoefQ14 {
			add(int64(plc.LTPCoefQ14[i]))
		}
		for i := range plc.PrevLPCQ12 {
			add(int64(plc.PrevLPCQ12[i]))
		}
		add(int64(plc.LastFrameLost))
		add(int64(plc.RandSeed))
		add(int64(plc.RandScaleQ14))
		add(int64(plc.ConcEnergy))
		add(int64(plc.ConcEnergyShift))
		add(int64(plc.PrevLTPScaleQ14))
		add(int64(plc.PrevGainQ16[0]))
		add(int64(plc.PrevGainQ16[1]))
		add(int64(plc.FsKHz))
		add(int64(plc.NbSubfr))
		add(int64(plc.SubfrLength))
	}

	hashChannel(&d.ChannelState[0])
	hashChannel(&d.ChannelState[1])

	for i := range d.SStereo.PredPrevQ13 {
		add(int64(d.SStereo.PredPrevQ13[i]))
	}
	for i := range d.SStereo.SMid {
		add(int64(d.SStereo.SMid[i]))
	}
	for i := range d.SStereo.SSide {
		add(int64(d.SStereo.SSide[i]))
	}
	add(int64(d.NChannelsAPI))
	add(int64(d.NChannelsInternal))
	add(int64(d.PrevDecodeOnlyMiddle))

	return h
}

// goDecodeOp replicates the shim's oracle_silkdecapi_decode (opus_decode_frame's SILK
// driver) with the pure-Go decoder: set up decControl, then loop silk.Decoder.Decode
// (newPacketFlag on the first call) until frameSizeMs of API-rate samples are produced,
// recording one silkdecapiFrame per silk_Decode call. buf may be nil on the loss path.
func goDecodeOp(s *silkdecapiCtx, goDec *silk.Decoder, lostFlag int, buf []byte, frameSizeMs int) []silkdecapiFrame {
	decControl := silk.DecControlStruct{
		NChannelsAPI:       s.nChannels,
		NChannelsInternal:  s.nChannels,
		APISampleRate:      int32(s.apiRate),
		InternalSampleRate: int32(s.fsKHz * 1000),
		PayloadSizeMs:      frameSizeMs,
	}
	frameSize := frameSizeMs * (s.apiRate / 1000) // per channel

	var dec rangecoding.Decoder
	if buf != nil {
		dec.Init(buf)
	} else {
		dec.Init([]byte{0, 0}) // dummy; unread on the packet-loss path
	}

	pcm := make([]int16, (silkdecapiMaxFrames+1)*2*960)
	var frames []silkdecapiFrame
	decodedSamples := 0
	pcmOff := 0
	for decodedSamples < frameSize && len(frames) < silkdecapiMaxFrames {
		newPacketFlag := 0
		if decodedSamples == 0 {
			newPacketFlag = 1
		}
		nSamples, ret := goDec.Decode(&decControl, lostFlag, newPacketFlag, &dec, pcm[pcmOff:])
		if ret != 0 {
			panic("Go silk.Decode returned error")
		}
		ns := int(nSamples)
		total := ns * s.nChannels
		fpcm := make([]int16, total)
		copy(fpcm, pcm[pcmOff:pcmOff+total])
		frames = append(frames, silkdecapiFrame{
			pcm:          fpcm,
			nSamplesOut:  ns,
			stateHash:    silkdecapiGoHash(goDec),
			rng:          dec.Rng(),
			tell:         dec.Tell(),
			prevPitchLag: decControl.PrevPitchLag,
		})
		pcmOff += total
		decodedSamples += ns
	}
	return frames
}

// framesEqualDA reports whether a C-decoded and Go-decoded frame agree on the samples and
// the observable state. rng/tell are compared only when the range decoder was actually
// consumed (cmpRngTell), i.e. not on the packet-loss (dummy decoder) path.
func framesEqualDA(c, g silkdecapiFrame, cmpRngTell bool) bool {
	if c.nSamplesOut != g.nSamplesOut || len(c.pcm) != len(g.pcm) {
		return false
	}
	for i := range c.pcm {
		if c.pcm[i] != g.pcm[i] {
			return false
		}
	}
	if c.stateHash != g.stateHash || c.prevPitchLag != g.prevPitchLag {
		return false
	}
	if cmpRngTell && (c.rng != g.rng || c.tell != g.tell) {
		return false
	}
	return true
}

// firstPcmDiff returns a short description of the first differing output sample.
func firstPcmDiff(c, g []int16) string {
	n := len(c)
	if len(g) < n {
		n = len(g)
	}
	for i := 0; i < n; i++ {
		if c[i] != g[i] {
			return "sample[" + itoa(i) + "] C=" + itoa(int(c[i])) + " G=" + itoa(int(g[i]))
		}
	}
	if len(c) != len(g) {
		return "length differs"
	}
	return "output identical (state-only divergence)"
}

// daMakeStereo builds an interleaved stereo signal from a mono left channel: the right
// channel is a correlated-but-distinct mix (attenuated 1-sample-delayed left plus a light
// 400 Hz tone) so the stereo predictor and the side channel are genuinely exercised.
func daMakeStereo(l []int16, fsHz int) []int16 {
	n := len(l)
	out := make([]int16, 2*n)
	for i := 0; i < n; i++ {
		out[2*i] = l[i]
		var d int16
		if i > 0 {
			d = l[i-1]
		}
		tt := float64(i) / float64(fsHz)
		r := 0.6*float64(d) + 3000*math.Sin(2*math.Pi*400*tt)
		out[2*i+1] = clampI16(r)
	}
	return out
}

// ---- The primary end-to-end differential test ---------------------------------------

func TestSilkDecAPISequenceMatchesC(t *testing.T) {
	type genSpec struct {
		name string
		gen  func(fsHz, n int) []int16
	}
	gens := []genSpec{
		{"voiced", scVoiced},
		{"mixed", scMixed},
		{"sweep", scSweep},
		{"noise", scNoise(0x5432)},
		{"silence", scSilence},
	}
	fsRates := []int{8, 12, 16}
	apiRates := []int{8000, 12000, 16000, 24000, 48000}
	payloads := []int{10, 20, 40, 60}
	channelsList := []int{1, 2}
	const durationMs = 500

	var (
		frames                 int
		fsSeen                 = map[int]bool{}
		apiSeen                = map[int]bool{}
		payloadSeen            = map[int]bool{}
		monoSeen, stereoSeen   bool
		copySeen, resampleSeen bool
		multiFramePacket       bool
		resamplerCell          = map[[2]int]bool{}
	)

	gi := 0
	for _, fs := range fsRates {
		for _, api := range apiRates {
			for _, payload := range payloads {
				for _, ch := range channelsList {
					gs := gens[gi%len(gens)]
					gi++

					bitrate := (fs / 8) * 14000 * ch
					ctx := newSilkdecapiCtx(fs, api, payload, ch, bitrate, 5, 0, 0)
					if ctx == nil {
						t.Fatalf("newSilkdecapiCtx(fs=%d api=%d payload=%d ch=%d) failed", fs, api, payload, ch)
					}
					goDec := &silk.Decoder{}
					goDec.InitDecoder()

					nPackets := durationMs / payload
					samplesPerCh := payload * fs // payload_ms * fs_kHz
					mono := gs.gen(fs*1000, nPackets*samplesPerCh)

					for p := 0; p < nPackets; p++ {
						var chunk []int16
						if ch == 1 {
							chunk = mono[p*samplesPerCh : (p+1)*samplesPerCh]
						} else {
							chunk = daMakeStereo(mono[p*samplesPerCh:(p+1)*samplesPerCh], fs*1000)
						}
						buf := ctx.encode(chunk)
						if buf == nil {
							continue // DTX off, so not expected
						}

						cFrames := ctx.decode(silkFlagDecodeNormal, buf, payload)
						goFrames := goDecodeOp(ctx, goDec, silkFlagDecodeNormal, buf, payload)
						if len(cFrames) != len(goFrames) {
							t.Fatalf("fs=%d api=%d payload=%d ch=%d gen=%s pkt=%d: frame count C=%d Go=%d",
								fs, api, payload, ch, gs.name, p, len(cFrames), len(goFrames))
						}
						if len(cFrames) > 1 {
							multiFramePacket = true
						}
						for f := range cFrames {
							c, g := cFrames[f], goFrames[f]
							if !framesEqualDA(c, g, true) {
								t.Fatalf("MISMATCH fs=%d api=%d payload=%d ch=%d gen=%s pkt=%d frame=%d (global %d):\n"+
									"  nOut C=%d G=%d  hash C=%016x G=%016x  rng C=%08x G=%08x  tell C=%d G=%d  ppl C=%d G=%d\n  firstDiff=%s",
									fs, api, payload, ch, gs.name, p, f, frames,
									c.nSamplesOut, g.nSamplesOut, c.stateHash, g.stateHash, c.rng, g.rng, c.tell, g.tell,
									c.prevPitchLag, g.prevPitchLag, firstPcmDiff(c.pcm, g.pcm))
							}
							frames++
						}
					}

					fsSeen[fs] = true
					apiSeen[api] = true
					payloadSeen[payload] = true
					resamplerCell[[2]int{fs * 1000, api}] = true
					if ch == 1 {
						monoSeen = true
					} else {
						stereoSeen = true
					}
					if api == fs*1000 {
						copySeen = true
					} else {
						resampleSeen = true
					}
					ctx.close()
				}
			}
		}
	}

	// Non-vacuity: assert the branches we care about were exercised.
	if frames < 2000 {
		t.Fatalf("too few frames decoded (%d); sequence coverage is suspect", frames)
	}
	for _, fs := range fsRates {
		if !fsSeen[fs] {
			t.Errorf("internal rate %d kHz never exercised", fs)
		}
	}
	for _, api := range apiRates {
		if !apiSeen[api] {
			t.Errorf("API rate %d never exercised", api)
		}
	}
	for _, payload := range payloads {
		if !payloadSeen[payload] {
			t.Errorf("%d ms packets never exercised", payload)
		}
	}
	// All 15 resampler cells (3 internal x 5 API) must be covered.
	for _, fs := range fsRates {
		for _, api := range apiRates {
			if !resamplerCell[[2]int{fs * 1000, api}] {
				t.Errorf("resampler cell %d->%d never exercised", fs*1000, api)
			}
		}
	}
	if !monoSeen || !stereoSeen {
		t.Errorf("mono/stereo coverage incomplete (mono=%v stereo=%v)", monoSeen, stereoSeen)
	}
	if !copySeen || !resampleSeen {
		t.Errorf("copy/resample coverage incomplete (copy=%v resample=%v)", copySeen, resampleSeen)
	}
	if !multiFramePacket {
		t.Errorf("multi-frame (40/60 ms) packets never exercised")
	}
	t.Logf("decoded %d frames bit-exact across NB/MB/WB internal x 8/12/16/24/48 kHz API, 10/20/40/60 ms, mono+stereo", frames)
}

// ---- FEC/LBRR + PLC crafted-loss differential test ----------------------------------

func TestSilkDecAPIFECAndPLCMatchesC(t *testing.T) {
	type cfg struct {
		fs, api, ch int
	}
	cfgs := []cfg{
		{16, 48000, 1}, // WB -> 48 kHz, mono (resample)
		{16, 16000, 1}, // WB -> 16 kHz, mono (copy)
		{16, 48000, 2}, // WB -> 48 kHz, stereo (resample)
		{12, 24000, 2}, // MB -> 24 kHz, stereo (resample)
	}
	const payload = 20 // 20 ms => nFramesPerPacket == 1 => LBRR_flag set implies LBRR_flags[0]==1
	const nPackets = 60

	var totalFrames, lbrrOps, plcOps, normalOps int

	for _, cf := range cfgs {
		bitrate := (cf.fs / 8) * 16000 * cf.ch
		ctx := newSilkdecapiCtx(cf.fs, cf.api, payload, cf.ch, bitrate, 6, 0, 1 /* useFEC */)
		if ctx == nil {
			t.Fatalf("newSilkdecapiCtx FEC (fs=%d api=%d ch=%d) failed", cf.fs, cf.api, cf.ch)
		}
		goDec := &silk.Decoder{}
		goDec.InitDecoder()

		samplesPerCh := payload * cf.fs
		mono := scVoiced(cf.fs*1000, nPackets*samplesPerCh)

		// Encode all packets up front so the loss script can reference the "next" packet.
		bufs := make([][]byte, nPackets)
		lbrr := make([]bool, nPackets)
		for p := 0; p < nPackets; p++ {
			var chunk []int16
			if cf.ch == 1 {
				chunk = mono[p*samplesPerCh : (p+1)*samplesPerCh]
			} else {
				chunk = daMakeStereo(mono[p*samplesPerCh:(p+1)*samplesPerCh], cf.fs*1000)
			}
			buf := ctx.encode(chunk)
			bufs[p] = buf
			if buf != nil {
				lbrr[p] = ctx.hasLBRR(buf)
			}
		}

		// Loss script: some packets are lost and concealed by PLC; others are lost and
		// recovered from the NEXT packet's LBRR redundant frame (FLAG_DECODE_LBRR), then
		// that next packet is decoded normally. Faithful to opus_decode_frame's FEC path.
		lostForPLC := map[int]bool{7: true, 23: true, 41: true}
		lostForFEC := map[int]bool{12: true, 30: true, 48: true}

		run := func(lostFlag int, buf []byte) {
			cFrames := ctx.decode(lostFlag, buf, payload)
			goFrames := goDecodeOp(ctx, goDec, lostFlag, buf, payload)
			if len(cFrames) != len(goFrames) {
				t.Fatalf("FEC fs=%d api=%d ch=%d flag=%d: frame count C=%d Go=%d",
					cf.fs, cf.api, cf.ch, lostFlag, len(cFrames), len(goFrames))
			}
			cmp := buf != nil
			for f := range cFrames {
				c, g := cFrames[f], goFrames[f]
				if !framesEqualDA(c, g, cmp) {
					t.Fatalf("FEC MISMATCH fs=%d api=%d ch=%d flag=%d frame=%d:\n"+
						"  nOut C=%d G=%d  hash C=%016x G=%016x  rng C=%08x G=%08x  ppl C=%d G=%d  firstDiff=%s",
						cf.fs, cf.api, cf.ch, lostFlag, f,
						c.nSamplesOut, g.nSamplesOut, c.stateHash, g.stateHash, c.rng, g.rng,
						c.prevPitchLag, g.prevPitchLag, firstPcmDiff(c.pcm, g.pcm))
				}
				totalFrames++
			}
		}

		for p := 0; p < nPackets; p++ {
			if bufs[p] == nil {
				continue
			}
			if lostForPLC[p] {
				// Lost packet, no redundancy used: conceal with PLC.
				run(silkFlagPacketLost, nil)
				plcOps++
				continue // packet dropped, not decoded normally
			}
			if lostForFEC[p] {
				// Lost packet, recovered later from p+1's LBRR: skip normal decode now.
				continue
			}
			// If the previous packet was lost-for-FEC and this packet carries LBRR,
			// recover that lost frame from this packet's redundant data first.
			if p > 0 && lostForFEC[p-1] && lbrr[p] {
				run(silkFlagDecodeLBRR, bufs[p])
				lbrrOps++
			}
			run(silkFlagDecodeNormal, bufs[p])
			normalOps++
		}
		ctx.close()
	}

	if totalFrames < 200 {
		t.Fatalf("FEC/PLC test decoded too few frames (%d)", totalFrames)
	}
	if lbrrOps == 0 {
		t.Fatalf("FLAG_DECODE_LBRR path never exercised (encoder produced no usable LBRR); adjust bitrate/FEC config")
	}
	if plcOps == 0 {
		t.Fatalf("FLAG_PACKET_LOST (PLC) path never exercised")
	}
	if normalOps == 0 {
		t.Fatalf("FLAG_DECODE_NORMAL path never exercised")
	}
	t.Logf("FEC/PLC bit-exact: %d frames, %d LBRR recoveries, %d PLC conceals, %d normal decodes",
		totalFrames, lbrrOps, plcOps, normalOps)
}

// ---- DTX (discontinuous transmission) differential test -----------------------------

func TestSilkDecAPIDTXMatchesC(t *testing.T) {
	// Silence with DTX on produces empty packets; drive those slots through the loss/CNG
	// path (FLAG_PACKET_LOST), the way a real decoder handles a DTX gap.
	fsRates := []int{8, 16}
	apiRates := []int{48000, 16000}
	const payload = 20
	const nPackets = 80

	var frames, plcFrames, normalFrames int

	for i, fs := range fsRates {
		api := apiRates[i]
		ctx := newSilkdecapiCtx(fs, api, payload, 1, (fs/8)*14000, 5, 1 /* useDTX */, 0)
		if ctx == nil {
			t.Fatalf("newSilkdecapiCtx DTX (fs=%d) failed", fs)
		}
		goDec := &silk.Decoder{}
		goDec.InitDecoder()

		samplesPerCh := payload * fs
		// ~100 ms of tone then ~500 ms of silence, repeated, so DTX reliably stops coding
		// during the long silent runs (encoder emits empty packets once inactive).
		mono := make([]int16, nPackets*samplesPerCh)
		for p := 0; p < nPackets; p++ {
			if p%30 < 5 {
				copy(mono[p*samplesPerCh:(p+1)*samplesPerCh], scTone(fs*1000, samplesPerCh, 300))
			}
			// else leave as silence
		}

		for p := 0; p < nPackets; p++ {
			chunk := mono[p*samplesPerCh : (p+1)*samplesPerCh]
			buf := ctx.encode(chunk)
			var lostFlag int
			var decBuf []byte
			if buf == nil {
				lostFlag = silkFlagPacketLost // DTX gap -> conceal / CNG
			} else {
				lostFlag = silkFlagDecodeNormal
				decBuf = buf
			}
			cFrames := ctx.decode(lostFlag, decBuf, payload)
			goFrames := goDecodeOp(ctx, goDec, lostFlag, decBuf, payload)
			if len(cFrames) != len(goFrames) {
				t.Fatalf("DTX fs=%d pkt=%d: frame count C=%d Go=%d", fs, p, len(cFrames), len(goFrames))
			}
			cmp := decBuf != nil
			for f := range cFrames {
				c, g := cFrames[f], goFrames[f]
				if !framesEqualDA(c, g, cmp) {
					t.Fatalf("DTX MISMATCH fs=%d pkt=%d frame=%d flag=%d:\n  hash C=%016x G=%016x  firstDiff=%s",
						fs, p, f, lostFlag, c.stateHash, g.stateHash, firstPcmDiff(c.pcm, g.pcm))
				}
				frames++
				if lostFlag == silkFlagPacketLost {
					plcFrames++
				} else {
					normalFrames++
				}
			}
		}
		ctx.close()
	}

	if plcFrames == 0 {
		t.Fatalf("DTX test never hit an empty (DTX) packet; PLC/CNG path not exercised")
	}
	if normalFrames == 0 {
		t.Fatalf("DTX test never hit a normal packet")
	}
	t.Logf("DTX bit-exact: %d frames (%d PLC/CNG, %d normal)", frames, plcFrames, normalFrames)
}

// ---- Mutation (non-vacuity) check ---------------------------------------------------

func TestSilkDecAPIMutationDetected(t *testing.T) {
	ctx := newSilkdecapiCtx(16, 48000, 20, 2, 40000, 6, 0, 0)
	if ctx == nil {
		t.Fatal("newSilkdecapiCtx failed")
	}
	defer ctx.close()
	goDec := &silk.Decoder{}
	goDec.InitDecoder()

	samplesPerCh := 20 * 16
	mono := scVoiced(16000, 8*samplesPerCh)

	var c, g silkdecapiFrame
	got := false
	for p := 0; p < 8; p++ {
		chunk := daMakeStereo(mono[p*samplesPerCh:(p+1)*samplesPerCh], 16000)
		buf := ctx.encode(chunk)
		if buf == nil {
			continue
		}
		cFrames := ctx.decode(silkFlagDecodeNormal, buf, 20)
		goFrames := goDecodeOp(ctx, goDec, silkFlagDecodeNormal, buf, 20)
		c, g = cFrames[len(cFrames)-1], goFrames[len(goFrames)-1]
		got = true
	}
	if !got {
		t.Fatal("no frame decoded")
	}
	if !framesEqualDA(c, g, true) {
		t.Fatal("baseline C/Go frame unexpectedly differ")
	}
	if len(g.pcm) == 0 {
		t.Fatal("empty output frame")
	}

	// Mutate one output sample: must now be detected.
	mOut := g
	mOut.pcm = append([]int16(nil), g.pcm...)
	mOut.pcm[0] ^= 1
	if framesEqualDA(c, mOut, true) {
		t.Fatal("mutation of an output sample was NOT detected")
	}

	// Mutate the persistent-state hash: must now be detected.
	mHash := g
	mHash.stateHash ^= 1
	if framesEqualDA(c, mHash, true) {
		t.Fatal("mutation of the state hash was NOT detected")
	}
}
