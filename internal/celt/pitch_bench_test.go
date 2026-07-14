package celt

import "testing"

// BenchmarkCeltPitchXcorr runs the kernel at the exact shape the 48 kHz / 20 ms
// encoder drives it at: pitch_search's coarse (4x-decimated) pass, where
// len_ = 960>>2 = 240 and maxPitch = 979>>2 = 244, over a y history of
// (960+979)>>2 = 484 samples. This is the hottest loop in the encoder
// (run_prefilter's subtree), so it is worth pinning on its own rather than only
// through the whole-encode benchmarks.
func BenchmarkCeltPitchXcorr(b *testing.B) {
	const (
		len_     = 240
		maxPitch = 244
	)
	x := make([]int16, len_)
	y := make([]int16, len_+maxPitch)
	// Deterministic, full-scale-ish input; the values only need to exercise the
	// multiply-accumulate, not be meaningful audio.
	s := uint32(0x9E3779B9)
	for i := range x {
		s = s*1664525 + 1013904223
		x[i] = int16(s >> 17)
	}
	for i := range y {
		s = s*1664525 + 1013904223
		y[i] = int16(s >> 17)
	}
	xcorr := make([]int32, maxPitch)

	b.ReportAllocs()
	var sink int32
	for b.Loop() {
		sink = celtPitchXcorr(x, y, xcorr, len_, maxPitch)
	}
	_ = sink
}
