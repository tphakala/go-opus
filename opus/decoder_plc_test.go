package opus

import (
	"fmt"
	"hash/fnv"
	"slices"
	"testing"
)

// plcGolden holds the FNV-1a 64-bit hash of the full decoded output (primer,
// loss burst, recovery) of the PLC exercise below, keyed "ch%d/%dHz". These are
// the byte-exact concealment output of the frozen CELT decoder; they are the
// same on every platform because the port is fixed-point, and the same under the
// poison build tag because correct pooling overwrites every 0x5A stamp before it
// is read. Regenerate only on a deliberate, reviewed change to concealment
// output (log sum in the test body temporarily to read the new values).
var plcGolden = map[string]uint64{
	"ch1/48000Hz": 0xbece2c0d68f41143,
	"ch1/24000Hz": 0x8a6191975cf92135,
	"ch2/48000Hz": 0x6c0b71a7fb0413da,
	"ch2/24000Hz": 0xa64c2736239a259d,
}

// TestDecoderPLCScratchCarryover guards the packet-loss concealment (PLC) buffers
// that issue #17 moved off per-frame make() onto the decoder's pooled scratch:
// decExcBuf, decFirTmp, decLpPitchBuf and decEtmp, plus a reuse of the normal
// path's decX in the noise-PLC branch. The pooling contract (internal/celt/
// scratch.go) is that every element read on a concealed frame is first written on
// that same frame; a partial write would leak a previous frame's leftovers into
// output. The 12 RFC 6716 vectors contain no lost frames, so nothing else in the
// suite exercises celtDecodeLost at all.
//
// The exercise drives a decoder through primer -> 8 x 20 ms loss burst ->
// recovery. The burst crosses plc_duration>=40, so it runs the pitch-PLC branch
// on the first losses (celtPlcPitchSearch fills decLpPitchBuf; celtFir fills
// decFirTmp; the excitation fill and copy fill decExcBuf), the noise-PLC branch
// once loss is sustained (the decX reuse), and the prefilter-and-fold (decEtmp)
// that a concealed frame arms and a later frame consumes. The 24 kHz case adds
// the downsample path.
//
// Two independent checks run over that exercise:
//
//  1. Golden output. The full decoded PCM is hashed and compared to plcGolden.
//     This is the comprehensive read-before-write guard, and it earns its keep
//     under -tags poison: alloc restamps every pooled buffer with 0x5A on borrow,
//     so any element read before it is written this frame leaks 0x5A garbage and
//     the hash diverges from the golden. It covers every buffer above, including
//     the ones whose producer writes exactly its consumer's read window
//     (celtFir/decFirTmp, comb_filter/decEtmp, the LCG fill/decX), which a
//     cross-instance carryover check alone cannot discriminate.
//
//  2. Pool isolation. A fresh decoder (zeroed pool) and a decoder that first
//     concealed a burst and was then Reset (concealment leftovers in the pool)
//     are driven through the identical sequence and must produce byte-identical
//     PCM. Reset restores decoder STATE but deliberately leaves the pooled
//     scratch, so a buffer whose whole-array fill regresses (its filler stops
//     covering an index another path already dirtied, as in decExcBuf) shows up
//     as a divergence on the NORMAL build. Verified by sabotage: dropping the
//     last excBuf fill index fails at the first lost frame. The priming and
//     measured runs use the identical packet sequence so every pooled buffer
//     allocates at the same size in both, and the one variable-length buffer,
//     decFirTmp (excLength = IMIN(2*pitchIndex, maxPeriodC)), never reallocates
//     smaller and zero-fills the carryover away.
func TestDecoderPLCScratchCarryover(t *testing.T) {
	const (
		frame = 960 // 20 ms at 48 kHz
		nGood = 4
		nLost = 8
	)
	for _, ch := range []int{1, 2} {
		for _, rate := range []int{48000, 24000} {
			t.Run(fmt.Sprintf("ch%d/%dHz", ch, rate), func(t *testing.T) {
				enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: 64000 * ch})
				if err != nil {
					t.Fatalf("NewEncoder: %v", err)
				}
				good := make([][]byte, nGood)
				for i := range good {
					buf := make([]byte, 1500)
					n, err := enc.Encode(encTestPCM(frame, ch, 48000, i), buf)
					if err != nil {
						t.Fatalf("Encode: %v", err)
					}
					good[i] = buf[:n]
				}

				// out is 20 ms at the output rate; a nil packet requests PLC for
				// the frame duration len(out) implies.
				out := make([]int16, rate/50*ch)
				drive := func(dec *Decoder, record func(frame, n int, pcm []int16)) {
					idx := 0
					step := func(pkt []byte) {
						n, err := dec.Decode(pkt, out)
						if err != nil {
							t.Fatalf("frame %d: Decode: %v", idx, err)
						}
						record(idx, n, out)
						idx++
					}
					for _, p := range good {
						step(p) // prime the decoder state and pitch history
					}
					for range nLost {
						step(nil) // loss burst: pitch-PLC then noise-PLC
					}
					for _, p := range good {
						step(p) // recovery: the first frame consumes prefilter-and-fold
					}
				}

				// Leave concealment values behind in the reused pool, then Reset
				// (which restores decoder STATE but not the pooled scratch).
				reused, err := NewDecoder(rate, ch)
				if err != nil {
					t.Fatalf("NewDecoder(reused): %v", err)
				}
				drive(reused, func(int, int, []int16) {})
				reused.Reset()

				fresh, err := NewDecoder(rate, ch)
				if err != nil {
					t.Fatalf("NewDecoder(fresh): %v", err)
				}
				h := fnv.New64a()
				var freshFrames [][]int16
				drive(fresh, func(_, n int, pcm []int16) {
					s := pcm[:n*ch]
					freshFrames = append(freshFrames, slices.Clone(s))
					for _, v := range s {
						h.Write([]byte{byte(v), byte(uint16(v) >> 8)})
					}
				})

				// 1. Golden output (the poison-build read-before-write guard).
				sum := h.Sum64()
				key := fmt.Sprintf("ch%d/%dHz", ch, rate)
				if want, ok := plcGolden[key]; ok && want != 0 && sum != want {
					t.Fatalf("%s: PLC output hash %#016x, want %#016x; concealment output changed (or a poisoned pooled buffer leaked)", key, sum, want)
				}

				// 2. Pool isolation (fresh vs dirty-then-Reset, byte-identical).
				fi := 0
				drive(reused, func(frame, n int, pcm []int16) {
					want := freshFrames[fi]
					fi++
					if !slices.Equal(pcm[:n*ch], want) {
						t.Fatalf("frame %d: PCM diverges between a fresh decoder and a dirty-then-Reset decoder; a pooled PLC buffer is read before it is written", frame)
					}
				})
			})
		}
	}
}
