// Transliteration of the DECODE side of celt/cwrs.c (libopus v1.6.1, commit
// 3da9f7a6) for the frozen non-SMALL_FOOTPRINT, non-CUSTOM_MODES, non-ENABLE_QEXT
// build: the CELT PVQ (pulse vector) codeword decoder. cwrsi turns a codebook
// index into a vector of signed pulses, and DecodePulses reads that index from
// the range decoder. The combinatorial U(N,K) table lives in cwrs_tables.go.
//
// The table macros CELT_PVQ_U / CELT_PVQ_V are functions here (celtPVQU /
// celtPVQV); pvqURow mirrors the C "row = CELT_PVQ_U_ROW[n]" pointer so the
// column indexing in cwrsi reads the same as the source. Exported with C-style
// names on the relaxed internal/celt lint list; helpers stay unexported.
//
// The encoder ranking (icwrs / encode_pulses) and the CUSTOM_MODES-only
// get_required_bits / log2_frac are phase-4 concerns and are omitted here; see
// the TODO below.

package celt

import (
	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// TODO(phase 4): port the encoder side of cwrs.c (icwrs, encode_pulses) and the
// CUSTOM_MODES-only get_required_bits / log2_frac when the CELT encoder lands.

// celtPVQU returns U(N,K), the number of PVQ combinations wherein N-1 objects are
// taken at most K-1 at a time. U is symmetric (U(n,k)==U(k,n)), so the row is
// indexed by min(n,k) and the column by max(n,k), matching the C macro
// CELT_PVQ_U(_n,_k) = CELT_PVQ_U_ROW[IMIN(_n,_k)][IMAX(_n,_k)] (cwrs.c:200).
func celtPVQU(n, k int) uint32 {
	lo, hi := n, k
	if k < n {
		lo, hi = k, n
	}
	return celtPVQUData[celtPVQURowOffset[lo]+hi]
}

// celtPVQV returns V(N,K) = U(N,K)+U(N,K+1), the number of PVQ codewords for a
// band of size N with K pulses. (cwrs.c:203)
func celtPVQV(n, k int) uint32 {
	return celtPVQU(n, k) + celtPVQU(n, k+1)
}

// PVQV exposes V(N,K) for the differential test (codeword-count cross-check and
// range for a raw index). Only valid for table-representable (N,K).
func PVQV(n, k int) uint32 { return celtPVQV(n, k) }

// pvqURow returns row _n of U() as a slice, mirroring the C pointer
// row = CELT_PVQ_U_ROW[_n]; row[j] is then U(_n,j) for j >= _n. (cwrs.c:479)
func pvqURow(n int) []uint32 {
	return celtPVQUData[celtPVQURowOffset[n]:]
}

// cwrsi returns the _i'th combination of _k pulses over _n dimensions with
// associated signs, writing the pulse vector into y and returning yy = sum of
// squares of the pulses (the Ryy energy). Direct transliteration of the
// non-SMALL_FOOTPRINT cwrsi (cwrs.c:467). Requires _k>0 and _n>1.
func cwrsi(n, k int, i uint32, y []int32) int32 {
	var p uint32
	var s int
	var k0 int
	var val int16
	var yy int32
	j := 0
	for n > 2 {
		var q uint32
		// Lots of pulses case:
		if k >= n {
			row := pvqURow(n)
			// Are the pulses in this dimension negative?
			p = row[k+1]
			s = -b2i(i >= p)
			i -= p & uint32(s)
			// Count how many pulses were placed in this dimension.
			k0 = k
			q = row[n]
			if q > i {
				// celt_sig_assert(p>q)
				k = n
				for {
					k--
					p = celtPVQUData[celtPVQURowOffset[k]+n]
					if p <= i {
						break
					}
				}
			} else {
				for p = row[k]; p > i; p = row[k] {
					k--
				}
			}
			i -= p
			val = int16((k0 - k + s) ^ s)
			y[j] = int32(val)
			j++
			yy = fixedmath.MAC16_16(yy, val, val)
		} else {
			// Lots of dimensions case:
			// Are there any pulses in this dimension at all?
			p = celtPVQUData[celtPVQURowOffset[k]+n]
			q = celtPVQUData[celtPVQURowOffset[k+1]+n]
			if p <= i && i < q {
				i -= p
				y[j] = 0
				j++
			} else {
				// Are the pulses in this dimension negative?
				s = -b2i(i >= q)
				i -= q & uint32(s)
				// Count how many pulses were placed in this dimension.
				k0 = k
				for {
					k--
					p = celtPVQUData[celtPVQURowOffset[k]+n]
					if p <= i {
						break
					}
				}
				i -= p
				val = int16((k0 - k + s) ^ s)
				y[j] = int32(val)
				j++
				yy = fixedmath.MAC16_16(yy, val, val)
			}
		}
		n--
	}
	// _n==2
	p = uint32(2*k + 1)
	s = -b2i(i >= p)
	i -= p & uint32(s)
	k0 = k
	k = int((i + 1) >> 1)
	if k != 0 {
		i -= uint32(2*k - 1)
	}
	val = int16((k0 - k + s) ^ s)
	y[j] = int32(val)
	j++
	yy = fixedmath.MAC16_16(yy, val, val)
	// _n==1
	s = -int(i)
	val = int16((k + s) ^ s)
	y[j] = int32(val)
	yy = fixedmath.MAC16_16(yy, val, val)
	return yy
}

// DecodePulses reads a PVQ codeword index from the range decoder over the V(N,K)
// alphabet and decodes it into the pulse vector y (length N), returning yy = the
// sum of squares of the pulses. (cwrs.c:543). Requires K>0 and N>1.
func DecodePulses(y []int32, n, k int, dec *rangecoding.Decoder) int32 {
	return cwrsi(n, k, dec.DecUint(celtPVQV(n, k)), y)
}

// b2i is the Go form of the C idiom -(cond): it returns 1 when cond is true and 0
// otherwise, so callers can negate it to get the 0/-1 sign mask.
func b2i(cond bool) int {
	if cond {
		return 1
	}
	return 0
}
