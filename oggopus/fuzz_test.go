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
		for range 64 {
			pg, err := readPage(br)
			if err != nil {
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
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseOpusHead(data)
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
		cr, err := newContainerReader(bytes.NewReader(data))
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
