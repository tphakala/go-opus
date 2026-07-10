//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This differential test pins the pure-Go CELT bit allocation (internal/celt
// rate.go: ComputeAllocation / InitCaps / GetPulses / Bits2Pulses / Pulses2Bits)
// to the pinned libopus C oracle (celt/rate.c clt_compute_allocation +
// interp_bits2pulses, celt/celt.c init_caps, celt/rate.h cache helpers).
//
// clt_compute_allocation interleaves the range coder for the skip/intensity/dual
// flags, so every case is driven through the SAME range-coder buffer four ways:
//   1. C-encode (encode=1)  -> ground-truth bytes + outputs + coder end-state.
//   2. Go-encode (encode=1) -> asserted byte-identical to (1) and outputs equal.
//   3. C-decode (encode=0) over (1)'s bytes -> asserted equal to (1) (round-trip).
//   4. Go-decode (encode=0) over (1)'s bytes -> asserted equal to (3) (primary).
// Every output (pulses[], ebits/fine_quant[], fine_priority[], codedBands,
// balance, resolved intensity/dual_stereo) and the coder end-state (ec_tell + rng)
// must be bit-identical. The sweep is exhaustive over the realistic decoder input
// space because rate.c's thresh/cap/balance-carry corners are unenumerable by
// review (docs/hard-parts.md section 3).

// diffAlloc reports the first differing allocation output, or "" if a==b. Coder
// end-state (tell, rng) is compared separately by diffCoder.
func diffAlloc(a, b rateAllocResult) string {
	if a.codedBands != b.codedBands {
		return fmt.Sprintf("codedBands %d != %d", a.codedBands, b.codedBands)
	}
	if a.intensity != b.intensity {
		return fmt.Sprintf("intensity %d != %d", a.intensity, b.intensity)
	}
	if a.dualStereo != b.dualStereo {
		return fmt.Sprintf("dualStereo %d != %d", a.dualStereo, b.dualStereo)
	}
	if a.balance != b.balance {
		return fmt.Sprintf("balance %d != %d", a.balance, b.balance)
	}
	for i := range a.pulses {
		if a.pulses[i] != b.pulses[i] {
			return fmt.Sprintf("pulses[%d] %d != %d", i, a.pulses[i], b.pulses[i])
		}
	}
	for i := range a.ebits {
		if a.ebits[i] != b.ebits[i] {
			return fmt.Sprintf("ebits[%d] %d != %d", i, a.ebits[i], b.ebits[i])
		}
	}
	for i := range a.finePriority {
		if a.finePriority[i] != b.finePriority[i] {
			return fmt.Sprintf("fine_priority[%d] %d != %d", i, a.finePriority[i], b.finePriority[i])
		}
	}
	return ""
}

func diffCoder(a, b rateAllocResult) string {
	if a.tell != b.tell {
		return fmt.Sprintf("tell %d != %d", a.tell, b.tell)
	}
	if a.rng != b.rng {
		return fmt.Sprintf("rng %d != %d", a.rng, b.rng)
	}
	return ""
}

// goRateAlloc drives the pure-Go celt.ComputeAllocation once over buf. encode!=0
// emits the flags into buf (finalized in place via EncDone); encode==0 reads them
// back. tell/rng are captured BEFORE EncDone, matching the C shim.
func goRateAlloc(start, end int, offsets, caps []int, allocTrim, intensityIn, dualStereoIn,
	total, C, LM, encode, prev, signalBandwidth int, buf []byte) rateAllocResult {
	nbEBands := len(offsets)
	pulses := make([]int, nbEBands)
	ebits := make([]int, nbEBands)
	fp := make([]int, nbEBands)
	intensity := intensityIn
	dualStereo := dualStereoIn
	var res rateAllocResult
	if encode != 0 {
		var enc rangecoding.Encoder
		enc.Init(buf)
		cb, bal := celt.ComputeAllocation(start, end, offsets, caps, allocTrim,
			&intensity, &dualStereo, total, pulses, ebits, fp, C, LM,
			&enc, nil, 1, prev, signalBandwidth)
		res = rateAllocResult{codedBands: cb, pulses: pulses, ebits: ebits, finePriority: fp,
			intensity: intensity, dualStereo: dualStereo, balance: bal, tell: enc.Tell(), rng: enc.Rng()}
		enc.EncDone()
	} else {
		var dec rangecoding.Decoder
		dec.Init(buf)
		cb, bal := celt.ComputeAllocation(start, end, offsets, caps, allocTrim,
			&intensity, &dualStereo, total, pulses, ebits, fp, C, LM,
			nil, &dec, 0, prev, signalBandwidth)
		res = rateAllocResult{codedBands: cb, pulses: pulses, ebits: ebits, finePriority: fp,
			intensity: intensity, dualStereo: dualStereo, balance: bal, tell: dec.Tell(), rng: dec.Rng()}
	}
	return res
}

// TestRateModeInfo confirms the oracle mode dimensions and the frozen build
// config the whole sweep relies on (OPUS_FAST_INT64, no CUSTOM_MODES, no QEXT).
func TestRateModeInfo(t *testing.T) {
	cfg := GetBuildConfig()
	if !cfg.FixedPoint || !cfg.DisableFloatAPI || !cfg.FastInt64 || cfg.CustomModes || cfg.EnableQEXT {
		t.Fatalf("unexpected oracle build config: %+v", cfg)
	}
	nb, nv, eff := cRateModeInfo()
	if nb != 21 || nv != 11 || eff != 21 {
		t.Fatalf("mode dims = (nbEBands=%d, nbAllocVectors=%d, effEBands=%d), want (21, 11, 21)", nb, nv, eff)
	}
}

// TestRateInitCaps sweeps init_caps over every (LM, C) the decoder uses.
func TestRateInitCaps(t *testing.T) {
	nb, _, _ := cRateModeInfo()
	for LM := 0; LM <= 3; LM++ {
		for C := 1; C <= 2; C++ {
			want := cRateInitCaps(LM, C, nb)
			got := make([]int, nb)
			celt.InitCaps(got, LM, C)
			for i := 0; i < nb; i++ {
				if got[i] != want[i] {
					t.Fatalf("InitCaps LM=%d C=%d cap[%d] = %d, want %d", LM, C, i, got[i], want[i])
				}
			}
		}
	}
}

// TestRateCacheHelpers pins get_pulses/bits2pulses/pulses2bits. bits2pulses
// produces a valid pulse index for each (band, LM, bits), which then feeds
// pulses2bits and get_pulses, so no read ever runs past the cache's K cap.
func TestRateCacheHelpers(t *testing.T) {
	nb, _, _ := cRateModeInfo()
	for i := 0; i <= 44; i++ { // MAX_PSEUDO=40; a few beyond for good measure
		if got, want := celt.GetPulses(i), cGetPulses(i); got != want {
			t.Fatalf("GetPulses(%d) = %d, want %d", i, got, want)
		}
	}
	for LM := 0; LM <= 3; LM++ {
		for band := 0; band < nb; band++ {
			for bits := 0; bits <= 512; bits++ {
				q := celt.Bits2Pulses(band, LM, bits)
				if cq := cBits2Pulses(band, LM, bits); q != cq {
					t.Fatalf("Bits2Pulses(band=%d, LM=%d, bits=%d) = %d, want %d", band, LM, bits, q, cq)
				}
				if got, want := celt.Pulses2Bits(band, LM, q), cPulses2Bits(band, LM, q); got != want {
					t.Fatalf("Pulses2Bits(band=%d, LM=%d, pulses=%d) = %d, want %d", band, LM, q, got, want)
				}
			}
		}
	}
}

// rateTotals is the swept total-bit-budget set: dense in the tiny region (which
// forces skips and the budget-exhausted branches) and sparse up to a large budget
// (full allocation). Values are eighth-bits (BITRES units), as the decoder passes.
func rateTotals(short bool) []int {
	if short {
		return []int{0, 1, 5, 16, 40, 96, 240, 800, 4000, 20000}
	}
	var ts []int
	for t := 0; t <= 320; t++ { // dense tiny: every skip/exhaustion corner
		ts = append(ts, t)
	}
	for t := 352; t <= 4096; t += 32 { // mid budgets
		ts = append(ts, t)
	}
	for t := 4608; t <= 65536; t += 512 { // large budgets up to full allocation
		ts = append(ts, t)
	}
	return ts
}

// makeOffsets builds a dynalloc-boost pattern (eighth-bits), clamped below cap so
// it stays in the realistic decoder domain. scenario 0 is all-zero.
func makeOffsets(scenario, nbEBands, start, end int, caps []int) []int {
	off := make([]int, nbEBands)
	set := func(band, v int) {
		if band >= start && band < end && caps[band] > 1 {
			if v > caps[band]-1 {
				v = caps[band] - 1
			}
			off[band] = v
		}
	}
	switch scenario {
	case 1:
		set(8, 64)
	case 2:
		set(3, 48)
		set(11, 96)
		set(17, 32)
	}
	return off
}

// coderScenario is an (encode-side) skip-heuristic setting. The decode side reads
// whatever the encode side wrote, so these only steer which bands the encoder
// chooses to skip; the decode differential validates the full skip path either way.
type coderScenario struct{ prev, sbw int }

// TestRateComputeAllocationSweep is the exhaustive differential sweep. See the
// file header for the four-way comparison performed per case.
func TestRateComputeAllocationSweep(t *testing.T) {
	nb, _, eff := cRateModeInfo()
	start, end := 0, eff // decoder uses start=0, end=effEBands=21

	short := testing.Short()
	totals := rateTotals(short)
	trims := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	offScenarios := []int{0, 1, 2}
	coders := []coderScenario{{0, end - 1}, {0, 8}, {18, end - 1}}
	if short {
		trims = []int{0, 5, 10}
		offScenarios = []int{0, 2}
		coders = []coderScenario{{0, end - 1}}
	}

	// Coverage witnesses proving the sweep is not vacuous: it must actually
	// exercise skips (codedBands < end) and non-trivial pulse allocations.
	var cases int
	minCoded, maxCoded := end, start
	sawSkip := false
	sawPulses := false

	goEncBuf := make([]byte, rateAllocBufBytes)
	cEncBuf := make([]byte, rateAllocBufBytes)

	for LM := 0; LM <= 3; LM++ {
		for C := 1; C <= 2; C++ {
			caps := cRateInitCaps(LM, C, nb)
			// Feed both sides the exact same caps; assert Go InitCaps agrees.
			goCaps := make([]int, nb)
			celt.InitCaps(goCaps, LM, C)
			for i := 0; i < nb; i++ {
				if goCaps[i] != caps[i] {
					t.Fatalf("InitCaps mismatch LM=%d C=%d cap[%d]: go=%d c=%d", LM, C, i, goCaps[i], caps[i])
				}
			}

			var intensitySet, dualSet []int
			if C == 2 {
				intensitySet = []int{start, (start + end) / 2, end}
				dualSet = []int{0, 1}
			} else {
				intensitySet = []int{0}
				dualSet = []int{0}
			}

			for _, offSc := range offScenarios {
				offsets := makeOffsets(offSc, nb, start, end, caps)
				for _, cd := range coders {
					for _, intIn := range intensitySet {
						for _, dualIn := range dualSet {
							for _, trim := range trims {
								for _, total := range totals {
									cases++

									// 1. C-encode -> ground-truth bytes + outputs.
									cEnc := cRateAlloc(start, end, offsets, caps, trim, intIn, dualIn,
										total, C, LM, 1, cd.prev, cd.sbw, cEncBuf)
									// 2. Go-encode -> must be byte-identical + same outputs.
									goEnc := goRateAlloc(start, end, offsets, caps, trim, intIn, dualIn,
										total, C, LM, 1, cd.prev, cd.sbw, goEncBuf)
									if !bytes.Equal(goEncBuf, cEncBuf) {
										t.Fatalf("encode bytes differ LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d\n go=%x\n  c=%x",
											LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total,
											trimBytes(goEncBuf), trimBytes(cEncBuf))
									}
									if d := diffAlloc(goEnc, cEnc); d != "" {
										t.Fatalf("go-encode vs c-encode: %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}
									if d := diffCoder(goEnc, cEnc); d != "" {
										t.Fatalf("go-encode vs c-encode coder: %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}

									// 3. C-decode over the ground-truth bytes -> must reproduce encode.
									cDec := cRateAlloc(start, end, offsets, caps, trim, intIn, dualIn,
										total, C, LM, 0, cd.prev, cd.sbw, cEncBuf)
									if d := diffAlloc(cDec, cEnc); d != "" {
										t.Fatalf("c-decode vs c-encode (round-trip): %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}
									if d := diffCoder(cDec, cEnc); d != "" {
										t.Fatalf("c-decode vs c-encode coder (round-trip): %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}

									// 4. Go-decode over the ground-truth bytes -> the primary check.
									goDec := goRateAlloc(start, end, offsets, caps, trim, intIn, dualIn,
										total, C, LM, 0, cd.prev, cd.sbw, cEncBuf)
									if d := diffAlloc(goDec, cDec); d != "" {
										t.Fatalf("go-decode vs c-decode: %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}
									if d := diffCoder(goDec, cDec); d != "" {
										t.Fatalf("go-decode vs c-decode coder: %s [LM=%d C=%d off=%d prev=%d sbw=%d int=%d dual=%d trim=%d total=%d]",
											d, LM, C, offSc, cd.prev, cd.sbw, intIn, dualIn, trim, total)
									}

									// Coverage witnesses.
									if goDec.codedBands < minCoded {
										minCoded = goDec.codedBands
									}
									if goDec.codedBands > maxCoded {
										maxCoded = goDec.codedBands
									}
									if goDec.codedBands < end {
										sawSkip = true
									}
									for _, p := range goDec.pulses {
										if p > 0 {
											sawPulses = true
											break
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	if !sawSkip {
		t.Errorf("sweep never exercised a band skip (codedBands<%d); coverage is suspect", end)
	}
	if !sawPulses {
		t.Errorf("sweep never allocated any PVQ pulses; coverage is suspect")
	}
	t.Logf("swept %d cases; codedBands in [%d,%d]; totals=%d trims=%d offScenarios=%d coders=%d",
		cases, minCoded, maxCoded, len(totals), len(trims), len(offScenarios), len(coders))
}

// trimBytes returns b up to its last non-zero byte, for compact failure output.
func trimBytes(b []byte) []byte {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return b[:n]
}

// TestRateComputeAllocationMutation is the negative control: it proves the sweep
// comparators are not vacuous. A representative case that passes is taken, then a
// single balance-carry perturbation (and, separately, one pulse-count perturbation)
// is injected into a copy of the Go result and diffAlloc MUST flag it. If diffAlloc
// stayed silent, every bit-exact assertion in the sweep above would be worthless.
func TestRateComputeAllocationMutation(t *testing.T) {
	nb, _, eff := cRateModeInfo()
	start, end := 0, eff
	const (
		C, LM     = 2, 3
		trim      = 5
		intIn     = 21
		dualIn    = 0
		total     = 900
		prev, sbw = 0, 20
	)
	caps := cRateInitCaps(LM, C, nb)
	offsets := makeOffsets(2, nb, start, end, caps)
	cEncBuf := make([]byte, rateAllocBufBytes)

	cEnc := cRateAlloc(start, end, offsets, caps, trim, intIn, dualIn, total, C, LM, 1, prev, sbw, cEncBuf)
	goDec := goRateAlloc(start, end, offsets, caps, trim, intIn, dualIn, total, C, LM, 0, prev, sbw, cEncBuf)

	// Sanity: the baseline actually matches, so the mutation below is meaningful.
	if d := diffAlloc(goDec, cEnc); d != "" {
		t.Fatalf("baseline case unexpectedly differs (%s); mutation control needs a matching baseline", d)
	}
	if !anyPulses(goDec.pulses) {
		t.Fatalf("baseline case allocated no pulses; pick a case that does so the pulse mutation bites")
	}

	// Perturb the balance carry by one eighth-bit.
	balMut := cloneResult(goDec)
	balMut.balance++
	if diffAlloc(balMut, cEnc) == "" {
		t.Fatalf("balance perturbation was NOT detected: the sweep's balance comparison is vacuous")
	}

	// Perturb one pulse count.
	pulseMut := cloneResult(goDec)
	for i := range pulseMut.pulses {
		if pulseMut.pulses[i] > 0 {
			pulseMut.pulses[i]--
			break
		}
	}
	if diffAlloc(pulseMut, cEnc) == "" {
		t.Fatalf("pulse perturbation was NOT detected: the sweep's pulses comparison is vacuous")
	}
}

func anyPulses(p []int) bool {
	for _, v := range p {
		if v > 0 {
			return true
		}
	}
	return false
}

// cloneResult deep-copies the slice fields so a mutation does not alias the input.
func cloneResult(r rateAllocResult) rateAllocResult {
	c := r
	c.pulses = append([]int(nil), r.pulses...)
	c.ebits = append([]int(nil), r.ebits...)
	c.finePriority = append([]int(nil), r.finePriority...)
	return c
}
