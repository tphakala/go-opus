//go:build refc

package oracle

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/go-opus/internal/packet"
)

// This file is the differential gate for the multiframe encoder path
// (opus_encoder.c:1698-1838): a 40/60/80/100/120 ms frame split into 20 ms
// sub-frames, each coded through encodeFrameNative and reassembled by the
// repacketizer. Every packet is proved byte-identical to the C FIXED_POINT oracle
// via the shared framePkt harness (return value, length, every byte, rangeFinal,
// the full cross-frame state, the state hash, and the witness invariants), plus
// two multiframe-specific checks the byte compare does not localize: the TOC
// frame-count bits, and that the packet parses back as nb_frames 20 ms frames.

// gateMultiframeSizes are 40, 60, 80, 100 and 120 ms at 48 kHz: nb_frames 2..6.
var gateMultiframeSizes = []int{1920, 2880, 3840, 4800, 5760}

// multiframeFramesPerPoint bounds the frames encoded per grid point. Two or more
// is required to exercise cross-frame encoder state; the multiframe assembly logic
// itself is position-independent, and the CELT sub-frame payloads are already
// swept across every clip position by the single-frame phase-4 gate.
const multiframeFramesPerPoint = 3

// TestGateMultiframeSmoke is the first-increment differential test: a single mono,
// CBR, complexity-10, 64 kbps config at 40 ms (1920 samples), a handful of frames.
// It is deliberately tiny for a fast red/green loop; the full sweep is
// TestOpusencMultiframeGate.
func TestGateMultiframeSmoke(t *testing.T) {
	clip := firstClip(t, gateCorpus(t), 1)

	cfg := defaultOpusencCfg(1)
	cfg.Fs = corpusRate
	cfg.Bitrate = 64000
	cfg.VBR = 0
	cfg.VBRConstraint = 0
	cfg.Complexity = 10
	pair := newEncPair(t, cfg)

	const frameSize = 1920 // 40 ms at 48 kHz -> 2x 20 ms sub-frames
	n := clip.countFrames(frameSize)
	if n == 0 {
		t.Fatalf("clip %s holds no whole %d-sample frame", clip.name, frameSize)
	}
	if n > 4 {
		n = 4
	}
	for i := 0; i < n; i++ {
		ret, _, _ := pair.framePkt("mf-smoke", clip.frameAt(i, frameSize), frameSize, gateMaxDataBytes)
		if ret <= 0 {
			t.Fatalf("frame %d: C encoder returned %d for a %d-sample frame; the smoke "+
				"config is not supposed to be starved", i, ret, frameSize)
		}
	}
}

// TestOpusencMultiframeGate is the full multiframe differential sweep: every clip,
// at every multiframe size, over every (bitrate x rate mode x complexity) point.
func TestOpusencMultiframeGate(t *testing.T) {
	clips := gateCorpus(t)
	cov := newMultiframeCoverage()
	start := time.Now()

	t.Run("grid", func(t *testing.T) {
		for _, c := range clips {
			for _, frameSize := range gateMultiframeSizes {
				c, frameSize := c, frameSize
				t.Run(fmt.Sprintf("%s/fs%d", c.name, frameSize), func(t *testing.T) {
					t.Parallel()
					local := newMultiframeCoverage()
					for _, bitrate := range gateBitrates {
						for _, rm := range gateRateModes {
							for _, complexity := range gateComplexities() {
								runMultiframePoint(t, c, frameSize, bitrate, rm,
									complexity, gateMaxDataBytes, local)
							}
						}
					}
					cov.merge(local)
				})
			}
		}
	})

	elapsed := time.Since(start)
	t.Logf("\n=== MULTIFRAME GATE ===\n"+
		"grid:          bitrate %v x {cbr,vbr,cvbr} x complexity 0..10 x frame %v samples @ %d Hz, %d frames/point\n"+
		"clips:         %d\n"+
		"FRAME-PAIRS:   %d (Go encode + C encode + full state compare + TOC + parse-back, each)\n"+
		"nb_frames:     %v\n"+
		"TOC codes:     %v\n"+
		"wall time:     %s\n",
		gateBitrates, gateMultiframeSizes, corpusRate, multiframeFramesPerPoint,
		len(clips), cov.pairs, cov.seenNbFrames(), cov.seenCodes(),
		elapsed.Round(time.Millisecond))

	// NON-VACUITY. The sweep claims to reach every multiframe count and both the
	// two-frame (code 1/2) and the code-3 assembly paths; if any never fired the
	// grid is not what this test says it is.
	for _, nb := range []int{2, 3, 4, 5, 6} {
		if !cov.nbFrames[nb] {
			t.Errorf("vacuous grid: no %d-sub-frame packet was ever assembled (saw %v)",
				nb, cov.seenNbFrames())
		}
	}
	if !cov.codes[3] {
		t.Error("vacuous grid: code 3 (>2 frames, or CBR-padded) was never emitted")
	}
	if !cov.codes[1] && !cov.codes[2] {
		t.Error("vacuous grid: neither code 1 nor code 2 (the two-frame forms) was ever emitted")
	}
	if cov.pairs == 0 {
		t.Fatal("vacuous grid: no multiframe packet was encoded at all")
	}
}

// TestOpusencMultiframeLargeBuffer proves the uncapped-out_data_bytes path
// (opus_encoder.c:1750/1753) byte-for-byte against C. It uses a caller buffer far
// above the :1221 cap (packet_size_cap*6 = 7656) at a high bitrate on a 120 ms
// frame, the regime where repacketize_len and curr_max are actually bound by the
// buffer term. A regression that fed repacketize_len the :1221-CAPPED max_data_bytes
// instead of the uncapped out_data_bytes would diverge here; the ordinary gate,
// which uses a 1500-byte buffer, could never see it.
func TestOpusencMultiframeLargeBuffer(t *testing.T) {
	clips := gateCorpus(t)
	const bigBuf = 10000 // > packet_size_cap*6 (7656)

	// Both 40 ms (nb_frames 2) and 120 ms (nb_frames 6) are covered on purpose: at a
	// high bitrate the 40 ms case drives cbr_bytes above nb_frames*packetPayloadCap,
	// which is exactly where a tempting "cap max_len_sum to nb_frames*packetPayloadCap"
	// optimization would change curr_max and diverge from C. max_len_sum feeds curr_max
	// and C computes it uncapped, so it must stay uncapped here.
	maxSeen := 0
	for _, frameSize := range []int{1920, 5760} {
		for _, ch := range []int{1, 2} {
			clip := firstClip(t, clips, ch)
			for _, rm := range gateRateModes {
				ch, rm, frameSize := ch, rm, frameSize
				t.Run(fmt.Sprintf("fs%d/c%d/%s", frameSize, ch, rm.name), func(t *testing.T) {
					cfg := defaultOpusencCfg(ch)
					cfg.Fs = corpusRate
					cfg.Bitrate = 512000 // clamps down to the buffer ceiling; drives curr_max high
					cfg.VBR = rm.vbr
					cfg.VBRConstraint = rm.vbrConstraint
					cfg.Complexity = 10
					pair := newEncPair(t, cfg)

					n := clip.countFrames(frameSize)
					if n == 0 {
						t.Fatalf("clip %s holds no whole %d-sample frame", clip.name, frameSize)
					}
					if n > 3 {
						n = 3
					}
					label := fmt.Sprintf("bigbuf/fs%d/c%d/%s", frameSize, ch, rm.name)
					for i := 0; i < n; i++ {
						ret, pkt, _ := pair.framePkt(label, clip.frameAt(i, frameSize), frameSize, bigBuf)
						if ret <= 0 {
							t.Fatalf("frame %d: C encoder returned %d", i, ret)
						}
						assertMultiframe(t, label, pkt, frameSize/(corpusRate/50), rm.vbr == 0)
						if len(pkt) > maxSeen {
							maxSeen = len(pkt)
						}
					}
				})
			}
		}
	}

	// NON-VACUITY: the whole point of this test is the uncapped out_data_bytes path,
	// which only differs from the :1221-capped max_data_bytes when the caller buffer
	// exceeds 7656. If every packet stayed at or below the ordinary gate's 1500-byte
	// buffer, the large buffer was never actually exercised and the byte-compare above
	// silently degraded into a duplicate of TestOpusencMultiframeGate. A packet larger
	// than gateMaxDataBytes is only physically possible because bigBuf allowed it.
	if maxSeen <= gateMaxDataBytes {
		t.Fatalf("vacuous: the largest packet was %d bytes, not above the ordinary gate's "+
			"%d-byte buffer, so the large-buffer (> 7656) regime was never reached",
			maxSeen, gateMaxDataBytes)
	}
}

// runMultiframePoint runs one grid point: newEncPair for the config, then
// multiframeFramesPerPoint frames, each byte-compared and structurally checked.
func runMultiframePoint(t *testing.T, clip corpusClip, frameSize, bitrate int,
	rm gateRateMode, complexity, maxDataBytes int, cov *multiframeCoverage) {
	t.Helper()
	cfg := defaultOpusencCfg(clip.channels)
	cfg.Fs = corpusRate
	cfg.Bitrate = bitrate
	cfg.VBR = rm.vbr
	cfg.VBRConstraint = rm.vbrConstraint
	cfg.Complexity = complexity
	pair := newEncPair(t, cfg)

	label := fmt.Sprintf("%s/fs%d/%s/%dbps/cx%d", clip.name, frameSize, rm.name, bitrate, complexity)
	nbFrames := frameSize / (corpusRate / 50)

	n := clip.countFrames(frameSize)
	if n == 0 {
		t.Fatalf("%s: clip holds no whole %d-sample frame", label, frameSize)
	}
	if n > multiframeFramesPerPoint {
		n = multiframeFramesPerPoint
	}
	for i := 0; i < n; i++ {
		ret, pkt, _ := pair.framePkt(label, clip.frameAt(i, frameSize), frameSize, maxDataBytes)
		if ret <= 0 {
			t.Fatalf("%s: frame %d: C encoder returned %d; no grid point is supposed to be "+
				"starved (max_data_bytes = %d)", label, i, ret, maxDataBytes)
		}
		code := assertMultiframe(t, label, pkt, nbFrames, rm.vbr == 0)
		cov.observe(nbFrames, code)
	}
}

// assertMultiframe checks the two multiframe-specific properties the byte compare
// does not localize: the assembled packet parses back as exactly nbFrames 20 ms
// frames, and the TOC frame-count code is the expected one. It returns the TOC code
// (low two bits) for coverage. cbr selects whether a two-frame packet is allowed to
// be promoted to code 3 by padding (repacketizer.go writeHeader: pad && totSize <
// maxlen).
func assertMultiframe(t *testing.T, label string, pkt []byte, nbFrames int, cbr bool) int {
	t.Helper()
	if len(pkt) == 0 {
		t.Fatalf("%s: empty packet", label)
	}
	p, err := packet.Parse(pkt)
	if err != nil {
		t.Fatalf("%s: parsing the assembled packet failed: %v", label, err)
	}
	if len(p.Frames) != nbFrames {
		t.Fatalf("%s: assembled packet holds %d frames, want %d", label, len(p.Frames), nbFrames)
	}
	if spf := p.TOC.SamplesPerFrame(corpusRate); spf != corpusRate/50 {
		t.Fatalf("%s: sub-frame duration = %d samples, want %d (20 ms)", label, spf, corpusRate/50)
	}
	code := int(pkt[0] & 0x3)
	if nbFrames == 2 {
		// Code 1 (two equal frames) or code 2 (two unequal). Under CBR, padding can
		// promote a two-frame packet to code 3 (writeHeader: pad && totSize < maxlen).
		if code != 1 && code != 2 && !(cbr && code == 3) {
			t.Fatalf("%s: two-frame TOC code = %d, want 1 or 2 (or 3 under CBR padding)", label, code)
		}
	} else if code != 3 {
		t.Fatalf("%s: %d-frame TOC code = %d, want 3", label, nbFrames, code)
	}
	return code
}

// firstClip returns the first corpus clip with the requested channel count.
func firstClip(t *testing.T, clips []corpusClip, channels int) corpusClip {
	t.Helper()
	for _, c := range clips {
		if c.channels == channels {
			return c
		}
	}
	t.Fatalf("no %d-channel clip in the corpus", channels)
	return corpusClip{}
}

// multiframeCoverage is the non-vacuity ledger: which sub-frame counts and TOC
// codes the sweep actually reached. observe runs on a per-subtest LOCAL ledger
// (no lock traffic on the hot path); merge folds a local ledger into the shared
// receiver and IS called concurrently as parallel subtests finish, so it locks.
type multiframeCoverage struct {
	mu       sync.Mutex
	pairs    int64
	nbFrames map[int]bool
	codes    map[int]bool
}

func newMultiframeCoverage() *multiframeCoverage {
	return &multiframeCoverage{nbFrames: map[int]bool{}, codes: map[int]bool{}}
}

func (m *multiframeCoverage) observe(nbFrames, code int) {
	m.pairs++
	m.nbFrames[nbFrames] = true
	m.codes[code] = true
}

func (m *multiframeCoverage) merge(o *multiframeCoverage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pairs += o.pairs
	for k := range o.nbFrames {
		m.nbFrames[k] = true
	}
	for k := range o.codes {
		m.codes[k] = true
	}
}

func (m *multiframeCoverage) seenNbFrames() []int { return sortedSetKeys(m.nbFrames) }
func (m *multiframeCoverage) seenCodes() []int    { return sortedSetKeys(m.codes) }

func sortedSetKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
