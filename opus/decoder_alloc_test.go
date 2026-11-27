package opus_test

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tphakala/go-opus/opus"
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

// requireDecodeAllocatesNothing builds a fresh rate Hz, ch-channel Decoder for
// each mallocsUntilZero attempt and fails t unless run decodes with it without
// an error and allocates nothing in at least one attempt. what names the decoded
// sequence in the failure message.
func requireDecodeAllocatesNothing(t *testing.T, rate, ch int, what string, run func(*opus.Decoder) error) {
	t.Helper()
	var runErr error
	counts := mallocsUntilZero(func() func() {
		d, err := opus.NewDecoder(rate, ch)
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		return func() { runErr = run(d) }
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if counts[len(counts)-1] != 0 {
		t.Errorf("decoding %s from the first Decode allocated %v times (one count per attempt), want 0 in at least one attempt", what, counts)
	}
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
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: ch, DTX: true})
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
			requireDecodeAllocatesNothing(t, rate, 1, fmt.Sprintf("%d packets", len(pkts)), func(d *opus.Decoder) error {
				for i, p := range pkts {
					out := pcm
					if p == nil {
						out = lost
					}
					if _, err := d.Decode(p, out); err != nil {
						return fmt.Errorf("packet %d Decode: %w", i, err)
					}
				}
				return nil
			})
		})
	}
}

// silkAllocStep is one call of the SILK and hybrid allocation sequence.
type silkAllocStep struct {
	packet []byte // nil or empty for a lost packet
	lossMS int    // concealed duration of a lost packet (20 or 5 ms); 0 decodes into the whole buffer
	fec    bool   // conceal the lost frame before packet from its in-band FEC first
}

// silkAllocSteps returns a fixed SILK, hybrid and CELT call sequence that reaches
// every per-frame buffer of the SILK decoder and of the mode-switching layer
// above it. SILK and hybrid packets are a valid code 0 TOC with a seeded random
// payload; CELT packets come from the encoder. Only fixed-bound IntN calls are
// used, so the bytes are the same on every architecture. Events that depend on
// random bits (voiced frames, LBRR flags, SILK-only redundancy and its
// direction) get at least 10 chances; a mode boundary that must not carry
// redundancy uses a SILK packet with a 2-byte frame, which is too short for the
// redundancy flag (opus_decoder.c ec_tell check) yet not a PLC marker.
//
// Which group reaches which path (checked with a coverage profile of this test;
// rerun that check after changing the seed or the groups):
//  1. 40 SILK packets: DecodeCore with voiced and unvoiced frames, DecodeFrame,
//     the API buffers of silk Decoder.Decode, the resamplers (LBRR frames occur).
//  2. Losses, an empty packet, a TOC-only (DTX) packet and a 5 ms loss: the SILK
//     PLC and CNG buffers and the 10 ms SILK buffer of a short decode.
//  3. (loss, SILK packet) pairs concealed with DecodeFEC: the LBRR decode and
//     the FEC completion in opus_decode_native.
//  4. (short SILK packet, CELT packet): SILK to CELT transition and its nested
//     SILK PLC.
//  5. (CELT packet, short SILK packet): non-redundant CELT to SILK transition and
//     its nested CELT PLC.
//  6. Long SILK packets: SILK-only redundancy in both directions.
//  7. Hybrid packets, then (hybrid packet, short SILK packet): hybrid decode and
//     the hybrid to SILK fade that decodes the static silence frame.
//  8. CELT, then hybrid (CELT to hybrid), two losses and one more SILK packet.
func silkAllocSteps(t *testing.T) []silkAllocStep {
	t.Helper()
	r := rand.New(rand.NewPCG(119, 1))
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	encBuf := make([]byte, 4000)
	celt := func() []byte {
		pcm := make([]int16, 960*2)
		for i := range pcm {
			pcm[i] = int16(r.IntN(20000) - 10000)
		}
		nb, err := enc.Encode(pcm, encBuf)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return bytes.Clone(encBuf[:nb])
	}
	// packet returns a code 0 packet with a TOC config in [cfgLo, cfgLo+cfgN)
	// (0..11 SILK NB/MB/WB at 10 to 60 ms, 12..15 hybrid), random stereo flag
	// and a random payload of minLen to maxLen bytes.
	packet := func(cfgLo, cfgN, minLen, maxLen int) []byte {
		cfg := cfgLo + r.IntN(cfgN)
		stereo := r.IntN(2)
		p := make([]byte, 1+minLen+r.IntN(maxLen-minLen+1))
		p[0] = byte(cfg<<3 | stereo<<2)
		for i := 1; i < len(p); i++ {
			p[i] = byte(r.IntN(256))
		}
		return p
	}
	silk := func(minLen, maxLen int) []byte { return packet(0, 12, minLen, maxLen) }
	hybrid := func(minLen, maxLen int) []byte { return packet(12, 4, minLen, maxLen) }
	shortSILK := func() []byte { return silk(2, 2) }

	var steps []silkAllocStep
	add := func(p []byte) { steps = append(steps, silkAllocStep{packet: p}) }
	lose := func(ms int) { steps = append(steps, silkAllocStep{lossMS: ms}) }

	for range 40 { // group 1
		add(silk(20, 80))
	}
	lose(20) // group 2
	lose(20)
	lose(20)
	steps = append(steps, silkAllocStep{packet: []byte{}, lossMS: 20})
	add(silk(0, 0)) // TOC only: DTX
	lose(5)
	for range 10 { // group 3
		steps = append(steps, silkAllocStep{packet: silk(20, 80), fec: true})
	}
	for range 5 { // group 4
		add(shortSILK())
		add(celt())
	}
	for range 5 { // group 5
		add(celt())
		add(shortSILK())
	}
	for range 10 { // group 6
		add(silk(120, 250))
	}
	for range 20 { // group 7
		add(hybrid(40, 200))
	}
	for range 5 {
		add(hybrid(40, 200))
		add(shortSILK())
	}
	for range 3 { // group 8
		add(celt())
	}
	for range 5 {
		add(hybrid(40, 200))
	}
	lose(20)
	lose(20)
	add(silk(20, 80))
	return steps
}

// TestDecoderSILKHybridAllocatesNothing pins that SILK and hybrid decoding
// allocates nothing from a fresh decoder's first Decode, including packet loss,
// short concealment, CNG, DTX, in-band FEC, SILK-only redundancy and every mode
// transition. The rows cover the largest buffers (48 kHz stereo, which also runs
// the IIR_FIR upsampler) and the down_FIR resampler at 12 kHz mono (stereo
// packets into a mono decoder) and at 8 kHz stereo. Before the fix this
// sequence took 2,447 to 2,845 allocations per row.
func TestDecoderSILKHybridAllocatesNothing(t *testing.T) {
	steps := silkAllocSteps(t)
	for _, row := range []struct {
		name     string
		rate, ch int
	}{
		{"48kHz_stereo", 48000, 2},
		{"12kHz_mono", 12000, 1},
		{"8kHz_stereo", 8000, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			rate, ch := row.rate, row.ch
			pcm := make([]int16, rate/1000*120*ch) // 120 ms
			fecPCM := pcm[:3*rate/50*ch]           // 60 ms covers any SILK packet
			requireDecodeAllocatesNothing(t, rate, ch, fmt.Sprintf("%d steps", len(steps)), func(d *opus.Decoder) error {
				for i, s := range steps {
					out := pcm
					if s.lossMS != 0 {
						out = pcm[:rate/1000*s.lossMS*ch]
					}
					if s.fec {
						if _, err := d.DecodeFEC(s.packet, fecPCM); err != nil {
							return fmt.Errorf("step %d DecodeFEC: %w", i, err)
						}
					}
					if _, err := d.Decode(s.packet, out); err != nil {
						return fmt.Errorf("step %d Decode: %w", i, err)
					}
				}
				return nil
			})
		})
	}
}

// TestDecoderRFCVectorsAllocateNothing pins that decoding the SILK, hybrid and
// mode-transition RFC vectors allocates nothing from a fresh decoder's first
// Decode. Real encoder output reaches paths that random payloads reach only by
// chance, hybrid-mode redundancy in particular (vectors 10 and 12). The
// CELT-only vectors 01, 07 and 11 are not decoded here;
// TestDecoderCELTAllocatesNothing pins CELT decoding on encoder-made packets.
func TestDecoderRFCVectorsAllocateNothing(t *testing.T) {
	if _, err := os.Stat(vectorsDir); err != nil {
		t.Skipf("test vectors not present (%v); run scripts/fetch-vectors.sh", err)
	}
	for _, name := range []string{"02", "03", "04", "05", "06", "08", "09", "10", "12"} {
		t.Run("vector"+name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(vectorsDir, "testvector"+name+".bit"))
			if err != nil {
				t.Fatalf("read vector: %v", err)
			}
			recs := readBitRecords(t, data)
			pcm := make([]int16, 5760*2) // 120 ms at 48 kHz stereo: max packet
			requireDecodeAllocatesNothing(t, 48000, 2, fmt.Sprintf("%d packets", len(recs)), func(d *opus.Decoder) error {
				for i, rec := range recs {
					if _, err := d.Decode(rec.packet, pcm); err != nil {
						return fmt.Errorf("packet %d Decode: %w", i, err)
					}
				}
				return nil
			})
		})
	}
}
