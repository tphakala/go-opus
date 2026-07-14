//go:build refc

package oracle

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

// GO vs C, HEAD TO HEAD. This file answers one question: how much does the Go port
// cost us against the identical C, and nothing else.
//
// It is the ONLY comparison in the repo that holds every variable but the language
// constant. libopus here is the pinned v1.6.1 oracle, compiled FIXED_POINT +
// DISABLE_FLOAT_API with NO SIMD (see oracle_cgo.go's CFLAGS): the same arithmetic,
// the same algorithm, the same scalar kernels the Go port transliterates. A
// comparison against a shipped libopus (opusenc, ffmpeg) measures something else
// entirely, because those are float builds with hand-written SSE/NEON, i.e. three
// variables at once. That number is worth having too, and it is not this one.
//
// WHY BOTH SIDES LIVE IN THIS FILE. The Go benchmarks in opus/bench_test.go already
// exist, and the obvious move is to run those, run a C benchmark here, and diff the
// two with benchstat. That comparison would be a lie by construction: two benchmarks
// in two packages, each with its own copy of the input generator and its own config
// literal, drift apart the first time someone edits one of them, and a benchmark
// pair fed different audio or different bitrates measures nothing. So both sides are
// here, in one file, driven from ONE pcm generator and ONE config, in one process.
//
// AND THE CONFIGS ARE ASSERTED EQUAL, NOT ASSUMED EQUAL. TestBenchConfigsMatch below
// encodes the benchmark's own frames through both encoders and requires the packets
// to come out BYTE-IDENTICAL. That is a real check and not a formality: the oracle's
// defaultOpusencCfg is complexity 9 with constrained VBR, while opus.EncoderConfig's
// zero value is complexity 10 with unconstrained VBR. Benchmarking those two against
// each other would have compared a cheaper encode to a dearer one and reported the
// difference as a language cost. Bit-exactness is what makes the check possible: if
// the two sides are doing the same work, the bytes match, and if they are not, they
// do not.
//
// Run:
//
//	go test -tags refc ./internal/reftest/oracle/ -run TestBenchConfigsMatch -v
//	go test -tags refc ./internal/reftest/oracle/ -run '^$' -bench 'Encode|Decode' -benchmem

// benchRate, benchFrames and benchDurations mirror opus/bench_test.go exactly; see
// its header for why the input cycles several dozen frames rather than repeating one
// (the encoder is a cross-frame state machine and a single repeated frame settles it
// into a degenerate steady state that flatters the transient and prefilter paths).
const (
	benchRate   = 48000
	benchFrames = 32
	benchMaxPkt = 1500
)

var benchDurations = []struct {
	name string
	div  int
}{
	{"2.5ms", 400},
	{"5ms", 200},
	{"10ms", 100},
	{"20ms", 50},
}

// benchBitrate is the rate both encoders are asked for. opus/bench_test.go uses the
// same 64 kbps per channel.
func benchBitrate(channels int) int { return 64000 * channels }

// benchPCMFrame is a verbatim copy of encTestPCM (opus/encoder_test.go), which is an
// unexported test helper in another package and so cannot be imported. The copy is
// safe because TestBenchConfigsMatch feeds THESE samples to both encoders and
// requires byte-identical packets: a transcription slip here would change the audio,
// which would change the packets, which would fail the test.
func benchPCMFrame(n, channels, fs, seed int) []int16 {
	pcm := make([]int16, n*channels)
	phase := float64(seed) * 0.37
	for i := range n {
		t := float64(i) + float64(seed)*float64(n)
		f := float64(fs)
		v := 0.45*math.Sin(2*math.Pi*(0.0092*f+0.05*t)*t/f+phase) +
			0.18*math.Sin(2*math.Pi*0.065*f*t/f) +
			0.07*math.Sin(2*math.Pi*0.23*f*t/f)
		for c := range channels {
			s := int16(v * 22000)
			if c == 1 {
				s = int16(v*19000) - int16(v*v*3000)
			}
			pcm[i*channels+c] = s
		}
	}
	return pcm
}

// benchPCM builds the benchFrames frames both sides encode.
func benchPCM(frameSize, channels int) [][]int16 {
	frames := make([][]int16, benchFrames)
	for i := range frames {
		frames[i] = benchPCMFrame(frameSize, channels, benchRate, i)
	}
	return frames
}

// newBenchGoEncoder is the Go encoder under test, configured exactly as
// opus/bench_test.go configures it. Complexity, VBR and the VBR constraint are left
// at EncoderConfig's zero values, which resolve to 10 / on / off.
func newBenchGoEncoder(tb testing.TB, channels int) *opus.Encoder {
	tb.Helper()
	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: benchRate,
		Channels:   channels,
		Bitrate:    benchBitrate(channels),
	})
	if err != nil {
		tb.Fatalf("opus.NewEncoder: %v", err)
	}
	return enc
}

// newBenchCEncoder is libopus, configured to match newBenchGoEncoder field for
// field. The three overrides on top of defaultOpusencCfg are the whole point: its
// frozen CP9 defaults are NOT opus.EncoderConfig's defaults.
func newBenchCEncoder(tb testing.TB, channels int) *cOpusencHandle {
	tb.Helper()
	cfg := defaultOpusencCfg(channels)
	cfg.Bitrate = benchBitrate(channels) // oracle default: OPUS_AUTO
	cfg.Complexity = 10                  // oracle default: 9
	cfg.VBRConstraint = 0                // oracle default: 1
	h, err := cOpusencCreate(cfg)
	if err != nil {
		tb.Fatalf("cOpusencCreate: %v", err)
	}
	return h
}

// TestBenchConfigsMatch is the fairness gate for every benchmark in this file. It is
// not a codec test; the codec is already gated. It tests THE BENCHMARK: that the two
// things being timed are doing the same work on the same audio.
func TestBenchConfigsMatch(t *testing.T) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			t.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(t *testing.T) {
				frameSize := benchRate / d.div
				frames := benchPCM(frameSize, ch)

				goEnc := newBenchGoEncoder(t, ch)
				cEnc := newBenchCEncoder(t, ch)
				defer cEnc.Close()

				goBuf := make([]byte, benchMaxPkt)
				cBuf := make([]byte, benchMaxPkt)

				for i, pcm := range frames {
					goN, err := goEnc.Encode(pcm, goBuf)
					if err != nil {
						t.Fatalf("frame %d: go Encode: %v", i, err)
					}
					cN := cEnc.EncodeInto(pcm, frameSize, cBuf)
					if cN < 0 {
						t.Fatalf("frame %d: c encode returned %d", i, cN)
					}
					if !bytes.Equal(goBuf[:goN], cBuf[:cN]) {
						t.Fatalf("frame %d: packets differ (go %d bytes, c %d bytes); the two "+
							"benchmarks are NOT encoding the same thing, so any timing "+
							"comparison between them is meaningless", i, goN, cN)
					}
				}
			})
		}
	}
}

// reportRealtime mirrors opus/bench_test.go: ns/op is only meaningful against the
// frame's own duration.
func reportRealtime(b *testing.B, frameSize int) {
	b.Helper()
	frameNS := float64(frameSize) / benchRate * 1e9
	b.ReportMetric(frameNS/float64(b.Elapsed().Nanoseconds())*float64(b.N), "x_realtime")
}

func BenchmarkEncodeGo(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div
				enc := newBenchGoEncoder(b, ch)
				frames := benchPCM(frameSize, ch)
				buf := make([]byte, benchMaxPkt)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2))
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

func BenchmarkEncodeC(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div
				enc := newBenchCEncoder(b, ch)
				defer enc.Close()
				frames := benchPCM(frameSize, ch)
				buf := make([]byte, benchMaxPkt)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2))
				i := 0
				for b.Loop() {
					if n := enc.EncodeInto(frames[i%benchFrames], frameSize, buf); n < 0 {
						b.Fatalf("EncodeInto: %d", n)
					}
					i++
				}
				reportRealtime(b, frameSize)
			})
		}
	}
}

// benchPackets encodes the benchmark frames once, so the decode benchmarks run on
// the packets a real go-opus stream contains.
func benchPackets(tb testing.TB, frameSize, channels int) [][]byte {
	tb.Helper()
	enc := newBenchGoEncoder(tb, channels)
	frames := benchPCM(frameSize, channels)
	pkts := make([][]byte, len(frames))
	for i, pcm := range frames {
		buf := make([]byte, benchMaxPkt)
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			tb.Fatalf("Encode: %v", err)
		}
		pkts[i] = buf[:n]
	}
	return pkts
}

func BenchmarkDecodeGo(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div
				pkts := benchPackets(b, frameSize, ch)

				dec, err := opus.NewDecoder(benchRate, ch)
				if err != nil {
					b.Fatalf("opus.NewDecoder: %v", err)
				}
				pcm := make([]int16, frameSize*ch)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2))
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

func BenchmarkDecodeC(b *testing.B) {
	for _, ch := range []int{1, 2} {
		for _, d := range benchDurations {
			b.Run(fmt.Sprintf("ch%d/%s", ch, d.name), func(b *testing.B) {
				frameSize := benchRate / d.div
				pkts := benchPackets(b, frameSize, ch)

				dec, err := NewDecoder(benchRate, ch)
				if err != nil {
					b.Fatalf("oracle.NewDecoder: %v", err)
				}
				defer dec.Close()
				pcm := make([]int16, frameSize*ch)

				b.ReportAllocs()
				b.SetBytes(int64(frameSize * ch * 2))
				i := 0
				for b.Loop() {
					if _, err := dec.DecodeInto(pkts[i%benchFrames], pcm); err != nil {
						b.Fatalf("DecodeInto: %v", err)
					}
					i++
				}
				reportRealtime(b, frameSize)
			})
		}
	}
}
