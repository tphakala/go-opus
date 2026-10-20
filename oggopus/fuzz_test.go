package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// The fuzz targets assert the never-panic invariant on the page reader, the
// header parsers, and the whole container reader: hostile or truncated input
// must return an error, never crash. This mirrors go-flac's parser fuzzing.

func FuzzReadPage(f *testing.F) {
	f.Add(goldenHeadPage())
	f.Add([]byte("OggS"))
	f.Add([]byte{})
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

	f.Fuzz(func(_ *testing.T, data []byte) {
		cr, err := newContainerReader(bufio.NewReader(bytes.NewReader(data)), nil)
		if err != nil {
			return
		}
		for range 4096 {
			if _, err := cr.nextPacket(); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				return
			}
		}
	})
}
