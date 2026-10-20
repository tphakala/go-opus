package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"runtime"
	"slices"
	"testing"
	"weak"
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

// TestResetReleasesPreviousSource pins that a pooled Decoder does not keep the
// last source reachable: the read buffer outlives each stream, so Reset must drop
// its reference to the source, and so must a bind that fails in the container
// reader or in the codec constructor.
func TestResetReleasesPreviousSource(t *testing.T) {
	stream := seekFixture(t, 1, 20, 48000, 64000)
	d, err := NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	// bind resets d onto a new source and returns a weak pointer to the source and
	// the Reset error.
	bind := func(data []byte) (weak.Pointer[bytes.Reader], error) {
		r := bytes.NewReader(data)
		err := d.Reset(r)
		return weak.Make(r), err
	}
	released := func(p weak.Pointer[bytes.Reader]) bool {
		for range 3 {
			runtime.GC()
			if p.Value() == nil {
				return true
			}
		}
		return false
	}

	good, err := bind(stream)
	if err != nil {
		t.Fatalf("Reset to a valid stream: %v", err)
	}
	if err := d.Reset(nil); err == nil {
		t.Fatal("Reset(nil): want error")
	}
	if !released(good) {
		t.Error("Decoder still references the previous source after Reset(nil)")
	}

	bad, err := bind([]byte("this is not an Ogg stream"))
	if err == nil {
		t.Fatal("Reset to garbage: want error")
	}
	if !released(bad) {
		t.Error("Decoder still references a source whose headers failed to parse")
	}

	orig := newFrameDecoder
	t.Cleanup(func() { newFrameDecoder = orig })
	newFrameDecoder = func(opusHead) (frameDecoder, error) { return nil, errors.New("codec unavailable") }
	noCodec, err := bind(stream)
	if err == nil {
		t.Fatal("Reset with a failing codec constructor: want error")
	}
	if !released(noCodec) {
		t.Error("Decoder still references a source whose codec could not be built")
	}
	runtime.KeepAlive(d) // d must stay reachable, or its collection would release the source too
}

// TestDecoderReadsCallerBufioReaderInPlace pins that a *bufio.Reader source is
// read directly, page by page, so the decoder stops at the end-of-stream page and
// the bytes after it stay in the caller's reader. Two chained streams then decode
// from one reader through NewDecoder and Reset. Wrapping the reader in the
// Decoder's own read buffer would consume up to sourceBufSize bytes past the
// first stream and lose the start of the second.
func TestDecoderReadsCallerBufioReaderInPlace(t *testing.T) {
	stream := seekFixture(t, 1, 20, 48000, 64000)
	want := streamingDecode(t, stream)
	br := bufio.NewReader(bytes.NewReader(slices.Concat(stream, stream)))

	d, err := NewDecoder(br)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	for i := range 2 {
		if i > 0 {
			if err := d.Reset(br); err != nil {
				t.Fatalf("Reset onto the second stream: %v", err)
			}
		}
		var got bytes.Buffer
		if _, err := d.WriteTo(&got); err != nil {
			t.Fatalf("stream %d: WriteTo: %v", i, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("stream %d: decoded %d bytes that differ from a fresh decode of %d bytes", i, got.Len(), len(want))
		}
	}
	if rest, err := io.ReadAll(br); err != nil || len(rest) != 0 {
		t.Fatalf("after both streams the reader holds %d bytes (err %v), want 0", len(rest), err)
	}
}

// TestDecoderBufioSourceFailures pins that a header or codec failure on a
// caller's *bufio.Reader returns an error. A Decoder that has only been given
// *bufio.Reader sources has no read buffer of its own, so the failure paths must
// not touch one.
func TestDecoderBufioSourceFailures(t *testing.T) {
	garbage := func() *bufio.Reader {
		return bufio.NewReader(bytes.NewReader([]byte("this is not an Ogg stream")))
	}
	if _, err := NewDecoder(garbage()); err == nil {
		t.Fatal("NewDecoder over garbage: want error")
	}
	var d Decoder
	if err := d.Reset(garbage()); err == nil {
		t.Fatal("Reset over garbage: want error")
	}

	stream := seekFixture(t, 1, 20, 48000, 64000)
	orig := newFrameDecoder
	t.Cleanup(func() { newFrameDecoder = orig })
	newFrameDecoder = func(opusHead) (frameDecoder, error) { return nil, errors.New("codec unavailable") }
	if _, err := NewDecoder(bufio.NewReader(bytes.NewReader(stream))); err == nil {
		t.Fatal("NewDecoder with a failing codec constructor: want error")
	}
}
