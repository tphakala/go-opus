package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// vectorsDir is the extracted RFC 8251 bitstream set, relative to this package.
const vectorsDir = "../../testdata/vectors/opus_newvectors"

// firstRecords returns the first n opus_demo records of a .bit file as a new
// .bit byte slice, so a CLI test can exercise the full decode path on a small,
// fast slice of a real pure-CELT vector.
func firstRecords(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	off, taken := 0, 0
	for off+8 <= len(data) && taken < n {
		length := int(binary.BigEndian.Uint32(data[off:]))
		next := off + 8 + length
		if length != 0 && next > len(data) {
			break
		}
		off = next
		taken++
	}
	return data[:off]
}

// TestRunDecodesVector runs the CLI decode path over the first records of the
// pure-CELT vector 11 (a fast slice) and checks it decodes without error, passes
// the per-packet final-range verify, and writes the expected number of PCM
// bytes. It skips cleanly when the vectors are not fetched.
func TestRunDecodesVector(t *testing.T) {
	bitPath := filepath.Join(vectorsDir, "testvector11.bit")
	full, err := os.ReadFile(bitPath)
	if err != nil {
		t.Skipf("test vectors not present (%v); run scripts/fetch-vectors.sh", err)
	}

	const nRecords = 8
	small := firstRecords(t, full, nRecords)

	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.bit")
	outPath := filepath.Join(dir, "out.pcm")
	if err := os.WriteFile(inPath, small, 0o600); err != nil {
		t.Fatalf("write slice: %v", err)
	}

	if err := run(inPath, outPath, 48000, 2, true); err != nil {
		t.Fatalf("run: %v", err)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	// Vector 11 is all 20 ms packets (960 samples/channel per frame), decoded
	// stereo at 2 bytes per sample. A packet may hold several such frames, so the
	// output is a positive whole number of 20 ms stereo frames.
	const frameBytes = 960 * 2 * 2
	if info.Size() == 0 || info.Size()%frameBytes != 0 {
		t.Errorf("output size = %d bytes, want a positive multiple of %d", info.Size(), frameBytes)
	}
}

// TestRunRejectsBadConfig confirms the CLI surfaces NewDecoder validation errors.
func TestRunRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.bit")
	if err := os.WriteFile(inPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write empty input: %v", err)
	}
	if err := run(inPath, filepath.Join(dir, "out.pcm"), 44100, 2, false); err == nil {
		t.Error("run with rate 44100 should fail, got nil")
	}
}
