package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"
	"weak"

	"github.com/tphakala/go-opus/opus"
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
// show. At the commit before the fix the difference is 5423 (native pages) and
// 10112 (one segment per page).
func TestDecoderAllocationDoesNotGrowWithStreamLength(t *testing.T) {
	// The measured difference is 1 on darwin/arm64, linux/amd64 and linux/386. A
	// per-page slice that doubles would add several more in the one-packet-per-page
	// layout, where the 20 s stream has a page for every packet.
	const slack = 2
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
	// Length makes the Decoder build its page-scan window, which Reset keeps; it
	// must not keep the source with it.
	if _, err := d.Length(); err != nil {
		t.Fatalf("Length: %v", err)
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

	// Bind a valid mono stream so the Decoder holds a codec, then fail the build for
	// a stereo one: the shape change forces the constructor call, and the failed
	// Reset must not keep the previous codec.
	if _, err := bind(stream); err != nil {
		t.Fatalf("Reset to a valid stream: %v", err)
	}
	orig := newFrameDecoder
	t.Cleanup(func() { newFrameDecoder = orig })
	newFrameDecoder = func(opusHead) (frameDecoder, error) { return nil, errors.New("codec unavailable") }
	noCodec, err := bind(seekFixture(t, 2, 20, 48000, 64000))
	if err == nil {
		t.Fatal("Reset with a failing codec constructor: want error")
	}
	if !released(noCodec) {
		t.Error("Decoder still references a source whose codec could not be built")
	}
	if _, err := d.Read(make([]byte, 16)); !errors.Is(err, errUninitialized) {
		t.Errorf("Read after a failed Reset: err = %v, want errUninitialized", err)
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

// TestDecoderResetReusesCodecAllocs pins that Reset onto a stream of the same layout
// keeps the codec instead of rebuilding it. Measured on linux/amd64 and linux/386:
// 2 allocations for the mono stream and 3 for the family-1 stream; before the
// change they took 46 and 87. The bound is the larger measurement plus 2.
func TestDecoderResetReusesCodecAllocs(t *testing.T) {
	rows := []struct {
		name   string
		stream []byte
		bound  float64
	}{
		{"mono", seekFixture(t, 1, 20, 48000, 64000), 5},
		{"family1", family1Stream(t, 40), 5},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d := seekDecoder(t, row.stream)
			drainDecoder(t, d, nil)
			got := testing.AllocsPerRun(5, func() {
				if err := d.Reset(bytes.NewReader(row.stream)); err != nil {
					t.Fatalf("Reset: %v", err)
				}
				drainDecoder(t, d, nil)
			})
			if got > row.bound {
				t.Errorf("Reset + decode: %v allocs, want at most %v", got, row.bound)
			}
		})
	}
}

// mallocsUntilZero runs prepare and then the measured function it returns, up to
// three times, and returns the exact number of heap allocations the measured
// function made in each attempt. It stops at the first attempt that allocates
// nothing. A decoder allocation is deterministic on a fresh decoder and repeats in
// every attempt, while the runtime occasionally attributes a stray malloc to the
// window, so one clean attempt is the pass condition. The two GCs empty any
// sync.Pool, so a retry cannot pass on what an earlier attempt allocated.
func mallocsUntilZero(prepare func() (measured func())) []uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var counts []uint64
	var ms runtime.MemStats
	for range 3 {
		runtime.GC()
		runtime.GC()
		measured := prepare()
		runtime.ReadMemStats(&ms)
		before := ms.Mallocs
		measured()
		runtime.ReadMemStats(&ms)
		counts = append(counts, ms.Mallocs-before)
		if ms.Mallocs == before {
			break
		}
	}
	return counts
}

// allocNoise returns n frames of uniform noise of the given peak amplitude.
func allocNoise(r *rand.Rand, n, ch int, amp float64) []int16 {
	pcm := make([]int16, n*ch)
	for i := range pcm {
		pcm[i] = int16((r.Float64()*2 - 1) * amp)
	}
	return pcm
}

// preSkip3840Stream is a CBR stereo stream of 120 ms noise packets whose OpusHead
// carries a pre-skip of 3840, so the first packet yields only 1920 samples per
// channel and the second is the first to need a full 120 ms of decoded PCM. With
// the encoder's pre-skip of 312 the first packet already yields nearly 120 ms,
// and the allocator's size class for it holds every later packet.
func preSkip3840Stream(t *testing.T) []byte {
	t.Helper()
	const (
		frame   = 5760
		preSkip = 3840
		nPkts   = 60
	)
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: 96000, CBR: true})
	if err != nil {
		t.Fatalf("opus.NewEncoder: %v", err)
	}
	r := rand.New(rand.NewPCG(9, 9))
	buf := make([]byte, 8000)
	pkts := make([][]byte, nPkts)
	for i := range pkts {
		n, err := enc.Encode(allocNoise(r, frame, 2, 20000), buf)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		pkts[i] = slices.Clone(buf[:n])
	}
	var b bytes.Buffer
	writeContainer(t, &b, testHead(2, preSkip), opusTags{vendor: "go-opus"}, pkts, frame, int64(nPkts*frame-preSkip))
	return b.Bytes()
}

// growingPacketsStream is a stereo stream whose packets grow while its pages do
// not: 255 CBR 20 ms packets of 250 bytes fill the first audio page, then 40 CBR
// 120 ms packets of 4950 bytes follow, 12 or 13 to a page of a similar size,
// and every full page among them continues its last packet on the next page.
func growingPacketsStream(t *testing.T) []byte {
	t.Helper()
	const preSkip = 312
	r := rand.New(rand.NewPCG(3, 4))
	var b bytes.Buffer
	cw, err := newContainerWriter(&b, 0x1234, testHead(2, preSkip), opusTags{vendor: "go-opus"})
	if err != nil {
		t.Fatalf("newContainerWriter: %v", err)
	}
	buf := make([]byte, 8000)
	total := 0
	for _, run := range []struct{ n, frame, bitrate int }{{255, 960, 100000}, {40, 5760, 330000}} {
		enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: 2, Bitrate: run.bitrate, CBR: true})
		if err != nil {
			t.Fatalf("opus.NewEncoder: %v", err)
		}
		for range run.n {
			n, err := enc.Encode(allocNoise(r, run.frame, 2, 20000), buf)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if err := cw.writePacket(buf[:n], run.frame); err != nil {
				t.Fatalf("writePacket: %v", err)
			}
			total += run.frame
		}
	}
	if err := cw.close(int64(total - preSkip)); err != nil {
		t.Fatalf("close: %v", err)
	}
	return b.Bytes()
}

// TestDecoderReadCELTAllocatesNothingAfterFirstRead pins that once the first Read
// has decoded the first packet, reading a CELT stream to the end allocates
// nothing: the codec's scratch is sized for the worst case when it is built
// (bitstream-dependent sizes, two coded channels, packet-loss concealment), and
// the decoded-PCM buffer for the longest frame of the stream's channel count; the
// container's page and packet buffers are sized when the Decoder is bound, and
// packets that start and end on one page are read from the page buffer. The rows
// include later pages that are larger or complete more packets than the first
// audio page, packets that grow and continue across pages, and restarts from the
// first audio page by both seek methods. Before the codec fix the first rows took
// between 3 and 64 allocations; before the container fix the rising-page,
// more-packets, growing-packets and rewind rows took 2, 1, 8 and 38.
func TestDecoderReadCELTAllocatesNothingAfterFirstRead(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	preSkip3840 := preSkip3840Stream(t)

	// 1 s blocks cycling noise, tone, click train and digital silence. CBR keeps
	// every packet, and so every full page, the same size.
	var mixed []int16
	for s := range 12 {
		switch s % 4 {
		case 0:
			mixed = append(mixed, allocNoise(r, 48000, 1, 20000)...)
		case 1:
			mixed = append(mixed, tone(48000, 1, 440, 48000)...)
		case 2:
			clicks := make([]int16, 48000)
			for i := 0; i < len(clicks); i += 4800 {
				for k := range 40 {
					clicks[i+k] = 30000
				}
			}
			mixed = append(mixed, clicks...)
		default:
			mixed = append(mixed, make([]int16, 48000)...)
		}
	}
	// 1 s of tone and 1 s of digital silence, alternating: DTX codes the silence
	// as TOC-only packets, which the decoder conceals.
	var dtx []int16
	for s := range 10 {
		if s%2 == 0 {
			dtx = append(dtx, tone(48000, 1, 330, 48000)...)
		} else {
			dtx = append(dtx, make([]int16, 48000)...)
		}
	}

	rows := []struct {
		name   string
		stream []byte
		before []byte // when set, decoded to the end and then Reset onto stream
		// rewind measures SeekToSample(0) and SeekToSampleExact(0), each followed by
		// reading to the end, instead of reading on from the first Read. The index
		// they need is built before the measurement.
		rewind bool
	}{
		// The issue's reproduction: full-scale stereo noise at the defaults.
		{name: "stereo/20ms/noise", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 2}, allocNoise(r, 5*48000, 2, 32767))},
		{name: "mono/20ms/mixed", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 1, Bitrate: 64000, CBR: true}, mixed)},
		{name: "stereo/120ms/preskip3840", stream: preSkip3840},
		{name: "mono/20ms/dtx", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 1, DTX: true}, dtx)},
		{name: "family1/3ch", stream: family1Stream(t, 300)},
		// A mono decoded-PCM buffer holds the stereo stream's first packet but not
		// its second, so this pins the sizing on Reset as well as the codec rebuild.
		{name: "reset/mono-to-stereo", stream: preSkip3840, before: encodeOgg(t, Config{SampleRate: 48000, Channels: 1}, allocNoise(r, 2*48000, 1, 20000))},
		// Audio page bodies of about 32, 52 and 41 KB: the second is larger than the
		// first.
		{name: "stereo/256k/silence-then-noise", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 2, Bitrate: 256000},
			append(make([]int16, 2*48000*2), allocNoise(r, 4*48000, 2, 32767)...))},
		// Pages complete 127, 168, 255 and 251 packets: more than the first audio
		// page's 127, and more than a queue grown by append to hold those would fit.
		{name: "stereo/160k/dtx-long-silence", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 2, Bitrate: 160000, DTX: true},
			append(tone(4*48000, 2, 440, 48000), make([]int16, 12*48000*2)...))},
		{name: "stereo/growing-packets", stream: growingPacketsStream(t)},
		{name: "stereo/rewind", stream: encodeOgg(t, Config{SampleRate: 48000, Channels: 2}, allocNoise(r, 3*48000, 2, 20000)), rewind: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var readErr error
			counts := mallocsUntilZero(func() func() {
				first := row.stream
				if row.before != nil {
					first = row.before
				}
				d, err := NewDecoder(bytes.NewReader(first))
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				if row.before != nil {
					drainDecoder(t, d, nil)
					if err := d.Reset(bytes.NewReader(row.stream)); err != nil {
						t.Fatalf("Reset: %v", err)
					}
				}
				if row.rewind {
					if _, err := d.Length(); err != nil {
						t.Fatalf("Length: %v", err)
					}
				}
				buf := make([]byte, 2*960*d.Info().Channels) // 20 ms
				if _, err := d.Read(buf); err != nil {
					t.Fatalf("first Read: %v", err)
				}
				readToEnd := func() {
					for {
						_, err := d.Read(buf)
						if errors.Is(err, io.EOF) {
							return
						}
						if err != nil {
							readErr = err
							return
						}
					}
				}
				if !row.rewind {
					return readToEnd
				}
				// SeekToSample leaves the decoder out of sequence, so the exact seek
				// restarts from the first audio page too rather than decoding on. The
				// method values are made here, outside the measured window.
				seeks := []func(int64) (int64, error){d.SeekToSample, d.SeekToSampleExact}
				return func() {
					for _, seek := range seeks {
						if _, err := seek(0); err != nil {
							readErr = err
							return
						}
						readToEnd()
						if readErr != nil {
							return
						}
					}
				}
			})
			if readErr != nil {
				t.Fatalf("Read: %v", readErr)
			}
			if counts[len(counts)-1] != 0 {
				t.Errorf("Reads after the first allocated %v times (one count per attempt), want 0 in at least one attempt", counts)
			}
		})
	}
}
