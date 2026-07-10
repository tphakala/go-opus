// Command gentables converts the libopus CELT static mode data
// (static_modes_fixed.h + the eband5ms/band_allocation tables in modes.c) into
// checked-in Go data: internal/celt/static_modes_gen.go, holding the single
// mode48000_960 needed for 48 kHz fullband. See docs/hard-parts.md section 6:
// the data is fully precomputed, so this is pure transcription, no mode-building
// math (compute_pulse_cache runs only under CUSTOM_MODES, which is off).
//
// Method (robust, avoids fragile header text parsing): compile and run a tiny C
// program (cdump/dump_modes.c) that #includes the pinned libopus sources with
// -DFIXED_POINT -DDISABLE_FLOAT_API (non-CUSTOM_MODES, non-QEXT), which prints
// every array and scalar as a flat token stream. The generator parses that,
// validates each length against a fixed schema (so an upstream reshape fails
// loudly), and emits gofmt'd Go. The QEXT tables (qext_*50) are skipped.
//
// Regenerate with `go generate ./internal/celt/...`. Re-running is idempotent:
// the output is byte-identical given the same submodule.
package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"go/format"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// upstreamTag records the libopus release the data is transcribed from. It is
// written into the generated file's header for provenance.
const upstreamTag = "v1.6.1"

//go:embed cdump/dump_modes.c
var dumpModesC []byte

func main() {
	log.SetFlags(0)
	log.SetPrefix("gentables: ")

	repoRoot := repoRootFromCaller()
	defaultLibopus := filepath.Join(repoRoot, "internal", "reftest", "libopus")
	defaultOut := filepath.Join(repoRoot, "internal", "celt", "static_modes_gen.go")

	libopus := flag.String("libopus", defaultLibopus, "path to the pinned libopus source tree")
	out := flag.String("out", "", "output Go file (default internal/celt/static_modes_gen.go)")
	cc := flag.String("cc", ccDefault(), "C compiler to build the dumper")
	check := flag.Bool("check", false, "verify the checked-in file matches a fresh C dump; do not write (exit 1 on drift)")
	flag.Parse()

	outPath := *out
	switch {
	case outPath == "":
		outPath = defaultOut
	case !filepath.IsAbs(outPath):
		wd, err := os.Getwd()
		if err != nil {
			log.Fatalf("getwd: %v", err)
		}
		outPath = filepath.Join(wd, outPath)
	}

	dump, err := runDumper(*cc, *libopus)
	if err != nil {
		log.Fatalf("run C dumper: %v", err)
	}

	tables, err := parseDump(dump)
	if err != nil {
		log.Fatalf("parse dump: %v", err)
	}
	if err := validate(tables); err != nil {
		log.Fatalf("validate dump: %v", err)
	}

	src, err := emitGo(tables)
	if err != nil {
		log.Fatalf("emit Go: %v", err)
	}

	// -check diffs the freshly dumped data against the checked-in file and fails
	// on any mismatch, instead of writing. This is the CI gate that a submodule
	// bump or a manual edit did not silently desync the generated tables.
	if *check {
		existing, err := os.ReadFile(outPath)
		if err != nil {
			log.Fatalf("read %s for -check: %v", outPath, err)
		}
		if !bytes.Equal(existing, src) {
			log.Fatalf("%s is out of date: re-run `go generate ./internal/celt/...`", outPath)
		}
		log.Printf("%s is up to date", outPath)
		return
	}

	if err := os.WriteFile(outPath, src, 0o644); err != nil {
		log.Fatalf("write %s: %v", outPath, err)
	}
	log.Printf("wrote %s (%d bytes)", outPath, len(src))
}

// repoRootFromCaller derives the module root from this source file's location
// (<root>/cmd/gentables/main.go), so path defaults work regardless of cwd.
func repoRootFromCaller() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatal("cannot determine caller path")
	}
	genDir := filepath.Dir(thisFile)          // <root>/cmd/gentables
	return filepath.Dir(filepath.Dir(genDir)) // <root>
}

func ccDefault() string {
	if v := os.Getenv("CC"); v != "" {
		return v
	}
	return "cc"
}

// runDumper writes the embedded dumper to a temp dir, compiles it against the
// libopus tree with the frozen oracle flags, runs it, and returns its stdout.
func runDumper(cc, libopus string) (string, error) {
	if _, err := os.Stat(filepath.Join(libopus, "celt", "static_modes_fixed.h")); err != nil {
		return "", fmt.Errorf("libopus not found at %s: %w (init the submodule)", libopus, err)
	}

	tmp, err := os.MkdirTemp("", "gentables")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	srcC := filepath.Join(tmp, "dump_modes.c")
	if err := os.WriteFile(srcC, dumpModesC, 0o644); err != nil {
		return "", err
	}
	bin := filepath.Join(tmp, "dump_modes")

	// Mirror internal/reftest/oracle/oracle_cgo.go CFLAGS exactly so the dumper
	// sees the same frozen build as the differential oracle.
	args := []string{
		"-O2", "-DFIXED_POINT", "-DDISABLE_FLOAT_API", "-DOPUS_BUILD",
		"-DHAVE_STDINT_H", "-DVAR_ARRAYS",
		"-I" + libopus,
		"-I" + filepath.Join(libopus, "include"),
		"-I" + filepath.Join(libopus, "celt"),
		"-I" + filepath.Join(libopus, "silk"),
		"-I" + filepath.Join(libopus, "silk", "fixed"),
		"-I" + filepath.Join(libopus, "src"),
		srcC, "-o", bin, "-lm",
	}
	compile := exec.Command(cc, args...)
	if outb, err := compile.CombinedOutput(); err != nil {
		return "", fmt.Errorf("compile: %w\n%s", err, outb)
	}

	run := exec.Command(bin)
	outb, err := run.Output()
	if err != nil {
		return "", fmt.Errorf("execute dumper: %w", err)
	}
	return string(outb), nil
}

// parseDump turns the "KEY COUNT v0 v1 ..." token stream into named int slices.
func parseDump(dump string) (map[string][]int, error) {
	tables := make(map[string][]int)
	lineNo := 0
	for line := range strings.Lines(strings.TrimSpace(dump)) {
		lineNo++
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: too few fields: %q", lineNo, line)
		}
		key := fields[0]
		count, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: bad count %q: %w", lineNo+1, fields[1], err)
		}
		vals := fields[2:]
		if len(vals) != count {
			return nil, fmt.Errorf("key %s: declared %d values, got %d", key, count, len(vals))
		}
		nums := make([]int, count)
		for i, s := range vals {
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("key %s index %d: bad int %q: %w", key, i, s, err)
			}
			nums[i] = n
		}
		tables[key] = nums
	}
	return tables, nil
}

// schema is the expected length of every key the dumper emits. validate checks
// each against it so a submodule reshape (new tag) fails at generation time
// instead of silently producing wrong data.
var schema = map[string]int{
	"window120":           120,
	"logN400":             21,
	"cacheIndex50":        105,
	"cacheBits50":         392,
	"cacheCaps50":         168,
	"eband5ms":            22,  // nbEBands + 1
	"bandAllocation":      231, // nbAllocVectors * nbEBands = 11 * 21
	"mdctTwiddles960":     1800,
	"fftBitrev480":        480,
	"fftBitrev240":        240,
	"fftBitrev120":        120,
	"fftBitrev60":         60,
	"fftTwiddles48000960": 960, // 480 complex pairs, flattened
	"fftState0":           20,  // nfft, scale, scaleShift, shift + 16 factors
	"fftState1":           20,
	"fftState2":           20,
	"fftState3":           20,
	"preemph":             4,
	"modeScalars":         11,
}

func validate(tables map[string][]int) error {
	for key, want := range schema {
		got, ok := tables[key]
		if !ok {
			return fmt.Errorf("missing key %s", key)
		}
		if len(got) != want {
			return fmt.Errorf("key %s: want %d values, got %d", key, want, len(got))
		}
	}
	for key := range tables {
		if _, ok := schema[key]; !ok {
			return fmt.Errorf("unexpected key %s in dump", key)
		}
	}
	return nil
}

// emitGo assembles and gofmt's the generated Go source from the parsed tables.
func emitGo(t map[string][]int) ([]byte, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "// Code generated by cmd/gentables; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Source: libopus celt/static_modes_fixed.h + celt/modes.c (CELT\n")
	fmt.Fprintf(&b, "// mode48000_960), upstream tag %s. Method: compile and run\n", upstreamTag)
	fmt.Fprintf(&b, "// cmd/gentables/cdump/dump_modes.c under -DFIXED_POINT -DDISABLE_FLOAT_API\n")
	fmt.Fprintf(&b, "// (non-CUSTOM_MODES, non-QEXT) and transcribe its dump. The QEXT tables are\n")
	fmt.Fprintf(&b, "// intentionally omitted. Regenerate with `go generate ./internal/celt/...`.\n\n")
	fmt.Fprintf(&b, "package celt\n\n")

	// Leaf data tables.
	emitInt16(&b, "window120", "window120 is the celt_coef (int16) MDCT/overlap window (window[overlap]).", t["window120"], 12)
	emitInt16(&b, "logN400", "logN400 holds the per-band log-N allocation trim (logN[nbEBands]).", t["logN400"], 21)
	emitInt16(&b, "eband5ms", "eband5ms holds the eBand boundaries in 5 ms units (nbEBands+1 entries).", t["eband5ms"], 22)
	emitBytes(&b, "bandAllocation", "bandAllocation is the static bit-allocation matrix band_allocation:\n// nbAllocVectors x nbEBands = 11 x 21 bytes, 1/32 bit/sample units.", t["bandAllocation"], 21)
	emitInt16(&b, "cacheIndex50", "cacheIndex50 indexes cacheBits50 per (LM, band); PVQ pulse cache.", t["cacheIndex50"], 21)
	emitBytes(&b, "cacheBits50", "cacheBits50 is the prebuilt PVQ pulse-bit cache consumed by rate.c.", t["cacheBits50"], 20)
	emitBytes(&b, "cacheCaps50", "cacheCaps50 holds the per-band bit caps (cache.caps).", t["cacheCaps50"], 21)
	emitInt16(&b, "mdctTwiddles960", "mdctTwiddles960 is the celt_coef (int16) MDCT trig table (mdct.trig).", t["mdctTwiddles960"], 12)
	emitInt16(&b, "fftBitrev480", "fftBitrev480 is the bit-reversal table for the 480-point FFT stage.", t["fftBitrev480"], 20)
	emitInt16(&b, "fftBitrev240", "fftBitrev240 is the bit-reversal table for the 240-point FFT stage.", t["fftBitrev240"], 20)
	emitInt16(&b, "fftBitrev120", "fftBitrev120 is the bit-reversal table for the 120-point FFT stage.", t["fftBitrev120"], 20)
	emitInt16(&b, "fftBitrev60", "fftBitrev60 is the bit-reversal table for the 60-point FFT stage.", t["fftBitrev60"], 20)
	emitTwiddles(&b, t["fftTwiddles48000960"])

	// The four kiss_fft_state instances shared by the MDCT lookup. All four
	// point at the same twiddle table; bitrev differs per stage.
	bitrevNames := []string{"fftBitrev480", "fftBitrev240", "fftBitrev120", "fftBitrev60"}
	for i := range 4 {
		emitFFTState(&b, i, t[fmt.Sprintf("fftState%d", i)], bitrevNames[i])
	}

	// The assembled mode. Scalars come straight from the dump.
	ms := t["modeScalars"]
	pe := t["preemph"]
	fmt.Fprintf(&b, "// mode48000_960 is the sole CELT mode required for 48 kHz fullband, mirroring\n")
	fmt.Fprintf(&b, "// libopus mode48000_960_120 (celt/static_modes_fixed.h).\n")
	fmt.Fprintf(&b, "var mode48000_960 = celtMode{\n")
	fmt.Fprintf(&b, "\tFs: %d,\n", ms[0])
	fmt.Fprintf(&b, "\toverlap: %d,\n", ms[1])
	fmt.Fprintf(&b, "\tnbEBands: %d,\n", ms[2])
	fmt.Fprintf(&b, "\teffEBands: %d,\n", ms[3])
	fmt.Fprintf(&b, "\tpreemph: [4]int16{%d, %d, %d, %d},\n", pe[0], pe[1], pe[2], pe[3])
	fmt.Fprintf(&b, "\teBands: eband5ms,\n")
	fmt.Fprintf(&b, "\tmaxLM: %d,\n", ms[4])
	fmt.Fprintf(&b, "\tnbShortMdcts: %d,\n", ms[5])
	fmt.Fprintf(&b, "\tshortMdctSize: %d,\n", ms[6])
	fmt.Fprintf(&b, "\tnbAllocVectors: %d,\n", ms[7])
	fmt.Fprintf(&b, "\tallocVectors: bandAllocation,\n")
	fmt.Fprintf(&b, "\tlogN: logN400,\n")
	fmt.Fprintf(&b, "\twindow: window120,\n")
	fmt.Fprintf(&b, "\tmdct: mdctLookup{\n")
	fmt.Fprintf(&b, "\t\tn: %d,\n", ms[9])
	fmt.Fprintf(&b, "\t\tmaxshift: %d,\n", ms[10])
	fmt.Fprintf(&b, "\t\tkfft: [4]*kissFFTState{&fftState48000960_0, &fftState48000960_1, &fftState48000960_2, &fftState48000960_3},\n")
	fmt.Fprintf(&b, "\t\ttrig: mdctTwiddles960,\n")
	fmt.Fprintf(&b, "\t},\n")
	fmt.Fprintf(&b, "\tcache: pulseCache{\n")
	fmt.Fprintf(&b, "\t\tsize: %d,\n", ms[8])
	fmt.Fprintf(&b, "\t\tindex: cacheIndex50,\n")
	fmt.Fprintf(&b, "\t\tbits: cacheBits50,\n")
	fmt.Fprintf(&b, "\t\tcaps: cacheCaps50,\n")
	fmt.Fprintf(&b, "\t},\n")
	fmt.Fprintf(&b, "}\n")

	return format.Source([]byte(b.String()))
}

func emitInt16(b *strings.Builder, name, doc string, vals []int, perLine int) {
	fmt.Fprintf(b, "// %s\n", doc)
	fmt.Fprintf(b, "var %s = []int16{\n", name)
	writeRows(b, vals, perLine)
	fmt.Fprintf(b, "}\n\n")
}

func emitBytes(b *strings.Builder, name, doc string, vals []int, perLine int) {
	fmt.Fprintf(b, "// %s\n", doc)
	fmt.Fprintf(b, "var %s = []byte{\n", name)
	writeRows(b, vals, perLine)
	fmt.Fprintf(b, "}\n\n")
}

func writeRows(b *strings.Builder, vals []int, perLine int) {
	for i, v := range vals {
		if i%perLine == 0 {
			b.WriteString("\t")
		}
		fmt.Fprintf(b, "%d,", v)
		if i%perLine == perLine-1 || i == len(vals)-1 {
			b.WriteString("\n")
		} else {
			b.WriteString(" ")
		}
	}
}

func emitTwiddles(b *strings.Builder, flat []int) {
	fmt.Fprintf(b, "// fftTwiddles48000960 is the shared kiss_twiddle_cpx (int16 r,i) FFT twiddle\n")
	fmt.Fprintf(b, "// table (480 complex pairs) referenced by all four kissFFTState stages.\n")
	fmt.Fprintf(b, "var fftTwiddles48000960 = []kissTwiddleCpx{\n")
	for i := 0; i < len(flat); i += 2 {
		fmt.Fprintf(b, "\t{%d, %d},\n", flat[i], flat[i+1])
	}
	fmt.Fprintf(b, "}\n\n")
}

func emitFFTState(b *strings.Builder, idx int, s []int, bitrev string) {
	// s = [nfft, scale, scaleShift, shift, f0..f15].
	fmt.Fprintf(b, "// fftState48000960_%d mirrors libopus fft_state48000_960_%d.\n", idx, idx)
	fmt.Fprintf(b, "var fftState48000960_%d = kissFFTState{\n", idx)
	fmt.Fprintf(b, "\tnfft: %d,\n", s[0])
	fmt.Fprintf(b, "\tscale: %d,\n", s[1])
	fmt.Fprintf(b, "\tscaleShift: %d,\n", s[2])
	fmt.Fprintf(b, "\tshift: %d,\n", s[3])
	fmt.Fprintf(b, "\tfactors: [2 * maxFactors]int16{")
	for i := range 16 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%d", s[4+i])
	}
	fmt.Fprintf(b, "},\n")
	fmt.Fprintf(b, "\tbitrev: %s,\n", bitrev)
	fmt.Fprintf(b, "\ttwiddles: fftTwiddles48000960,\n")
	fmt.Fprintf(b, "}\n\n")
}
