package opus

import (
	"bytes"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"
)

// mallocsUntilZero runs prepare and then the measured function it returns, up to
// three times, and returns the exact number of heap allocations the measured
// function made in each attempt. It stops at the first attempt that allocates
// nothing. A decoder allocation is deterministic on a fresh decoder and repeats in
// every attempt, while the runtime occasionally attributes a stray malloc to the
// window, so one clean attempt is the pass condition. The two GCs empty any
// sync.Pool, so a retry cannot pass on what an earlier attempt allocated.
func mallocsUntilZero(prepare func() (measured func())) []uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var counts []uint64
	var ms runtime.MemStats
	for range 3 {
		runtime.GC()
		runtime.GC()
		measured := prepare()
		runtime.ReadMemStats(&ms)
		before := ms.Mallocs
		measured()
		runtime.ReadMemStats(&ms)
		counts = append(counts, ms.Mallocs-before)
		if ms.Mallocs == before {
			break
		}
	}
	return counts
}

// celtAllocPackets returns a CELT packet sequence that reaches every decode
// scratch buffer, with nil entries for lost frames: noise and impulse trains at
// every frame duration from 2.5 to 120 ms (each LM, transient and non-transient
// MDCTs, wide PVQ leaves), a 75 Hz tone whose loss runs periodic PLC with a long
// pitch, a recovery, a loss long enough to switch to noise PLC, and digital
// silence that DTX codes as TOC-only packets. The encoder is stereo, so a mono
// decoder sees two coded channels.
func celtAllocPackets(t *testing.T) [][]byte {
	t.Helper()
	const ch = 2
	enc, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: ch, DTX: true})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	r := rand.New(rand.NewPCG(5, 6))
	buf := make([]byte, 8000)
	pos := 0 // running sample position, so tones stay phase-continuous across frames
	var pkts [][]byte
	emit := func(n int, gen func(i int) int16) {
		pcm := make([]int16, n*ch)
		for i := range n {
			v := gen(pos + i)
			for c := range ch {
				pcm[i*ch+c] = v
			}
		}
		pos += n
		nb, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		pkts = append(pkts, bytes.Clone(buf[:nb]))
	}
	lose := func(n int) {
		for range n {
			pkts = append(pkts, nil)
		}
	}
	noise := func(int) int16 { return int16(r.IntN(60000) - 30000) }
	impulses := func(i int) int16 {
		if i%97 == 0 {
			return 30000
		}
		return 0
	}
	tone := func(hz float64) func(int) int16 {
		return func(i int) int16 { return int16(12000 * math.Sin(2*math.Pi*hz*float64(i)/48000)) }
	}
	silence := func(int) int16 { return 0 }
	frames := func(count, n int, gen func(int) int16) {
		for range count {
			emit(n, gen)
		}
	}

	for _, n := range []int{120, 240, 480, 960, 5760} {
		frames(10, n, noise)
		frames(5, n, impulses)
	}
	frames(20, 960, tone(75))
	lose(5) // periodic PLC; the 75 Hz pitch drives the PLC excitation to its longest
	frames(5, 960, tone(75))
	lose(60) // long enough for the PLC to switch to noise
	frames(10, 960, tone(440))
	frames(30, 960, silence) // DTX: TOC-only packets, decoded through the PLC
	frames(10, 960, tone(440))
	return pkts
}

// TestDecoderCELTAllocatesNothing pins that a CELT Decoder allocates nothing from
// its very first Decode: every CELT scratch buffer is sized for the worst case
// when the decoder is built, including the two-coded-channel buffers of a mono
// decoder fed stereo packets, the deemphasis buffer of a sub-48 kHz decoder, and
// the packet-loss concealment buffers. Before the fix this sequence took 89
// allocations at 48 kHz and 93 at 24 kHz.
func TestDecoderCELTAllocatesNothing(t *testing.T) {
	pkts := celtAllocPackets(t)
	for _, row := range []struct {
		name string
		rate int
	}{
		{"48kHz", 48000},
		{"24kHz", 24000}, // downsample 2 reaches the deemphasis buffer
	} {
		t.Run(row.name, func(t *testing.T) {
			rate := row.rate
			pcm := make([]int16, rate/1000*120) // 120 ms, mono
			lost := pcm[:rate/50]               // 20 ms of concealment per lost packet
			var decodeErr error
			counts := mallocsUntilZero(func() func() {
				d, err := NewDecoder(rate, 1)
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				return func() {
					for _, p := range pkts {
						out := pcm
						if p == nil {
							out = lost
						}
						if _, err := d.Decode(p, out); err != nil {
							decodeErr = err
							return
						}
					}
				}
			})
			if decodeErr != nil {
				t.Fatalf("Decode: %v", decodeErr)
			}
			if counts[len(counts)-1] != 0 {
				t.Errorf("decoding %d packets from the first Decode allocated %v times (one count per attempt), want 0 in at least one attempt", len(pkts), counts)
			}
		})
	}
}
