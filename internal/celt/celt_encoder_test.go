package celt

import "testing"

// TestResamplingFactor pins resampling_factor (celt/celt.c:62). The value is the
// encoder's st->upsample and the decoder's st->downsample, i.e. the integer ratio
// between the 48 kHz CELT mode and the rate the caller runs at. The differential
// test TestCeltResamplingFactorMatchesC asserts these same numbers against the C;
// this one keeps the table honest in the default (no-cgo) test job.
func TestResamplingFactor(t *testing.T) {
	cases := map[int32]int{
		48000: 1,
		24000: 2,
		16000: 3,
		12000: 4,
		8000:  6,
		// Everything else is 0, which is what the C returns once celt_assert is
		// compiled out (the frozen config has no ENABLE_ASSERTIONS). 96000 is
		// ENABLE_QEXT-only and therefore unsupported here.
		0:      0,
		44100:  0,
		32000:  0,
		96000:  0,
		-48000: 0,
	}
	for rate, want := range cases {
		if got := ResamplingFactor(rate); got != want {
			t.Errorf("ResamplingFactor(%d) = %d, want %d", rate, got, want)
		}
	}
}

// TestNewEncoderRateDomain is the CELT half of the CP10 invariant: THE API MUST
// NEVER ACCEPT A SAMPLE RATE IT CANNOT ENCODE. A rate resampling_factor returns 0
// for would leave st.upsample == 0, and celt_preemphasis (celt_encoder.c:583)
// divides N by it. In the C that is an assertion the frozen build compiles out,
// leaving a division by zero; here the constructor refuses.
func TestNewEncoderRateDomain(t *testing.T) {
	for _, fs := range []int32{8000, 12000, 16000, 24000, 48000} {
		st := NewEncoder(fs, 2)
		if st == nil {
			t.Fatalf("NewEncoder(%d, 2) = nil, want an encoder", fs)
		}
		if got, want := st.upsample, ResamplingFactor(fs); got != want {
			t.Errorf("NewEncoder(%d, 2).upsample = %d, want %d", fs, got, want)
		}
	}
	for _, fs := range []int32{0, 1, 32000, 44100, 96000, -48000} {
		if st := NewEncoder(fs, 2); st != nil {
			t.Errorf("NewEncoder(%d, 2) = %p, want nil (resampling_factor = 0)", fs, st)
		}
	}
	// The channel check is unchanged by CP10 and must still hold at every rate.
	for _, ch := range []int{0, 3, -1} {
		if st := NewEncoder(48000, ch); st != nil {
			t.Errorf("NewEncoder(48000, %d) = %p, want nil", ch, st)
		}
	}
}
