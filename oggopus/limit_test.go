package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// limitPkt is one audio packet for limitStream and the number of 48 kHz samples
// it adds to the granule position.
type limitPkt struct {
	b   []byte
	dur int64
}

// limitStream lays out an Ogg stream with raw header packets, then the audio
// packets with cumulative granules, on pages of at most maxSegs lacing values.
// The header packets are not validated, so a test can pass any bytes. The last
// page carries the end-of-stream flag when eos is set.
func limitStream(t *testing.T, headPkt, tagsPkt []byte, pkts []limitPkt, maxSegs int, eos bool) []byte {
	t.Helper()
	p := &pager{t: t, serial: 0x1234, max: maxPageSegments}
	p.add(headPkt, 0)
	p.flush(false)
	p.add(tagsPkt, 0)
	p.flush(false)
	p.max = maxSegs
	var cum int64
	for _, pk := range pkts {
		cum += pk.dur
		p.add(pk.b, cum)
	}
	p.flush(eos)
	return p.out.Bytes()
}

// filled returns n bytes that start with a CELT 20 ms code-0 TOC byte.
func filled(n int) []byte {
	b := bytes.Repeat([]byte{0x55}, n)
	b[0] = 0xF8
	return b
}

// TestContainerPacketLimit pins the audio packet limit at the byte: a packet of
// exactly the limit is returned intact, one byte more fails with
// ErrPacketTooLarge, the limit scales with the stream count from OpusHead, and
// after the error the reader neither resumes mid-packet nor holds the packet.
func TestContainerPacketLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  opusHead
		limit int
	}{
		{"family0", testHead(1, 312), 122880},
		{"family255x3", testStreamsHead(3), 368640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := audioPacketLimit(tc.head); got != tc.limit {
				t.Fatalf("audioPacketLimit = %d, want %d", got, tc.limit)
			}
			tags := opusTags{vendor: "v"}.marshal()
			small := filled(17)

			exact := limitStream(t, tc.head.marshal(), tags,
				[]limitPkt{{filled(tc.limit), 960}, {small, 960}}, maxPageSegments, true)
			_, got := readContainer(t, bytes.NewReader(exact))
			if len(got) != 2 || len(got[0]) != tc.limit || !bytes.Equal(got[1], small) {
				t.Fatalf("exact-limit stream: got %d packets, want 2 with a first of %d bytes", len(got), tc.limit)
			}

			over := limitStream(t, tc.head.marshal(), tags,
				[]limitPkt{{filled(tc.limit + 1), 960}, {small, 960}}, maxPageSegments, true)
			cr, err := newContainerReader(bufio.NewReader(bytes.NewReader(over)), nil)
			if err != nil {
				t.Fatalf("newContainerReader: %v", err)
			}
			for i := range 2 {
				p, err := cr.nextPacket()
				if !errors.Is(err, ErrPacketTooLarge) {
					t.Fatalf("nextPacket call %d: got %d bytes, err %v; want ErrPacketTooLarge", i+1, len(p), err)
				}
			}
			if c := cap(cr.partial); c != 0 {
				t.Errorf("partial capacity %d after the limit error, want it released", c)
			}
		})
	}
}

// pageStream serves pre-built pages: the header bytes, the first page, then the
// continuation page n times. It counts the bytes handed out so a test can bound
// how far a reader got without buffering the stream.
type pageStream struct {
	head, first, cont []byte
	n                 int
	pending           []byte
	stage             int
	served            int
}

func (s *pageStream) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		switch {
		case s.stage == 0:
			s.pending = s.head
		case s.stage == 1:
			s.pending = s.first
		case s.stage <= s.n+1:
			s.pending = s.cont
		default:
			return 0, io.EOF
		}
		s.stage++
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	s.served += n
	return n, nil
}

// TestDecoderPacketLimitEndlessSource feeds a stream whose audio packet never
// ends: continuation pages of full 255-byte segments. Read must stop with
// ErrPacketTooLarge after taking only about the limit from the source, and keep
// returning it. Without a limit the reader consumes the whole source.
func TestDecoderPacketLimitEndlessSource(t *testing.T) {
	head := limitStream(t, testHead(1, 312).marshal(), opusTags{vendor: "v"}.marshal(), nil, maxPageSegments, false)
	segs := bytes.Repeat([]byte{maxSegmentLen}, maxPageSegments)
	body := make([]byte, maxPageBodyLen)
	var first, cont bytes.Buffer
	if err := writePage(&first, 0, granuleNone, 0x1234, 2, segs, body); err != nil {
		t.Fatal(err)
	}
	if err := writePage(&cont, flagContinued, granuleNone, 0x1234, 3, segs, body); err != nil {
		t.Fatal(err)
	}
	src := &pageStream{head: head, first: first.Bytes(), cont: cont.Bytes(), n: streamPacketLimit/maxPageBodyLen + 8}
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	buf := make([]byte, 4096)
	for i := range 2 {
		n, err := d.Read(buf)
		if n != 0 || !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("Read call %d = %d, %v; want 0, ErrPacketTooLarge", i+1, n, err)
		}
	}
	// The reader stores one byte per body byte read, so what it took from the
	// source bounds its memory: the limit, one page past it, and the read-ahead.
	bound := len(head) + streamPacketLimit + pageHeaderFixedLen + maxPageSegments + maxPageBodyLen + sourceBufSize
	if src.served > bound {
		t.Errorf("source served %d bytes, want at most %d", src.served, bound)
	}
}

// TestDecoderPacketLimitLengthAndSeek pins the limit on the paths that do not
// go through sequential reads: Length with and without an end-of-stream page,
// and SeekToSample passing the packet while it fills the pre-roll.
func TestDecoderPacketLimitLengthAndSeek(t *testing.T) {
	head := testHead(1, 312).marshal()
	tags := opusTags{vendor: "v"}.marshal()
	big := limitPkt{filled(audioPacketLimit(testHead(1, 312)) + 1), 960}

	for _, tc := range []struct {
		name string
		eos  bool
	}{{"Length/EOS", true}, {"Length/noEOS", false}} {
		t.Run(tc.name, func(t *testing.T) {
			d := seekDecoder(t, limitStream(t, head, tags, []limitPkt{big}, maxPageSegments, tc.eos))
			if n, err := d.Length(); !errors.Is(err, ErrPacketTooLarge) {
				t.Fatalf("Length = %d, %v; want ErrPacketTooLarge", n, err)
			}
		})
	}

	t.Run("SeekToSample", func(t *testing.T) {
		_, audio := readContainer(t, bytes.NewReader(seekFixture(t, 1, 20, 2*48000, 64000)))
		pkts := make([]limitPkt, 0, len(audio)+1)
		for _, p := range audio[:len(audio)-1] {
			n, err := packetSamples48k(p)
			if err != nil {
				t.Fatal(err)
			}
			pkts = append(pkts, limitPkt{p, int64(n)})
		}
		last := audio[len(audio)-1]
		n, err := packetSamples48k(last)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, big, limitPkt{last, int64(n)})
		d := seekDecoder(t, limitStream(t, head, tags, pkts, 8, true))
		total, err := d.Length()
		if err != nil {
			t.Fatalf("Length: %v", err)
		}
		if _, err := d.SeekToSample(total - 1); !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("SeekToSample = %v, want ErrPacketTooLarge", err)
		}
		if _, err := d.Read(make([]byte, 4096)); !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("Read after the failed seek = %v, want ErrPacketTooLarge", err)
		}
	})
}

// TestDecoderOversizedHeaders pins that OpusHead and OpusTags are cut, not
// failed, when they are large: a stream never fails on the size of its tags, a
// cut OpusTags still needs its magic, and a huge first packet that is not an
// OpusHead is still reported as not Opus.
func TestDecoderOversizedHeaders(t *testing.T) {
	stream := seekFixture(t, 1, 20, 24000, 32000)
	cr, audio := readContainer(t, bytes.NewReader(stream))
	pkts := make([]limitPkt, 0, len(audio))
	for _, p := range audio {
		n, err := packetSamples48k(p)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, limitPkt{p, int64(n)})
	}
	head := cr.head.marshal()
	tags := opusTags{vendor: "v", comments: []string{"COVER=" + strings.Repeat("x", 200000)}}.marshal()

	t.Run("OpusTags cut", func(t *testing.T) {
		// The same audio behind ordinary tags, laid out the same way.
		want, err := io.ReadAll(seekDecoder(t, limitStream(t, head, opusTags{vendor: "v"}.marshal(), pkts, maxPageSegments, true)))
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecoder(bytes.NewReader(limitStream(t, head, tags, pkts, maxPageSegments, true)))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, want) || len(got) == 0 {
			t.Fatalf("decoded %d bytes after the cut OpusTags, want the %d bytes decoded behind ordinary tags", len(got), len(want))
		}
	})
	t.Run("OpusTags cut, bad magic", func(t *testing.T) {
		_, err := NewDecoder(bytes.NewReader(limitStream(t, head, filled(200000), pkts, maxPageSegments, true)))
		if err == nil || errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("NewDecoder = %v, want an error that is not ErrPacketTooLarge", err)
		}
	})
	t.Run("first packet huge, not Opus", func(t *testing.T) {
		_, err := NewDecoder(bytes.NewReader(limitStream(t, filled(100000), tags, pkts, maxPageSegments, true)))
		if !errors.Is(err, ErrNotOpus) {
			t.Fatalf("NewDecoder = %v, want ErrNotOpus", err)
		}
	})
}

// TestContainerDroppedCutTagsDoesNotCutNext pins that the cut flag belongs to the
// packet that was cut: an OpusTags that never completes (the next page does not
// continue it) is dropped, and the packet that then takes the OpusTags role is
// parsed in full.
func TestContainerDroppedCutTagsDoesNotCutNext(t *testing.T) {
	var out bytes.Buffer
	page := func(flags headerType, seq uint32, segs, body []byte) {
		t.Helper()
		if err := writePage(&out, flags, 0, 0x1234, seq, segs, body); err != nil {
			t.Fatal(err)
		}
	}
	headPkt := testHead(1, 312).marshal()
	page(flagBOS, 0, lacing(len(headPkt)), headPkt)
	// A first segment run past the header limit that never ends in a short segment.
	page(0, 1, bytes.Repeat([]byte{maxSegmentLen}, maxPageSegments), make([]byte, maxPageBodyLen))
	tagsPkt := opusTags{vendor: "kept"}.marshal()
	page(0, 2, lacing(len(tagsPkt)), tagsPkt)

	cr, err := newContainerReader(bufio.NewReader(&out), nil)
	if err != nil {
		t.Fatalf("newContainerReader: %v", err)
	}
	if cr.tags.vendor != "kept" {
		t.Errorf("vendor %q, want the replacement OpusTags parsed in full", cr.tags.vendor)
	}
}
