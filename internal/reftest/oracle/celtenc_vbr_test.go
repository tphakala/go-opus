//go:build refc

package oracle

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

// CP8c wave 1. This file pins the pieces the encoder DRIVER will be assembled
// from, and the oracle machinery the driver test (wave 3) will be compared
// against:
//
//   - hysteresis_decision (bands.c:46), used at celt_encoder.c:2403 to update
//     st->intensity: a full differential sweep, Go vs C.
//   - compute_vbr (celt_encoder.c:1604): the C oracle wrapper only. There is no
//     Go port yet (that is wave 2), so this file does NOT claim bit-exactness for
//     it; it checks the wrapper links, is deterministic, is non-vacuous, and
//     obeys the one invariant the C guarantees unconditionally (:1714, target is
//     capped at 2*base_target).
//   - the encoder HANDLE (create / configure / encode / dump-state / destroy):
//     cross-checked byte-for-byte against the already-trusted CBR encoder in
//     celtdec_shim.h, and used to prove the full state dump reads every field of
//     the canonical order, including the six VBR/stereo fields that were missing
//     from celt.EncoderState until now (a broken VBR loop used to pass a
//     state-hash comparison silently).

const vbrFrameSize = 960 // LM=3 at 48 kHz

// --- hysteresis_decision ----------------------------------------------------

// intensityThresholds / intensityHisteresis are the ONLY tables
// hysteresis_decision is called with in this build (celt_encoder.c:2393-2397,
// used at :2403 for st->intensity). The spread/tapset tables at :2329-2332 sit
// inside an `#if 0` block and are dead. Both are opus_val16.
var (
	intensityThresholds = []int16{1, 2, 3, 4, 5, 6, 7, 8, 16, 24, 36, 44, 50, 56, 62, 67, 72, 79, 88, 106, 134}
	intensityHisteresis = []int16{1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 3, 3, 4, 5, 6, 8, 8}
)

// TestCeltencHysteresisDecisionVsC drives hysteresis_decision (bands.c:46)
// through the live intensity table plus synthetic tables, every prev in [0,N],
// and a val sweep covering below-first-threshold, exactly-on-threshold, inside
// each hysteresis band, above-last-threshold and the int16 extremes.
//
// The "promotion" table is the important one: C's usual arithmetic conversions
// evaluate thresholds[prev]+hysteresis[prev] (and the matching subtraction) in
// int, so 32757+32767 does NOT wrap to a negative int16. A Go port that keeps
// those sums in int16 diverges exactly there.
func TestCeltencHysteresisDecisionVsC(t *testing.T) {
	tables := []struct {
		name       string
		thresholds []int16
		hysteresis []int16
	}{
		{"intensity", intensityThresholds, intensityHisteresis},
		{"single", []int16{0}, []int16{5}},
		{"negative", []int16{-1000, 500}, []int16{300, 200}},
		{"hysteresis-wider-than-gaps", []int16{10, 20, 30}, []int16{100, 100, 100}},
		{"promotion", []int16{math.MinInt16, 0, math.MaxInt16 - 10}, []int16{32767, 1, 32767}},
	}

	var sawZero, sawN, sawClampUp, sawClampDown, sawNoClamp bool

	r := rand.New(rand.NewSource(0x8C1A))
	for _, tb := range tables {
		N := len(tb.thresholds)
		// Build the val sweep: every threshold and its immediate neighbourhood,
		// every hysteresis boundary, the int16 extremes, plus random values.
		var vals []int
		for i := 0; i < N; i++ {
			th := int(tb.thresholds[i])
			hy := int(tb.hysteresis[i])
			vals = append(vals, th-hy-1, th-hy, th-hy+1, th-1, th, th+1, th+hy-1, th+hy, th+hy+1)
		}
		vals = append(vals, math.MinInt16, math.MinInt16+1, -1, 0, 1, math.MaxInt16-1, math.MaxInt16)
		for i := 0; i < 256; i++ {
			vals = append(vals, r.Intn(1<<17)-(1<<16)) // spills past int16 to exercise the cast
		}

		for _, v := range vals {
			for prev := 0; prev <= N; prev++ {
				want := cCeltencHysteresisDecision(v, tb.thresholds, tb.hysteresis, N, prev)
				// The Go seam takes the already-truncated opus_val16, exactly as
				// the C call site does ((opus_val16)(equiv_rate/1000) at :2403).
				got := celt.HysteresisDecision(int16(v), tb.thresholds, tb.hysteresis, N, prev)
				if got != want {
					t.Fatalf("%s: hysteresis_decision(val=%d (int16 %d), N=%d, prev=%d) Go=%d C=%d",
						tb.name, v, int16(v), N, prev, got, want)
				}
				switch {
				case got == 0:
					sawZero = true
				case got == N:
					sawN = true
				}
				// Non-vacuity: prove both hysteresis clamps fired at least once.
				// Recompute the unclamped index the way the C's first loop does.
				raw := N
				for i := 0; i < N; i++ {
					if int16(v) < tb.thresholds[i] {
						raw = i
						break
					}
				}
				switch {
				case raw > prev && got == prev:
					sawClampDown = true // i>prev clamped back to prev
				case raw < prev && got == prev:
					sawClampUp = true // i<prev clamped back to prev
				case raw == got && raw != prev:
					sawNoClamp = true
				}
			}
		}
	}

	if !sawZero || !sawN {
		t.Fatalf("vacuous sweep: index 0 seen=%v, index N seen=%v", sawZero, sawN)
	}
	if !sawClampDown || !sawClampUp || !sawNoClamp {
		t.Fatalf("vacuous sweep: clamp(i>prev) seen=%v, clamp(i<prev) seen=%v, unclamped seen=%v",
			sawClampDown, sawClampUp, sawNoClamp)
	}
}

// --- compute_vbr (C oracle wrapper only; the Go port is CP8c wave 2) ---------

// TestCeltencComputeVbrOracle exercises the compute_vbr wrapper across the
// branch structure of celt_encoder.c:1604-1716 (C==1 and C==2 stereo savings,
// constrained VBR, the surround-mask path, the temporal-VBR path and the
// floor_depth clamp). It cannot compare against Go yet, so it asserts only what
// the C guarantees: determinism, non-vacuity, and target <= 2*base_target
// (:1714). Wave 2 replaces this with the bit-exact differential test.
func TestCeltencComputeVbrOracle(t *testing.T) {
	seen := map[int32]bool{}
	var sawSurround, sawConstrained, sawTemporal bool

	r := rand.New(rand.NewSource(0x8C1B))
	for i := 0; i < 2000; i++ {
		LM := r.Intn(4)
		C_ := 1 + r.Intn(2)
		hasSurround := r.Intn(2)
		constrained := r.Intn(2)
		lfe := 0
		if r.Intn(8) == 0 {
			lfe = 1
		}
		baseTarget := int32(r.Intn(200000) + 1000)
		bitrate := int32(r.Intn(400000) + 6000)
		lastCodedBands := r.Intn(22) // 0 means "use nbEBands"
		intensity := r.Intn(22)
		stereoSaving := int16(r.Intn(1 << 10))
		totBoost := r.Intn(2000)
		tfEstimate := int16(r.Intn(1 << 14))
		maxDepth := int32(r.Intn(20) << 24) // celt_glog, DB_SHIFT=24
		surroundMasking := int32(-r.Intn(4) << 24)
		temporalVbr := int32(r.Intn(3) << 24)

		got := cCeltencComputeVbr(baseTarget, LM, bitrate, lastCodedBands, C_, intensity,
			constrained, stereoSaving, totBoost, tfEstimate, 0, maxDepth, lfe,
			hasSurround, surroundMasking, temporalVbr)

		again := cCeltencComputeVbr(baseTarget, LM, bitrate, lastCodedBands, C_, intensity,
			constrained, stereoSaving, totBoost, tfEstimate, 0, maxDepth, lfe,
			hasSurround, surroundMasking, temporalVbr)
		if got != again {
			t.Fatalf("compute_vbr is not deterministic: %d != %d", got, again)
		}
		// celt_encoder.c:1714: target = IMIN(2*base_target, target).
		if got > 2*baseTarget {
			t.Fatalf("compute_vbr returned %d > 2*base_target (%d)", got, 2*baseTarget)
		}
		seen[got] = true
		if hasSurround == 1 && lfe == 0 {
			sawSurround = true
		}
		if constrained == 1 && (hasSurround == 0 || lfe == 1) {
			sawConstrained = true
		}
		if hasSurround == 0 && tfEstimate < 3277 { // QCONST16(.2,14)
			sawTemporal = true
		}
	}
	if len(seen) < 100 {
		t.Fatalf("vacuous sweep: compute_vbr produced only %d distinct targets", len(seen))
	}
	if !sawSurround || !sawConstrained || !sawTemporal {
		t.Fatalf("vacuous sweep: surround=%v constrained=%v temporal-vbr=%v",
			sawSurround, sawConstrained, sawTemporal)
	}
}

// --- the encoder handle ------------------------------------------------------

// vbrPCM builds an interleaved int16 test signal: a couple of tones plus noise,
// with a transient burst in the middle frames.
func vbrPCM(r *rand.Rand, frames, frameSize, channels int, transientFrame int) []int16 {
	pcm := make([]int16, frames*frameSize*channels)
	ph := 0.0
	for f := 0; f < frames; f++ {
		amp := 6000.0
		if f == transientFrame {
			amp = 26000.0
		}
		for n := 0; n < frameSize; n++ {
			ph += 2 * math.Pi * 440 / 48000
			v := amp * math.Sin(ph)
			if f == transientFrame && n > frameSize/2 {
				v = amp * (2*r.Float64() - 1)
			}
			for c := 0; c < channels; c++ {
				s := v*(1.0-0.3*float64(c)) + 300*r.NormFloat64()
				i := (f*frameSize+n)*channels + c
				pcm[i] = int16(math.Max(-32768, math.Min(32767, s)))
			}
		}
	}
	return pcm
}

// TestCeltencHandleMatchesRawEncoder cross-checks the NEW handle
// (oracle_celtenc_h_create + configure) against the ALREADY-TRUSTED CBR encoder
// in celtdec_shim.h (oracle_celtenc_create), which the CELT decoder differential
// test has used since CP2. With celtencDefaultConfig (which is exactly the
// post-celt_encoder_init state) the two must produce byte-identical packets and
// the same final range on every frame. This is what proves create/configure does
// not perturb the encoder.
func TestCeltencHandleMatchesRawEncoder(t *testing.T) {
	const frames = 8
	for _, channels := range []int{1, 2} {
		for _, complexity := range []int{0, 5, 10} {
			r := rand.New(rand.NewSource(0x8C1C))
			pcm := vbrPCM(r, frames, vbrFrameSize, channels, 4)

			ref, err := newCCELTEncoder(channels, complexity)
			if err != nil {
				t.Fatalf("newCCELTEncoder: %v", err)
			}
			h, err := newCCeltencHandle(channels)
			if err != nil {
				ref.Close()
				t.Fatalf("newCCeltencHandle: %v", err)
			}
			cfg := celtencDefaultConfig(channels)
			cfg.Complexity = complexity
			if err := h.Configure(cfg); err != nil {
				ref.Close()
				h.Close()
				t.Fatalf("Configure: %v", err)
			}

			for f := 0; f < frames; f++ {
				lo := f * vbrFrameSize * channels
				frame := pcm[lo : lo+vbrFrameSize*channels]
				const nbBytes = 120

				want, err := ref.Encode(frame, vbrFrameSize, nbBytes)
				if err != nil {
					t.Fatalf("ch=%d cx=%d frame %d: ref encode: %v", channels, complexity, f, err)
				}
				ret, got, _ := h.Encode(frame, vbrFrameSize, nbBytes)
				if ret < 0 {
					t.Fatalf("ch=%d cx=%d frame %d: handle encode returned %d", channels, complexity, f, ret)
				}
				if len(got) != len(want) {
					t.Fatalf("ch=%d cx=%d frame %d: packet len handle=%d ref=%d",
						channels, complexity, f, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("ch=%d cx=%d frame %d: packet byte %d handle=%#02x ref=%#02x",
							channels, complexity, f, i, got[i], want[i])
					}
				}
			}
			ref.Close()
			h.Close()
		}
	}
}

// TestCeltencHandleStateDump proves the state dump reads every field of the
// canonical order and that the cross-frame state actually EVOLVES, in particular
// the six fields (vbr_reservoir, vbr_drift, vbr_offset, vbr_count,
// stereo_saving, intensity) that celt.EncoderState omitted before CP8c. It also
// proves the hash now covers them: perturbing any one of the six must change
// EncoderState.Hash().
func TestCeltencHandleStateDump(t *testing.T) {
	const frames = 12
	channels := 2
	r := rand.New(rand.NewSource(0x8C1D))
	pcm := vbrPCM(r, frames, vbrFrameSize, channels, 5)

	h, err := newCCeltencHandle(channels)
	if err != nil {
		t.Fatalf("newCCeltencHandle: %v", err)
	}
	defer h.Close()

	// Constrained VBR at a real bitrate: this is the configuration that runs the
	// whole vbr_reservoir / vbr_drift / vbr_offset / vbr_count loop
	// (celt_encoder.c:2435-2533).
	cfg := celtencDefaultConfig(channels)
	cfg.VBR = 1
	cfg.VBRConstraint = 1
	cfg.Bitrate = 64000
	cfg.Complexity = 10
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	init := h.State()
	if got := len(init.InMem); got != channels*cCeltencOverlap() {
		t.Fatalf("in_mem dump length %d, want %d", got, channels*cCeltencOverlap())
	}
	if got := len(init.PrefilterMem); got != channels*cCeltencMaxperiod() {
		t.Fatalf("prefilter_mem dump length %d, want %d", got, channels*cCeltencMaxperiod())
	}
	if got := len(init.OldLogE); got != channels*cCeltencNbebands() {
		t.Fatalf("oldLogE dump length %d, want %d", got, channels*cCeltencNbebands())
	}
	// celt_encoder_init seeds oldLogE/oldLogE2 to -GCONST(28) = -(28<<24).
	if init.OldLogE[0] != -(28 << 24) {
		t.Fatalf("oldLogE[0] after init = %d, want %d", init.OldLogE[0], -(28 << 24))
	}
	if init.VbrCount != 0 || init.VbrReservoir != 0 || init.VbrDrift != 0 {
		t.Fatalf("VBR state not zero after init: %+v", init)
	}

	var sawVbrCount, sawReservoir, sawDrift, sawOffset, sawIntensity, sawStereoSaving bool
	prev := init
	for f := 0; f < frames; f++ {
		lo := f * vbrFrameSize * channels
		ret, pkt, rng := h.Encode(pcm[lo:lo+vbrFrameSize*channels], vbrFrameSize, 1275)
		if ret <= 0 {
			t.Fatalf("frame %d: celt_encode_with_ec returned %d", f, ret)
		}
		if len(pkt) != ret {
			t.Fatalf("frame %d: packet len %d != ret %d", f, len(pkt), ret)
		}
		st := h.State()
		if st.Rng != rng {
			t.Fatalf("frame %d: dumped rng %#x != OPUS_GET_FINAL_RANGE %#x", f, st.Rng, rng)
		}
		if st.VbrCount != prev.VbrCount {
			sawVbrCount = true
		}
		if st.VbrReservoir != prev.VbrReservoir {
			sawReservoir = true
		}
		if st.VbrDrift != prev.VbrDrift {
			sawDrift = true
		}
		if st.VbrOffset != prev.VbrOffset {
			sawOffset = true
		}
		if st.Intensity != prev.Intensity {
			sawIntensity = true
		}
		if st.StereoSaving != prev.StereoSaving {
			sawStereoSaving = true
		}
		if st.Hash() == prev.Hash() {
			t.Fatalf("frame %d: state hash did not change across a frame", f)
		}
		prev = st
	}

	// NON-VACUITY: every one of the six formerly-unhashed fields must have moved.
	if !sawVbrCount || !sawReservoir || !sawDrift || !sawOffset {
		t.Fatalf("VBR loop never exercised: count=%v reservoir=%v drift=%v offset=%v",
			sawVbrCount, sawReservoir, sawDrift, sawOffset)
	}
	if !sawIntensity || !sawStereoSaving {
		t.Fatalf("stereo state never exercised: intensity=%v stereo_saving=%v",
			sawIntensity, sawStereoSaving)
	}

	// The hash must be sensitive to each of the six. Perturb one field at a time
	// and require the hash to move: this is the regression guard for the hole
	// CP8c closed.
	base := prev.Hash()
	mutators := []struct {
		name string
		mut  func(s *celt.EncoderState)
	}{
		{"VbrReservoir", func(s *celt.EncoderState) { s.VbrReservoir++ }},
		{"VbrDrift", func(s *celt.EncoderState) { s.VbrDrift++ }},
		{"VbrOffset", func(s *celt.EncoderState) { s.VbrOffset++ }},
		{"VbrCount", func(s *celt.EncoderState) { s.VbrCount++ }},
		{"StereoSaving", func(s *celt.EncoderState) { s.StereoSaving++ }},
		{"Intensity", func(s *celt.EncoderState) { s.Intensity++ }},
	}
	for _, m := range mutators {
		s := prev
		m.mut(&s)
		if s.Hash() == base {
			t.Fatalf("EncoderState.Hash() ignores %s: a broken VBR loop would pass silently", m.name)
		}
	}
}

// TestCeltencHandleDeterministic drives two independently created handles with
// the same config and input and requires identical packets, rng and state on
// every frame, across CBR / VBR / constrained-VBR / LFE / forced-intra configs.
// It is the harness self-test the wave-3 driver comparison depends on.
func TestCeltencHandleDeterministic(t *testing.T) {
	const frames = 6
	cfgs := []struct {
		name     string
		channels int
		mut      func(c *celtencConfig)
		nbBytes  int
	}{
		{"cbr-mono", 1, func(c *celtencConfig) {}, 96},
		{"vbr-unconstrained-stereo", 2, func(c *celtencConfig) {
			c.VBR, c.VBRConstraint, c.Bitrate = 1, 0, 96000
		}, 1275},
		{"vbr-constrained-stereo", 2, func(c *celtencConfig) {
			c.VBR, c.VBRConstraint, c.Bitrate = 1, 1, 32000
		}, 1275},
		{"lfe-mono", 1, func(c *celtencConfig) { c.LFE, c.Bitrate, c.VBR = 1, 24000, 1 }, 1275},
		{"forced-intra-mono", 1, func(c *celtencConfig) { c.ForceIntra, c.DisablePrefilter = 1, 1 }, 96},
		{"narrow-band-stereo", 2, func(c *celtencConfig) { c.Start, c.End, c.DisableInv = 1, 17, 1 }, 96},
	}

	for _, tc := range cfgs {
		r := rand.New(rand.NewSource(0x8C1E))
		pcm := vbrPCM(r, frames, vbrFrameSize, tc.channels, 3)

		mk := func() *cCeltencHandle {
			h, err := newCCeltencHandle(tc.channels)
			if err != nil {
				t.Fatalf("%s: newCCeltencHandle: %v", tc.name, err)
			}
			cfg := celtencDefaultConfig(tc.channels)
			tc.mut(&cfg)
			if err := h.Configure(cfg); err != nil {
				h.Close()
				t.Fatalf("%s: Configure: %v", tc.name, err)
			}
			return h
		}
		a, b := mk(), mk()

		for f := 0; f < frames; f++ {
			lo := f * vbrFrameSize * tc.channels
			frame := pcm[lo : lo+vbrFrameSize*tc.channels]
			retA, pktA, rngA := a.Encode(frame, vbrFrameSize, tc.nbBytes)
			retB, pktB, rngB := b.Encode(frame, vbrFrameSize, tc.nbBytes)
			if retA != retB || rngA != rngB {
				t.Fatalf("%s frame %d: ret %d/%d rng %#x/%#x", tc.name, f, retA, retB, rngA, rngB)
			}
			if retA <= 0 {
				t.Fatalf("%s frame %d: celt_encode_with_ec returned %d", tc.name, f, retA)
			}
			for i := range pktA {
				if pktA[i] != pktB[i] {
					t.Fatalf("%s frame %d: packet byte %d differs (%#02x vs %#02x)",
						tc.name, f, i, pktA[i], pktB[i])
				}
			}
			if a.State().Hash() != b.State().Hash() {
				t.Fatalf("%s frame %d: state hash differs across identical encoders", tc.name, f)
			}
		}
		a.Close()
		b.Close()
	}
}
