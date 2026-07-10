//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This is the Checkpoint 6 ENCODE-direction differential harness for the CELT bit
// allocation (internal/celt rate.go: ComputeAllocation / interpBits2pulses on the
// encode=1 path). rate.go is already ported both directions (a named Layer C
// verbatim zone); this file VERIFIES the encode branches (the ec_enc writes at
// rate.go:213/233 skip flags, :262 intensity, :275 dual-stereo) are bit-exact
// against the pinned libopus clt_compute_allocation driven with encode=1.
//
// clt_compute_allocation interleaves the range coder while it computes the budget,
// so each case is driven three ways over the SAME buffer:
//   1. C-encode (encode=1)  -> ground-truth bytes + outputs + coder end-state.
//   2. Go-encode (encode=1) -> asserted byte-identical to (1), same outputs and
//      same ec_tell/rng end-state.
//   3. Go-decode (encode=0) over (1)'s bytes -> asserted equal to (1). This
//      round-trip proves the emitted skip/intensity/dual flags are real and
//      decodable (a vacuous encoder that wrote nothing could not reconstruct a
//      non-trivial codedBands<end from the bytes).
// Every output (pulses[], ebits/fine_quant[], fine_priority[], codedBands,
// balance, resolved intensity/dual_stereo) and the coder end-state (ec_tell + rng)
// must be bit-identical.
//
// The decode-direction and the four-way shared-buffer sweep live in rate_test.go;
// this harness is encode-focused and additionally sweeps coding windows the
// decoder sweep never visits (hybrid CELT start=17, narrowband/wideband/
// superwideband ends), because rate.c's thresh/cap/balance-carry and skip corners
// are unenumerable by review (docs/hard-parts.md section 3).

// diffRateEnc reports the first differing allocation output, or "" if a==b. Coder
// end-state (tell, rng) is compared separately by diffRateEncCoder.
func diffRateEnc(a, b rateEncResult) string {
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

func diffRateEncCoder(a, b rateEncResult) string {
	if a.tell != b.tell {
		return fmt.Sprintf("tell %d != %d", a.tell, b.tell)
	}
	if a.rng != b.rng {
		return fmt.Sprintf("rng %d != %d", a.rng, b.rng)
	}
	return ""
}

// goRateEncAlloc drives the pure-Go celt.ComputeAllocation with encode=1 once over
// buf, emitting the flags (finalized in place via EncDone). tell/rng are captured
// BEFORE EncDone, matching the C shim.
func goRateEncAlloc(start, end int, offsets, caps []int, allocTrim, intensityIn, dualStereoIn,
	total, C, LM, prev, signalBandwidth int, buf []byte) rateEncResult {
	nbEBands := len(offsets)
	pulses := make([]int, nbEBands)
	ebits := make([]int, nbEBands)
	fp := make([]int, nbEBands)
	intensity := intensityIn
	dualStereo := dualStereoIn
	var enc rangecoding.Encoder
	enc.Init(buf)
	cb, bal := celt.ComputeAllocation(start, end, offsets, caps, allocTrim,
		&intensity, &dualStereo, total, pulses, ebits, fp, C, LM,
		&enc, nil, 1, prev, signalBandwidth)
	res := rateEncResult{codedBands: cb, pulses: pulses, ebits: ebits, finePriority: fp,
		intensity: intensity, dualStereo: dualStereo, balance: bal, tell: enc.Tell(), rng: enc.Rng()}
	enc.EncDone()
	return res
}

// goRateEncDecode drives the pure-Go celt.ComputeAllocation with encode=0 once
// over buf (the C-emitted, finalized bytes), reading the flags back. tell/rng are
// captured after the read, matching the encode end-state.
func goRateEncDecode(start, end int, offsets, caps []int, allocTrim, intensityIn, dualStereoIn,
	total, C, LM, prev, signalBandwidth int, buf []byte) rateEncResult {
	nbEBands := len(offsets)
	pulses := make([]int, nbEBands)
	ebits := make([]int, nbEBands)
	fp := make([]int, nbEBands)
	intensity := intensityIn
	dualStereo := dualStereoIn
	var dec rangecoding.Decoder
	dec.Init(buf)
	cb, bal := celt.ComputeAllocation(start, end, offsets, caps, allocTrim,
		&intensity, &dualStereo, total, pulses, ebits, fp, C, LM,
		nil, &dec, 0, prev, signalBandwidth)
	return rateEncResult{codedBands: cb, pulses: pulses, ebits: ebits, finePriority: fp,
		intensity: intensity, dualStereo: dualStereo, balance: bal, tell: dec.Tell(), rng: dec.Rng()}
}

// coderScenarioEnc is an encode-side skip-heuristic setting: prev is the previous
// frame's coded-band count (drives the depth_threshold hysteresis) and sbw is the
// signalBandwidth guard. Both only steer which bands the encoder chooses to skip;
// the byte-exact + round-trip checks validate whatever it chose.
type coderScenarioEnc struct{ prev, sbw int }

// rateEncCase bundles the parameters of one allocation call.
type rateEncCase struct {
	start, end                 int
	offsets, caps              []int
	trim, intIn, dualIn, total int
	C, LM, prev, sbw           int
}

func (c rateEncCase) label() string {
	return fmt.Sprintf("start=%d end=%d C=%d LM=%d trim=%d int=%d dual=%d total=%d prev=%d sbw=%d",
		c.start, c.end, c.C, c.LM, c.trim, c.intIn, c.dualIn, c.total, c.prev, c.sbw)
}

// rateEncCoverage records witnesses that prove a sweep is not vacuous.
type rateEncCoverage struct {
	cases              int
	minCoded, maxCoded int
	sawSkip, sawNoSkip bool
	sawPulses          bool
	sawDualCoded       bool
	sawIntCoded        bool
}

// runRateEncCase runs the three-way check for one case and updates coverage.
func runRateEncCase(t *testing.T, cov *rateEncCoverage, c rateEncCase, goBuf, cBuf []byte) {
	t.Helper()
	cov.cases++

	// 1. C-encode -> ground-truth bytes + outputs + coder end-state.
	cEnc := cRateEncAlloc(c.start, c.end, c.offsets, c.caps, c.trim, c.intIn, c.dualIn,
		c.total, c.C, c.LM, c.prev, c.sbw, cBuf)
	// 2. Go-encode -> must be byte-identical + same outputs + same end-state.
	goEnc := goRateEncAlloc(c.start, c.end, c.offsets, c.caps, c.trim, c.intIn, c.dualIn,
		c.total, c.C, c.LM, c.prev, c.sbw, goBuf)
	if !bytes.Equal(goBuf, cBuf) {
		t.Fatalf("encode bytes differ [%s]\n go=%x\n  c=%x", c.label(),
			trimBytesEnc(goBuf), trimBytesEnc(cBuf))
	}
	if d := diffRateEnc(goEnc, cEnc); d != "" {
		t.Fatalf("go-encode vs c-encode: %s [%s]", d, c.label())
	}
	if d := diffRateEncCoder(goEnc, cEnc); d != "" {
		t.Fatalf("go-encode vs c-encode coder: %s [%s]", d, c.label())
	}

	// 3. Go-decode over the C-emitted bytes -> must reproduce the encode exactly.
	goDec := goRateEncDecode(c.start, c.end, c.offsets, c.caps, c.trim, c.intIn, c.dualIn,
		c.total, c.C, c.LM, c.prev, c.sbw, cBuf)
	if d := diffRateEnc(goDec, cEnc); d != "" {
		t.Fatalf("go-decode round-trip vs c-encode: %s [%s]", d, c.label())
	}
	if d := diffRateEncCoder(goDec, cEnc); d != "" {
		t.Fatalf("go-decode round-trip vs c-encode coder: %s [%s]", d, c.label())
	}

	// Coverage witnesses.
	cb := cEnc.codedBands
	if cb < cov.minCoded {
		cov.minCoded = cb
	}
	if cb > cov.maxCoded {
		cov.maxCoded = cb
	}
	if cb < c.end {
		cov.sawSkip = true
	} else {
		cov.sawNoSkip = true
	}
	if anyPulsesEnc(cEnc.pulses) {
		cov.sawPulses = true
	}
	if c.C == 2 && cEnc.dualStereo == 1 {
		cov.sawDualCoded = true
	}
	if c.C == 2 && cEnc.intensity > c.start {
		cov.sawIntCoded = true
	}
}

// rateEncTotalsDense is the primary swept budget: dense in the tiny region (which
// forces skips and the budget-exhausted branches) and coarser up to a full
// allocation. Values are eighth-bits (BITRES units), as the codec passes.
func rateEncTotalsDense(short bool) []int {
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
	for t := 4608; t <= 65536; t += 1024 { // large budgets up to full allocation
		ts = append(ts, t)
	}
	return ts
}

// rateEncTotalsCoarse is a representative budget set for the band-window sweep.
func rateEncTotalsCoarse(short bool) []int {
	if short {
		return []int{0, 16, 96, 800, 20000}
	}
	return []int{0, 1, 8, 24, 48, 96, 160, 240, 384, 640, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
}

// makeRateEncOffsets builds a dynalloc-boost pattern (eighth-bits) inside the
// [start,end) window, clamped below cap. A nonzero offset moves skip_start (the
// never-skip boundary) up to that band, exercising the "never skip a boosted band"
// path. scenario 0 is all-zero.
func makeRateEncOffsets(scenario, nbEBands, start, end int, caps []int) []int {
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
		set((start+end)/2, 64)
	case 2:
		set(start+(end-start)/4, 48)
		set((start+end)/2, 96)
		set(end-2, 32)
	}
	return off
}

// rateEncIntensitySet is the swept intensity-input set (stereo only).
func rateEncIntensitySet(C, start, end int) []int {
	if C == 2 {
		return []int{start, (start + end) / 2, end}
	}
	return []int{0}
}

// rateEncDualSet is the swept dual-stereo-input set (stereo only).
func rateEncDualSet(C int) []int {
	if C == 2 {
		return []int{0, 1}
	}
	return []int{0}
}

// TestRateEncBuildAndCaps confirms the frozen oracle build config and that the
// pure-Go InitCaps matches the C init_caps for every (LM, C) this harness feeds.
func TestRateEncBuildAndCaps(t *testing.T) {
	cfg := GetBuildConfig()
	if !cfg.FixedPoint || !cfg.DisableFloatAPI || !cfg.FastInt64 || cfg.CustomModes || cfg.EnableQEXT {
		t.Fatalf("unexpected oracle build config: %+v", cfg)
	}
	nb, nv, eff := cRateEncModeInfo()
	if nb != 21 || nv != 11 || eff != 21 {
		t.Fatalf("mode dims = (nbEBands=%d, nbAllocVectors=%d, effEBands=%d), want (21, 11, 21)", nb, nv, eff)
	}
	for LM := 0; LM <= 3; LM++ {
		for C := 1; C <= 2; C++ {
			want := cRateEncInitCaps(LM, C, nb)
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

// TestRateEncAllocationSweep is the exhaustive encode-direction sweep over the
// full band window (start=0, end=effEBands), the codec's real CELT window and the
// deepest skip loop. See the file header for the three-way per-case comparison.
func TestRateEncAllocationSweep(t *testing.T) {
	nb, _, eff := cRateEncModeInfo()
	start, end := 0, eff

	short := testing.Short()
	totals := rateEncTotalsDense(short)
	trims := []int{0, 2, 5, 8, 10}
	offScenarios := []int{0, 1, 2}
	coders := []coderScenarioEnc{{start, end - 1}, {start, 8}, {18, end - 1}}
	if short {
		trims = []int{0, 5, 10}
		offScenarios = []int{0, 2}
		coders = []coderScenarioEnc{{start, end - 1}}
	}

	cov := rateEncCoverage{minCoded: end, maxCoded: start}
	goBuf := make([]byte, rateEncBufBytes)
	cBuf := make([]byte, rateEncBufBytes)

	for LM := 0; LM <= 3; LM++ {
		for C := 1; C <= 2; C++ {
			caps := cRateEncInitCaps(LM, C, nb)
			goCaps := make([]int, nb)
			celt.InitCaps(goCaps, LM, C)
			for i := 0; i < nb; i++ {
				if goCaps[i] != caps[i] {
					t.Fatalf("InitCaps mismatch LM=%d C=%d cap[%d]: go=%d c=%d", LM, C, i, goCaps[i], caps[i])
				}
			}

			intensitySet := rateEncIntensitySet(C, start, end)
			dualSet := rateEncDualSet(C)

			for _, offSc := range offScenarios {
				offsets := makeRateEncOffsets(offSc, nb, start, end, caps)
				for _, cd := range coders {
					for _, intIn := range intensitySet {
						for _, dualIn := range dualSet {
							for _, trim := range trims {
								for _, total := range totals {
									runRateEncCase(t, &cov, rateEncCase{
										start: start, end: end, offsets: offsets, caps: caps,
										trim: trim, intIn: intIn, dualIn: dualIn, total: total,
										C: C, LM: LM, prev: cd.prev, sbw: cd.sbw,
									}, goBuf, cBuf)
								}
							}
						}
					}
				}
			}
		}
	}

	if !cov.sawSkip {
		t.Errorf("sweep never exercised a band skip (codedBands<%d); coverage is suspect", end)
	}
	if !cov.sawNoSkip {
		t.Errorf("sweep never reached codedBands==end (skip=0 flags coded to exhaustion); coverage is suspect")
	}
	if !cov.sawPulses {
		t.Errorf("sweep never allocated any PVQ pulses; coverage is suspect")
	}
	if !cov.sawDualCoded {
		t.Errorf("sweep never coded a dual_stereo=1 flag; the dual-stereo encode branch is unexercised")
	}
	if !cov.sawIntCoded {
		t.Errorf("sweep never coded a non-trivial intensity (>start); the intensity encode branch is unexercised")
	}
	t.Logf("primary sweep: %d cases; codedBands in [%d,%d]; totals=%d trims=%d off=%d coders=%d",
		cov.cases, cov.minCoded, cov.maxCoded, len(totals), len(trims), len(offScenarios), len(coders))
}

// TestRateEncBandWindows sweeps the alternate coding windows the primary sweep
// never visits: narrowband/wideband/superwideband ends (start=0) and the hybrid
// CELT start (start=17). Same three-way per-case comparison.
func TestRateEncBandWindows(t *testing.T) {
	nb, _, eff := cRateEncModeInfo()
	type window struct{ start, end int }
	windows := []window{{0, 13}, {0, 17}, {0, 19}, {17, eff}}

	short := testing.Short()
	totals := rateEncTotalsCoarse(short)
	trims := []int{0, 5, 10}
	offScenarios := []int{0, 2}
	if short {
		trims = []int{0, 10}
		offScenarios = []int{0}
	}

	cov := rateEncCoverage{minCoded: eff, maxCoded: 0}
	goBuf := make([]byte, rateEncBufBytes)
	cBuf := make([]byte, rateEncBufBytes)

	for _, w := range windows {
		start, end := w.start, w.end
		coders := []coderScenarioEnc{{start, end - 1}, {(start + end) / 2, end - 1}}
		for LM := 0; LM <= 3; LM++ {
			for C := 1; C <= 2; C++ {
				caps := cRateEncInitCaps(LM, C, nb)
				intensitySet := rateEncIntensitySet(C, start, end)
				dualSet := rateEncDualSet(C)
				for _, offSc := range offScenarios {
					offsets := makeRateEncOffsets(offSc, nb, start, end, caps)
					for _, cd := range coders {
						for _, intIn := range intensitySet {
							for _, dualIn := range dualSet {
								for _, trim := range trims {
									for _, total := range totals {
										runRateEncCase(t, &cov, rateEncCase{
											start: start, end: end, offsets: offsets, caps: caps,
											trim: trim, intIn: intIn, dualIn: dualIn, total: total,
											C: C, LM: LM, prev: cd.prev, sbw: cd.sbw,
										}, goBuf, cBuf)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	if !cov.sawSkip {
		t.Errorf("window sweep never exercised a band skip; coverage is suspect")
	}
	if !cov.sawPulses {
		t.Errorf("window sweep never allocated any PVQ pulses; coverage is suspect")
	}
	t.Logf("window sweep: %d cases; codedBands in [%d,%d]; windows=%d totals=%d",
		cov.cases, cov.minCoded, cov.maxCoded, len(windows), len(totals))
}

// TestRateEncMutationControl is the negative control: it proves the encode
// harness's comparators are not vacuous. A representative matching case is taken,
// then each comparison dimension is perturbed in isolation and MUST be flagged. If
// any perturbation stayed silent, the corresponding bit-exact assertion in the
// sweeps above would be worthless.
func TestRateEncMutationControl(t *testing.T) {
	nb, _, eff := cRateEncModeInfo()
	start, end := 0, eff
	const (
		C, LM     = 2, 3
		trim      = 5
		intIn     = 21
		dualIn    = 0
		total     = 900
		prev, sbw = 0, 20
	)
	caps := cRateEncInitCaps(LM, C, nb)
	offsets := makeRateEncOffsets(2, nb, start, end, caps)
	goBuf := make([]byte, rateEncBufBytes)
	cBuf := make([]byte, rateEncBufBytes)

	cEnc := cRateEncAlloc(start, end, offsets, caps, trim, intIn, dualIn, total, C, LM, prev, sbw, cBuf)
	goEnc := goRateEncAlloc(start, end, offsets, caps, trim, intIn, dualIn, total, C, LM, prev, sbw, goBuf)

	// Baseline must match, so the perturbations below are meaningful.
	if d := diffRateEnc(goEnc, cEnc); d != "" {
		t.Fatalf("baseline case unexpectedly differs (%s); mutation control needs a matching baseline", d)
	}
	if !bytes.Equal(goBuf, cBuf) {
		t.Fatalf("baseline bytes differ; mutation control needs a matching baseline")
	}
	if !anyPulsesEnc(goEnc.pulses) {
		t.Fatalf("baseline case allocated no pulses; pick a case that does so the pulse mutation bites")
	}

	// Balance carry perturbation.
	balMut := cloneRateEncResult(goEnc)
	balMut.balance++
	if diffRateEnc(balMut, cEnc) == "" {
		t.Fatalf("balance perturbation was NOT detected: the balance comparison is vacuous")
	}

	// Pulse count perturbation.
	pulseMut := cloneRateEncResult(goEnc)
	for i := range pulseMut.pulses {
		if pulseMut.pulses[i] > 0 {
			pulseMut.pulses[i]--
			break
		}
	}
	if diffRateEnc(pulseMut, cEnc) == "" {
		t.Fatalf("pulse perturbation was NOT detected: the pulses comparison is vacuous")
	}

	// Coder end-state perturbations.
	tellMut := cloneRateEncResult(goEnc)
	tellMut.tell++
	if diffRateEncCoder(tellMut, cEnc) == "" {
		t.Fatalf("tell perturbation was NOT detected: the coder comparison is vacuous")
	}
	rngMut := cloneRateEncResult(goEnc)
	rngMut.rng ^= 1
	if diffRateEncCoder(rngMut, cEnc) == "" {
		t.Fatalf("rng perturbation was NOT detected: the coder comparison is vacuous")
	}

	// A single flipped emitted byte must break the byte comparison.
	mutBuf := append([]byte(nil), goBuf...)
	mutBuf[0] ^= 0xFF
	if bytes.Equal(mutBuf, cBuf) {
		t.Fatalf("byte perturbation was NOT detected: the byte comparison is vacuous")
	}
}

// trimBytesEnc returns b up to its last non-zero byte, for compact failure output.
func trimBytesEnc(b []byte) []byte {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return b[:n]
}

func anyPulsesEnc(p []int) bool {
	for _, v := range p {
		if v > 0 {
			return true
		}
	}
	return false
}

// cloneRateEncResult deep-copies the slice fields so a mutation does not alias.
func cloneRateEncResult(r rateEncResult) rateEncResult {
	c := r
	c.pulses = append([]int(nil), r.pulses...)
	c.ebits = append([]int(nil), r.ebits...)
	c.finePriority = append([]int(nil), r.finePriority...)
	return c
}
