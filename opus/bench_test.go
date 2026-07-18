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
// THE 0-ALLOC BASELINE IS MET, ON BOTH PATHS. Every C `VARDECL(...)` / `ALLOC(...)`
// in the transliterated CELT core is a VLA or alloca on the C stack (free, reclaimed
// by the frame pop); the port first mapped each to a `make([]T, n)` on the heap. At
// CP10 that cost 501 allocs/op for a stereo 20 ms encode and 93 for the matching
// decode. Those are now pooled onto the Encoder and the Decoder respectively:
//
//	PR #29 (issue #21): the encode path. op_pvq_search's y/iy/signx, alg_quant's y,
//	                    the Hadamard tmp, and celt_encode_with_ec's own frame-sized
//	                    VARDECLs became fields on the Encoder's scratch.
//	This change (issue #5): the decode path. alg_unquant's iy, celt_decode_with_ec's
//	                    tf_res/cap/offsets/fine_quant/pulses/fine_priority/X/collapse_masks,
//	                    celt_synthesis's freq, clt_mdct_backward's f2, deemphasis's
//	                    scratch, the decode_mem/out_syn pointer arrays, and the packet
//	                    parse frame table all became decoder-held pooled storage.
//
// BenchmarkEncode and BenchmarkDecode now report 0 allocs/op on every config. Each
// benchmark makes one untimed warm-up pass over its corpus so the pools' first-use
// growth lands outside the timed loop; decode then reads 0 B/op at any -benchtime,
// while encode can still show a residual (up to a couple of kB/op at -benchtime=1x,
// gone at realistic N) because a few encode scratch sizes depend on cross-frame
// state (the VBR reservoir shifts band allocations across corpus cycles) and keep
// growing briefly past the warm-up pass. The
// public API never allocated (Encode writes the caller's []byte, Decode the caller's
// []int16). The pool is one buffer per codec instance, sized by (mode, channels,
// frameSize), so after the first largest frame nothing reallocates. See
// internal/celt/scratch.go.
//
// THE ZEROING CONTRACT is what keeps this bit-exact. A C stack ALLOC is uninitialised,
// a Go make() zeroes, and a pooled buffer carries the previous frame's values. Every
// pooled site was audited to write its entire read window before reading it (so all
// three collapse to the same behaviour); the handful of encode sites that do not are
// cleared explicitly. scratch.go documents the audit per site, and a build-tagged
// poison pass (`go test -tags poison`, and `-tags "refc poison"` against the C oracle)
// stamps every window with 0x5A to prove no site reads before it writes. The
// differential and conformance suites are the judge: the packets and the decoded PCM
// must not move by a single byte, and they do not.
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

				// One untimed pass over the whole corpus grows the pools to steady
				// state, so allocs/op reads 0 at any -benchtime instead of only when
				// a large N dilutes the first-use growth to zero. The full pass
				// matters: a few scratch sites are reached only on content-dependent
				// paths (transients, the prefilter), not by every frame.
				for _, f := range frames {
					if _, err := enc.Encode(f, buf); err != nil {
						b.Fatalf("Encode: %v", err)
					}
				}

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

				// Untimed full-corpus pool warm-up; see BenchmarkEncode.
				for _, p := range pkts {
					if _, err := dec.Decode(p, pcm); err != nil {
						b.Fatalf("Decode: %v", err)
					}
				}

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
