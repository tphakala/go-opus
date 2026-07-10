//go:build refc

package oracle

import (
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
	"github.com/tphakala/go-opus/internal/silk"
)

// This differential test pins the pure-Go SILK excitation decode (internal/silk
// decode_pulses.go / shell_coder.go / code_signs.go: DecodePulses and its shell /
// sign helpers) to the pinned libopus C oracle (silk/decode_pulses.c,
// shell_coder.c, code_signs.c). The Go encoder does not exist yet, so the C
// silk_encode_pulses PRODUCES each coded excitation bitstream; both C and Go then
// DECODE the same bitstream and every output is asserted bit-identical (int16
// element-wise) with matching range-decoder end state (tell + rng). Because the
// excitation coder is a lossless entropy round-trip, the C decode is also checked
// to reproduce the original pulse vector.

// silkFrameLengths is the set of SILK excitation frame lengths in samples: the
// 10 ms and 20 ms frames at the NB(8) / MB(12) / WB(16) kHz internal rates
// (subframe = 5 ms). 120 (10 ms @ 12 kHz) is the only value that is not a multiple
// of SHELL_CODEC_FRAME_LENGTH and exercises the round-up-to-a-whole-shell-block
// path (iter 7 -> 8, 128 decoded samples).
//
//	 8 kHz: 10 ms = 80,  20 ms = 160
//	12 kHz: 10 ms = 120, 20 ms = 240
//	16 kHz: 10 ms = 160, 20 ms = 320
var silkFrameLengths = []int{80, 120, 160, 240, 320}

// silkSignalTypes is TYPE_NO_VOICE_ACTIVITY / TYPE_UNVOICED / TYPE_VOICED
// (define.h). signalType>>1 selects the rate-level iCDF (0 for inactive/unvoiced,
// 1 for voiced) and, together with quantOffsetType, the silk_sign_iCDF row.
var silkSignalTypes = []int{0, 1, 2}

// silkQuantOffsetTypes is the low/high quantization offset selector.
var silkQuantOffsetTypes = []int{0, 1}

// energyTier describes the per-sample magnitude distribution used to build a
// random excitation, from silence to near-max. magMax caps |pulse|; zeroBias is
// the probability a sample is forced to 0 (sparser excitation). The high tiers
// push per-block pulse sums past silk_max_pulses_table so the encoder emits LSB
// shifts (nLshifts > 0 on decode), covering the low-order-bit path and producing
// decoded magnitudes above what a single shell block (sum <= 16) can carry.
type energyTier struct {
	name     string
	magMax   int
	zeroBias float64
	trials   int
}

var pulseEnergyTiers = []energyTier{
	{"silence", 0, 1.0, 1},
	{"sparse", 2, 0.7, 3},
	{"low", 4, 0.3, 3},
	{"medium", 8, 0.1, 4},
	{"high", 20, 0.0, 4},
	{"nearmax", 60, 0.0, 4},
}

// genExcitation builds a random signed pulse vector of the given frame length in
// the opus_int8 domain according to the tier. magMax <= 60 always fits int8.
func genExcitation(r *rand.Rand, frameLength int, tier energyTier) []int8 {
	p := make([]int8, frameLength)
	if tier.magMax == 0 {
		return p
	}
	for i := range p {
		if r.Float64() < tier.zeroBias {
			continue
		}
		m := r.Intn(tier.magMax + 1)
		if r.Intn(2) == 1 {
			m = -m
		}
		p[i] = int8(m)
	}
	return p
}

// TestSilkDecodePulsesMatchesC is the primary check: over every frame length,
// signalType, quantOffsetType and energy tier, the C silk_encode_pulses encodes a
// random excitation into a bitstream, then both C silk_decode_pulses and Go
// silk.DecodePulses decode the SAME bitstream. The decoded pulse vectors must be
// bit-identical (int16 element-wise) and the range-decoder tell/rng must match;
// the C decode must also losslessly reproduce the original excitation.
func TestSilkDecodePulsesMatchesC(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC01))
	cases := 0
	maxAbs := 0 // largest |decoded pulse|; > 16 proves the LSB-shift path ran
	silenceSeen := false
	for _, frameLength := range silkFrameLengths {
		for _, signalType := range silkSignalTypes {
			for _, quantOffsetType := range silkQuantOffsetTypes {
				for _, tier := range pulseEnergyTiers {
					for trial := 0; trial < tier.trials; trial++ {
						in := genExcitation(r, frameLength, tier)

						buf, nbytes := cSilkEncodePulses(in, frameLength, signalType, quantOffsetType)
						if nbytes <= 0 {
							t.Fatalf("frameLength=%d sig=%d qoff=%d tier=%s: encoder produced %d bytes",
								frameLength, signalType, quantOffsetType, tier.name, nbytes)
						}

						pulsesC, tellC, rngC := cSilkDecodePulses(buf, frameLength, signalType, quantOffsetType)

						pulsesGo := make([]int16, len(pulsesC))
						var dec rangecoding.Decoder
						dec.Init(buf)
						silk.DecodePulses(&dec, pulsesGo, signalType, quantOffsetType, frameLength)

						if !equalInt16(pulsesGo, pulsesC) {
							t.Fatalf("frameLength=%d sig=%d qoff=%d tier=%s: pulses Go=%v C=%v",
								frameLength, signalType, quantOffsetType, tier.name, pulsesGo, pulsesC)
						}
						if got := dec.Tell(); got != tellC {
							t.Fatalf("frameLength=%d sig=%d qoff=%d tier=%s: tell Go=%d C=%d",
								frameLength, signalType, quantOffsetType, tier.name, got, tellC)
						}
						if got := dec.Rng(); got != rngC {
							t.Fatalf("frameLength=%d sig=%d qoff=%d tier=%s: rng Go=%d C=%d",
								frameLength, signalType, quantOffsetType, tier.name, got, rngC)
						}

						// Lossless round-trip: the C decode must reproduce the
						// original excitation over the frame_length samples.
						for i := 0; i < frameLength; i++ {
							if int(pulsesC[i]) != int(in[i]) {
								t.Fatalf("frameLength=%d sig=%d qoff=%d tier=%s: round-trip C[%d]=%d != in=%d",
									frameLength, signalType, quantOffsetType, tier.name, i, pulsesC[i], in[i])
							}
						}

						if tier.magMax == 0 {
							silenceSeen = true
							for i, v := range pulsesC {
								if v != 0 {
									t.Fatalf("silence frameLength=%d: pulses[%d]=%d, want 0",
										frameLength, i, v)
								}
							}
						}
						for _, v := range pulsesC {
							a := int(v)
							if a < 0 {
								a = -a
							}
							if a > maxAbs {
								maxAbs = a
							}
						}
						cases++
					}
				}
			}
		}
	}
	if !silenceSeen {
		t.Fatal("silence tier never ran")
	}
	// A decoded magnitude above SILK_MAX_PULSES (16) can only arise via the LSB
	// shift path, so this confirms the high-energy tiers actually exercised it.
	if maxAbs <= 16 {
		t.Fatalf("max |pulse| = %d <= 16: the LSB-shift (nLshifts>0) path was never exercised", maxAbs)
	}
	t.Logf("silk_decode_pulses differential cases: %d (max |pulse| = %d)", cases, maxAbs)
}

// TestSilkDecodePulsesMutation is the non-vacuity check: decoding the same
// bitstream with the WRONG quantOffsetType or signalType selects a different
// silk_sign_iCDF row (and, for signalType, a different rate-level table), so the
// Go decode under the wrong parameter must diverge from the correct C decode in at
// least one high-energy case. If a bug made the port ignore those parameters, this
// would silently pass, so the correct decode is asserted to match first.
func TestSilkDecodePulsesMutation(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC02))
	const frameLength = 320
	qoffMutationCaught := false
	sigMutationCaught := false
	for trial := 0; trial < 32; trial++ {
		signalType := 2 // voiced: dense signs, exercises the sign path fully
		quantOffsetType := trial & 1
		in := genExcitation(r, frameLength, energyTier{magMax: 40})

		buf, nbytes := cSilkEncodePulses(in, frameLength, signalType, quantOffsetType)
		if nbytes <= 0 {
			t.Fatalf("trial %d: encoder produced %d bytes", trial, nbytes)
		}
		pulsesC, _, _ := cSilkDecodePulses(buf, frameLength, signalType, quantOffsetType)

		// Baseline: the correct decode must match (guards against a vacuous test).
		base := make([]int16, len(pulsesC))
		var dec rangecoding.Decoder
		dec.Init(buf)
		silk.DecodePulses(&dec, base, signalType, quantOffsetType, frameLength)
		if !equalInt16(base, pulsesC) {
			t.Fatalf("trial %d: baseline Go decode diverged from C", trial)
		}

		// Mutation A: wrong quantOffsetType (flips the sign iCDF row).
		mutQoff := make([]int16, len(pulsesC))
		var decQ rangecoding.Decoder
		decQ.Init(buf)
		silk.DecodePulses(&decQ, mutQoff, signalType, quantOffsetType^1, frameLength)
		if !equalInt16(mutQoff, pulsesC) || decQ.Rng() != dec.Rng() {
			qoffMutationCaught = true
		}

		// Mutation B: wrong signalType (voiced -> unvoiced changes the rate-level
		// table and the sign iCDF row).
		mutSig := make([]int16, len(pulsesC))
		var decS rangecoding.Decoder
		decS.Init(buf)
		silk.DecodePulses(&decS, mutSig, 1, quantOffsetType, frameLength)
		if !equalInt16(mutSig, pulsesC) || decS.Rng() != dec.Rng() {
			sigMutationCaught = true
		}
	}
	if !qoffMutationCaught {
		t.Fatal("wrong quantOffsetType never diverged; the differential assertion may be vacuous")
	}
	if !sigMutationCaught {
		t.Fatal("wrong signalType never diverged; the differential assertion may be vacuous")
	}
}

// TestSilkDecodePulsesComparatorCatchesPerturbation is a harness self-check: a
// single-element perturbation of a correct C output must be flagged by the
// bit-exact comparator, proving the differential assertions are not vacuous.
func TestSilkDecodePulsesComparatorCatchesPerturbation(t *testing.T) {
	r := rand.New(rand.NewSource(0x51CC03))
	const frameLength = 240
	in := genExcitation(r, frameLength, energyTier{magMax: 30})
	buf, _ := cSilkEncodePulses(in, frameLength, 2, 0)
	pulsesC, _, _ := cSilkDecodePulses(buf, frameLength, 2, 0)

	pulsesGo := make([]int16, len(pulsesC))
	var dec rangecoding.Decoder
	dec.Init(buf)
	silk.DecodePulses(&dec, pulsesGo, 2, 0, frameLength)
	if !equalInt16(pulsesGo, pulsesC) {
		t.Fatalf("baseline decode diverged: Go=%v C=%v", pulsesGo, pulsesC)
	}
	// Perturb one nonzero element and confirm the comparator flags it.
	mut := append([]int16(nil), pulsesC...)
	for i := range mut {
		if mut[i] != 0 {
			mut[i]++
			break
		}
	}
	if equalInt16(mut, pulsesC) {
		t.Fatal("comparator failed to detect a one-element perturbation")
	}
}

func equalInt16(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
