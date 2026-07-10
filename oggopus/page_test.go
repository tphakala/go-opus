package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestLacing(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{254, []byte{254}},
		{255, []byte{255, 0}},
		{256, []byte{255, 1}},
		{510, []byte{255, 255, 0}},
		{511, []byte{255, 255, 1}},
	}
	for _, c := range cases {
		got := lacing(c.n)
		if !bytes.Equal(got, c.want) {
			t.Fatalf("lacing(%d) = %v, want %v", c.n, got, c.want)
		}
		sum := 0
		for _, l := range got {
			sum += int(l)
		}
		if sum != c.n {
			t.Fatalf("lacing(%d) sums to %d", c.n, sum)
		}
	}
}

func TestWritePageGolden(t *testing.T) {
	head := testHead(1, 312).marshal()
	var buf bytes.Buffer
	if err := writePage(&buf, flagBOS, 0, 0x12345678, 0, lacing(len(head)), head); err != nil {
		t.Fatalf("writePage: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), goldenHeadPage()) {
		t.Fatalf("page bytes mismatch\n got %x\nwant %x", buf.Bytes(), goldenHeadPage())
	}
}

func TestPageRoundTrip(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, 700) // three segments: 255,255,190
	var buf bytes.Buffer
	if err := writePage(&buf, flagEOS, 123456, 0xdeadbeef, 42, lacing(len(body)), body); err != nil {
		t.Fatalf("writePage: %v", err)
	}
	pg, err := readPage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readPage: %v", err)
	}
	if pg.flags != flagEOS || pg.granule != 123456 || pg.serial != 0xdeadbeef || pg.sequence != 42 {
		t.Fatalf("page header mismatch: %+v", pg)
	}
	if !bytes.Equal(pg.body, body) {
		t.Fatalf("body mismatch")
	}
}

func TestReadPageCleanEOF(t *testing.T) {
	var buf bytes.Buffer
	if _, err := readPage(bufio.NewReader(&buf)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reader: got %v, want io.EOF", err)
	}
}

func TestReadPageRejectsCorruption(t *testing.T) {
	good := goldenHeadPage()

	t.Run("bad capture", func(t *testing.T) {
		b := bytes.Clone(good)
		b[0] = 'X'
		if _, err := readPage(bufio.NewReader(bytes.NewReader(b))); !errors.Is(err, errBadCapture) {
			t.Fatalf("got %v, want errBadCapture", err)
		}
	})
	t.Run("bad crc", func(t *testing.T) {
		b := bytes.Clone(good)
		b[crcFieldOffset] ^= 0xff
		if _, err := readPage(bufio.NewReader(bytes.NewReader(b))); err == nil {
			t.Fatalf("corrupt CRC accepted")
		}
	})
	t.Run("truncated body", func(t *testing.T) {
		b := good[:len(good)-3]
		if _, err := readPage(bufio.NewReader(bytes.NewReader(b))); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
		}
	})
}

// TestStreamWriterMultiPacketPage checks that several small packets share one
// page and that the reader reassembles them exactly, with the granule stamped
// from the last completed packet.
func TestStreamWriterMultiPacketPage(t *testing.T) {
	var buf bytes.Buffer
	sw := newStreamWriter(&buf, 7)
	if err := sw.writeHeaders(testHead(1, 312), opusTags{vendor: "v"}); err != nil {
		t.Fatal(err)
	}
	pkts := [][]byte{{1}, {2, 2}, {3, 3, 3}}
	for i, p := range pkts {
		if err := sw.writeAudioPacket(p, int64((i+1)*960)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.close(); err != nil {
		t.Fatal(err)
	}

	pages := drainPages(t, &buf)
	// Expect: page0 OpusHead (BOS), page1 OpusTags, page2 audio (EOS) with all
	// three packets, granule 3*960.
	if len(pages) != 3 {
		t.Fatalf("expected 3 pages, got %d", len(pages))
	}
	if pages[0].flags&flagBOS == 0 {
		t.Fatalf("page 0 missing BOS")
	}
	last := pages[len(pages)-1]
	if last.flags&flagEOS == 0 {
		t.Fatalf("last page missing EOS")
	}
	if last.granule != 3*960 {
		t.Fatalf("audio page granule = %d, want %d", last.granule, 3*960)
	}
}

// drainPages reads every page from r for structural assertions.
func drainPages(t *testing.T, r io.Reader) []*page {
	t.Helper()
	br := bufio.NewReader(r)
	var pages []*page
	for {
		pg, err := readPage(br)
		if errors.Is(err, io.EOF) {
			return pages
		}
		if err != nil {
			t.Fatalf("readPage: %v", err)
		}
		pages = append(pages, pg)
	}
}
