package celt

import "testing"

// TestPackedTwiddlesPrecomputed independently re-walks the opusFFTImpl fstride/m
// factorization and, for every radix-3/5 stage, asserts st.packedTw[stage][k] equals
// the expected strided twiddle gather computed inline here (NOT via the production
// packTwiddleRun, so an r/i swap or layout bug in that helper is caught too). It also
// asserts radix-2/4 stages and the unused radix-3 slots stay nil. This is the
// correctness half of dropping the process-global sync.Map: the init-time precompute
// must reproduce byte-for-byte what the old lazy gather produced, indexed by the same
// stage the driver passes back down. The 0-alloc contract is pinned separately by
// TestFFTCintAllocFree, and the end-to-end bit-exactness by TestFFTCintBitExact.
func TestPackedTwiddlesPrecomputed(t *testing.T) {
	// wantRun computes the expected [tr,ti,...] gather inline, independent of the
	// production packTwiddleRun helper, so this test is a genuine oracle for it.
	wantRun := func(stride, count int, tw []kissTwiddleCpx) []int16 {
		p := make([]int16, 2*count)
		for j := 0; j < count; j++ {
			c := tw[j*stride]
			p[2*j] = c.r
			p[2*j+1] = c.i
		}
		return p
	}

	for idx := 0; idx < 4; idx++ {
		st := mode48000_960.mdct.kfft[idx]

		var fstride [maxFactors]int
		shift := 0
		if st.shift > 0 {
			shift = st.shift
		}
		fstride[0] = 1
		L := 0
		var m, m2 int
		for {
			p := int(st.factors[2*L])
			m = int(st.factors[2*L+1])
			fstride[L+1] = fstride[L] * p
			L++
			if m == 1 {
				break
			}
		}
		m = int(st.factors[2*L-1])
		for i := L - 1; i >= 0; i-- {
			if i != 0 {
				m2 = int(st.factors[2*i-1])
			} else {
				m2 = 1
			}
			bf := fstride[i] << shift
			// check asserts packedTw[i][k] equals the inline expected gather.
			check := func(k, stride int) {
				got := st.packedTw[i][k]
				want := wantRun(stride, m, st.twiddles)
				if len(got) != len(want) {
					t.Fatalf("idx %d stage %d k %d: len got=%d want=%d", idx, i, k, len(got), len(want))
				}
				for j := range want {
					if got[j] != want[j] {
						t.Fatalf("idx %d stage %d k %d: run[%d] got=%d want=%d", idx, i, k, j, got[j], want[j])
					}
				}
			}
			// nilRun asserts an unused slot stays nil (the radix-2/4 and unused
			// radix-3 rows must not be populated).
			nilRun := func(k int) {
				if st.packedTw[i][k] != nil {
					t.Fatalf("idx %d stage %d (radix %d) k %d: want nil, got len %d",
						idx, i, st.factors[2*i], k, len(st.packedTw[i][k]))
				}
			}
			switch st.factors[2*i] {
			case 3:
				check(0, bf)
				check(1, 2*bf)
				nilRun(2)
				nilRun(3)
			case 5:
				check(0, bf)
				check(1, 2*bf)
				check(2, 3*bf)
				check(3, 4*bf)
			default: // radix 2/4 consume no packed twiddles
				nilRun(0)
				nilRun(1)
				nilRun(2)
				nilRun(3)
			}
			m = m2
		}
	}
}
