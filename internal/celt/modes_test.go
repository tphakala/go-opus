package celt

import "testing"

// These tests pin the generated CELT mode data (static_modes_gen.go) against
// values read independently from libopus celt/static_modes_fixed.h (tag v1.6.1)
// and celt/modes.c. They need no C toolchain, so normal CI catches any drift
// introduced by a submodule bump + regeneration. Every mode field is also read
// at least once here, which doubles as the "these tables have a consumer" guard.

func TestModeStructuralInvariants(t *testing.T) {
	m := mode48000_960

	// Scalars (celt/static_modes_fixed.h mode48000_960_120).
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"Fs", int(m.Fs), 48000},
		{"overlap", m.overlap, 120},
		{"nbEBands", m.nbEBands, 21},
		{"effEBands", m.effEBands, 21},
		{"maxLM", m.maxLM, 3},
		{"nbShortMdcts", m.nbShortMdcts, 8},
		{"shortMdctSize", m.shortMdctSize, 120},
		{"nbAllocVectors", m.nbAllocVectors, 11},
		{"mdct.n", m.mdct.n, 1920},
		{"mdct.maxshift", m.mdct.maxshift, 3},
		{"cache.size", m.cache.size, 392},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// Table lengths.
	lengths := []struct {
		name string
		got  int
		want int
	}{
		{"eBands", len(m.eBands), 22}, // nbEBands + 1
		{"logN", len(m.logN), 21},
		{"window", len(m.window), 120},
		{"allocVectors", len(m.allocVectors), 231}, // 11 * 21
		{"cache.index", len(m.cache.index), 105},
		{"cache.bits", len(m.cache.bits), 392},
		{"cache.caps", len(m.cache.caps), 168},
		{"mdct.trig", len(m.mdct.trig), 1800},
		{"fftTwiddles", len(fftTwiddles48000960), 480},
		{"fftBitrev480", len(fftBitrev480), 480},
		{"fftBitrev240", len(fftBitrev240), 240},
		{"fftBitrev120", len(fftBitrev120), 120},
		{"fftBitrev60", len(fftBitrev60), 60},
	}
	for _, c := range lengths {
		if c.got != c.want {
			t.Errorf("len(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}

	// cache.size must equal len(cache.bits) (it is the C cache.size field).
	if m.cache.size != len(m.cache.bits) {
		t.Errorf("cache.size %d != len(cache.bits) %d", m.cache.size, len(m.cache.bits))
	}
	// allocVectors is nbAllocVectors rows of nbEBands.
	if got, want := len(m.allocVectors), m.nbAllocVectors*m.nbEBands; got != want {
		t.Errorf("len(allocVectors) = %d, want nbAllocVectors*nbEBands = %d", got, want)
	}
	// window length equals overlap.
	if len(m.window) != m.overlap {
		t.Errorf("len(window) = %d, want overlap %d", len(m.window), m.overlap)
	}
}

func TestModeSpotValues(t *testing.T) {
	m := mode48000_960

	// preemph (opus_val16[4]).
	if want := [4]int16{27853, 0, 4096, 8192}; m.preemph != want {
		t.Errorf("preemph = %v, want %v", m.preemph, want)
	}

	spot := []struct {
		name string
		got  int
		want int
	}{
		// eBands boundaries (eband5ms).
		{"eBands[0]", int(m.eBands[0]), 0},
		{"eBands[9]", int(m.eBands[9]), 10},
		{"eBands[21]", int(m.eBands[21]), 100},
		// logN400.
		{"logN[0]", int(m.logN[0]), 0},
		{"logN[8]", int(m.logN[8]), 8},
		{"logN[15]", int(m.logN[15]), 21},
		{"logN[20]", int(m.logN[20]), 36},
		// window120.
		{"window[0]", int(m.window[0]), 2},
		{"window[1]", int(m.window[1]), 20},
		{"window[2]", int(m.window[2]), 55},
		{"window[119]", int(m.window[119]), 32767},
		// band_allocation matrix.
		{"allocVectors[0]", int(m.allocVectors[0]), 0},
		{"allocVectors[21]", int(m.allocVectors[21]), 90}, // row 1, band 0
		{"allocVectors[230]", int(m.allocVectors[230]), 104},
		// PVQ pulse cache (cache_index50 / cache_bits50 / cache_caps50).
		{"cache.index[0]", int(m.cache.index[0]), -1},
		{"cache.index[8]", int(m.cache.index[8]), 0},
		{"cache.index[20]", int(m.cache.index[20]), 222},
		{"cache.bits[0]", int(m.cache.bits[0]), 40},
		{"cache.bits[1]", int(m.cache.bits[1]), 7},
		{"cache.caps[0]", int(m.cache.caps[0]), 224},
		{"cache.caps[8]", int(m.cache.caps[8]), 160},
		{"cache.caps[167]", int(m.cache.caps[167]), 40},
		// MDCT trig table (mdct_twiddles960).
		{"mdct.trig[0]", int(m.mdct.trig[0]), 32767},
		{"mdct.trig[3]", int(m.mdct.trig[3]), 32766},
		// FFT bit-reversal (fft_bitrev480).
		{"fftBitrev480[0]", int(fftBitrev480[0]), 0},
		{"fftBitrev480[1]", int(fftBitrev480[1]), 96},
		{"fftBitrev480[2]", int(fftBitrev480[2]), 192},
	}
	for _, c := range spot {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// FFT twiddles (kiss_twiddle_cpx: int16 r, i).
	if got := fftTwiddles48000960[0]; got != (kissTwiddleCpx{r: 32767, i: 0}) {
		t.Errorf("fftTwiddles[0] = %+v, want {32767 0}", got)
	}
	if got := fftTwiddles48000960[1]; got != (kissTwiddleCpx{r: 32765, i: -429}) {
		t.Errorf("fftTwiddles[1] = %+v, want {32765 -429}", got)
	}
}

func TestModeFFTStates(t *testing.T) {
	m := mode48000_960

	if got := len(m.mdct.kfft); got != 4 {
		t.Fatalf("mdct.kfft len = %d, want 4", got)
	}

	// nfft, scaleShift and shift per decimation stage (fft_state48000_960_0..3).
	// scale (17476) is shared; twiddles are shared; bitrev differs.
	want := []struct {
		nfft       int
		scaleShift int
		shift      int
		bitrevLen  int
		factors0   int16
		factors1   int16
	}{
		{480, 8, -1, 480, 5, 96},
		{240, 7, 1, 240, 5, 48},
		{120, 6, 2, 120, 5, 24},
		{60, 5, 3, 60, 5, 12},
	}
	for i, w := range want {
		s := m.mdct.kfft[i]
		if s == nil {
			t.Fatalf("kfft[%d] is nil", i)
		}
		if s.nfft != w.nfft {
			t.Errorf("kfft[%d].nfft = %d, want %d", i, s.nfft, w.nfft)
		}
		if s.scale != 17476 {
			t.Errorf("kfft[%d].scale = %d, want 17476", i, s.scale)
		}
		if s.scaleShift != w.scaleShift {
			t.Errorf("kfft[%d].scaleShift = %d, want %d", i, s.scaleShift, w.scaleShift)
		}
		if s.shift != w.shift {
			t.Errorf("kfft[%d].shift = %d, want %d", i, s.shift, w.shift)
		}
		if len(s.bitrev) != w.bitrevLen {
			t.Errorf("kfft[%d] bitrev len = %d, want %d", i, len(s.bitrev), w.bitrevLen)
		}
		if s.factors[0] != w.factors0 || s.factors[1] != w.factors1 {
			t.Errorf("kfft[%d].factors[0:2] = %d,%d, want %d,%d",
				i, s.factors[0], s.factors[1], w.factors0, w.factors1)
		}
		if len(s.factors) != 2*maxFactors {
			t.Errorf("kfft[%d] factors len = %d, want %d", i, len(s.factors), 2*maxFactors)
		}
		// All stages share the one twiddle table.
		if len(s.twiddles) != 480 {
			t.Errorf("kfft[%d] twiddles len = %d, want 480", i, len(s.twiddles))
		}
	}
}
