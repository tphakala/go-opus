// Transliteration of the decode side of celt/quant_bands.c (libopus v1.6.1,
// commit 3da9f7a6) for the frozen FIXED_POINT + DISABLE_FLOAT_API build: band
// energy dequantization. unquantCoarseEnergy decodes the coarse (Laplace-coded)
// log energies with the inter/intra prediction filter over oldEBands (the
// cross-frame state); unquantFineEnergy and unquantEnergyFinalise add the fine
// and finalise refinement bits. The encoder halves (quant_coarse_energy and
// friends) and amp2Log2/log2Amp are phase-2/phase-4 material elsewhere and are
// not ported here.
//
// Cross-frame contract (docs/hard-parts.md 5): oldEBands is both the prediction
// base (input) and the reconstruction (output). The caller carries it across
// frames; unquantCoarseEnergy clamps and overwrites each entry in place.

package celt

import (
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// dbShift is celt/arch.h DB_SHIFT: the fractional bits of a celt_glog log-energy
// value (Q24) in the FIXED_POINT build (arch.h:219).
const dbShift = 24

// maxFineBits is celt/rate.h MAX_FINE_BITS: the cap on fine-energy bits per band.
const maxFineBits = 8

// Fixed-point log-energy constants, GCONST(x) = QCONST32(x, DB_SHIFT). Computed
// with the same compile-time float-to-fixed rounding as the C macro
// (celt/fixed_generic.h:98-101), evaluated once at package init.
var (
	gconst9  = fixedmath.QCONST32(9, dbShift)   // GCONST(9.f)
	gconst28 = fixedmath.QCONST32(28, dbShift)  // GCONST(28.f)
	ghalf    = fixedmath.QCONST32(0.5, dbShift) // GCONST(.5f)
)

// Prediction coefficients 0.9, 0.8, 0.65, 0.5 in Q15, indexed by LM
// (celt/quant_bands.c:61-65, FIXED_POINT). The decoder uses the same constants
// as the encoder: coef predicts from the previous frame's energy (inter),
// betaCoef/betaIntra drive the intra-frame leakage into prev[].
var (
	predCoef = [4]int16{29440, 26112, 21248, 16384}
	betaCoef = [4]int16{30147, 22282, 12124, 6554}
)

// betaIntra is beta_intra, the intra-frame beta coefficient in Q15
// (celt/quant_bands.c:65).
const betaIntra int16 = 4915

// eProbModel transcribes e_prob_model[4][2][42] (celt/quant_bands.c:77).
// Laplace-like probability models for the coarse energy, one pair per frame
// size (LM), prediction type (0=inter, 1=intra) and band: the first of each
// pair is the probability of 0, the second the decay rate, both in Q8.
var eProbModel = [4][2][42]uint8{
	// 120-sample frames (LM=0).
	{
		{ // Inter.
			72, 127, 65, 129, 66, 128, 65, 128, 64, 128, 62, 128, 64, 128,
			64, 128, 92, 78, 92, 79, 92, 78, 90, 79, 116, 41, 115, 40,
			114, 40, 132, 26, 132, 26, 145, 17, 161, 12, 176, 10, 177, 11,
		},
		{ // Intra.
			24, 179, 48, 138, 54, 135, 54, 132, 53, 134, 56, 133, 55, 132,
			55, 132, 61, 114, 70, 96, 74, 88, 75, 88, 87, 74, 89, 66,
			91, 67, 100, 59, 108, 50, 120, 40, 122, 37, 97, 43, 78, 50,
		},
	},
	// 240-sample frames (LM=1).
	{
		{ // Inter.
			83, 78, 84, 81, 88, 75, 86, 74, 87, 71, 90, 73, 93, 74,
			93, 74, 109, 40, 114, 36, 117, 34, 117, 34, 143, 17, 145, 18,
			146, 19, 162, 12, 165, 10, 178, 7, 189, 6, 190, 8, 177, 9,
		},
		{ // Intra.
			23, 178, 54, 115, 63, 102, 66, 98, 69, 99, 74, 89, 71, 91,
			73, 91, 78, 89, 86, 80, 92, 66, 93, 64, 102, 59, 103, 60,
			104, 60, 117, 52, 123, 44, 138, 35, 133, 31, 97, 38, 77, 45,
		},
	},
	// 480-sample frames (LM=2).
	{
		{ // Inter.
			61, 90, 93, 60, 105, 42, 107, 41, 110, 45, 116, 38, 113, 38,
			112, 38, 124, 26, 132, 27, 136, 19, 140, 20, 155, 14, 159, 16,
			158, 18, 170, 13, 177, 10, 187, 8, 192, 6, 175, 9, 159, 10,
		},
		{ // Intra.
			21, 178, 59, 110, 71, 86, 75, 85, 84, 83, 91, 66, 88, 73,
			87, 72, 92, 75, 98, 72, 105, 58, 107, 54, 115, 52, 114, 55,
			112, 56, 129, 51, 132, 40, 150, 33, 140, 29, 98, 35, 77, 42,
		},
	},
	// 960-sample frames (LM=3).
	{
		{ // Inter.
			42, 121, 96, 66, 108, 43, 111, 40, 117, 44, 123, 32, 120, 36,
			119, 33, 127, 33, 134, 34, 139, 21, 147, 23, 152, 20, 158, 25,
			154, 26, 166, 21, 173, 16, 184, 13, 184, 10, 150, 13, 139, 15,
		},
		{ // Intra.
			22, 178, 63, 114, 74, 82, 84, 83, 92, 82, 103, 62, 96, 72,
			96, 67, 101, 73, 107, 72, 113, 55, 118, 52, 125, 52, 118, 52,
			117, 55, 135, 49, 137, 39, 157, 32, 145, 29, 97, 33, 77, 40,
		},
	},
}

// smallEnergyICDF is small_energy_icdf (celt/quant_bands.c:140): the 3-entry
// inverse CDF used to code a coarse-energy delta when only 2-14 bits remain.
var smallEnergyICDF = [3]byte{2, 1, 0}

// unquantCoarseEnergy ports unquant_coarse_energy (celt/quant_bands.c:431). It
// decodes the coarse log energies into oldEBands using the inter/intra
// prediction filter, clamping each reconstructed value to [-28, 28] in Q24.
// prev[] is opus_val64 (int64) as in C; the intermediate sum is formed in int64
// and narrowed to the int32 celt_glog exactly like the C assignment.
func unquantCoarseEnergy(m *celtMode, start, end int, oldEBands []int32, intra int, dec *rangecoding.Decoder, C, LM int) {
	probModel := eProbModel[LM][intra]
	var prev [2]int64 // opus_val64 prev[2]
	var coef, beta int16

	if intra != 0 {
		coef = 0
		beta = betaIntra
	} else {
		beta = betaCoef[LM]
		coef = predCoef[LM]
	}

	budget := int32(dec.Storage() * 8)

	// Decode at a fixed coarse resolution.
	for i := start; i < end; i++ {
		c := 0
		for {
			var qi int
			tell := int32(dec.Tell())
			switch {
			case budget-tell >= 15:
				pi := 2 * fixedmath.IMIN(i, 20)
				qi = ecLaplaceDecode(dec, uint32(probModel[pi])<<7, int(probModel[pi+1])<<6)
			case budget-tell >= 2:
				qi = dec.DecIcdf(smallEnergyICDF[:], 2)
				qi = (qi >> 1) ^ -(qi & 1)
			case budget-tell >= 1:
				qi = -dec.DecBitLogp(1)
			default:
				qi = -1
			}
			q := fixedmath.SHL32(int32(qi), dbShift)

			idx := i + c*m.nbEBands
			oldEBands[idx] = fixedmath.MAX32(-gconst9, oldEBands[idx])
			tmp := int32(int64(fixedmath.MULT16_32_Q15(coef, oldEBands[idx])) + prev[c] + int64(q))
			tmp = fixedmath.MIN32(gconst28, fixedmath.MAX32(-gconst28, tmp))
			oldEBands[idx] = tmp
			prev[c] = prev[c] + int64(q) - int64(fixedmath.MULT16_32_Q15(beta, q))

			c++
			if c >= C {
				break
			}
		}
	}
}

// unquantFineEnergy ports unquant_fine_energy (celt/quant_bands.c:496). It reads
// extraQuant[i] raw bits per band (per channel) and adds the fine-resolution
// offset into oldEBands. prevQuant is the QEXT "previous fine bits" array (NULL
// on the main path); it is honored for faithfulness though the non-QEXT decoder
// always passes nil.
func unquantFineEnergy(m *celtMode, start, end int, oldEBands []int32, prevQuant, extraQuant []int, dec *rangecoding.Decoder, C int) {
	// Decode finer resolution.
	for i := start; i < end; i++ {
		extra := int16(extraQuant[i])
		if extraQuant[i] <= 0 {
			continue
		}
		if int32(dec.Tell())+int32(C*extraQuant[i]) > int32(dec.Storage()*8) {
			continue
		}
		var prev int16
		if prevQuant != nil {
			prev = int16(prevQuant[i])
		}
		c := 0
		for {
			q2 := int32(dec.DecBits(uint32(extra)))
			// Has to be without rounding.
			offset := fixedmath.SUB32(fixedmath.VSHR32(2*q2+1, int(extra)-dbShift+1), ghalf)
			offset = fixedmath.SHR32(offset, int(prev))
			oldEBands[i+c*m.nbEBands] += offset
			c++
			if c >= C {
				break
			}
		}
	}
}

// unquantEnergyFinalise ports unquant_energy_finalise (celt/quant_bands.c:525).
// It uses up the remaining bits, one per (band, channel), in two priority
// passes, adding a +/-0.5-LSB offset into oldEBands. oldEBands may be nil (the
// QEXT path), in which case the bits are consumed but discarded.
func unquantEnergyFinalise(m *celtMode, start, end int, oldEBands []int32, fineQuant, finePriority []int, bitsLeft int, dec *rangecoding.Decoder, C int) {
	// Use up the remaining bits.
	for prio := 0; prio < 2; prio++ {
		for i := start; i < end && bitsLeft >= C; i++ {
			if fineQuant[i] >= maxFineBits || finePriority[i] != prio {
				continue
			}
			c := 0
			for {
				q2 := int32(dec.DecBits(1))
				offset := fixedmath.SHR32(fixedmath.SHL32(q2, dbShift)-ghalf, fineQuant[i]+1)
				if oldEBands != nil {
					oldEBands[i+c*m.nbEBands] += offset
				}
				bitsLeft--
				c++
				if c >= C {
					break
				}
			}
		}
	}
}

// UnquantCoarseEnergy is the exported test seam over unquantCoarseEnergy bound
// to the frozen mode48000_960 (mirroring InverseMDCT's adapter shape). It lets
// the refc cgo differential harness (internal/reftest/oracle) drive the pure-Go
// coarse-energy decode against libopus; it is not part of the decoder API. C is
// the channel count (1 or 2); intra is the already-decoded intra-energy flag.
func UnquantCoarseEnergy(start, end int, oldEBands []int32, intra int, dec *rangecoding.Decoder, C, LM int) {
	unquantCoarseEnergy(&mode48000_960, start, end, oldEBands, intra, dec, C, LM)
}

// UnquantFineEnergy is the exported test seam over unquantFineEnergy, bound to
// mode48000_960 with prevQuant=nil (the non-QEXT decoder path,
// celt/celt_decoder.c:1455). fineQuant is the per-band fine bit count.
func UnquantFineEnergy(start, end int, oldEBands []int32, fineQuant []int, dec *rangecoding.Decoder, C int) {
	unquantFineEnergy(&mode48000_960, start, end, oldEBands, nil, fineQuant, dec, C)
}

// UnquantEnergyFinalise is the exported test seam over unquantEnergyFinalise
// bound to mode48000_960 (celt/celt_decoder.c:1520). bitsLeft is len*8 minus the
// bits used so far, exactly as the decoder pipeline computes it.
func UnquantEnergyFinalise(start, end int, oldEBands []int32, fineQuant, finePriority []int, bitsLeft int, dec *rangecoding.Decoder, C int) {
	unquantEnergyFinalise(&mode48000_960, start, end, oldEBands, fineQuant, finePriority, bitsLeft, dec, C)
}
