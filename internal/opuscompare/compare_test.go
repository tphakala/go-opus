package opuscompare

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// clip16 rounds and clamps a float sample into the int16 range.
func clip16(v float64) int16 {
	v = math.Round(v)
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}

// genStereo builds a deterministic, spectrally rich interleaved-stereo signal of
// n frames: a couple of sinusoids per channel plus low-level pseudo-random noise.
func genStereo(n int, seed int64) []int16 {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test fixture, not security-sensitive
	out := make([]int16, n*2)
	for i := 0; i < n; i++ {
		t := float64(i)
		base := 8000*math.Sin(2*math.Pi*440*t/48000) +
			4000*math.Sin(2*math.Pi*1200*t/48000)
		l := base + float64(r.Intn(1001)-500)
		rr := 0.9*base + float64(r.Intn(1001)-500)
		out[2*i] = clip16(l)
		out[2*i+1] = clip16(rr)
	}
	return out
}

// fixedNoise returns a reusable per-sample noise vector in [-1,1].
func fixedNoise(n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test fixture
	noise := make([]float64, n)
	for i := range noise {
		noise[i] = r.Float64()*2 - 1
	}
	return noise
}

// addScaledNoise returns src with amp*noise[i] added to each sample.
func addScaledNoise(src []int16, noise []float64, amp float64) []int16 {
	out := make([]int16, len(src))
	for i, v := range src {
		out[i] = clip16(float64(v) + amp*noise[i])
	}
	return out
}

func int16ToBytesLE(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

// TestIdenticalStereoPerfect: identical reference and test yield a perfect 100
// quality metric and a PASS, exactly as opus_compare does (re==1 => err==0 => Q==100).
func TestIdenticalStereoPerfect(t *testing.T) {
	ref := genStereo(4800, 1)
	test := append([]int16(nil), ref...)
	res, err := CompareInt16(ref, test, Config{Channels: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("identical signals must PASS, got Passed=false (Q=%v)", res.Quality)
	}
	if math.Abs(res.Quality-100) > 1e-9 {
		t.Fatalf("identical signals: Quality = %v, want 100", res.Quality)
	}
	if res.WeightedError != 0 {
		t.Fatalf("identical signals: WeightedError = %v, want 0", res.WeightedError)
	}
	if res.Frames != (4800-testWinSize+testWinStep)/testWinStep {
		t.Fatalf("Frames = %d, want %d", res.Frames, (4800-testWinSize+testWinStep)/testWinStep)
	}
}

// TestIdenticalMonoPerfect exercises the mono comparison path (Channels: 1),
// which downmixes the always-stereo reference. A stereo reference whose two
// channels are equal to the mono test yields a perfect score.
func TestIdenticalMonoPerfect(t *testing.T) {
	const n = 4800
	r := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test fixture
	mono := make([]int16, n)
	stereo := make([]int16, n*2)
	for i := 0; i < n; i++ {
		v := clip16(6000*math.Sin(2*math.Pi*300*float64(i)/48000) + float64(r.Intn(801)-400))
		mono[i] = v
		stereo[2*i] = v
		stereo[2*i+1] = v
	}
	res, err := CompareInt16(stereo, mono, Config{Channels: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Passed || math.Abs(res.Quality-100) > 1e-9 {
		t.Fatalf("mono identical: Quality = %v, Passed = %v, want 100/true", res.Quality, res.Passed)
	}
}

// TestDegradationMonotonic asserts more distortion lowers the quality metric.
// The same noise vector is scaled by increasing amplitudes, so the error
// spectrum scales monotonically and the metric must be non-increasing.
func TestDegradationMonotonic(t *testing.T) {
	ref := genStereo(4800, 2)
	noise := fixedNoise(len(ref), 99)
	amps := []float64{0, 20, 60, 150, 400, 1000}

	prev := math.Inf(1)
	var qualities []float64
	for _, amp := range amps {
		test := addScaledNoise(ref, noise, amp)
		res, err := CompareInt16(ref, test, Config{Channels: 2})
		if err != nil {
			t.Fatalf("amp=%v: unexpected error: %v", amp, err)
		}
		qualities = append(qualities, res.Quality)
		if res.Quality > prev+1e-9 {
			t.Fatalf("quality not monotonic: amp=%v gave Q=%v > previous %v (all=%v)",
				amp, res.Quality, prev, qualities)
		}
		prev = res.Quality
	}
	// amp=0 is identical -> perfect; the largest distortion must be strictly worse.
	if qualities[0] != 100 {
		t.Fatalf("amp=0 should be perfect, got %v", qualities[0])
	}
	if qualities[len(qualities)-1] >= qualities[1] {
		t.Fatalf("heaviest distortion Q=%v should be below light distortion Q=%v",
			qualities[len(qualities)-1], qualities[1])
	}
}

// TestLightDegradationPasses: a small amount of noise reduces quality below 100
// but still passes the RFC conformance threshold (Q >= 0).
func TestLightDegradationPasses(t *testing.T) {
	ref := genStereo(4800, 3)
	noise := fixedNoise(len(ref), 42)
	test := addScaledNoise(ref, noise, 40)
	res, err := CompareInt16(ref, test, Config{Channels: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("light degradation should PASS, got Q=%v", res.Quality)
	}
	if res.Quality >= 100 || res.Quality < 0 {
		t.Fatalf("light degradation: Quality = %v, want in [0,100)", res.Quality)
	}
}

// TestBadlyCorruptedFails: a maximally corrupted test signal (silence against a
// real reference) drives the quality metric negative and FAILS.
func TestBadlyCorruptedFails(t *testing.T) {
	ref := genStereo(4800, 4)
	silence := make([]int16, len(ref)) // all zeros
	res, err := CompareInt16(ref, silence, Config{Channels: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Passed {
		t.Fatalf("silence vs signal must FAIL, got Passed=true (Q=%v)", res.Quality)
	}
	if res.Quality >= 0 {
		t.Fatalf("silence vs signal: Quality = %v, want < 0", res.Quality)
	}
}

// TestSampleCountMismatch mirrors opus_compare's "Sample counts do not match".
func TestSampleCountMismatch(t *testing.T) {
	ref := genStereo(4800, 5)
	test := genStereo(4700, 5) // different length
	_, err := CompareInt16(ref, test, Config{Channels: 2})
	if !errors.Is(err, ErrSampleCountMismatch) {
		t.Fatalf("got %v, want ErrSampleCountMismatch", err)
	}
}

// TestInsufficientData mirrors opus_compare's "Insufficient sample data".
func TestInsufficientData(t *testing.T) {
	ref := genStereo(400, 6) // < TEST_WIN_SIZE frames
	test := append([]int16(nil), ref...)
	_, err := CompareInt16(ref, test, Config{Channels: 2})
	if !errors.Is(err, ErrInsufficientData) {
		t.Fatalf("got %v, want ErrInsufficientData", err)
	}
}

// TestInvalidRate rejects rates opus_compare does not accept.
func TestInvalidRate(t *testing.T) {
	ref := genStereo(4800, 7)
	test := append([]int16(nil), ref...)
	_, err := CompareInt16(ref, test, Config{Channels: 2, Rate: 44100})
	if !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("got %v, want ErrInvalidRate", err)
	}
}

// TestInvalidChannels rejects channel counts other than 1 or 2.
func TestInvalidChannels(t *testing.T) {
	ref := genStereo(4800, 8)
	test := append([]int16(nil), ref...)
	_, err := CompareInt16(ref, test, Config{Channels: 3})
	if !errors.Is(err, ErrInvalidChannels) {
		t.Fatalf("got %v, want ErrInvalidChannels", err)
	}
}

// TestReaderMatchesInt16 confirms the io.Reader entry point decodes little-endian
// PCM to the exact same result as the []int16 entry point.
func TestReaderMatchesInt16(t *testing.T) {
	ref := genStereo(4800, 9)
	noise := fixedNoise(len(ref), 11)
	test := addScaledNoise(ref, noise, 120)

	want, err := CompareInt16(ref, test, Config{Channels: 2})
	if err != nil {
		t.Fatalf("CompareInt16 error: %v", err)
	}
	got, err := Compare(
		bytes.NewReader(int16ToBytesLE(ref)),
		bytes.NewReader(int16ToBytesLE(test)),
		Config{Channels: 2},
	)
	if err != nil {
		t.Fatalf("Compare error: %v", err)
	}
	if got != want {
		t.Fatalf("reader path %+v != int16 path %+v", got, want)
	}
}

// TestDownsampledRateExecutes exercises the -r (downsample != 1) path: a 24 kHz
// test compared against the 48 kHz reference. It checks the reduced-band / reduced
// -frequency indexing runs without panic and produces a result.
func TestDownsampledRateExecutes(t *testing.T) {
	ref := genStereo(4800, 10)  // 4800 stereo frames @ 48 kHz
	test := genStereo(2400, 10) // 2400 stereo frames @ 24 kHz (xlength == ylength*2)
	res, err := CompareInt16(ref, test, Config{Channels: 2, Rate: 24000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Frames != (4800-testWinSize+testWinStep)/testWinStep {
		t.Fatalf("Frames = %d, want %d", res.Frames, (4800-testWinSize+testWinStep)/testWinStep)
	}
}

// TestDefaultChannelsIsMono confirms Channels==0 defaults to mono, matching
// opus_compare's default (no -s flag).
func TestDefaultChannelsIsMono(t *testing.T) {
	const n = 4800
	stereo := make([]int16, n*2)
	mono := make([]int16, n)
	for i := 0; i < n; i++ {
		v := clip16(5000 * math.Sin(2*math.Pi*250*float64(i)/48000))
		stereo[2*i] = v
		stereo[2*i+1] = v
		mono[i] = v
	}
	res, err := CompareInt16(stereo, mono, Config{}) // Channels omitted -> mono
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Passed || math.Abs(res.Quality-100) > 1e-9 {
		t.Fatalf("default (mono) identical: Q=%v Passed=%v, want 100/true", res.Quality, res.Passed)
	}
}
