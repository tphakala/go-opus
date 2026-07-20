package celt

import (
	"math/rand"
	"testing"
)

// BenchmarkSpreadingDecision drives the full encoder spreading_decision over
// mode48000_960 at LM3 stereo (the heaviest case: 21 bands x 2 channels, the
// widest band ~176 wide). The per-position CDF histogram (spreadingHist) is the
// bulk of the work, so this benchmark tracks the histogram optimization in its
// real context. Inputs are a seeded normalized-band spectrum whose magnitudes
// straddle the three Q13 thresholds so all bins are exercised.
func BenchmarkSpreadingDecision(b *testing.B) {
	m := &mode48000_960
	const LM = 3
	const M = 1 << LM
	const C = 2
	N := M * m.shortMdctSize
	r := rand.New(rand.NewSource(0x59DEC))

	X := make([]int32, C*N)
	for i := range X {
		// Spread magnitudes across ~[-2^21, 2^21] so |x|^2*N lands on both sides
		// of the 128 / 512 / 2048 (Q13) thresholds across bands.
		X[i] = int32(r.Intn(1<<22) - (1 << 21))
	}
	spreadWeight := make([]int, m.nbEBands)
	for i := range spreadWeight {
		spreadWeight[i] = 1 + r.Intn(4)
	}
	end := m.nbEBands

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		avg, hf, tap := 200, 30, 1
		_ = SpreadingDecision(X, &avg, 1, &hf, &tap, 1, end, C, M, spreadWeight)
	}
}
