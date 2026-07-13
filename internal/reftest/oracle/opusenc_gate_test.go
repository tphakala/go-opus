//go:build refc

package oracle

import (
	"flag"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/go-opus/internal/opuscompare"
	"github.com/tphakala/go-opus/internal/opusenc"
	"github.com/tphakala/go-opus/opus"
)

// THE PHASE 4 GATE (plan.md:146-149): "packets bit-exact vs the pinned C
// FIXED_POINT encoder across the corpus at multiple bitrates, VBR and CBR,
// complexities 0-10 (multi-frame sequences with state hashes); round-trip quality
// verified through the reference decoder."
//
// Read in its strongest sense, and that is the reading implemented here:
//
//	CORPUS x {mono, stereo} x BITRATE x {CBR, VBR, constrained VBR} x COMPLEXITY
//	0..10 x FRAME {2.5, 5, 10, 20} ms
//
// with, AFTER EVERY SINGLE FRAME: the opus_encode return value, the packet length,
// EVERY PACKET BYTE, st->rangeFinal, the whole cross-frame state field by field,
// and the state hash. The corpus is corpus_test.go; the per-frame comparison is
// framePkt below, which is encPair.frame (opusenc_encode_test.go:85) with the
// packet bytes handed back so the round-trip tiers can decode them.
//
// TIERS.
//
//	TIER A (TestOpusencPhase4Gate) is THE GATE and runs on every push: the full grid
//	  over the whole corpus, a window of -gate.frames frames per configuration. It
//	  is t.Parallel across (clip x frame size), which is safe because each encPair
//	  owns its own C handle (opusenc_cgo.go:271) and nothing is shared: packets are
//	  compared and dropped per frame.
//	TIER B (TestOpusencPhase4GateLong, -gate.long) is nightly: the FULL-LENGTH RFC
//	  vectors (21 to 32 s each) on a reduced grid. It is the only thing that can
//	  catch a slow drift in the delay ring, hp_mem, the CELT energy history or the
//	  VBR reservoir that a 2 s clip is too short to expose.
//
// THE ROUND-TRIP IS THREE SEPARATE ASSERTIONS, and conflating them would weaken all
// three:
//
//	(a) TestOpusencPhase4GateRoundTrip: the Go packets (already proven byte-identical
//	    to the C encoder's) are decoded through BOTH the C libopus decoder and the Go
//	    opus.Decoder, and the two int16 outputs must be BIT-IDENTICAL with matching
//	    per-packet final ranges, which must in turn equal the ENCODER's final range.
//	    That is "round-trip through the reference decoder" done properly, and it is
//	    strictly stronger than any opus_compare threshold.
//	(b) TestOpusencPhase4GateQuality: internal/opuscompare on ORIGINAL vs DECODED, as
//	    a quality smoke test with an empirical per-bitrate floor. It deliberately
//	    does NOT use Result.Passed: Q >= 0 is calibrated for reference-decode vs
//	    test-decode of the SAME bitstream, not for original-vs-lossy, where it is
//	    hugely negative and means nothing.
//	(c) The sample-accurate alignment test belongs to the oggopus/public-API unit
//	    (it needs the pre-skip drop that only the container layer performs) and is
//	    NOT duplicated here.
//
// WHAT THIS GATE STRUCTURALLY CANNOT SEE: anything that never reaches a packet
// byte. The pre-skip written into OpusHead, samples48k per packet, the page
// granules and the end trim are all invisible to a differential packet gate, and
// they have their own assertions in the container unit. Sample rates other than 48
// kHz are covered by opusenc_samplerate_test.go (the CP10-0 gate), which is why
// there is no Fs axis here: it would multiply the grid without adding a branch.

// ---------------------------------------------------------------------------
// Flags.
// ---------------------------------------------------------------------------

var (
	// gateFrames is the number of frames encoded per grid point in TIER A. ZERO, the
	// default, means EVERY FRAME OF EVERY CLIP: the full 2 s at every frame size, so
	// each grid point is a 100-frame (20 ms) to 800-frame (2.5 ms) sequence and the
	// cross-frame state machines have to survive the whole clip. Measured cost of the
	// full grid is ~0.11 ms of CPU per frame-pair, i.e. ~10.3 M pairs in ~19 CPU
	// minutes, which is ~1.6 min of wall time on 12 cores and ~5 min on a 4-core CI
	// runner: inside the 20 minute CI budget with room to spare. A positive value
	// windows each grid point instead (the windows ROTATE through the clip), which is
	// the knob to reach for if the gate ever has to be made cheaper.
	gateFrames = flag.Int("gate.frames", 0,
		"frames per grid point in the TIER A phase 4 gate (0 = every frame of every clip)")
	// gateLong turns on TIER B, which is a nightly job, not a per-push one.
	gateLong = flag.Bool("gate.long", false, "run TIER B of the phase 4 gate: the full-length RFC vectors")
)

// gateMaxDataBytes is the caller's buffer for every gate frame. It is deliberately
// generous: the gate varies the RATE, not the buffer, so no grid point is ever
// pushed onto the degenerate TOC-only branch by starvation. That branch has its own
// dedicated tests (TestOpusencEncodeDegenerate).
const gateMaxDataBytes = 1500

// ---------------------------------------------------------------------------
// The grid.
// ---------------------------------------------------------------------------

// gateRateMode is one of the three rate controllers.
type gateRateMode struct {
	name          string
	vbr           int
	vbrConstraint int
}

var (
	// gateBitrates spans the range the CELT-only encoder is actually used over: 16
	// kbps is below the stereo->mono downmix threshold (17281 bps at voice_est 48)
	// and inside the stereo-width fade, 32 kbps sits exactly on the fade's upper
	// knee, and 64/128 kbps are the transparent end.
	gateBitrates = []int{16000, 32000, 64000, 128000}

	gateRateModes = []gateRateMode{
		{"cbr", 0, 0},
		{"vbr", 1, 0},
		{"cvbr", 1, 1}, // libopus' default
	}

	// gateFrameSizes are 2.5, 5, 10 and 20 ms at 48 kHz, i.e. LM 0..3. Both arms of
	// the delay-buffer update at opus_encoder.c:2304 need covering, and the split is
	// at frame_size < 288: LM0/LM1 take the MOVE arm, LM2/LM3 the COPY arm.
	gateFrameSizes = []int{120, 240, 480, 960}
)

// gateComplexities is 0..10 inclusive.
func gateComplexities() []int {
	out := make([]int, 11)
	for i := range out {
		out[i] = i
	}
	return out
}

// ---------------------------------------------------------------------------
// The per-frame gate step.
// ---------------------------------------------------------------------------

// framePkt is encPair.frame plus the PACKET BYTES. It runs the identical
// comparison (return value, packet length, every packet byte, rangeFinal, the
// cross-frame state field by field, the state hash, and the three asserted-dead /
// asserted-live witnesses) and additionally hands back the Go packet, which
// encPair.frame drops.
//
// It exists because the round-trip tiers have to DECODE the packet the gate just
// proved bit-exact, and because opusenc_encode_test.go is the CP9 unit's file. When
// the two units are integrated, frame() should simply return the packet and this
// wrapper should go away.
func (p *encPair) framePkt(label string, pcm []int16, frameSize, maxDataBytes int) (int, []byte, opusenc.Witness) {
	p.t.Helper()
	p.n++
	where := fmt.Sprintf("%s: frame %d (frame_size=%d, max_data_bytes=%d)",
		label, p.n, frameSize, maxDataBytes)

	cRet, cPkt, cState := p.c.Encode(pcm, frameSize, maxDataBytes)

	gBuf := make([]byte, maxDataBytes+1)
	gRet := p.g.EncodeRaw(pcm, frameSize, gBuf, maxDataBytes)
	wit := p.g.Witness()

	// 1. The return value, including the exact negative OPUS_* code.
	if gRet != cRet {
		p.t.Fatalf("%s: return value: Go = %d, C = %d", where, gRet, cRet)
	}

	// 2 and 3. The packet length and every byte of it.
	var gPkt []byte
	if gRet > 0 {
		gPkt = gBuf[:gRet]
		if len(gPkt) != len(cPkt) {
			p.t.Fatalf("%s: packet length: Go = %d, C = %d", where, len(gPkt), len(cPkt))
		}
		for i := range cPkt {
			if gPkt[i] != cPkt[i] {
				p.t.Fatalf("%s: PACKET BYTE %d of %d differs: Go = 0x%02x, C = 0x%02x\n"+
					"  Go: % x\n  C : % x\n  witness: %+v",
					where, i, len(cPkt), gPkt[i], cPkt[i],
					headBytes(gPkt, i), headBytes(cPkt, i), wit)
			}
		}
	}

	gState := p.g.State()

	// 4. rangeFinal: the entropy coder's own witness that the PAYLOAD agrees, not
	// just the bytes.
	if gState.RangeFinal != cState.RangeFinal {
		p.t.Fatalf("%s: rangeFinal: Go = 0x%08x, C = 0x%08x",
			where, gState.RangeFinal, cState.RangeFinal)
	}

	// 5. The cross-frame state, field by field, then the hash as a cross-check that
	// Hash() really covers what compareState covers.
	compareState(p.t, where, gState, cState)
	if h1, h2 := gState.Hash(), cStateToGo(cState).Hash(); h1 != h2 {
		p.t.Fatalf("%s: state hash: Go = 0x%016x, C = 0x%016x", where, h1, h2)
	}

	// The two branches CP9 asserts DEAD, re-asserted over the whole corpus. If the
	// corpus reaches either of them, the reasoning behind them is wrong and it is a
	// finding, not a test failure to paper over.
	if wit.BustCheckFired {
		p.t.Fatalf("%s: opus_encoder.c:2580 bust check FIRED, and it is supposed to be "+
			"unreachable in CELT-only. witness: %+v", where, wit)
	}
	if wit.CBRIMinFired {
		p.t.Fatalf("%s: the opus_encoder.c:1330 IMIN clamped cbr_bytes, which the exact "+
			"bits_to_bitrate round-trip makes impossible. witness: %+v", where, wit)
	}
	// Asserted LIVE: the :2488 budget check can never be false.
	if gRet > 0 && !wit.Degenerate && !wit.CeltEncodeRan {
		p.t.Fatalf("%s: opus_encoder.c:2488 skipped celt_encode_with_ec on a coded frame. "+
			"witness: %+v", where, wit)
	}
	return cRet, gPkt, wit
}

// ---------------------------------------------------------------------------
// Coverage: what the grid actually reached.
// ---------------------------------------------------------------------------

// gateCoverage is the non-vacuity ledger. A gate that agrees with C on 1.3 million
// frames it never made an interesting decision on is worth nothing, so the grid
// records the decisions it DID reach and the declaration at the end fails if any of
// the ones it claims to cover never fired.
type gateCoverage struct {
	mu             sync.Mutex
	pairs          int64
	bytes          int64
	bandwidth      map[int]bool
	endBand        map[int]bool
	streamChannels map[int]bool
	delayMove      bool
	delayCopy      bool
	stereoFade     bool
	downmix        bool
	degenerate     int64
	padding        int64
	// silentShrink counts packets of 3 bytes or fewer. THREE, not two: a CELT-only
	// VBR frame that takes the silence path still carries the 1-byte TOC plus the
	// entropy coder's minimum payload, so 3 bytes is the floor the encoder can
	// actually reach, and the whole grid confirms it (min packet == 3).
	silentShrink int64
	minPkt       int
	maxPkt       int
}

func newGateCoverage() *gateCoverage {
	return &gateCoverage{
		bandwidth:      map[int]bool{},
		endBand:        map[int]bool{},
		streamChannels: map[int]bool{},
		minPkt:         1 << 30,
	}
}

// observe records one frame. It is called on a per-subtest LOCAL ledger (no lock
// traffic on the hot path) and merged once when the subtest ends.
func (g *gateCoverage) observe(wit opusenc.Witness, pktLen int) {
	g.pairs++
	g.bytes += int64(pktLen)
	g.bandwidth[wit.CurrBandwidth] = true
	g.endBand[wit.EndBand] = true
	g.streamChannels[wit.StreamChannels] = true
	g.delayMove = g.delayMove || wit.DelayMoveBranch
	g.delayCopy = g.delayCopy || wit.DelayCopyBranch
	g.stereoFade = g.stereoFade || wit.StereoFadeRan
	g.downmix = g.downmix || wit.Downmixed
	if wit.Degenerate {
		g.degenerate++
	}
	if wit.PaddingRan {
		g.padding++
	}
	if pktLen > 0 && pktLen <= 3 {
		g.silentShrink++
	}
	if pktLen > 0 && pktLen < g.minPkt {
		g.minPkt = pktLen
	}
	if pktLen > g.maxPkt {
		g.maxPkt = pktLen
	}
}

func (g *gateCoverage) merge(o *gateCoverage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pairs += o.pairs
	g.bytes += o.bytes
	for k := range o.bandwidth {
		g.bandwidth[k] = true
	}
	for k := range o.endBand {
		g.endBand[k] = true
	}
	for k := range o.streamChannels {
		g.streamChannels[k] = true
	}
	g.delayMove = g.delayMove || o.delayMove
	g.delayCopy = g.delayCopy || o.delayCopy
	g.stereoFade = g.stereoFade || o.stereoFade
	g.downmix = g.downmix || o.downmix
	g.degenerate += o.degenerate
	g.padding += o.padding
	g.silentShrink += o.silentShrink
	if o.minPkt < g.minPkt {
		g.minPkt = o.minPkt
	}
	if o.maxPkt > g.maxPkt {
		g.maxPkt = o.maxPkt
	}
}

// report is the GATE DECLARATION: exactly what ran, and what it reached.
func (g *gateCoverage) report(t *testing.T, tier, grid string, clips int, elapsed time.Duration) {
	t.Helper()
	rate := float64(elapsed.Microseconds()) / float64(max(g.pairs, 1)) / 1000.0
	t.Logf("\n=== PHASE 4 GATE, %s ===\n"+
		"grid:            %s\n"+
		"clips:           %d\n"+
		"FRAME-PAIRS:     %d (Go encode + C encode + full state compare, each)\n"+
		"packet bytes:    %d\n"+
		"packet size:     %d..%d bytes\n"+
		"wall time:       %s (%.3f ms per frame-pair, wall / pair, %d-way parallel)\n"+
		"bandwidths:      %v\n"+
		"CELT end bands:  %v\n"+
		"stream channels: %v\n"+
		"delay ring:      MOVE=%v COPY=%v\n"+
		"stereo fade:     %v   rate downmix: %v\n"+
		"degenerate:      %d frames   code-3 padded: %d frames   <=3-byte packets: %d\n",
		tier, grid, clips, g.pairs, g.bytes, g.minPkt, g.maxPkt,
		elapsed.Round(time.Millisecond), rate, testParallelism(),
		sortedKeys(g.bandwidth), sortedKeys(g.endBand), sortedKeys(g.streamChannels),
		g.delayMove, g.delayCopy, g.stereoFade, g.downmix,
		g.degenerate, g.padding, g.silentShrink)
}

// sortedKeys is a stable rendering of a set of ints, for the declaration.
func sortedKeys(m map[int]bool) []int {
	return slices.Sorted(maps.Keys(m))
}

// testParallelism is what -test.parallel resolved to, for the declaration.
func testParallelism() int {
	if f := flag.Lookup("test.parallel"); f != nil {
		var n int
		if _, err := fmt.Sscanf(f.Value.String(), "%d", &n); err == nil {
			return n
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// TIER A: THE GATE.
// ---------------------------------------------------------------------------

// TestOpusencPhase4Gate is the phase 4 gate. Every clip in the corpus, at every
// frame size, over every (bitrate x rate mode x complexity) point, with the whole
// per-frame comparison on every frame.
func TestOpusencPhase4Gate(t *testing.T) {
	clips := gateCorpus(t)
	nFrames := *gateFrames
	if nFrames == 1 {
		t.Fatal("-gate.frames = 1: a gate on single frames cannot see cross-frame state")
	}
	if nFrames < 0 {
		t.Fatalf("-gate.frames = %d: negative", nFrames)
	}

	perPoint := fmt.Sprintf("%d frames per point", nFrames)
	if nFrames == 0 {
		perPoint = "EVERY frame of every clip"
	}
	grid := fmt.Sprintf("bitrate %v x {%s} x complexity 0..10 x frame {%v} samples @ %d Hz, %s",
		gateBitrates, "cbr,vbr,cvbr", gateFrameSizes, corpusRate, perPoint)

	cov := newGateCoverage()
	start := time.Now()

	// The nested group is what makes the declaration below well defined: t.Run
	// returns only once every PARALLEL subtest inside it has finished.
	t.Run("grid", func(t *testing.T) {
		for _, c := range clips {
			for _, frameSize := range gateFrameSizes {
				name := fmt.Sprintf("%s/fs%d", c.name, frameSize)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					local := newGateCoverage()
					cfgIdx := 0
					for _, bitrate := range gateBitrates {
						for _, rm := range gateRateModes {
							for _, complexity := range gateComplexities() {
								gateRunPoint(t, gatePoint{
									clip:       c,
									frameSize:  frameSize,
									bitrate:    bitrate,
									rateMode:   rm,
									complexity: complexity,
									frames:     nFrames,
									index:      cfgIdx,
								}, local)
								cfgIdx++
							}
						}
					}
					cov.merge(local)
				})
			}
		}
	})

	elapsed := time.Since(start)
	cov.report(t, "TIER A", grid, len(clips), elapsed)

	// NON-VACUITY. The grid claims to cover these; if any of them never fired, the
	// grid is not the grid this test says it is.
	for _, bw := range []int{oBandwidthNarrowband, oBandwidthWideband,
		oBandwidthSuperwideband, oBandwidthFullband} {
		if !cov.bandwidth[bw] {
			t.Errorf("vacuous grid: bandwidth tier %d was never selected (saw %v)",
				bw, sortedKeys(cov.bandwidth))
		}
	}
	for _, ch := range []int{1, 2} {
		if !cov.streamChannels[ch] {
			t.Errorf("vacuous grid: stream_channels never took the value %d", ch)
		}
	}
	if !cov.delayMove || !cov.delayCopy {
		t.Errorf("vacuous grid: the two arms of the :2304 delay-buffer update did not both "+
			"fire (MOVE=%v COPY=%v)", cov.delayMove, cov.delayCopy)
	}
	if !cov.stereoFade {
		t.Error("vacuous grid: stereo_fade (:2344) never ran, so the stereo-width cross-fade " +
			"is untested")
	}
	if !cov.downmix {
		t.Error("vacuous grid: no stereo encoder was ever rate-downmixed to a mono stream " +
			"(:1428-1456), so the TOC's stereo bit is only tested in one direction")
	}
	if cov.silentShrink == 0 {
		t.Error("vacuous grid: not one packet shrank to 3 bytes or fewer, so neither the CELT " +
			"silence path nor the VBR shrink was reached")
	}
	if cov.pairs == 0 {
		t.Fatal("vacuous grid: no frame was encoded at all")
	}
}

// gatePoint is one point of the grid.
type gatePoint struct {
	clip       corpusClip
	frameSize  int
	bitrate    int
	rateMode   gateRateMode
	complexity int
	frames     int
	index      int
}

func (p gatePoint) label() string {
	return fmt.Sprintf("%s/fs%d/%s/%dbps/cx%d", p.clip.name, p.frameSize,
		p.rateMode.name, p.bitrate, p.complexity)
}

func (p gatePoint) cfg() cOpusencCfg {
	cfg := defaultOpusencCfg(p.clip.channels)
	cfg.Fs = corpusRate
	cfg.Bitrate = p.bitrate
	cfg.VBR = p.rateMode.vbr
	cfg.VBRConstraint = p.rateMode.vbrConstraint
	cfg.Complexity = p.complexity
	return cfg
}

// window is where in the clip this grid point starts, in frames. frames == 0 (the
// TIER A default) means the WHOLE clip from sample zero. A positive frames windows
// the clip, and the windows ROTATE across the grid (a stride coprime with the frame
// counts) so that even a windowed grid still sweeps every part of every clip.
func (p gatePoint) window() (start, n int) {
	total := p.clip.countFrames(p.frameSize)
	n = p.frames
	if n <= 0 || n > total {
		n = total
	}
	if total > n {
		start = (p.index * 13) % (total - n + 1)
	}
	return start, n
}

// gateRunPoint encodes one grid point on a fresh encoder pair.
func gateRunPoint(t *testing.T, p gatePoint, cov *gateCoverage) {
	t.Helper()
	pair := newEncPair(t, p.cfg())
	label := p.label()
	start, n := p.window()
	if n == 0 {
		t.Fatalf("%s: the clip holds no whole %d-sample frame", label, p.frameSize)
	}
	for i := 0; i < n; i++ {
		ret, pkt, wit := pair.framePkt(label, p.clip.frameAt(start+i, p.frameSize),
			p.frameSize, gateMaxDataBytes)
		if ret <= 0 {
			t.Fatalf("%s: frame %d: opus_encode returned %d; no grid point is supposed to be "+
				"starved (max_data_bytes = %d)", label, i, ret, gateMaxDataBytes)
		}
		cov.observe(wit, len(pkt))
	}
}

// ---------------------------------------------------------------------------
// TIER B: the nightly long-sequence gate.
// ---------------------------------------------------------------------------

var (
	// longBitrates includes 16 kbps so that the bandwidth tiers and the stereo/mono
	// downmix HYSTERESIS are crossed over hundreds of frames, not just over the
	// hundred a 2 s clip can hold. Without it TIER B never leaves FULLBAND.
	longBitrates     = []int{16000, 24000, 64000, 128000}
	longComplexities = []int{0, 5, 10}
	// longFrameSizes is 10 and 20 ms. TIER B deliberately does NOT sweep 2.5 and 5 ms:
	// those are the MOVE arm of the :2304 delay-buffer update, which TIER A already
	// exercises on EVERY clip, and at 2.5 ms a 30 s vector is 12000 frames per grid
	// point. TIER B is here for LENGTH, not for width.
	longFrameSizes = []int{480, 960}
)

// TestOpusencPhase4GateLong is TIER B: the FULL-LENGTH RFC vectors (21 to 32 s of
// real speech and music each, mono and stereo) on a reduced grid. It exists for one
// reason: a 2 s clip cannot expose a slow drift. The delay ring, hp_mem, the CELT
// energy history (oldBandE / oldLogE / oldLogE2) and the VBR reservoir all carry
// state across hundreds of frames, and a one-LSB error that only accumulates would
// pass TIER A and fail here.
func TestOpusencPhase4GateLong(t *testing.T) {
	if testing.Short() {
		t.Skip("TIER B: skipped under -short")
	}
	if !*gateLong {
		t.Skip("TIER B: nightly gate; pass -gate.long to run it")
	}
	if !rfcVectorsPresent() {
		t.Skipf("TIER B needs the RFC vectors at %s; run scripts/fetch-vectors.sh", rfcVectorsDir)
	}

	clips := rfcClips(t, 0) // 0 == the whole vector
	grid := fmt.Sprintf("FULL-LENGTH RFC vectors x bitrate %v x {cbr,vbr,cvbr} x complexity %v "+
		"x frame {%v} samples, EVERY frame of every clip", longBitrates, longComplexities, longFrameSizes)

	cov := newGateCoverage()
	start := time.Now()

	t.Run("grid", func(t *testing.T) {
		for _, c := range clips {
			for _, frameSize := range longFrameSizes {
				name := fmt.Sprintf("%s/fs%d", c.name, frameSize)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					local := newGateCoverage()
					for _, bitrate := range longBitrates {
						for _, rm := range gateRateModes {
							for _, complexity := range longComplexities {
								gateRunPoint(t, gatePoint{
									clip:       c,
									frameSize:  frameSize,
									bitrate:    bitrate,
									rateMode:   rm,
									complexity: complexity,
									frames:     c.countFrames(frameSize),
								}, local)
							}
						}
					}
					cov.merge(local)
				})
			}
		}
	})

	elapsed := time.Since(start)
	cov.report(t, "TIER B (nightly)", grid, len(clips), elapsed)
	if cov.pairs == 0 {
		t.Fatal("vacuous grid: no frame was encoded at all")
	}
}

// ---------------------------------------------------------------------------
// ROUND-TRIP (a): the Go decoder against the C decoder, on the Go packets.
// ---------------------------------------------------------------------------

// roundTripClips is the subset the round-trip tiers run over: the whole synthetic
// bank plus four RFC vectors (two speech, two music/mixed), which is enough real
// audio to be meaningful without re-running the whole grid.
func roundTripClips(t *testing.T) []corpusClip {
	t.Helper()
	clips := syntheticBank()
	if !rfcVectorsPresent() {
		t.Logf("round-trip: the RFC vectors are absent, running on the synthetic bank alone")
		return clips
	}
	all := rfcClips(t, corpusSeconds)
	want := map[string]bool{
		"rfc01-stereo": true, "rfc01-mono": true,
		"rfc05-stereo": true, "rfc05-mono": true,
		"rfc09-stereo": true, "rfc09-mono": true,
		"rfc11-stereo": true, "rfc11-mono": true,
	}
	for _, c := range all {
		if want[c.name] {
			clips = append(clips, c)
		}
	}
	return clips
}

// TestOpusencPhase4GateRoundTrip is the "round-trip through the reference decoder"
// clause, done as an EXACT assertion rather than a quality score.
//
// Every packet the Go encoder produces (and framePkt has already proved every byte
// of it identical to the C encoder's) is decoded through BOTH the pinned C libopus
// decoder and the pure-Go opus.Decoder, and the two must agree on EVERY int16
// SAMPLE and on the per-packet final range. The decoder's final range must also
// equal the ENCODER's, which is what ties the two halves of the codec together: it
// is the range coder's own witness that the decoder read back exactly the symbols
// the encoder wrote.
func TestOpusencPhase4GateRoundTrip(t *testing.T) {
	clips := roundTripClips(t)

	var (
		mu      sync.Mutex
		packets int64
		samples int64
	)

	t.Run("grid", func(t *testing.T) {
		for _, c := range clips {
			for _, frameSize := range []int{480, 960} {
				for _, bitrate := range []int{24000, 64000} {
					for _, rm := range gateRateModes {
						for _, complexity := range []int{0, 5, 10} {
							p := gatePoint{
								clip: c, frameSize: frameSize, bitrate: bitrate,
								rateMode: rm, complexity: complexity, frames: 50,
							}
							t.Run(p.label(), func(t *testing.T) {
								t.Parallel()
								np, ns := roundTripPoint(t, p)
								mu.Lock()
								packets += int64(np)
								samples += int64(ns)
								mu.Unlock()
							})
						}
					}
				}
			}
		}
	})

	t.Logf("ROUND-TRIP (a) Go decoder vs C decoder on the Go packets: %d packets, "+
		"%d int16 samples, ALL bit-identical, ALL final ranges matching the ENCODER's",
		packets, samples)
	if packets == 0 {
		t.Fatal("vacuous round-trip: no packet was decoded")
	}
}

// roundTripPoint encodes one grid point and decodes every packet through both
// decoders. It returns the packet and sample counts.
func roundTripPoint(t *testing.T, p gatePoint) (packets, samples int) {
	t.Helper()
	pair := newEncPair(t, p.cfg())

	cDec, err := NewDecoder(corpusRate, p.clip.channels)
	if err != nil {
		t.Fatalf("C decoder: %v", err)
	}
	defer cDec.Close()

	gDec, err := opus.NewDecoder(corpusRate, p.clip.channels)
	if err != nil {
		t.Fatalf("Go decoder: %v", err)
	}

	label := p.label()
	start, n := p.window()
	for i := 0; i < n; i++ {
		_, pkt, _ := pair.framePkt(label, p.clip.frameAt(start+i, p.frameSize),
			p.frameSize, gateMaxDataBytes)
		if len(pkt) == 0 {
			t.Fatalf("%s: frame %d produced no packet", label, i)
		}
		encRange := pair.g.State().RangeFinal

		cPCM, gPCM, cRng, gRng := decodePair(t, cDec, gDec, p.clip.channels, pkt, false)
		where := fmt.Sprintf("%s: packet %d (%d bytes)", label, i, len(pkt))
		assertPCMEqual(t, where, cPCM, gPCM)
		if cRng != gRng {
			t.Fatalf("%s: decoder final range: C = 0x%08x, Go = 0x%08x", where, cRng, gRng)
		}
		// The encoder/decoder cross-check. It is not implied by the PCM equality: two
		// decoders could agree with each other and both disagree with what the encoder
		// actually coded.
		if gRng != encRange {
			t.Fatalf("%s: DECODER final range 0x%08x != ENCODER final range 0x%08x: the "+
				"decoder did not read back the symbols the encoder wrote", where, gRng, encRange)
		}
		if len(cPCM) != p.frameSize*p.clip.channels {
			t.Fatalf("%s: decoded %d samples, want %d", where, len(cPCM),
				p.frameSize*p.clip.channels)
		}
		packets++
		samples += len(gPCM)
	}
	return packets, samples
}

// ---------------------------------------------------------------------------
// ROUND-TRIP (b): the quality smoke test.
// ---------------------------------------------------------------------------

// qualityFloor is the empirical opus_compare quality floor per bitrate, on the RFC
// audio at 20 ms / complexity 10 / constrained VBR.
//
// THESE ARE NOT THE SPEC THRESHOLD. opus_compare's Q >= 0 PASS is calibrated for
// "reference decode vs test decode of the SAME bitstream", where the two differ
// only by rounding. Here the comparison is ORIGINAL vs LOSSY DECODE, which is a
// completely different quantity: a perfectly good 24 kbps encode scores far below
// zero and Result.Passed is meaningless. These floors are REGRESSION TRIPWIRES
// measured on this encoder, with a wide margin: they catch "the encoder started
// emitting mush" and nothing finer.
// qualityBitrates are the three rates the smoke test scores.
var qualityBitrates = []int{24000, 64000, 128000}

// qualityFloor is the per-clip opus_compare floor at each bitrate, and it is a
// CRASH BARRIER, not a quality bar.
//
// THE ABSOLUTE NUMBER IS NOT A QUALITY JUDGEMENT, and it cannot be made into one.
// opus_compare's Q >= 0 PASS is calibrated for "reference decode vs test decode of
// the SAME bitstream", where the two differ only by rounding. Original-vs-lossy is
// a different quantity entirely, and the measured values below (on the RFC audio,
// 20 ms, complexity 10, constrained VBR) show exactly why no absolute threshold can
// be trusted:
//
//   - Q is NOT monotone in bitrate per clip. rfc12-stereo scores -127.7 at 24 kbps
//     and -210.4 at 64 kbps. rfc03-stereo scores +7.3 at 64 kbps and -3.8 at 128
//     kbps. Raising the rate can LOWER Q, because the extra bits go into high bands
//     where the reference has almost no energy, and Q is a per-band LOG error.
//   - Q is dominated by the reference's own BANDWIDTH. rfc04's .dec is the decode of
//     a narrowband SILK vector, so it has essentially nothing above 4 kHz; the
//     CELT-only encoder codes fullband and puts quantization noise up there, and
//     rfc04 therefore scores about -150 at EVERY rate, forever, while rfc10 scores
//     +78 at 128 kbps.
//
// So the floors below are set from the WORST measured clip at each rate with a wide
// margin (worst: -467.9 at 24k, -222.6 at 64k, -159.1 at 128k). They catch "the
// encoder started emitting mush" and nothing finer. The assertion that carries real
// weight is the AGGREGATE MONOTONICITY below it.
var qualityFloor = map[int]float64{
	24000:  -600,
	64000:  -350,
	128000: -260,
}

// qualityMeanFloor is the floor on the MEAN Q at 128 kbps over the 24 RFC clips
// (measured: -20.7). A mean is not dominated by the band-limited outliers the way a
// single clip is.
const qualityMeanFloor = -60.0

// qualityMeanGain is the minimum improvement in MEAN Q per bitrate step. This is the
// assertion with teeth: individual clips are not monotone in bitrate, but the mean
// over 24 real speech and music clips is, and by a wide margin (measured: -169.3 at
// 24 kbps, -60.5 at 64 kbps, -20.7 at 128 kbps, i.e. gains of 108.8 and 39.8). An
// encoder that had started emitting noise would flatten or invert that.
const qualityMeanGain = 20.0

// TestOpusencPhase4GateQuality is the OPTIONAL quality smoke test: encode the real
// RFC audio through the Go encoder, decode it through the Go decoder, and score the
// result against the ORIGINAL with the ported opus_compare.
//
// It is deliberately weaker than the round-trip above, and it is here for the one
// thing the bit-exact tiers cannot give: evidence that the codec is coding AUDIO and
// not noise. Bit-exactness against C would be satisfied just as happily by two
// encoders that agreed on garbage.
//
// BOTH of internal/opuscompare's traps are handled explicitly: the reference is
// always read as interleaved stereo regardless of Config.Channels (compare.go:143),
// so a mono reference is duplicated into 2 channels and its internal 0.5*(L+R)
// downmix recovers the mono exactly; and Result.Passed is NEVER consulted.
func TestOpusencPhase4GateQuality(t *testing.T) {
	if !rfcVectorsPresent() {
		t.Skipf("the quality smoke test needs real audio; %s is absent", rfcVectorsDir)
	}
	clips := rfcClips(t, corpusSeconds)

	var (
		mu    sync.Mutex
		score = map[int][]float64{} // bitrate -> Q, one per clip
	)

	t.Run("clips", func(t *testing.T) {
		for _, c := range clips {
			for _, bitrate := range qualityBitrates {
				name := fmt.Sprintf("%s/%dbps", c.name, bitrate)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					q := qualityOfClip(t, c, bitrate)
					t.Logf("Q = %8.2f   [Result.Passed is NOT consulted: it is calibrated for "+
						"decode-vs-decode, not original-vs-lossy]", q)
					if floor := qualityFloor[bitrate]; q < floor {
						t.Errorf("QUALITY CRASH BARRIER: Q = %.2f at %d bps, floor %.2f. The packets "+
							"are still bit-exact against C (the gate proves that), so this means the "+
							"Go AND C encoders now AGREE ON MUSH, or the barrier is stale.",
							q, bitrate, floor)
					}
					mu.Lock()
					score[bitrate] = append(score[bitrate], q)
					mu.Unlock()
				})
			}
		}
	})

	// The aggregate, which is the assertion with teeth.
	means := make([]float64, len(qualityBitrates))
	for i, br := range qualityBitrates {
		qs := score[br]
		if len(qs) != len(clips) {
			t.Fatalf("%d bps: %d scores for %d clips", br, len(qs), len(clips))
		}
		var sum float64
		for _, q := range qs {
			sum += q
		}
		means[i] = sum / float64(len(qs))
		t.Logf("MEAN Q over %d RFC clips at %6d bps: %8.2f", len(qs), br, means[i])
	}

	for i := 1; i < len(means); i++ {
		if means[i] < means[i-1]+qualityMeanGain {
			t.Errorf("QUALITY REGRESSION: mean Q went from %.2f at %d bps to %.2f at %d bps, "+
				"a gain of %.2f; at least %.2f is expected. More bits MUST buy a better score "+
				"in the mean, and an encoder that had degenerated into noise would not.",
				means[i-1], qualityBitrates[i-1], means[i], qualityBitrates[i],
				means[i]-means[i-1], qualityMeanGain)
		}
	}
	if top := means[len(means)-1]; top < qualityMeanFloor {
		t.Errorf("QUALITY REGRESSION: mean Q at the top rate (%d bps) is %.2f, floor %.2f",
			qualityBitrates[len(qualityBitrates)-1], top, qualityMeanFloor)
	}
}

// qualityOfClip encodes one clip at one bitrate through the Go encoder, decodes it
// through the Go decoder, aligns the two signals, and returns the opus_compare
// quality of the decode against the ORIGINAL.
func qualityOfClip(t *testing.T, c corpusClip, bitrate int) float64 {
	t.Helper()
	const frameSize = 960 // 20 ms

	cfg := defaultOpusencCfg(c.channels)
	cfg.Fs = corpusRate
	cfg.Bitrate = bitrate
	cfg.VBR = 1
	cfg.VBRConstraint = 1
	cfg.Complexity = 10

	enc := goEncoderMatchingCfg(t, cfg)
	dec, err := opus.NewDecoder(corpusRate, c.channels)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	nFrames := c.countFrames(frameSize)
	decoded := make([]int16, 0, nFrames*frameSize*c.channels)
	buf := make([]byte, gateMaxDataBytes+1)
	pcmOut := make([]int16, frameSize*c.channels)
	for i := 0; i < nFrames; i++ {
		ret := enc.EncodeRaw(c.frameAt(i, frameSize), frameSize, buf, gateMaxDataBytes)
		if ret <= 0 {
			t.Fatalf("frame %d: EncodeRaw returned %d", i, ret)
		}
		n, decErr := dec.Decode(buf[:ret], pcmOut)
		if decErr != nil {
			t.Fatalf("frame %d: decode: %v", i, decErr)
		}
		decoded = append(decoded, pcmOut[:n*c.channels]...)
	}

	// ALIGNMENT. The decoded stream LAGS the input by the encoder's lookahead
	// (Fs/400 + delay_compensation == 312 at 48 kHz), so the leading `lookahead`
	// decoded samples are the encoder's priming and must be dropped before the two
	// signals can be compared at all. A spectral score is far too blunt to catch a
	// misalignment of a few hundred samples on its own, which is exactly why the
	// SAMPLE-ACCURATE alignment test lives in the container unit; here the drop just
	// has to be right.
	la := enc.Lookahead()
	if la*c.channels >= len(decoded) {
		t.Fatalf("lookahead %d is longer than the decode", la)
	}
	test := decoded[la*c.channels:]

	// opus_compare ALWAYS reads the reference as interleaved stereo, regardless of
	// Config.Channels, so a MONO reference has to be duplicated into 2 channels.
	ref := c.pcm
	if c.channels == 1 {
		ref = stereoImage(c.pcm, "identical")
	}
	nf := min(len(ref)/2, len(test)/c.channels)
	res, err := opuscompare.CompareInt16(ref[:nf*2], test[:nf*c.channels],
		opuscompare.Config{Channels: c.channels})
	if err != nil {
		t.Fatalf("opus_compare: %v", err)
	}
	return res.Quality
}
