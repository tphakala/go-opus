package oggopus

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os/exec"
	"testing"
)

// This file closes the end-to-end loop the packet gate cannot: PCM in through the
// public Encoder, an Ogg stream out, and the same PCM back through the public
// Decoder, with an external libopus-based tool asked to agree. Our own round-trip
// passing is necessary but not sufficient, because a container can be
// self-consistently wrong; a foreign demuxer accepting the file and reporting the
// same duration and the same impulse position is the proof that it is not.

// TestOggOpusRoundTripAllRates encodes and decodes at every sample rate the API
// accepts. The decoder always emits 48 kHz (RFC 7845 defines granules, the pre-skip
// and the output there), so an Fs-rate input of N samples decodes to N*48000/Fs
// samples: the rate conversion is the coding rate itself, not a resampler.
func TestOggOpusRoundTripAllRates(t *testing.T) {
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		for _, ch := range []int{1, 2} {
			t.Run(rateChName(rate, ch), func(t *testing.T) {
				// 250 ms of a tone, deliberately not a whole number of 20 ms frames.
				n := rate/4 + 137
				pcm := tone(n, ch, 440, rate)
				stream := encodeOgg(t, Config{SampleRate: rate, Channels: ch, Bitrate: 96000}, pcm)

				cr, pkts := readContainer(t, bytes.NewReader(stream))
				if int(cr.head.preSkip) != expectedPreSkip {
					t.Fatalf("pre-skip = %d, want %d", cr.head.preSkip, expectedPreSkip)
				}
				n48 := n * sampleRate48k / rate
				if final, want := cr.finalGranule, int64(expectedPreSkip+n48); final != want {
					t.Fatalf("final granule = %d, want %d", final, want)
				}
				if coded := int64(len(pkts)) * frame48k; cr.finalGranule > coded {
					t.Fatalf("final granule %d exceeds the %d coded samples", cr.finalGranule, coded)
				}
				checkPageGranules(t, cr, len(pkts))

				got, info := decodeOgg(t, stream)
				if info.Channels != ch {
					t.Fatalf("Info.Channels = %d, want %d", info.Channels, ch)
				}
				if len(got)/ch != n48 {
					t.Fatalf("decoded %d samples per channel at 48 kHz, want %d (%d input samples at %d Hz)",
						len(got)/ch, n48, n, rate)
				}
				// The decode must be real audio, not silence: a seam that returned empty
				// PCM would still satisfy every count above if the counts came from the
				// container alone.
				if e := rms(got); e < 1000 {
					t.Fatalf("decoded RMS %.1f is implausibly low for a full-scale tone", e)
				}
			})
		}
	}
}

// tone generates a sine at hz, amplitude ~0.6 full scale, interleaved.
func tone(n, channels, hz, rate int) []int16 {
	pcm := make([]int16, n*channels)
	for i := range n {
		v := int16(19660 * math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(rate)))
		for c := range channels {
			pcm[i*channels+c] = v
		}
	}
	return pcm
}

func rms(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// TestComplexityZeroMeansDefault pins the resolved zero-value rule: Config's zero
// Complexity is the documented default (10), NOT an explicit complexity 0. It is
// asserted on the PACKETS rather than on the constants, so opus.EncoderConfig's
// default and oggopus' documentation of it cannot drift apart without this failing.
func TestComplexityZeroMeansDefault(t *testing.T) {
	pcm := tone(4800, 1, 1000, 48000)
	base := Config{SampleRate: 48000, Channels: 1, Bitrate: 64000}

	zero := base
	zero.Complexity = 0
	ten := base
	ten.Complexity = 10
	one := base
	one.Complexity = 1

	// The serial number is random per stream, so compare the audio packets, not the
	// raw bytes.
	packetsOf := func(cfg Config) [][]byte {
		_, pkts := readContainer(t, bytes.NewReader(encodeOgg(t, cfg, pcm)))
		return pkts
	}
	zeroPkts, tenPkts, onePkts := packetsOf(zero), packetsOf(ten), packetsOf(one)

	if !packetsEqual(zeroPkts, tenPkts) {
		t.Fatalf("Complexity 0 did not encode identically to Complexity %d: the zero value "+
			"must select the default, not an explicit complexity 0", defaultComplexity)
	}
	// And complexity really is being applied: a different one must produce different
	// packets, or the assertion above would pass vacuously.
	if packetsEqual(zeroPkts, onePkts) {
		t.Fatalf("Complexity 1 encoded identically to the default: the setting is not reaching the encoder")
	}
	// An out-of-range complexity is still rejected.
	if err := (&Config{SampleRate: 48000, Channels: 1, Complexity: 11}).validate(); err == nil {
		t.Fatalf("complexity 11 accepted")
	}
}

func packetsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestEncoderResetReproducible pins the pooled Reset path: the same PCM through a
// reset Encoder must produce the same audio packets, i.e. Reset really does clear
// the codec's cross-frame state (the delay ring, the CELT overlap, the VBR
// reservoir) and not just the container's.
func TestEncoderResetReproducible(t *testing.T) {
	cfg := Config{SampleRate: 48000, Channels: 2, Bitrate: 96000}
	pcm := tone(5000, 2, 700, 48000)

	var first bytes.Buffer
	e, err := NewEncoder(&first, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if _, err := e.Write(int16sToBytes(pcm)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var second bytes.Buffer
	if err := e.Reset(&second, cfg); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := e.Write(int16sToBytes(pcm)); err != nil {
		t.Fatalf("Write after Reset: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close after Reset: %v", err)
	}

	_, p1 := readContainer(t, bytes.NewReader(first.Bytes()))
	_, p2 := readContainer(t, bytes.NewReader(second.Bytes()))
	if !packetsEqual(p1, p2) {
		t.Fatalf("Reset did not reproduce the same packets: encoder state leaked across streams")
	}
}

// TestEncoderEmptyInput: zero samples is a valid, if degenerate, stream. It must
// close cleanly and produce a parseable header-only file rather than inventing
// audio or claiming a granule it has no packets for.
func TestEncoderEmptyInput(t *testing.T) {
	var buf bytes.Buffer
	e, err := NewEncoder(&buf, Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cr, pkts := readContainer(t, bytes.NewReader(buf.Bytes()))

	// An empty stream still carries ONE coded audio packet. A header-only Ogg Opus
	// file is not forbidden by RFC 7845, but libavformat refuses to open one
	// ("Error opening input: End of file"), so emitting one would hand an
	// ffmpeg-based consumer a file it cannot read. libopusenc does not produce one
	// either: ope_encoder_drain always codes at least one frame. The granule is
	// preSkip + 0 = preSkip, so the whole frame is end-trimmed away and the stream
	// still decodes to exactly zero samples.
	if len(pkts) != 1 {
		t.Fatalf("empty input produced %d audio packets, want 1 (a header-only stream "+
			"is unopenable by libavformat)", len(pkts))
	}
	if int(cr.head.preSkip) != expectedPreSkip {
		t.Fatalf("pre-skip = %d, want %d", cr.head.preSkip, expectedPreSkip)
	}
	last := cr.pages[len(cr.pages)-1]
	if last.flags&flagEOS == 0 {
		t.Fatalf("empty stream: last page is not flagged end-of-stream (flags %#x)", last.flags)
	}
	// RFC 7845 section 4.5: the EOS granule must not be smaller than pre-skip.
	if last.granule != int64(expectedPreSkip) {
		t.Fatalf("empty stream: final granule = %d, want %d (preSkip + 0 samples)",
			last.granule, expectedPreSkip)
	}
	got, _ := decodeOgg(t, buf.Bytes())
	if len(got) != 0 {
		t.Fatalf("empty stream decoded to %d samples, want 0", len(got))
	}

	// The entire reason the frame is coded is that a real demuxer must accept the
	// file. Asserting our own reader parses it proves nothing: it parsed the broken
	// header-only shape too, which is exactly how that shape survived. Skips when no
	// external tool is installed.
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping external validation of the empty stream")
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0", "-f", "s16le", "-acodec", "pcm_s16le", "-")
	cmd.Stdin = bytes.NewReader(buf.Bytes())
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg could not open the empty stream (%v): %s\n"+
			"a header-only Ogg Opus file fails here with \"End of file\"; the stream must "+
			"carry at least one coded frame", err, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("ffmpeg decoded the empty stream to %d bytes, want 0", out.Len())
	}
}

// TestOggOpusExternalValidation is the only assertion in this package that a tool
// other than ours agrees the container is legal. It hands our encoder's output to
// ffmpeg (a real libopus-based demuxer+decoder), which must accept it silently,
// decode it to EXACTLY the input length (proving it read our pre-skip and our final
// granule and got the same answer we did), and place the impulse on the sample we
// put it on (proving the pre-skip is 312 and not 192).
//
// It sweeps the same lengths as TestOggOpusGranuleRFC7845, so the RFC-invalid
// granules the F2 fix removes are exactly the cases a third-party tool sees.
func TestOggOpusExternalValidation(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping external container validation")
	}
	t.Logf("external validator: ffmpeg (%s)", ffmpeg)

	for _, ch := range []int{1, 2} {
		for _, tc := range f2Lengths {
			t.Run(rateChName(48000, ch)+"_n"+itoa(tc.n), func(t *testing.T) {
				k := tc.n / 2
				pcm := impulseSignal(tc.n, ch, k)
				stream := encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: 256000}, pcm)

				// ffmpeg decodes to raw interleaved s16le at the stream's own rate and
				// channel count, applying the Ogg Opus pre-skip and the end-trim itself.
				cmd := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0",
					"-f", "s16le", "-acodec", "pcm_s16le", "-")
				cmd.Stdin = bytes.NewReader(stream)
				var out, errb bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &errb
				if err := cmd.Run(); err != nil {
					t.Fatalf("ffmpeg rejected our stream: %v\n%s", err, errb.String())
				}
				if errb.Len() != 0 {
					t.Fatalf("ffmpeg reported errors on our stream:\n%s", errb.String())
				}

				got := bytesToInt16s(out.Bytes())
				if len(got)/ch != tc.n {
					t.Fatalf("ffmpeg decoded %d samples per channel, want exactly %d "+
						"(our granule accounting and ffmpeg's disagree)", len(got)/ch, tc.n)
				}
				if peak := peakIndex(got, ch); peak != k {
					t.Fatalf("ffmpeg placed the impulse at sample %d, want %d (off by %d): "+
						"our pre-skip and ffmpeg's reading of it disagree", peak, k, peak-k)
				}
			})
		}
	}
}

// TestOggOpusExternalValidatesRealAudio runs a longer, broadband signal past every
// external tool that happens to be installed, so a structural checker like ogginfo
// gets a look at the paging too, not just a decoder.
func TestOggOpusExternalValidatesRealAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; skipping external container validation")
	}
	// 2 s of a sweep: many pages, a full segment table, and real coding decisions.
	const n = 96000
	pcm := sweep(n, 2, 48000)
	stream := encodeOgg(t, Config{SampleRate: 48000, Channels: 2, Bitrate: 128000}, pcm)
	t.Logf("encoded %d samples of stereo sweep to %d bytes of Ogg Opus", n, len(stream))

	validateWithFFmpeg(t, stream)
	validateWithOgginfo(t, stream)
	validateWithOpusdec(t, stream)

	got, _ := decodeOgg(t, stream)
	if len(got)/2 != n {
		t.Fatalf("our decoder returned %d samples per channel, want %d", len(got)/2, n)
	}
}

// sweep generates a log sine sweep from 20 Hz to 20 kHz, the broadband signal that
// exercises every CELT band.
func sweep(n, channels, rate int) []int16 {
	const f0, f1 = 20.0, 20000.0
	pcm := make([]int16, n*channels)
	dur := float64(n) / float64(rate)
	k := math.Log(f1 / f0)
	for i := range n {
		tt := float64(i) / float64(rate)
		phase := 2 * math.Pi * f0 * dur / k * (math.Exp(tt/dur*k) - 1)
		v := int16(19660 * math.Sin(phase))
		for c := range channels {
			pcm[i*channels+c] = v
		}
	}
	return pcm
}

// TestDecoderReadContract exercises the io.Reader path (WriteTo is used everywhere
// else) with a small buffer, so short reads and the packet boundary are covered.
func TestDecoderReadContract(t *testing.T) {
	const n = 4800
	pcm := tone(n, 1, 500, 48000)
	stream := encodeOgg(t, Config{SampleRate: 48000, Channels: 1, Bitrate: 64000}, pcm)

	d, err := NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	var out bytes.Buffer
	buf := make([]byte, 100) // deliberately not a multiple of a frame or a sample
	for {
		nn, err := d.Read(buf)
		out.Write(buf[:nn])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if got := out.Len() / 2; got != n {
		t.Fatalf("Read produced %d samples, want %d", got, n)
	}
}
