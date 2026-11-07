package celt

import (
	"math"
	"math/rand/v2"
	"testing"
)

// deemphInput fills n samples of one input class. Channels are drawn from the
// same generator in turn, so a lane swap shows up as a mismatch.
func deemphInput(r *rand.Rand, class string, n int) []int32 {
	out := make([]int32, n)
	for i := range out {
		out[i] = deemphSample(r, class, i)
	}
	return out
}

func deemphSample(r *rand.Rand, class string, i int) int32 {
	switch class {
	case "random-int32":
		return int32(r.Uint32())
	case "within-sigsat":
		return int32(r.IntN(2*sigSat+1)) - sigSat
	case "small":
		return int32(r.IntN(1<<27+1)) - 1<<26
	case "near-sigsat":
		vals := [...]int32{sigSat, -sigSat, sigSat - 1, -(sigSat - 1), sigSat + 1, -(sigSat + 1), sigSat - 4096, -(sigSat - 4096)}
		return vals[r.IntN(len(vals))]
	default: // int32-extremes
		vals := [...]int32{math.MinInt32, math.MaxInt32, 0}
		return vals[(i+r.IntN(2))%len(vals)]
	}
}

// TestDeemphasisStereoMatchesPerChannel checks that the stereo dispatch of
// deemphasis (downsample 1, accum 0) equals two mono calls, which take the
// general per-channel loop, in PCM and in the carried filter memory.
func TestDeemphasisStereoMatchesPerChannel(t *testing.T) {
	classes := []string{"random-int32", "within-sigsat", "small", "near-sigsat", "int32-extremes"}
	coef := mode48000_960.preemph
	const frames = 4
	const sentinel = int16(0x5a5a)
	for _, n := range []int{120, 240, 480, 960} {
		for _, class := range classes {
			r := rand.New(rand.NewPCG(uint64(n), 105))
			var mem, wantMem [2]int32
			for c := range mem {
				mem[c] = deemphSample(r, class, c)
				wantMem[c] = mem[c]
			}
			for f := 0; f < frames; f++ {
				in := [][]int32{deemphInput(r, class, n), deemphInput(r, class, n)}
				got := make([]int16, 2*n+8)
				want := make([]int16, 2*n+8)
				for i := range got {
					got[i], want[i] = sentinel, sentinel
				}
				for c := 0; c < 2; c++ {
					mono := make([]int16, n)
					mc := [2]int32{wantMem[c]}
					deemphasis(in[c:c+1], mono, n, 1, 1, coef, &mc, 0, new(scratch))
					for j, v := range mono {
						want[2*j+c] = v
					}
					wantMem[c] = mc[0]
				}
				deemphasis(in, got, n, 2, 1, coef, &mem, 0, new(scratch))
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("N=%d class=%s frame=%d: pcm[%d] = %d, want %d", n, class, f, i, got[i], want[i])
					}
				}
				if mem != wantMem {
					t.Fatalf("N=%d class=%s frame=%d: mem = %v, want %v", n, class, f, mem, wantMem)
				}
			}
		}
	}
}

func BenchmarkDeemphasisStereo(b *testing.B) {
	coef := mode48000_960.preemph
	for _, n := range []int{120, 960} {
		b.Run(map[int]string{120: "N=120", 960: "N=960"}[n], func(b *testing.B) {
			r := rand.New(rand.NewPCG(uint64(n), 105))
			in := [][]int32{deemphInput(r, "within-sigsat", n), deemphInput(r, "within-sigsat", n)}
			pcm := make([]int16, 2*n)
			var mem [2]int32
			sc := new(scratch)
			b.ReportAllocs()
			for b.Loop() {
				deemphasis(in, pcm, n, 2, 1, coef, &mem, 0, sc)
			}
		})
	}
}
