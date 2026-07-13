package oggopus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"
)

// CONTAINER BENCHMARKS. The codec's own cost is measured in opus/bench_test.go (and
// that file records why the steady-state encode path is not yet 0 allocs/op: the
// transliterated CELT core heap-allocates the C's stack VARDECLs). What these measure
// is what the CONTAINER adds on top: the deinterleave, the page assembly, the CRC, and
// the per-stream setup.
//
// The four shapes here are the four ways a caller actually uses this package, and they
// answer different questions:
//
//   - EncodeFrame: steady-state Write of exactly one 20 ms frame. Isolates the
//     container's per-frame cost from stream setup. This is the one whose allocs/op is
//     meaningful, and the one to watch for regressions.
//   - EncodeStreamWrite: a whole 1 s stream through io.Copy-sized chunks, which is what
//     a caller piping PCM in actually does. The chunk size is deliberately NOT a frame
//     multiple, so the carry path (encoder.go:151) runs on every Write, as it does in
//     production.
//   - EncodeInterleaved: the pooled one-shot. This is the BirdNET-Go "many short clips"
//     workload the sync.Pool exists for, and the number that matters is allocs/op:
//     the pool's whole job is to keep per-clip allocation flat.
//   - DecodeStream: NewDecoder + WriteTo over a whole stream.
//
// Run them with
//
//	go test ./oggopus/ -run '^$' -bench . -benchmem

// benchSampleRate is 48 kHz: the coding rate, the granule rate, and the rate every
// real Ogg Opus stream uses.
const benchSampleRate = 48000

// benchClipSeconds is the length of the audio a stream-level benchmark op encodes or
// decodes. One second is 50 frames at the container's fixed 20 ms, enough that the
// per-stream setup (the two header pages) is a visible but not dominant share, which
// is exactly the ratio a short-clip workload sees.
const benchClipSeconds = 1

// benchAudio returns benchClipSeconds of deterministic interleaved little-endian
// int16 PCM: a swept tone plus a partial, decorrelated across channels so the stereo
// decisions are live and the encoder is not sitting on the silence fast path.
func benchAudio(channels int) []byte {
	n := benchSampleRate * benchClipSeconds
	out := make([]byte, 0, n*channels*2)
	for i := range n {
		t := float64(i)
		v := 0.45*math.Sin(2*math.Pi*(440+0.05*t)*t/benchSampleRate) +
			0.18*math.Sin(2*math.Pi*3100*t/benchSampleRate)
		for c := range channels {
			s := v
			if c == 1 {
				s = 0.85*v - 0.1*v*v
			}
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(s*22000)))
		}
	}
	return out
}

// benchStream encodes benchAudio into a complete Ogg Opus stream, the input for the
// decode benchmark.
func benchStream(b *testing.B, channels int) []byte {
	b.Helper()
	var buf bytes.Buffer
	cfg := Config{SampleRate: benchSampleRate, Channels: channels}
	if err := EncodeInterleaved(&buf, cfg, benchAudio(channels)); err != nil {
		b.Fatalf("EncodeInterleaved: %v", err)
	}
	return buf.Bytes()
}

// reportRealtime reports how many seconds of audio one second of CPU processes, given
// a per-op cost covering seconds of audio.
func reportRealtime(b *testing.B, seconds float64) {
	b.Helper()
	b.ReportMetric(seconds*1e9/float64(b.Elapsed().Nanoseconds())*float64(b.N), "x_realtime")
}

// BenchmarkEncodeFrame measures the steady-state per-frame cost: one Write of exactly
// one 20 ms frame into a long-lived Encoder, with no stream setup in the loop.
func BenchmarkEncodeFrame(b *testing.B) {
	for _, ch := range []int{1, 2} {
		b.Run(fmt.Sprintf("ch%d", ch), func(b *testing.B) {
			frameLen := benchSampleRate / (1000 / frameDurationMS) // 960 samples
			frameBytes := frameLen * ch * 2
			audio := benchAudio(ch)
			nFrames := len(audio) / frameBytes

			e, err := NewEncoder(io.Discard, Config{SampleRate: benchSampleRate, Channels: ch})
			if err != nil {
				b.Fatalf("NewEncoder: %v", err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(frameBytes))
			i := 0
			for b.Loop() {
				off := (i % nFrames) * frameBytes
				if _, err := e.Write(audio[off : off+frameBytes]); err != nil {
					b.Fatalf("Write: %v", err)
				}
				i++
			}
			reportRealtime(b, float64(frameDurationMS)/1000)
		})
	}
}

// BenchmarkEncodeStreamWrite measures a whole stream, headers to Close, fed in chunks
// that are NOT frame multiples so the carry path runs on every Write. That is what an
// io.Copy from a file or a network reader actually does, and it is the path a
// frame-aligned benchmark would never touch.
func BenchmarkEncodeStreamWrite(b *testing.B) {
	for _, ch := range []int{1, 2} {
		b.Run(fmt.Sprintf("ch%d", ch), func(b *testing.B) {
			audio := benchAudio(ch)
			const chunk = 4096 // io.Copy's default buffer; not a multiple of any frame

			b.ReportAllocs()
			b.SetBytes(int64(len(audio)))
			for b.Loop() {
				e, err := NewEncoder(io.Discard, Config{SampleRate: benchSampleRate, Channels: ch})
				if err != nil {
					b.Fatalf("NewEncoder: %v", err)
				}
				for off := 0; off < len(audio); off += chunk {
					if _, err := e.Write(audio[off:min(off+chunk, len(audio))]); err != nil {
						b.Fatalf("Write: %v", err)
					}
				}
				if err := e.Close(); err != nil {
					b.Fatalf("Close: %v", err)
				}
			}
			reportRealtime(b, benchClipSeconds)
		})
	}
}

// BenchmarkEncodeInterleaved measures the pooled one-shot: the "many short clips"
// workload EncodeInterleaved's sync.Pool exists for. Its allocs/op is the pool's
// scorecard.
func BenchmarkEncodeInterleaved(b *testing.B) {
	for _, ch := range []int{1, 2} {
		b.Run(fmt.Sprintf("ch%d", ch), func(b *testing.B) {
			audio := benchAudio(ch)
			cfg := Config{SampleRate: benchSampleRate, Channels: ch}

			b.ReportAllocs()
			b.SetBytes(int64(len(audio)))
			for b.Loop() {
				if err := EncodeInterleaved(io.Discard, cfg, audio); err != nil {
					b.Fatalf("EncodeInterleaved: %v", err)
				}
			}
			reportRealtime(b, benchClipSeconds)
		})
	}
}

// BenchmarkDecodeStream measures a whole-stream decode: header parse, page reassembly,
// CRC, packet decode, pre-skip drop and end-trim, straight to a discard sink.
func BenchmarkDecodeStream(b *testing.B) {
	for _, ch := range []int{1, 2} {
		b.Run(fmt.Sprintf("ch%d", ch), func(b *testing.B) {
			stream := benchStream(b, ch)

			b.ReportAllocs()
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				d, err := NewDecoder(bytes.NewReader(stream))
				if err != nil {
					b.Fatalf("NewDecoder: %v", err)
				}
				if _, err := d.WriteTo(io.Discard); err != nil {
					b.Fatalf("WriteTo: %v", err)
				}
			}
			reportRealtime(b, benchClipSeconds)
		})
	}
}
