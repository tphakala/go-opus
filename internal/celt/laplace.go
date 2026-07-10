// Transliteration of the decode side of celt/laplace.c (libopus v1.6.1, commit
// 3da9f7a6): the Laplace-like entropy decode used by the CELT coarse-energy
// quantizer (unquant_coarse_energy in quant_bands.go). The encoder counterpart
// ec_laplace_encode and the p0 variants are phase-4 concerns and are omitted
// here; only ec_laplace_decode and its ec_laplace_get_freq1 helper are needed
// for the phase-2 decoder.

package celt

import "github.com/tphakala/go-opus/internal/rangecoding"

// Laplace model constants (celt/laplace.c:37-41).
const (
	// laplaceLogMinP: LAPLACE_LOG_MINP, log2 of the minimum probability of an
	// energy delta (out of 32768).
	laplaceLogMinP = 0
	// laplaceMinP: LAPLACE_MINP = 1<<LAPLACE_LOG_MINP.
	laplaceMinP = 1 << laplaceLogMinP
	// laplaceNmin: LAPLACE_NMIN, the minimum number of guaranteed representable
	// energy deltas (in one direction).
	laplaceNmin = 16
)

// ecLaplaceGetFreq1 mirrors the static ec_laplace_get_freq1 (celt/laplace.c:44).
// When called, decay is positive and at most 11456. All arithmetic is unsigned
// 32-bit, matching C's `unsigned` result (the (opus_int32) cast of 16384-decay
// is folded back into the unsigned multiply by the usual conversions).
func ecLaplaceGetFreq1(fs0 uint32, decay int) uint32 {
	ft := uint32(32768-laplaceMinP*(2*laplaceNmin)) - fs0
	return (ft * uint32(16384-decay)) >> 15
}

// ecLaplaceDecode ports ec_laplace_decode (celt/laplace.c:94): it decodes a
// value assumed to be the realisation of a Laplace-distributed process. fs is
// the probability of 0 (times 32768); decay is the probability of +/-1 (times
// 16384). fl/fs/fm are unsigned, exactly as in C, so the wrapping arithmetic
// matches bit for bit.
func ecLaplaceDecode(dec *rangecoding.Decoder, fs uint32, decay int) int {
	val := 0
	var fl uint32
	fm := dec.DecodeBin(15)
	fl = 0
	if fm >= fs {
		val++
		fl = fs
		fs = ecLaplaceGetFreq1(fs, decay) + laplaceMinP
		// Search the decaying part of the PDF.
		for fs > laplaceMinP && fm >= fl+2*fs {
			fs *= 2
			fl += fs
			fs = ((fs - 2*laplaceMinP) * uint32(decay)) >> 15
			fs += laplaceMinP
			val++
		}
		// Everything beyond that has probability LAPLACE_MINP.
		if fs <= laplaceMinP {
			di := (fm - fl) >> (laplaceLogMinP + 1)
			val += int(di)
			fl += 2 * di * laplaceMinP
		}
		if fm < fl+fs {
			val = -val
		} else {
			fl += fs
		}
	}
	// ec_dec_update(dec, fl, IMIN(fl+fs,32768), 32768).
	fh := fl + fs
	if fh > 32768 {
		fh = 32768
	}
	dec.DecUpdate(fl, fh, 32768)
	return val
}
