//go:build refc

package oracle

// Differential test for the pure-Go SILK sample-rate converter (internal/silk
// resampler.go + resampler_private.go + resampler_rom.go) against the pinned libopus
// C (silk/resampler.c and its private up2_HQ / IIR_FIR / down_FIR / AR2 filters over
// the resampler_rom.c coefficient tables).
//
// TestSilkResamplerSequenceMatchesC covers every decoder-path (Fs_in, Fs_out) pair:
// Fs_in in {8, 12, 16} kHz x Fs_out in {8, 12, 16, 24, 48} kHz (15 pairs), which
// exercises all four dispatch cases of silk_resampler_init(forEnc=0):
//
//   - copy            (8->8, 12->12, 16->16)
//   - up2_HQ_wrapper  (8->16, 12->24)
//   - IIR_FIR         (8->12, 8->24, 8->48, 12->16, 12->48, 16->24, 16->48)
//   - down_FIR        (12->8 = 2:3, 16->12 = 3:4, 16->8 = 1:2)
//
// The resampler is a cross-call state machine (allpass / AR2 / FIR memory plus the
// 1 ms delay buffer carry between calls), so the test drives a multi-block SEQUENCE
// through the C and the Go resampler IN LOCKSTEP, carrying the state across blocks on
// both sides, and after every block asserts (a) the int16 output and (b) the full
// filter state (sIIR, the sFIR union, delayBuf) are bit-identical. Blocks mix tones,
// sweeps, white noise, impulses, full-scale square waves (to trip the SAT16 clamps),
// ramps and silence, at 10/20/30 ms lengths so the second dispatch call spans several
// batchSize (10 ms) inner blocks, exercising the cross-batch buffer shift and its state
// carry. It also asserts the Go ResamplerInit derives the identical configuration
// (Fs kHz, batchSize, invRatio_Q16, FIR order/fracs, selected function, input delay).
//
// TestSilkResamplerMutationDetected proves the equality assertion is not vacuous:
// perturbing a single input sample makes the Go output diverge from the C baseline of
// the unperturbed input.

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/silk"
)

// rsmpFsIn / rsmpFsOut are the SILK internal and API sampling rates the decoder
// resampler bridges (silk/resampler.c, decoder rows/columns of the method matrix).
var (
	rsmpFsIn  = []int32{8000, 12000, 16000}
	rsmpFsOut = []int32{8000, 12000, 16000, 24000, 48000}
)

// --- signal generators (each returns n int16 samples), prefixed rsmp to avoid
// colliding with the other oracle tests' generators in this package. ---

func rsmpSilence(n int) []int16 { return make([]int16, n) }

func rsmpTone(n int, cycles, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*cycles*float64(i)/float64(n)))
	}
	return out
}

func rsmpSweep(n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		// instantaneous frequency ramps across the block
		phase := math.Pi * float64(i) * float64(i) / float64(n)
		out[i] = int16(22000 * math.Sin(phase))
	}
	return out
}

func rsmpNoise(n int, r *rand.Rand) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(r.Intn(65536) - 32768)
	}
	return out
}

func rsmpImpulse(n int) []int16 {
	out := make([]int16, n)
	if n > 0 {
		out[n/2] = 30000
		out[0] = -32768
	}
	return out
}

func rsmpFullScale(n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = 32767
		} else {
			out[i] = -32768
		}
	}
	return out
}

func rsmpRamp(n int) []int16 {
	out := make([]int16, n)
	d := n - 1
	if d < 1 {
		d = 1
	}
	for i := range out {
		out[i] = int16(-30000 + (60000*i)/d)
	}
	return out
}

// rsmpBlockSpec describes one block of the driving sequence: length in ms and a
// generator keyed by kind.
type rsmpBlockSpec struct {
	ms   int
	kind string
}

func rsmpGenBlock(spec rsmpBlockSpec, fsInKHz int, r *rand.Rand) []int16 {
	n := spec.ms * fsInKHz
	switch spec.kind {
	case "silence":
		return rsmpSilence(n)
	case "tone":
		return rsmpTone(n, float64(spec.ms)*3.0, 20000)
	case "tone2":
		return rsmpTone(n, float64(spec.ms)*7.5, 26000)
	case "sweep":
		return rsmpSweep(n)
	case "noise":
		return rsmpNoise(n, r)
	case "impulse":
		return rsmpImpulse(n)
	case "fullscale":
		return rsmpFullScale(n)
	case "ramp":
		return rsmpRamp(n)
	default:
		return rsmpSilence(n)
	}
}

// rsmpSequence is the block sequence driven through both resamplers. Lengths mix
// 10/20/30 ms so the non-1ms dispatch call spans several batchSize (10 ms) inner blocks.
var rsmpSequence = []rsmpBlockSpec{
	{10, "silence"},
	{20, "tone"},
	{10, "impulse"},
	{30, "sweep"},
	{10, "noise"},
	{20, "fullscale"},
	{10, "ramp"},
	{20, "tone2"},
	{30, "noise"},
	{10, "tone"},
}

func rsmpGoSnapshot(S *silk.ResamplerState) resamplerStateSnapshot {
	return resamplerStateSnapshot{sIIR: S.SIIR, sFIR: S.SFIR, delayBuf: S.DelayBuf}
}

func rsmpEqual(a, b []int16) bool {
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

func rsmpAllZero(a []int16) bool {
	for _, v := range a {
		if v != 0 {
			return false
		}
	}
	return true
}

func rsmpFirstDiff(a, b []int16) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}

func rsmpRateName(fs int32) string {
	switch fs {
	case 8000:
		return "8k"
	case 12000:
		return "12k"
	case 16000:
		return "16k"
	case 24000:
		return "24k"
	case 48000:
		return "48k"
	default:
		return "x"
	}
}

func TestSilkResamplerSequenceMatchesC(t *testing.T) {
	for _, fsIn := range rsmpFsIn {
		for _, fsOut := range rsmpFsOut {
			fsIn, fsOut := fsIn, fsOut
			t.Run(rsmpRateName(fsIn)+"_to_"+rsmpRateName(fsOut), func(t *testing.T) {
				fsInKHz := int(fsIn / 1000)
				fsOutKHz := int(fsOut / 1000)

				rc, cfg := newResamplerC(fsIn, fsOut, 0)
				var goS silk.ResamplerState
				goRet := silk.ResamplerInit(&goS, fsIn, fsOut, 0)

				// Init configuration must be bit-identical.
				if goRet != cfg.ret {
					t.Fatalf("init ret: go=%d c=%d", goRet, cfg.ret)
				}
				if goS.FsInKHz != cfg.fsInKHz || goS.FsOutKHz != cfg.fsOutKHz ||
					goS.BatchSize != cfg.batchSize || goS.InvRatioQ16 != cfg.invRatioQ16 ||
					goS.FIROrder != cfg.firOrder || goS.FIRFracs != cfg.firFracs ||
					goS.ResamplerFunction != cfg.resamplerFunction || goS.InputDelay != cfg.inputDelay {
					t.Fatalf("init config mismatch:\n go func=%d fsIn=%d fsOut=%d batch=%d invR=%d firOrd=%d firFr=%d delay=%d\n  c = %+v",
						goS.ResamplerFunction, goS.FsInKHz, goS.FsOutKHz, goS.BatchSize, goS.InvRatioQ16,
						goS.FIROrder, goS.FIRFracs, goS.InputDelay, cfg)
				}

				r := rand.New(rand.NewSource(0x5117 ^ int64(fsIn) ^ int64(fsOut)<<8))
				sawNonZero := false
				for bi, spec := range rsmpSequence {
					in := rsmpGenBlock(spec, fsInKHz, r)
					inLen := len(in)
					outLen := (inLen / fsInKHz) * fsOutKHz

					goOut := make([]int16, outLen)
					cOut := make([]int16, outLen+64) // guard slack against a C over-write

					cSnap := rc.process(cOut, in, int32(inLen))
					silk.Resampler(&goS, goOut, in, int32(inLen))
					goSnap := rsmpGoSnapshot(&goS)

					if !rsmpEqual(goOut, cOut[:outLen]) {
						di := rsmpFirstDiff(goOut, cOut[:outLen])
						t.Fatalf("block %d (%dms %s): output mismatch at sample %d: go=%d c=%d",
							bi, spec.ms, spec.kind, di, goOut[di], cOut[di])
					}
					if goSnap != cSnap {
						t.Fatalf("block %d (%dms %s): state mismatch\n go=%+v\n  c=%+v",
							bi, spec.ms, spec.kind, goSnap, cSnap)
					}
					if !rsmpAllZero(goOut) {
						sawNonZero = true
					}
				}
				if !sawNonZero {
					t.Fatalf("all output blocks were zero; test would be vacuous")
				}
			})
		}
	}
}

func TestSilkResamplerMutationDetected(t *testing.T) {
	fsIn, fsOut := int32(16000), int32(48000) // IIR_FIR, wide allpass+FIR path
	fsInKHz := int(fsIn / 1000)
	fsOutKHz := int(fsOut / 1000)
	inLen := 20 * fsInKHz
	outLen := (inLen / fsInKHz) * fsOutKHz

	in := rsmpTone(inLen, 30, 24000)

	// Baseline: identical input through C and a fresh Go resampler must agree and be
	// non-trivial.
	rc, _ := newResamplerC(fsIn, fsOut, 0)
	var goS silk.ResamplerState
	silk.ResamplerInit(&goS, fsIn, fsOut, 0)
	cOut := make([]int16, outLen+64)
	goOut := make([]int16, outLen)
	rc.process(cOut, in, int32(inLen))
	silk.Resampler(&goS, goOut, in, int32(inLen))
	if !rsmpEqual(goOut, cOut[:outLen]) {
		t.Fatalf("baseline output mismatch")
	}
	if rsmpAllZero(goOut) {
		t.Fatalf("baseline output all zero; mutation check would be vacuous")
	}

	// Perturb a single input sample; a fresh Go run must now diverge from the C
	// baseline, proving the bit-exact comparison is sensitive to the input.
	in2 := append([]int16(nil), in...)
	in2[inLen/2] ^= 0x40
	var goS2 silk.ResamplerState
	silk.ResamplerInit(&goS2, fsIn, fsOut, 0)
	goOut2 := make([]int16, outLen)
	silk.Resampler(&goS2, goOut2, in2, int32(inLen))
	if rsmpEqual(goOut2, cOut[:outLen]) {
		t.Fatalf("mutation not detected: perturbed input produced identical output")
	}
}
