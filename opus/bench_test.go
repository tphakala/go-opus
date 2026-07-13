package opus

import (
	"fmt"
	"testing"
)

// CODEC BENCHMARKS. plan.md test-strategy 9 asked for these from phase 1 on, and
// until CP10 the only Benchmarks in the repo were in internal/rangecoding and
// internal/packet: nothing measured the codec itself.
//
// What they measure, and why each dimension is here:
//
//   - ns/op per FRAME, for the four frame durations the encoder codes. Cost does not
//     scale linearly with the frame length (the MDCT is N log N, the per-frame
//     overheads are fixed), so a 20 ms number tells you nothing about the 2.5 ms one,
//     and 2.5 ms frames are eight times as many calls per second of audio.
//   - x_realtime, reported alongside: ns/op is only meaningful against the frame's own
//     duration. A 20 ms frame in 200 us is 100x realtime; the same 200 us on a 2.5 ms
//     frame is 12.5x. This is the number a caller actually budgets against.
//   - allocs/op on the STEADY-STATE path, i.e. with the Encoder and the output buffer
//     built outside the loop, which is how a real caller uses them. The API is shaped
//     for zero: Encode takes a caller-owned []byte and Decode a caller-owned []int16.
//     Whatever this reports is the honest per-frame allocation cost of the port.
//
// These are benchmarks, not gates: they assert nothing and they are not run by CI's
// test job (go test runs no benchmarks without -bench). Run them with
//
//	go test ./opus/ -run '^$' -bench . -benchmem
//
// ---------------------------------------------------------------------------
// THE 0-ALLOC BASELINE IS NOT MET, AND THIS IS WHY. (measured at CP10, M4 Pro,
// go1.26, 48 kHz, 64 kbps/channel)
//
//	BenchmarkEncode/ch1/20ms      91 us/op    64 kB/op   151 allocs/op    220x realtime
//	BenchmarkEncode/ch2/20ms     221 us/op   145 kB/op   501 allocs/op     90x realtime
//	BenchmarkDecode/ch2/20ms      54 us/op    38 kB/op    93 allocs/op    370x realtime
//
// The public API contributes NONE of it: Encode writes into the caller's []byte and
// Decode into the caller's []int16, and the wrappers allocate nothing per call. Every
// one of those allocations is a C `VARDECL(...)` / `ALLOC(...)` scratch array inside
// the transliterated CELT core, which in C is a VLA or an alloca on the stack (free,
// and reclaimed by the frame pop) and in the Go port is a `make([]T, n)` on the heap.
// An alloc profile of ch2/20ms attributes them to exactly those sites:
//
//	op_pvq_search (celt/vq.c)              55% of objects   (y, iy, signx)
//	alg_quant / alg_unquant                11%              (y)
//	de/interleave_hadamard                 19%              (tmp)
//	celt_encode_with_ec's own VARDECLs      4% of objects, 20% of bytes
//	run_prefilter, clt_mdct_forward, quant_all_bands, tf_analysis: the rest
//
// This is the deferral plan.md made deliberately: the transliteration keeps the C's
// shape so the port stays diffable against libopus v1.6.1, and hoisting these into
// per-encoder scratch owned by the Encoder struct changes the shape of every function
// that declares one. It is a PERF-PHASE change, not a CP10 one, and doing it here
// would trade the property the whole project is built on (bit-exactness, verified by
// diffing against the C) for a number nobody is yet blocked by: at 90x realtime for
// 20 ms stereo, an hour of audio encodes in 40 seconds.
//
// NOTED FOR THE PERF PHASE, in the order the profile ranks them: op_pvq_search's three
// arrays, alg_quant's y, the Hadamard tmp, and celt_encode_with_ec's frame-sized
// VARDECLs. All four are per-call scratch whose size is bounded by the mode, so all
// four can become fields on the Encoder/Decoder struct with no change to the
// arithmetic, which is what keeps the change safe: the packets must not move by a
// single byte, and the differential gate is what will say so.
// ---------------------------------------------------------------------------

// benchRate is the coding rate the benchmarks run at. 48 kHz is the rate the whole
// gate corpus and every real Ogg Opus stream uses; the other four rates run the same
// code with a smaller N, so they would add rows without adding an insight.
const benchRate = 48000

// benchFrames is the number of distinct input frames cycled through a benchmark. The
// encoder is a cross-frame state machine (the delay ring, the energy history, the VBR
// reservoir), so feeding it ONE frame over and over would settle it into a degenerate
// steady state that flatters the transient, prefilter and dynalloc paths. Cycling a
// few dozen frames of a real signal keeps those paths live. It is also why the PCM is
// built up front: generating it inside the loop would measure math.Sin.
const benchFrames = 32

// benchDurations are the four frame durations this release codes, named by the
// divisor that turns a sample rate into a frame length.
var benchDurations = []struct {
	name string
	div  int // frameSize = SampleRate / div
}{
	{"2.5ms", 400},
	{"5ms", 200},
	{"10ms", 100},
	{"20ms", 50},
}

// benchPCM builds benchFrames frames of the same deterministic, non-degenerate signal
// the correctness tests use.
func benchPCM(frameSize, channels int) [][]int16 {
	frames := make([][]int16, benchFrames)
	for i := range frames {
		frames[i] = encTestPCM(frameSize, channels, benchRate, i)
	}
	return frames
}

// reportRealtime reports how many seconds of audio one second of CPU encodes or
// decodes, given the per-op cost of one frame of frameSize samples.
func reportRealtime(b *testing.B, frameSize int) {
	b.Helper()
	frameNS := float64(frameSize) / benchRate * 1e9
	b.ReportMetric(frameNS/float64(b.Elapsed().Nanoseconds())*float64(b.N), "x_realtime")
}

// BenchmarkEncode measures one opus_encode call: the steady-state encode path, with
// the Encoder and the packet buffer owned by the caller, which is the shape the API
// is built for and the only shape whose allocs/op is meaningful.
func BenchmarkEncode(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div
				enc, err := NewEncoder(EncoderConfig{
					SampleRate: benchRate,
					Channels:   ch,
					Bitrate:    64000 * ch,
				})
				if err != nil {
					b.Fatalf("NewEncoder: %v", err)
				}
				frames := benchPCM(frameSize, ch)
				buf := make([]byte, 1500)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2)) // input PCM bytes per op
				i := 0
				for b.Loop() {
					if _, err := enc.Encode(frames[i%benchFrames], buf); err != nil {
						b.Fatalf("Encode: %v", err)
					}
					i++
				}
				reportRealtime(b, frameSize)
			})
		}
	}
}

// BenchmarkDecode measures one opus_decode call on packets this encoder produced, so
// the decode path exercised is the one a go-opus stream actually contains. The
// Decoder and the PCM buffer are built outside the loop, as a real caller builds them.
func BenchmarkDecode(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div

				enc, err := NewEncoder(EncoderConfig{
					SampleRate: benchRate,
					Channels:   ch,
					Bitrate:    64000 * ch,
				})
				if err != nil {
					b.Fatalf("NewEncoder: %v", err)
				}
				frames := benchPCM(frameSize, ch)
				pkts := make([][]byte, benchFrames)
				for i, pcm := range frames {
					buf := make([]byte, 1500)
					n, err := enc.Encode(pcm, buf)
					if err != nil {
						b.Fatalf("Encode: %v", err)
					}
					pkts[i] = buf[:n]
				}

				dec, err := NewDecoder(benchRate, ch)
				if err != nil {
					b.Fatalf("NewDecoder: %v", err)
				}
				pcm := make([]int16, frameSize*ch)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2)) // output PCM bytes per op
				i := 0
				for b.Loop() {
					if _, err := dec.Decode(pkts[i%benchFrames], pcm); err != nil {
						b.Fatalf("Decode: %v", err)
					}
					i++
				}
				reportRealtime(b, frameSize)
			})
		}
	}
}
