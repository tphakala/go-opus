package opus

import (
	"fmt"
	"slices"
	"testing"
)

// TestDecoderScratchCarryover proves decoded PCM is independent of what the
// decoder-held scratch pool carried before: a fresh decoder and a decoder that
// first decoded a different stream and was then Reset must produce byte-identical
// PCM. Reset restores every piece of decoder STATE but deliberately leaves the
// pooled scratch untouched, so any pooled buffer read before it is written this
// frame shows up here as a diff. This is the zeroing contract of
// internal/celt/scratch.go as a black-box regression test. The NORMAL build is
// the discriminating one: under -tags poison alloc re-stamps every window with
// 0x5A for both decoders, so the two sides see identical carryover and cannot
// diverge; the poison tag earns its keep in the conformance and differential
// suites instead. The 24 kHz case decodes with downsample=2, reaching the
// deemphasis scratch that 48 kHz decoding never allocates.
//
// The dirty stream MUST use a frame size >= the real stream's: alloc reallocates
// (freshly zeroed) when a call asks for more than any previous one, so a smaller
// dirty frame would zero every N-proportional buffer on the first real frame,
// erasing the carried values exactly when they matter.
func TestDecoderScratchCarryover(t *testing.T) {
	for _, ch := range []int{1, 2} {
		for _, rate := range []int{48000, 24000} {
			t.Run(fmt.Sprintf("ch%d/%dHz", ch, rate), func(t *testing.T) {
				const nFrames = 8
				enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: 64000 * ch})
				if err != nil {
					t.Fatalf("NewEncoder: %v", err)
				}
				pkts := make([][]byte, nFrames)
				for i := range pkts {
					buf := make([]byte, 1500)
					n, err := enc.Encode(encTestPCM(960, ch, 48000, i), buf)
					if err != nil {
						t.Fatalf("Encode frame %d: %v", i, err)
					}
					pkts[i] = buf[:n]
				}

				// A second stream with different content (same 960-sample frame
				// size; see the doc comment for why it must not be smaller), used
				// only to leave its values behind in the reused decoder's pool.
				dirtyEnc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: ch, Bitrate: 64000 * ch})
				if err != nil {
					t.Fatalf("NewEncoder(dirty): %v", err)
				}
				dirtyPkts := make([][]byte, 4)
				for i := range dirtyPkts {
					buf := make([]byte, 1500)
					n, err := dirtyEnc.Encode(encTestPCM(960, ch, 48000, i+100), buf)
					if err != nil {
						t.Fatalf("Encode dirty frame %d: %v", i, err)
					}
					dirtyPkts[i] = buf[:n]
				}

				fresh, err := NewDecoder(rate, ch)
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				reused, err := NewDecoder(rate, ch)
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				dirtyOut := make([]int16, rate/50*ch)
				for i, p := range dirtyPkts {
					if _, err := reused.Decode(p, dirtyOut); err != nil {
						t.Fatalf("Decode dirty frame %d: %v", i, err)
					}
				}
				reused.Reset()

				a := make([]int16, rate/50*ch)
				b := make([]int16, rate/50*ch)
				for i, p := range pkts {
					na, err := fresh.Decode(p, a)
					if err != nil {
						t.Fatalf("fresh Decode frame %d: %v", i, err)
					}
					nb, err := reused.Decode(p, b)
					if err != nil {
						t.Fatalf("reused Decode frame %d: %v", i, err)
					}
					if na != nb {
						t.Fatalf("frame %d: sample count diverges: fresh %d, reused %d", i, na, nb)
					}
					if !slices.Equal(a[:na*ch], b[:nb*ch]) {
						t.Fatalf("frame %d: PCM diverges between a fresh decoder and a reused-scratch decoder; a pooled buffer is read before it is written", i)
					}
				}
			})
		}
	}
}
