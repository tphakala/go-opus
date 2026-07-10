//go:build refc

package oracle

import (
	"bytes"
	"math"
	"math/rand"
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This differential test pins the pure-Go CELT PVQ ENCODE path (internal/celt
// cwrs.go / vq.go: EncodePulses, Icwrs, AlgQuant, StereoItheta) to the pinned
// libopus C oracle. op_pvq_search_c and icwrs are static in libopus, so they are
// validated transitively: AlgQuant's emitted bytes, collapse mask, coded pulse
// vector, and resynth output are compared byte/element exact against alg_quant;
// encode_pulses byte-exactness plus the decoded index pin icwrs.

// goEncodePulses runs the Go celt.EncodePulses over y (length n, k pulses) into a
// fresh vqencBufBytes buffer and returns the finalized packet.
func goEncodePulses(y []int32, n, k int) []byte {
	buf := make([]byte, vqencBufBytes)
	var enc rangecoding.Encoder
	enc.Init(buf)
	celt.EncodePulses(y, n, k, &enc)
	enc.EncDone()
	return buf
}

// goDecodePulses decodes packet with the Go celt.DecodePulses into a length-n
// pulse vector.
func goDecodePulses(packet []byte, n, k int) []int32 {
	y := make([]int32, n)
	var dec rangecoding.Decoder
	dec.Init(packet)
	celt.DecodePulses(y, n, k, &dec)
	return y
}

// int32SliceEqual reports whether a and b are element-wise equal.
func int32SliceEqual(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertEncodePulses checks Go vs C encode_pulses bit-exactly for the pulse
// vector y (length n, k pulses): byte-identical packets, identical icwrs index,
// and a Go encode->decode round-trip back to y.
func assertEncodePulses(t *testing.T, y []int32, n, k int) {
	t.Helper()
	gb := goEncodePulses(y, n, k)
	cb := cPVQEncodePulses(y, n, k)
	if !bytes.Equal(gb, cb) {
		t.Fatalf("encode_pulses bytes differ N=%d K=%d y=%v\n go=%x\n  c=%x", n, k, y, gb, cb)
	}
	ft := celt.PVQV(n, k)
	gi := celt.Icwrs(n, y)
	ci := cPVQIcwrs(y, n, k, ft)
	if gi != ci {
		t.Fatalf("icwrs index differs N=%d K=%d y=%v go=%d c=%d (ft=%d)", n, k, y, gi, ci, ft)
	}
	if rt := goDecodePulses(gb, n, k); !int32SliceEqual(rt, y) {
		t.Fatalf("encode->decode round-trip differs N=%d K=%d\n want=%v\n  got=%v", n, k, y, rt)
	}
}

// genPulseVectors enumerates every N-dimensional signed pulse vector whose sum of
// absolute values is exactly k (there are V(n,k) of them), calling emit on each.
// emit must not retain the slice (it is reused between calls).
func genPulseVectors(n, k int, emit func([]int32)) {
	y := make([]int32, n)
	var rec func(pos, left int)
	rec = func(pos, left int) {
		if pos == n-1 {
			if left == 0 {
				y[pos] = 0
				emit(y)
			} else {
				y[pos] = int32(left)
				emit(y)
				y[pos] = int32(-left)
				emit(y)
			}
			return
		}
		for a := 0; a <= left; a++ {
			if a == 0 {
				y[pos] = 0
				rec(pos+1, left)
			} else {
				y[pos] = int32(a)
				rec(pos+1, left-a)
				y[pos] = int32(-a)
				rec(pos+1, left-a)
			}
		}
	}
	rec(0, k)
}

// TestPVQEncodePulsesExhaustive enumerates EVERY pulse vector for small (N,K) and
// asserts Go encode_pulses / icwrs are bit-exact against C, that the enumeration
// count equals V(N,K), and that every codeword round-trips through the decoder.
func TestPVQEncodePulsesExhaustive(t *testing.T) {
	for _, n := range []int{2, 3, 4, 5, 6, 7, 8} {
		maxK := cPVQMaxK(n)
		for k := 1; k <= 5 && k <= maxK; k++ {
			v := celt.PVQV(n, k)
			if v == 0 || v > 12000 { // keep the exhaustive sweep bounded
				continue
			}
			count := uint32(0)
			genPulseVectors(n, k, func(y []int32) {
				count++
				assertEncodePulses(t, y, n, k)
			})
			if count != v {
				t.Fatalf("N=%d K=%d enumerated %d vectors, V(N,K)=%d", n, k, count, v)
			}
		}
	}
}

// randPulseVec builds a random signed pulse vector of length n with sum of
// absolute values exactly k.
func randPulseVec(r *rand.Rand, n, k int) []int32 {
	y := make([]int32, n)
	for p := 0; p < k; p++ {
		y[r.Intn(n)]++
	}
	for i := range y {
		if y[i] != 0 && r.Intn(2) == 1 {
			y[i] = -y[i]
		}
	}
	return y
}

// boundaryPulseVecs returns extreme pulse vectors for (n,k): all mass on the
// first / last coordinate (both signs) and an evenly spread alternating-sign
// vector. These stress the U(N,K) index accumulation at its range extremes.
func boundaryPulseVecs(n, k int) [][]int32 {
	var out [][]int32
	for _, sign := range []int32{1, -1} {
		first := make([]int32, n)
		first[0] = sign * int32(k)
		out = append(out, first)
		last := make([]int32, n)
		last[n-1] = sign * int32(k)
		out = append(out, last)
	}
	flat := make([]int32, n)
	left := k
	for i := 0; i < n && left > 0; i++ {
		q := (left + (n - i) - 1) / (n - i) // ceil split
		if i%2 == 1 {
			q = -q
		}
		flat[i] = int32(q)
		if q < 0 {
			left += int(q)
		} else {
			left -= int(q)
		}
	}
	out = append(out, flat)
	return out
}

// TestPVQEncodePulsesRandomBoundary exercises encode_pulses / icwrs across the
// full band-size grid, including the largest table-valid K where V(N,K) is close
// to 2^32 (the u32 overflow corners of the icwrs accumulation).
func TestPVQEncodePulsesRandomBoundary(t *testing.T) {
	r := rand.New(rand.NewSource(0x5C0DE))
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		if maxK <= 0 {
			continue
		}
		ks := sampleKs(maxK)
		// Always include the cap K (largest V(N,K)) for the overflow corner.
		for _, k := range ks {
			for _, y := range boundaryPulseVecs(n, k) {
				assertEncodePulses(t, y, n, k)
			}
			for iter := 0; iter < 8; iter++ {
				assertEncodePulses(t, randPulseVec(r, n, k), n, k)
			}
		}
	}
}

// TestPVQEncodePulsesOverflowCorners targets (N,K) whose V(N,K) sits just under
// 2^32, so the icwrs partial-sum arithmetic runs against the top of the u32
// range. N=6,K=95 gives V ~= 4.13e9 (0xF6400B34).
func TestPVQEncodePulsesOverflowCorners(t *testing.T) {
	r := rand.New(rand.NewSource(0xF6400B34))
	type nk struct{ n, k int }
	for _, c := range []nk{{6, 95}, {6, 94}, {7, 53}, {8, 36}, {5, 175}, {4, 175}} {
		maxK := cPVQMaxK(c.n)
		if c.k > maxK {
			t.Fatalf("N=%d K=%d exceeds table cap %d", c.n, c.k, maxK)
		}
		v := celt.PVQV(c.n, c.k)
		if v != cPVQTrueV(c.n, c.k) || v == 0 {
			t.Fatalf("N=%d K=%d V mismatch go=%d c=%d", c.n, c.k, v, cPVQTrueV(c.n, c.k))
		}
		for _, y := range boundaryPulseVecs(c.n, c.k) {
			assertEncodePulses(t, y, c.n, c.k)
		}
		for iter := 0; iter < 64; iter++ {
			assertEncodePulses(t, randPulseVec(r, c.n, c.k), c.n, c.k)
		}
	}
}

// goAlgQuant runs the Go celt.AlgQuant over a copy of X (AlgQuant mutates its
// input), returning the finalized packet, the collapse mask, and the (resynth)
// reconstructed band.
func goAlgQuant(X []int32, n, k, spread, b int, gain int32, resynth int) (packet []byte, cm uint32, Xout []int32) {
	buf := make([]byte, vqencBufBytes)
	var enc rangecoding.Encoder
	enc.Init(buf)
	Xc := append([]int32(nil), X...)
	cm = celt.AlgQuant(Xc, n, k, spread, b, &enc, gain, resynth)
	enc.EncDone()
	return buf, cm, Xc
}

// algQuantVecs returns representative input bands X of length n, to exercise the
// op_pvq_search pre-search / silence / greedy branches: two random directions, a
// single unit pulse (energy fully concentrated), a flat band, silence, and two
// domain-edge spikes. All stay within the unit-norm celt_norm range the encoder
// feeds alg_quant (band norm below 1.0 = 2^NORM_SHIFT = 2^24): |X[i]| <= 2^20
// keeps the norm under 2^24 for every N<=176, and the single unit pulse hits
// exactly 2^24.
//
// The edge spikes are placed as a single component so the exp_rotation1 fixed
// >>10 scaledown lands right at the int16 lane edge (|X|>>10 rounds to 32767): a
// magnitude of 23,726,566 (the stereo_split/haar1 worst reachable value) and
// 33,553,919 (one below the 33,553,920 divergence threshold, where |X|>>10 would
// round to 32768 and wrap). Both signs, to pin both the safe edge and the sign
// restore. Anything at or above 33,553,920 diverges from the C MULT16_16
// truncation and is unreachable in the real encoder (see the AlgQuant comment).
func algQuantVecs(r *rand.Rand, n int) [][]int32 {
	var out [][]int32
	out = append(out, genVec(r, n), genVec(r, n))
	spike := make([]int32, n)
	spike[r.Intn(n)] = 1 << 24 // a unit pulse
	out = append(out, spike)
	flat := make([]int32, n)
	for i := range flat {
		flat[i] = 1 << 18
		if r.Intn(2) == 1 {
			flat[i] = -flat[i]
		}
	}
	out = append(out, flat)
	out = append(out, make([]int32, n)) // silence
	for _, mag := range []int32{23726566, 33553919} {
		for _, sign := range []int32{1, -1} {
			edge := make([]int32, n)
			edge[r.Intn(n)] = sign * mag
			out = append(out, edge)
		}
	}
	return out
}

// TestPVQAlgQuant validates AlgQuant (and thus op_pvq_search transitively) against
// C alg_quant across (N,K,spread,B,gain): identical emitted bytes, collapse mask,
// coded pulse vector, and reconstructed unit-norm output (resynth=1).
func TestPVQAlgQuant(t *testing.T) {
	r := rand.New(rand.NewSource(0xA16))
	gains := []int32{2147483647, 1073741824} // Q31ONE and 0.5
	for _, n := range pvqNs {
		maxK := cPVQMaxK(n)
		if maxK <= 0 {
			continue
		}
		for _, k := range sampleKs(maxK) {
			for _, spread := range pvqSpreads {
				for _, b := range pvqBs(n) {
					for _, gain := range gains {
						for _, X := range algQuantVecs(r, n) {
							// resynth=0 is the path the real encoder mostly uses (bytes only);
							// resynth=1 additionally reconstructs the band.
							for _, resynth := range []int{0, 1} {
								gb, gcm, gX := goAlgQuant(X, n, k, spread, b, gain, resynth)
								cb, ccm, ciy, cX := cPVQAlgQuantEnc(X, n, k, spread, b, gain, resynth)
								if !bytes.Equal(gb, cb) {
									t.Fatalf("alg_quant bytes differ N=%d K=%d spread=%d B=%d gain=%d resynth=%d\n go=%x\n  c=%x",
										n, k, spread, b, gain, resynth, gb, cb)
								}
								if gcm != ccm {
									t.Fatalf("alg_quant collapse mask differs N=%d K=%d spread=%d B=%d resynth=%d: go=%d c=%d",
										n, k, spread, b, resynth, gcm, ccm)
								}
								if giy := goDecodePulses(gb, n, k); !int32SliceEqual(giy, ciy) {
									t.Fatalf("alg_quant coded pulses differ N=%d K=%d spread=%d B=%d resynth=%d\n go=%v\n  c=%v",
										n, k, spread, b, resynth, giy, ciy)
								}
								// The reconstructed band is only produced when resynth != 0.
								if resynth != 0 && !int32SliceEqual(gX, cX) {
									t.Fatalf("alg_quant resynth band differs N=%d K=%d spread=%d B=%d gain=%d\n go=%v\n  c=%v",
										n, k, spread, b, gain, gX, cX)
								}
							}
						}
					}
				}
			}
		}
	}
}

// genStereoVec builds a small-magnitude length-n celt_norm band (|X| < 2^18).
func genStereoVec(r *rand.Rand, n int) []int32 {
	x := make([]int32, n)
	for i := range x {
		x[i] = int32(r.Intn(1<<19) - (1 << 18))
	}
	return x
}

// genStereoUnitVec builds a length-n celt_norm band scaled to ~unit norm (2^24 =
// 2^NORM_SHIFT), the magnitude the encoder actually feeds stereo_itheta. This
// drives celt_sqrt32's k>=12 branch (mid/side energy ~2^27) and the larger
// EXTRACT16(m) range (|m| up to ~2^14) through the composition, while keeping the
// int32 mid/side accumulation well within range (a unit-norm band has
// sum((X+Y)^2) <= ~2^49, so sum(m^2) <= ~2^27).
func genStereoUnitVec(r *rand.Rand, n int) []int32 {
	d := make([]float64, n)
	var ss float64
	for i := range d {
		d[i] = float64(r.Intn(1<<16) - (1 << 15))
		ss += d[i] * d[i]
	}
	if ss == 0 {
		d[0], ss = 1, 1
	}
	scale := float64(1<<24) / math.Sqrt(ss)
	x := make([]int32, n)
	for i := range x {
		x[i] = int32(d[i] * scale)
	}
	return x
}

// TestStereoItheta validates StereoItheta against C stereo_itheta for both stereo
// modes over random small-magnitude and unit-norm mid/side bands across the full
// band-size grid (up to N=176), plus the all-zero corner.
func TestStereoItheta(t *testing.T) {
	r := rand.New(rand.NewSource(0x57e2e0))
	ns := []int{2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64, 88, 96, 128, 144, 176}
	for _, n := range ns {
		for _, stereo := range []int{0, 1} {
			for iter := 0; iter < 20; iter++ {
				for _, gen := range []func(*rand.Rand, int) []int32{genStereoVec, genStereoUnitVec} {
					X := gen(r, n)
					Y := gen(r, n)
					g := celt.StereoItheta(X, Y, stereo, n)
					c := cPVQStereoItheta(X, Y, stereo, n)
					if g != c {
						t.Fatalf("stereo_itheta differs N=%d stereo=%d: go=%d c=%d\n X=%v\n Y=%v",
							n, stereo, g, c, X, Y)
					}
				}
			}
			// All-zero corner: mid==side==0 -> itheta 0.
			z := make([]int32, n)
			if g, c := celt.StereoItheta(z, z, stereo, n), cPVQStereoItheta(z, z, stereo, n); g != c {
				t.Fatalf("stereo_itheta zero corner differs N=%d stereo=%d: go=%d c=%d", n, stereo, g, c)
			}
		}
	}
}
