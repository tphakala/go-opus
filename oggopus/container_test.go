package oggopus

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// writeOnly wraps an io.Writer while deliberately NOT implementing io.Seeker.
// It is the regression guard for the go-flac BirdWeather zero-duration bug: an
// oggopus stream written through it must still carry the correct duration,
// because the granule is written inline per page rather than backpatched.
type writeOnly struct{ w io.Writer }

func (wo writeOnly) Write(p []byte) (int, error) { return wo.w.Write(p) }

// synthPackets builds n deterministic pseudo-packets with a spread of lengths,
// including lengths that are exact multiples of 255 (which force a terminating
// zero lacing segment). Contents are arbitrary because the container is
// codec-independent; only exact byte round-tripping is asserted.
func synthPackets(n int) [][]byte {
	r := rand.New(rand.NewPCG(42, 99))
	pkts := make([][]byte, n)
	for i := range pkts {
		var length int
		switch i % 5 {
		case 0:
			length = 1 + r.IntN(60)
		case 1:
			length = 255 // exact multiple: terminating zero segment
		case 2:
			length = 510
		default:
			length = 40 + r.IntN(400)
		}
		p := make([]byte, length)
		for j := range p {
			p[j] = byte(r.Uint32())
		}
		pkts[i] = p
	}
	return pkts
}

func writeContainer(t *testing.T, w io.Writer, head opusHead, tags opusTags, pkts [][]byte, dur int, sourceSamples int64) {
	t.Helper()
	cw, err := newContainerWriter(w, 0xCAFEBABE, head, tags)
	if err != nil {
		t.Fatalf("newContainerWriter: %v", err)
	}
	for _, p := range pkts {
		if err := cw.writePacket(p, dur); err != nil {
			t.Fatalf("writePacket: %v", err)
		}
	}
	if err := cw.close(sourceSamples); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func readContainer(t *testing.T, r io.Reader) (cr *containerReader, packets [][]byte) {
	t.Helper()
	cr, err := newContainerReader(r)
	if err != nil {
		t.Fatalf("newContainerReader: %v", err)
	}
	var got [][]byte
	for {
		p, err := cr.nextPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("nextPacket: %v", err)
		}
		got = append(got, p)
	}
	return cr, got
}

// TestContainerRoundTrip is the core "Ogg round-trips" gate: a sequence of opus
// packets with durations goes through the writer and comes back through the
// reader byte-identical, with the granule accounting exact.
func TestContainerRoundTrip(t *testing.T) {
	const (
		n       = 37
		dur     = 960 // 20 ms at 48 kHz
		preSkip = 312
	)
	pkts := synthPackets(n)
	// Choose a source count that end-trims part of the last frame.
	source := int64(n*dur - preSkip - 137)

	var buf bytes.Buffer
	head := testHead(2, preSkip)
	writeContainer(t, &buf, head, opusTags{vendor: "go-opus", comments: []string{"A=1", "B=2"}}, pkts, dur, source)

	cr, got := readContainer(t, &buf)

	if len(got) != len(pkts) {
		t.Fatalf("packet count: got %d, want %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i]) {
			t.Fatalf("packet %d differs (got %d bytes, want %d)", i, len(got[i]), len(pkts[i]))
		}
	}

	// Header round-trip.
	if cr.head.channels != 2 || cr.head.preSkip != preSkip {
		t.Fatalf("head mismatch: %+v", cr.head)
	}
	if cr.tags.vendor != "go-opus" || len(cr.tags.comments) != 2 {
		t.Fatalf("tags mismatch: %+v", cr.tags)
	}

	// Granule accounting: finalGranule - preSkip == source sample count.
	total, ok := cr.totalSamples()
	if !ok {
		t.Fatalf("totalSamples not available")
	}
	if total != source {
		t.Fatalf("total samples: got %d, want %d", total, source)
	}
	if cr.finalGranule != preSkip+source {
		t.Fatalf("final granule: got %d, want %d", cr.finalGranule, preSkip+source)
	}

	assertPageStructure(t, cr)
}

// TestContainerPacketSpanningPages exercises a packet larger than one page
// (> 255*255 bytes) plus enough packets to fill several pages, verifying the
// continuation flags and reassembly.
func TestContainerPacketSpanningPages(t *testing.T) {
	pkts := [][]byte{
		bytes.Repeat([]byte{0x11}, 30),
		makeRamp(200000),                           // spans multiple pages
		bytes.Repeat([]byte{0x22}, maxPageBodyLen), // exactly one full page body
		bytes.Repeat([]byte{0x33}, 17),
	}
	const dur, preSkip = 960, 312
	source := int64(len(pkts)*dur - preSkip)

	var buf bytes.Buffer
	writeContainer(t, &buf, testHead(1, preSkip), opusTags{vendor: "v"}, pkts, dur, source)

	cr, got := readContainer(t, &buf)
	if len(got) != len(pkts) {
		t.Fatalf("packet count: got %d, want %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i]) {
			t.Fatalf("spanning packet %d differs: got %d bytes want %d", i, len(got[i]), len(pkts[i]))
		}
	}
	// A continuation flag must appear because a packet spanned pages.
	sawContinued := false
	for _, pi := range cr.pages {
		if pi.continued {
			sawContinued = true
		}
	}
	if !sawContinued {
		t.Fatalf("expected a continued page for the spanning packet")
	}
	assertPageStructure(t, cr)
}

// TestNonSeekableDurationGuard writes a full stream through a write-only sink
// (no io.Seeker) and asserts the recovered duration equals the exact source
// sample count. This is the structural proof that oggopus does not depend on
// seeking to record duration (the go-flac non-seekable zero-duration class).
func TestNonSeekableDurationGuard(t *testing.T) {
	if _, ok := any(writeOnly{}).(io.Seeker); ok {
		t.Fatal("writeOnly must not implement io.Seeker")
	}

	const (
		n       = 50
		dur     = 960
		preSkip = 312
	)
	pkts := synthPackets(n)
	source := int64(n*dur - preSkip - 500)

	var raw bytes.Buffer
	sink := writeOnly{w: &raw} // the encoder only ever sees an io.Writer
	writeContainer(t, sink, testHead(1, preSkip), opusTags{vendor: "go-opus"}, pkts, dur, source)

	cr, got := readContainer(t, &raw)
	if len(got) != n {
		t.Fatalf("packet count: got %d want %d", len(got), n)
	}
	total, ok := cr.totalSamples()
	if !ok {
		t.Fatalf("duration unavailable on non-seekable sink (the bug this guards against)")
	}
	if total != source {
		t.Fatalf("non-seekable duration: got %d, want %d", total, source)
	}
}

// assertPageStructure checks the invariants the go-flac seekability lesson calls
// for: header pages at granule 0, BOS on the first page, EOS on the last, and a
// non-decreasing granule progression across audio pages.
func assertPageStructure(t *testing.T, cr *containerReader) {
	t.Helper()
	if len(cr.pages) < 3 {
		t.Fatalf("expected at least 3 pages (head, tags, audio), got %d", len(cr.pages))
	}
	if cr.pages[0].flags&flagBOS == 0 {
		t.Fatalf("first page missing BOS")
	}
	if cr.pages[0].granule != 0 {
		t.Fatalf("OpusHead page granule = %d, want 0", cr.pages[0].granule)
	}
	if cr.pages[1].granule != 0 {
		t.Fatalf("OpusTags page granule = %d, want 0", cr.pages[1].granule)
	}
	if cr.pages[len(cr.pages)-1].flags&flagEOS == 0 {
		t.Fatalf("last page missing EOS")
	}
	// Sequence numbers increase by one from zero.
	for i, pi := range cr.pages {
		if pi.sequence != uint32(i) {
			t.Fatalf("page %d has sequence %d", i, pi.sequence)
		}
	}
	// Granule non-decreasing across pages that complete a packet (granuleNone
	// pages are mid-packet and excluded).
	var prev int64 = -1
	for _, pi := range cr.pages {
		if pi.granule == granuleNone {
			continue
		}
		if pi.granule < prev {
			t.Fatalf("granule regressed: %d after %d", pi.granule, prev)
		}
		prev = pi.granule
	}
}

func makeRamp(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}
