package oggopus

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// drainDecoder decodes d to EOF with WriteTo when buf is nil, otherwise with Read
// calls of len(buf) bytes.
func drainDecoder(t *testing.T, d *Decoder, buf []byte) {
	t.Helper()
	if buf == nil {
		if _, err := d.WriteTo(io.Discard); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		return
	}
	for {
		_, err := d.Read(buf)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
}

// TestDecoderAllocationDoesNotGrowWithStreamLength pins that decoding does not
// allocate per packet or per page: after Reset, a 20 s stream costs about the same
// number of allocations as a 2 s one. The one-packet-per-page layout turns a
// per-page allocation into a per-packet one, which the native layout would not
// show. The allowance covers containerReader.pages, a test-only diagnostic that
// doubles as pages are appended (O(log pages) allocations). At the commit before
// the fix the difference is 5423 (native pages) and 10112 (one segment per page).
func TestDecoderAllocationDoesNotGrowWithStreamLength(t *testing.T) {
	const slack = 16
	short := seekFixture(t, 1, 20, 2*48000, 64000)
	long := seekFixture(t, 1, 20, 20*48000, 64000)
	layouts := []struct {
		name string
		o    *repageOpts
	}{
		{"native", nil},
		{"onePacketPerPage", &repageOpts{maxSegs: 1}},
	}
	consumers := []struct {
		name string
		buf  []byte
	}{
		{"WriteTo", nil},
		{"Read100B", make([]byte, 100)},
		{"Read32KiB", make([]byte, 32<<10)},
	}
	for _, l := range layouts {
		s, g := applyVariant(t, short, l.o), applyVariant(t, long, l.o)
		for _, c := range consumers {
			t.Run(l.name+"/"+c.name, func(t *testing.T) {
				d, err := NewDecoder(bytes.NewReader(s))
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				measure := func(stream []byte) float64 {
					return testing.AllocsPerRun(3, func() {
						if err := d.Reset(bytes.NewReader(stream)); err != nil {
							t.Fatalf("Reset: %v", err)
						}
						drainDecoder(t, d, c.buf)
					})
				}
				a, b := measure(s), measure(g)
				if b-a > slack {
					t.Errorf("allocs per Reset+drain: %.0f for 2 s, %.0f for 20 s; the difference %.0f exceeds %d, so decoding allocates per packet or per page", a, b, b-a, slack)
				}
			})
		}
	}
}

// countingReader is a non-seeking source that counts Read calls.
type countingReader struct {
	r     *bytes.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// TestDecoderBuffersSourceReads pins that small pages do not cost one source Read
// each: the Decoder reads through a sourceBufSize buffer. At the commit before the
// fix this stream took 66 reads; with the buffer it takes 5.
func TestDecoderBuffersSourceReads(t *testing.T) {
	stream := repage(t, seekFixture(t, 1, 20, 20*48000, 64000), &repageOpts{maxSegs: 1})
	src := &countingReader{r: bytes.NewReader(stream)}
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	drainDecoder(t, d, nil)
	if limit := len(stream)/sourceBufSize + 3; src.reads > limit {
		t.Errorf("%d source reads for %d bytes, want at most %d", src.reads, len(stream), limit)
	}
}
