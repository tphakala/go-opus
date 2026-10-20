package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// limitStream lays out an Ogg stream with raw header packets, then the audio
// packets with cumulative granules, on pages of at most maxSegs lacing values.
// The header packets are not validated, so a test can pass any bytes. The last
// page carries the end-of-stream flag when eos is set.
func limitStream(tb testing.TB, headPkt, tagsPkt []byte, pkts [][]byte, maxSegs int, eos bool) []byte {
	tb.Helper()
	p := &pager{t: tb, serial: 0x1234, max: maxPageSegments}
	p.add(headPkt, 0)
	p.flush(false)
	p.add(tagsPkt, 0)
	p.flush(false)
	p.max = maxSegs
	var cum int64
	for _, pk := range pkts {
		n, err := packetSamples48k(pk)
		if err != nil {
			tb.Fatalf("packetSamples48k: %v", err)
		}
		cum += int64(n)
		p.add(pk, cum)
	}
	p.flush(eos)
	return p.out.Bytes()
}

// filled returns n bytes that start with a CELT 20 ms code-0 TOC byte, so the
// packet counts as 960 samples whatever its length.
func filled(n int) []byte {
	b := bytes.Repeat([]byte{0x55}, n)
	b[0] = 0xF8
	return b
}

// TestContainerPacketLimit pins the audio packet limit at the byte: a packet of
// exactly the limit is returned intact, one byte more fails with
// ErrPacketTooLarge and releases the partial packet, and the limit scales with
// the stream count from OpusHead.
func TestContainerPacketLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  opusHead
		limit int
	}{
		{"family0", testHead(1, 312), 122880},
		{"family255x3", testStreamsHead(3, 312), 368640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := audioPacketLimit(tc.head); got != tc.limit {
				t.Fatalf("audioPacketLimit = %d, want %d", got, tc.limit)
			}
			tags := opusTags{vendor: "v"}.marshal()
			small := filled(17)

			exact := limitStream(t, tc.head.marshal(), tags, [][]byte{filled(tc.limit), small}, maxPageSegments, true)
			_, got := readContainer(t, bytes.NewReader(exact))
			if len(got) != 2 || len(got[0]) != tc.limit || !bytes.Equal(got[1], small) {
				t.Fatalf("exact-limit stream: got %d packets, want 2 with a first of %d bytes", len(got), tc.limit)
			}

			over := limitStream(t, tc.head.marshal(), tags, [][]byte{filled(tc.limit + 1), small}, maxPageSegments, true)
			cr, err := newContainerReader(bufio.NewReader(bytes.NewReader(over)), nil)
			if err != nil {
				t.Fatalf("newContainerReader: %v", err)
			}
			if p, err := cr.nextPacket(); !errors.Is(err, ErrPacketTooLarge) {
				t.Fatalf("nextPacket: got %d bytes, err %v; want ErrPacketTooLarge", len(p), err)
			}
			if c := cap(cr.partial); c != 0 {
				t.Errorf("partial capacity %d after the limit error, want it released", c)
			}
		})
	}
}

// TestDecoderPacketLimitStopsReading feeds a stream whose audio packet does not
// end within the limit: continuation pages of full 255-byte segments, far more
// of them than the limit needs. Read must stop with ErrPacketTooLarge after
// taking only about the limit from the source, and keep returning it, rather
// than read on from the middle of the packet. Without a limit the reader
// consumes the whole source.
func TestDecoderPacketLimitStopsReading(t *testing.T) {
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
	src := bytes.NewReader(slices.Concat(head, first.Bytes(), bytes.Repeat(cont.Bytes(), streamPacketLimit/maxPageBodyLen+8)))
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
	bound := int64(len(head) + streamPacketLimit + pageHeaderFixedLen + maxPageSegments + maxPageBodyLen + sourceBufSize)
	if served := src.Size() - int64(src.Len()); served > bound {
		t.Errorf("source served %d bytes, want at most %d", served, bound)
	}
}

// TestDecoderPacketLimitLatched pins that the limit error sticks when valid
// audio follows the oversized packet: a later Read must not decode on from the
// rest of that packet or from the packets after it.
func TestDecoderPacketLimitLatched(t *testing.T) {
	_, audio := readContainer(t, bytes.NewReader(seekFixture(t, 1, 20, 48000, 64000)))
	pkts := slices.Concat([][]byte{filled(audioPacketLimit(testHead(1, 312)) + 1)}, audio)
	d, err := NewDecoder(bytes.NewReader(limitStream(t, testHead(1, 312).marshal(), opusTags{vendor: "v"}.marshal(), pkts, maxPageSegments, true)))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	buf := make([]byte, 4096)
	for i := range 3 {
		if n, err := d.Read(buf); n != 0 || !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("Read call %d = %d, %v; want 0, ErrPacketTooLarge", i+1, n, err)
		}
	}
	var sink bytes.Buffer
	if n, err := d.WriteTo(&sink); n != 0 || !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("WriteTo = %d, %v; want 0, ErrPacketTooLarge", n, err)
	}
}

// TestDecoderPacketLimitLengthAndSeek pins the limit on the paths that do not
// go through sequential reads: Length with and without an end-of-stream page,
// and SeekToSample passing the packet while it fills the pre-roll.
func TestDecoderPacketLimitLengthAndSeek(t *testing.T) {
	head := testHead(1, 312).marshal()
	tags := opusTags{vendor: "v"}.marshal()
	big := filled(audioPacketLimit(testHead(1, 312)) + 1)

	_, audio := readContainer(t, bytes.NewReader(seekFixture(t, 1, 20, 2*48000, 64000)))
	for _, tc := range []struct {
		name string
		pkts [][]byte
		eos  bool
	}{
		{"Length/EOS", [][]byte{big}, true},
		{"Length/noEOS", [][]byte{big}, false},
		// The first audio page completes valid packets, so only the pass that
		// counts the tail reaches the oversized packet.
		{"Length/noEOS after audio", slices.Concat(audio, [][]byte{big}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := seekDecoder(t, limitStream(t, head, tags, tc.pkts, maxPageSegments, tc.eos))
			if n, err := d.Length(); !errors.Is(err, ErrPacketTooLarge) {
				t.Fatalf("Length = %d, %v; want ErrPacketTooLarge", n, err)
			}
		})
	}

	t.Run("SeekToSample", func(t *testing.T) {
		pkts := slices.Insert(slices.Clone(audio), len(audio)-1, big)
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

// TestContainerHeaderCut pins the header limit: an OpusTags longer than it is
// returned cut to exactly 61,440 bytes.
func TestContainerHeaderCut(t *testing.T) {
	tags := opusTags{vendor: "v", comments: []string{"COVER=" + strings.Repeat("x", 200000)}}.marshal()
	stream := limitStream(t, testHead(1, 312).marshal(), tags, [][]byte{filled(17)}, maxPageSegments, true)
	cr, err := newContainerReader(bufio.NewReader(bytes.NewReader(stream)), nil)
	if err != nil {
		t.Fatalf("newContainerReader: %v", err)
	}
	if cr.qi == 0 {
		t.Fatal("no OpusTags packet in the queue")
	}
	// RFC 7845 section 5.2 lets a reader ignore comments past the first 61,440
	// bytes; the literal pins that figure, not just the constant.
	if got := len(cr.queue[cr.qi-1]); got != 61440 {
		t.Fatalf("OpusTags kept %d bytes of %d, want 61440", got, len(tags))
	}
}

// TestDecoderOversizedHeaders pins that OpusHead and OpusTags do not fail for
// their size: a stream never fails on the size of its tags, a cut OpusTags still
// needs its magic, and a huge first packet that is not an OpusHead is still
// reported as not Opus.
func TestDecoderOversizedHeaders(t *testing.T) {
	cr, audio := readContainer(t, bytes.NewReader(seekFixture(t, 1, 20, 24000, 32000)))
	head := cr.head.marshal()
	tags := opusTags{vendor: "v", comments: []string{"COVER=" + strings.Repeat("x", 200000)}}.marshal()

	t.Run("OpusTags cut", func(t *testing.T) {
		// The same audio behind ordinary tags, laid out the same way.
		want, err := io.ReadAll(seekDecoder(t, limitStream(t, head, opusTags{vendor: "v"}.marshal(), audio, maxPageSegments, true)))
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecoder(bytes.NewReader(limitStream(t, head, tags, audio, maxPageSegments, true)))
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
		_, err := NewDecoder(bytes.NewReader(limitStream(t, head, filled(200000), audio, maxPageSegments, true)))
		if err == nil || errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("NewDecoder = %v, want an error that is not ErrPacketTooLarge", err)
		}
	})
	t.Run("first packet huge, not Opus", func(t *testing.T) {
		_, err := NewDecoder(bytes.NewReader(limitStream(t, filled(100000), tags, audio, maxPageSegments, true)))
		if !errors.Is(err, ErrNotOpus) {
			t.Fatalf("NewDecoder = %v, want ErrNotOpus", err)
		}
	})
}
