package oggopus

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These tests validate oggopus against the real Opus/Ogg ecosystem, the gap the
// go-flac lessons flag: our own round-trip passing is necessary but not
// sufficient, so we (1) parse a real .opus produced by ffmpeg/libopus, (2) feed
// its real packets back through our writer, and (3) have external tools accept
// and decode our output. Every step skips cleanly when its tool is absent.

// opusPacketDuration derives an Opus packet's decoded duration (samples per
// channel at 48 kHz) from its TOC byte per RFC 6716, so real packets can be fed
// back through the writer with correct granule accounting. It is test-only; the
// container itself stays codec-independent.
func opusPacketDuration(pkt []byte) int {
	if len(pkt) == 0 {
		return 0
	}
	config := pkt[0] >> 3
	var frameMS float64
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		frameMS = []float64{10, 20, 40, 60}[config%4]
	case config < 16: // Hybrid: 10, 20 ms
		frameMS = []float64{10, 20}[(config-12)%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		frameMS = []float64{2.5, 5, 10, 20}[(config-16)%4]
	}
	frameSamples := int(frameMS * 48)
	switch pkt[0] & 0x03 {
	case 0:
		return frameSamples
	case 1, 2:
		return 2 * frameSamples
	default: // code 3: frame count in the second byte
		if len(pkt) < 2 {
			return 0
		}
		return int(pkt[1]&0x3f) * frameSamples
	}
}

// genRealOpus produces a short real Ogg Opus file with ffmpeg/libopus, returning
// its bytes. It skips the test if ffmpeg is unavailable.
func genRealOpus(t *testing.T) []byte {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping real .opus fixture test")
	}
	out := filepath.Join(t.TempDir(), "fixture.opus")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1:sample_rate=48000",
		"-c:a", "libopus", "-b:a", "64k", "-f", "ogg", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not encode Opus (%v): %s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	t.Logf("generated real Ogg Opus fixture: %d bytes", len(data))
	return data
}

// TestParseRealOpusFixture parses an ffmpeg-produced stream with our reader and
// asserts the headers, page structure, and granule accounting are sane.
func TestParseRealOpusFixture(t *testing.T) {
	data := genRealOpus(t)

	cr, pkts := readContainer(t, bytes.NewReader(data))
	if cr.head.channels == 0 || cr.head.channels > 2 {
		t.Fatalf("unexpected channel count %d", cr.head.channels)
	}
	if cr.head.preSkip == 0 {
		t.Fatalf("real Opus stream should carry a nonzero pre-skip")
	}
	if cr.tags.vendor == "" {
		t.Fatalf("OpusTags vendor is empty")
	}
	if len(pkts) == 0 {
		t.Fatalf("no audio packets parsed")
	}
	total, ok := cr.totalSamples()
	if !ok || total <= 0 {
		t.Fatalf("bad total samples: %d (ok=%v)", total, ok)
	}
	// The duration derived from the TOC of each packet should be consistent with
	// the container's granule accounting (allowing the final-frame end-trim).
	var summed int
	for _, p := range pkts {
		summed += opusPacketDuration(p)
	}
	if int64(summed) < total {
		t.Fatalf("summed packet duration %d < container total %d", summed, total)
	}
	t.Logf("parsed real fixture: %d ch, pre-skip %d, %d packets, %d samples, vendor %q",
		cr.head.channels, cr.head.preSkip, len(pkts), total, cr.tags.vendor)
}

// TestReencodeRealPacketsAndValidate extracts the real packets from an
// ffmpeg-produced stream, writes them back through our writer, and asks ffmpeg
// to decode the result. A clean decode proves external tools accept our paging,
// headers, and granule accounting.
func TestReencodeRealPacketsAndValidate(t *testing.T) {
	data := genRealOpus(t)
	cr, pkts := readContainer(t, bytes.NewReader(data))

	preSkip := cr.head.preSkip
	source, _ := cr.totalSamples()

	var out bytes.Buffer
	head := opusHead{
		version:         opusHeadVersion,
		channels:        cr.head.channels,
		preSkip:         preSkip,
		inputSampleRate: cr.head.inputSampleRate,
		mappingFamily:   mappingFamily0,
	}
	cw, err := newContainerWriter(&out, 0x0B00B1E5, head, opusTags{vendor: "go-opus", comments: []string{"ENCODER=go-opus oggopus test"}})
	if err != nil {
		t.Fatalf("newContainerWriter: %v", err)
	}
	for _, p := range pkts {
		if err := cw.writePacket(p, opusPacketDuration(p)); err != nil {
			t.Fatalf("writePacket: %v", err)
		}
	}
	if err := cw.close(source); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Our own reader must round-trip the packets we just wrote.
	cr2, pkts2 := readContainer(t, bytes.NewReader(out.Bytes()))
	if len(pkts2) != len(pkts) {
		t.Fatalf("re-encoded packet count %d != %d", len(pkts2), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(pkts2[i], pkts[i]) {
			t.Fatalf("re-encoded packet %d differs", i)
		}
	}
	if got, _ := cr2.totalSamples(); got != source {
		t.Fatalf("re-encoded total samples %d != %d", got, source)
	}

	validateWithFFmpeg(t, out.Bytes())
	validateWithOgginfo(t, out.Bytes())
	validateWithOpusdec(t, out.Bytes())
}

// validateWithFFmpeg decodes stream via `ffmpeg -v error -f null`, which fully
// decodes and reports any container or codec error on stderr.
func validateWithFFmpeg(t *testing.T, stream []byte) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available for output validation")
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0", "-f", "null", "-")
	cmd.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg rejected our stream: %v\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("ffmpeg reported errors decoding our stream:\n%s", stderr.String())
	}
	t.Log("validator ran: ffmpeg -v error -f null (accepted and decoded)")
}

// validateWithOgginfo runs ogginfo when present; it checks Ogg page structure
// and granule progression independently of decoding.
func validateWithOgginfo(t *testing.T, stream []byte) {
	t.Helper()
	bin, err := exec.LookPath("ogginfo")
	if err != nil {
		t.Log("ogginfo not available; skipping structural validation")
		return
	}
	path := filepath.Join(t.TempDir(), "reencoded.opus")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatalf("writing temp: %v", err)
	}
	out, err := exec.Command(bin, path).CombinedOutput()
	if err != nil {
		t.Fatalf("ogginfo rejected our stream: %v\n%s", err, out)
	}
	if bytes.Contains(bytes.ToLower(out), []byte("warning")) || bytes.Contains(bytes.ToLower(out), []byte("error")) {
		t.Fatalf("ogginfo reported problems:\n%s", out)
	}
	t.Log("validator ran: ogginfo (accepted, no warnings)")
}

// validateWithOpusdec runs opusdec (opus-tools) when present, decoding to a WAV.
func validateWithOpusdec(t *testing.T, stream []byte) {
	t.Helper()
	bin, err := exec.LookPath("opusdec")
	if err != nil {
		t.Log("opusdec not available; skipping decode validation")
		return
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.opus")
	if err := os.WriteFile(in, stream, 0o600); err != nil {
		t.Fatalf("writing temp: %v", err)
	}
	out, err := exec.Command(bin, "--quiet", in, filepath.Join(dir, "out.wav")).CombinedOutput()
	if err != nil {
		t.Fatalf("opusdec rejected our stream: %v\n%s", err, out)
	}
	t.Log("validator ran: opusdec (decoded)")
}

// TestListValidators records which external validators are available so the run
// log states what actually ran.
func TestListValidators(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "opusenc", "opusdec", "ogginfo", "opusinfo"} {
		if p, err := exec.LookPath(tool); err == nil {
			t.Logf("validator available: %s (%s)", tool, p)
		} else {
			t.Logf("validator absent: %s", tool)
		}
	}
}
