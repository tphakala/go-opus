//go:build ectrace

package rangecoding

import (
	"math/rand/v2"
	"testing"
)

// TestTraceRecordsSymbols runs under `-tags ectrace` and confirms the symbol
// trace logs one record per coded symbol with a monotonically non-decreasing
// tell_frac (bits only ever accumulate). This is the phase-4 bisection tool, so
// it must actually observe the coder.
func TestTraceRecordsSymbols(t *testing.T) {
	TraceReset()
	r := rand.New(rand.NewPCG(3, 3))
	ops := genOps(r, 50)
	_ = encodeOps(ops, 8192)

	recs := TraceRecords()
	if len(recs) == 0 {
		t.Fatal("trace recorded no symbols")
	}
	// Every ec_encode/ec_enc_bit_logp/ec_enc_icdf/ec_enc_bits produces one record;
	// ec_enc_uint expands into an ec_encode plus (on the large branch) an
	// ec_enc_bits, so the count is at least the number of ops.
	if len(recs) < len(ops) {
		t.Errorf("recorded %d symbols for %d ops, want >= %d", len(recs), len(ops), len(ops))
	}
	prev := uint32(0)
	for i, rec := range recs {
		if rec.Index != i {
			t.Errorf("record %d has Index %d", i, rec.Index)
		}
		if rec.TellFrac < prev {
			t.Errorf("record %d tell_frac %d decreased from %d\n%s", i, rec.TellFrac, prev, rec)
		}
		prev = rec.TellFrac
	}
	t.Logf("traced %d symbols, final tell_frac=%d", len(recs), prev)
	t.Logf("first record: %s", recs[0])
}
