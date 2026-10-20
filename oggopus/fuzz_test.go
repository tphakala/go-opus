package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"testing"
)

// The fuzz targets assert the never-panic invariant on the page reader, the
// header parsers, and the whole container reader: hostile or truncated input
// must return an error, never crash. This mirrors go-flac's parser fuzzing.

func FuzzReadPage(f *testing.F) {
	f.Add(goldenHeadPage())
	f.Add([]byte("OggS"))
	f.Add([]byte{})
	// Valid pages of different sizes back to back, so the reused page buffer
	// regrows and shrinks within one input.
	var pages bytes.Buffer
	for _, n := range []int{10, 3000, 20, 40000, 5} {
		_ = writePage(&pages, 0, 0, 1, 0, lacing(n), make([]byte, n))
	}
	f.Add(pages.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		// One page across iterations, so pages of different sizes exercise the
		// reuse of its buffer.
		var pg page
		for range 64 {
			if err := readPage(br, &pg); err != nil {
				return
			}
			// Sanity: a returned page must have a consistent body length.
			sum := 0
			for _, l := range pg.segTable {
				sum += int(l)
			}
			if sum != len(pg.body) {
				t.Fatalf("inconsistent page: segsum %d body %d", sum, len(pg.body))
			}
		}
	})
}

func FuzzParseOpusHead(f *testing.F) {
	f.Add(testHead(1, 312).marshal())
	f.Add([]byte("OpusHead"))
	f.Add([]byte("Opus"))
	f.Add(vorbisIdentHeader())
	f.Add(oggFLACMappingHeader())
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := parseOpusHead(data)
		hasMagic := len(data) >= len(opusHeadMagic) && string(data[:len(opusHeadMagic)]) == opusHeadMagic
		isNotOpus := errors.Is(err, ErrNotOpus)
		if !hasMagic && !isNotOpus {
			t.Fatalf("no OpusHead magic but err = %v does not wrap ErrNotOpus", err)
		}
		if hasMagic && isNotOpus {
			t.Fatalf("OpusHead magic present but err = %v wraps ErrNotOpus", err)
		}
	})
}

func FuzzParseOpusTags(f *testing.F) {
	f.Add(opusTags{vendor: "go-opus", comments: []string{"A=1"}}.marshal())
	f.Add([]byte("OpusTags"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseOpusTags(data)
	})
}

func FuzzContainerReader(f *testing.F) {
	var valid bytes.Buffer
	cw, _ := newContainerWriter(&valid, 1, testHead(1, 312), opusTags{vendor: "v"})
	_ = cw.writePacket([]byte{1, 2, 3}, 960)
	_ = cw.close(960 - 312)
	f.Add(valid.Bytes())
	f.Add(goldenHeadPage())
	f.Add([]byte("OggS garbage"))
	f.Add(oversizedPacketSeed(f))

	f.Fuzz(func(t *testing.T, data []byte) {
		cr, err := newContainerReader(bufio.NewReader(bytes.NewReader(data)), nil)
		if err != nil {
			return
		}
		for range 4096 {
			// Whatever the bytes, the reader never holds more than its limit of a
			// packet that spans pages.
			if len(cr.partial) > cr.limit {
				t.Fatalf("partial holds %d bytes, limit %d", len(cr.partial), cr.limit)
			}
			if _, err := cr.nextPacket(); err != nil {
				return
			}
		}
	})
}

// oversizedPacketSeed returns headers and a mono audio packet one byte over the
// audio packet limit, split over full pages, so mutations of it reach the
// continuation, limit and header-cut paths.
func oversizedPacketSeed(f *testing.F) []byte {
	f.Helper()
	return limitStream(f, testHead(1, 312).marshal(), opusTags{vendor: "v"}.marshal(),
		[][]byte{filled(streamPacketLimit + 1)}, maxPageSegments, false)
}
