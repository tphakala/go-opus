package oggopus

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// repageVariants are the page layouts every table test runs over, besides the
// encoder's native layout (nil entry).
func repageVariants() []*repageOpts {
	return []*repageOpts{
		nil,
		{maxSegs: 1},
		{maxSegs: 2},
		{maxSegs: 3},
		{maxSegs: 255},
		{maxSegs: 3, trailing: make([]byte, 5000)},
		{eosDangling: true},
		{maxSegs: 255, trimFinal: 2000},
		{maxSegs: 1, bogusGran: 50000},
	}
}

func applyVariant(t *testing.T, stream []byte, o *repageOpts) []byte {
	t.Helper()
	if o == nil {
		return stream
	}
	return repage(t, stream, o)
}

func readAll(t *testing.T, d *Decoder) []byte {
	t.Helper()
	b, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return b
}

// TestLengthMatchesSequentialCount pins I4: Length is exactly the per-channel
// sample count sequential decoding yields, over mono and stereo, short and long
// frames, lengths around the pre-skip and pre-roll, and every page layout.
func TestLengthMatchesSequentialCount(t *testing.T) {
	counts := []int{0, 1, 311, 312, 313, 3839, 3840, 48000, 5*48000 + 7}
	for _, ch := range []int{1, 2} {
		for _, frameMS := range []int{20, 120} {
			for _, n := range counts {
				if frameMS == 120 && n != 0 && n != 313 && n != 48000 && n != 5*48000+7 {
					continue
				}
				base := seekFixture(t, ch, frameMS, n, 128000)
				for _, o := range repageVariants() {
					if n > 48000 && o != nil && o.maxSegs != 3 && !o.eosDangling {
						continue
					}
					if o != nil && o.trimFinal != 0 && n != 48000 {
						continue // the trim must stay within the single EOS page
					}
					stream := applyVariant(t, base, o)
					name := variantName(&repageOpts{})
					if o != nil {
						name = variantName(o)
					}
					t.Run(name, func(t *testing.T) {
						want := int64(len(streamingDecode(t, stream)) / (2 * ch))
						d := seekDecoder(t, stream)
						got, err := d.Length()
						if err != nil {
							t.Fatalf("ch=%d frame=%dms n=%d: Length: %v", ch, frameMS, n, err)
						}
						if got != want {
							t.Fatalf("ch=%d frame=%dms n=%d: Length = %d, want %d", ch, frameMS, n, got, want)
						}
					})
				}
			}
		}
	}
}

// TestLengthFamily1 covers Length and seek-to-zero identity on a surround stream.
func TestLengthFamily1(t *testing.T) {
	stream := family1Stream(t, 30)
	want := streamingDecode(t, stream)
	d := seekDecoder(t, stream)
	got, err := d.Length()
	if err != nil {
		t.Fatalf("Length: %v", err)
	}
	if got != int64(len(want)/(2*3)) {
		t.Fatalf("Length = %d, want %d", got, len(want)/6)
	}
}

func family1Stream(t *testing.T, nFrames int) []byte {
	t.Helper()
	const (
		frameSize = 960
		preSkip   = 312
		channels  = 3
	)
	mapping := []byte{0, 1, 2}
	packets := encodeMultistreamSeq(t, channels, 2, 1, mapping, frameSize, nFrames)
	head := opusHead{
		version: opusHeadVersion, channels: channels, preSkip: preSkip, inputSampleRate: 48000,
		mappingFamily: 1, streamCount: 2, coupledCount: 1, channelMapping: mapping,
	}
	var buf bytes.Buffer
	writeContainer(t, &buf, head, opusTags{vendor: "go-opus"}, packets, frameSize, int64(nFrames*frameSize-preSkip-77))
	return buf.Bytes()
}

// TestSeekToZeroBitIdentical pins the rewind path and that the codec reset equals
// a fresh codec: after reading part of the stream (dirtying decoder state), a seek
// below the pre-roll yields exactly the sequential bytes from that index.
func TestSeekToZeroBitIdentical(t *testing.T) {
	streams := map[string][]byte{
		"mono":    seekFixture(t, 1, 20, 3*48000, 64000),
		"stereo":  seekFixture(t, 2, 20, 3*48000, 96000),
		"family1": family1Stream(t, 40),
	}
	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			d0 := seekDecoder(t, stream)
			ch := d0.Info().Channels
			want := streamingDecode(t, stream)
			for _, n := range []int64{0, 1, 312, 3839} {
				for _, dirty := range []int{0, 5000} {
					d := seekDecoder(t, stream)
					if dirty > 0 {
						if _, err := io.ReadFull(d, make([]byte, dirty)); err != nil {
							t.Fatalf("dirtying read: %v", err)
						}
					}
					got, err := d.SeekToSample(n)
					if err != nil || got != n {
						t.Fatalf("SeekToSample(%d) = %d, %v", n, got, err)
					}
					rest := readAll(t, d)
					if !bytes.Equal(rest, want[int(n)*2*ch:]) {
						t.Fatalf("n=%d dirty=%d: output differs from sequential decode from sample n (got %d bytes, want %d, first diff %d)",
							n, dirty, len(rest), len(want)-int(n)*2*ch, firstDiffByte(rest, want[int(n)*2*ch:]))
					}
				}
			}
		})
	}
}

// TestSeekSampleAligned pins I3 and I6: an impulse lands at exactly the offset
// the seek implies, the return value is the target, and the drained count is the
// rest of the stream.
func TestSeekSampleAligned(t *testing.T) {
	const total = 4 * 48000
	for _, ch := range []int{1, 2} {
		for _, k := range []int{20000, 100000, 150001} {
			pcm := impulseSignal(total, ch, k)
			base := encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: 64000}, pcm)
			for _, o := range []*repageOpts{nil, {maxSegs: 3}, {maxSegs: 2}} {
				stream := applyVariant(t, base, o)
				for _, m := range []int{0, 1, 100, 4000} {
					d := seekDecoder(t, stream)
					length, err := d.Length()
					if err != nil {
						t.Fatalf("Length: %v", err)
					}
					target := int64(k - m)
					got, err := d.SeekToSample(target)
					if err != nil || got != target {
						t.Fatalf("ch=%d k=%d m=%d: SeekToSample = %d, %v; want %d", ch, k, m, got, err, target)
					}
					out := bytesToInt16s(readAll(t, d))
					if int64(len(out)/ch) != length-target {
						t.Fatalf("ch=%d k=%d m=%d: drained %d samples, want %d", ch, k, m, len(out)/ch, length-target)
					}
					if p := peakIndex(out, ch); p != m {
						t.Fatalf("ch=%d k=%d m=%d: peak at %d, want %d", ch, k, m, p, m)
					}
				}
			}
		}
	}
}

// TestSeekConvergesToSequential checks that after the pre-roll the decoder output
// matches sequential decoding closely, on a tonal chirp and on dense noise-like
// content, with a landing 3840 samples to one packet-pair before the target.
//
// Measured on this encoder with -v (minimum SNR over targets 3840 to 150000,
// comparing the first 48000 samples after the target): the chirp fixture at
// 96 kbps gives 32.9 dB mono and 33.1 dB stereo; the noise-like fixture is
// logged by the test. Landings are the worst case at small pages (1 to 3 lacing values).
// Measured minimums: chirp 32.9 dB (mono) and 33.1 dB (stereo), noise 33.2 dB
// (mono) and 33.5 dB (stereo, 3-value pages 39.2 dB). The floor below leaves
// about 13 dB under the lowest measurement.
func TestSeekConvergesToSequential(t *testing.T) {
	for _, ch := range []int{1, 2} {
		fixtures := map[string][]byte{
			"chirp": seekFixture(t, ch, 20, 4*48000, 96000),
			"noise": encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: 64000}, genInterleavedPCM(4*48000, ch)),
		}
		for fname, base := range fixtures {
			want := bytesToInt16s(streamingDecode(t, base))
			for _, o := range []*repageOpts{{maxSegs: 3}, {maxSegs: 1}} {
				stream := applyVariant(t, base, o)
				minSNR := 1e9
				for _, n := range []int{3840, 4000, 7777, 20000, 48000, 100001, 150000} {
					d := seekDecoder(t, stream)
					if _, err := d.SeekToSample(int64(n)); err != nil {
						t.Fatalf("SeekToSample(%d): %v", n, err)
					}
					got := bytesToInt16s(readAll(t, d))
					if len(got) != len(want)-n*ch {
						t.Fatalf("n=%d: got %d samples, want %d", n, len(got)/ch, len(want)/ch-n)
					}
					cmp := min(48000*ch, len(got))
					s := snr(want[n*ch:n*ch+cmp], got[:cmp])
					minSNR = min(minSNR, s)
					if s < convergeFloorDB {
						t.Errorf("%s ch=%d n=%d segs=%d: SNR %.1f dB below floor %.1f", fname, ch, n, o.maxSegs, s, convergeFloorDB)
					}
				}
				t.Logf("%s ch=%d segs=%d: min SNR %.1f dB", fname, ch, o.maxSegs, minSNR)
			}
		}
	}
}

// convergeFloorDB is the SNR floor for TestSeekConvergesToSequential; see the
// measurements in that test's comment.
const convergeFloorDB float64 = 20

// TestSeekNearEndTrims pins I6: the drained count after a seek near the end is
// exactly Length-n, including when the EOS page carries no granule.
func TestSeekNearEndTrims(t *testing.T) {
	for _, ch := range []int{1, 2} {
		base := seekFixture(t, ch, 20, 2*48000+123, 64000)
		for _, o := range []*repageOpts{nil, {maxSegs: 3}, {eosDangling: true}} {
			stream := applyVariant(t, base, o)
			want := streamingDecode(t, stream)
			for _, back := range []int64{1, 960, 3841, 5000} {
				d := seekDecoder(t, stream)
				length, err := d.Length()
				if err != nil {
					t.Fatal(err)
				}
				n := length - back
				got, err := d.SeekToSample(n)
				if err != nil || got != n {
					t.Fatalf("SeekToSample(%d) = %d, %v", n, got, err)
				}
				rest := readAll(t, d)
				if int64(len(rest)) != back*int64(2*ch) {
					t.Fatalf("variant=%v back=%d: drained %d bytes, want %d", o, back, len(rest), back*int64(2*ch))
				}
				if back <= 3840 && !bytes.Equal(rest, want[int(n)*2*ch:]) {
					// Not bit-identical in general (converged), only the count is exact.
					t.Logf("variant=%v back=%d: tail differs from sequential (expected after a landing)", o, back)
				}
			}
		}
	}
}

// TestSeekPastEnd pins the atEnd transition: seeks at or past the end return
// Length and read as EOF, and a following seek to 0 recovers bit-identically.
func TestSeekPastEnd(t *testing.T) {
	stream := seekFixture(t, 2, 20, 48000+5, 64000)
	want := streamingDecode(t, stream)
	d := seekDecoder(t, stream)
	length, err := d.Length()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{length, length + 1, length + 1_000_000_000, 1 << 62} {
		got, err := d.SeekToSample(n)
		if err != nil || got != length {
			t.Fatalf("SeekToSample(%d) = %d, %v; want %d, nil", n, got, err, length)
		}
		if _, err := d.Read(make([]byte, 16)); !errors.Is(err, io.EOF) {
			t.Fatalf("Read after seek to end = %v, want io.EOF", err)
		}
	}
	if _, err := d.SeekToSample(0); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, d); !bytes.Equal(got, want) {
		t.Fatalf("decode after recovering from end differs from sequential decode")
	}
}

// TestSeekAudioOnTagsPageUnsupported covers the RFC 7845 section 3 violation.
func TestSeekAudioOnTagsPageUnsupported(t *testing.T) {
	base := seekFixture(t, 1, 20, 48000, 64000)
	stream := repage(t, base, &repageOpts{audioOnTags: true, maxSegs: 3})
	want := streamingDecode(t, stream)
	d := seekDecoder(t, stream)
	for range 2 {
		_, err := d.Length()
		if !errors.Is(err, ErrSeekUnsupported) || !strings.Contains(err.Error(), "OpusTags") {
			t.Fatalf("Length err = %v, want ErrSeekUnsupported naming the OpusTags page", err)
		}
		if _, err := d.SeekToSample(100); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("SeekToSample err = %v, want ErrSeekUnsupported", err)
		}
	}
	if got := readAll(t, d); !bytes.Equal(got, want) {
		t.Fatal("sequential output changed after failed Length/SeekToSample")
	}
}

type countingWriter struct {
	buf    bytes.Buffer
	empty  int
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	if len(p) == 0 {
		c.empty++
	}
	return c.buf.Write(p)
}

// TestWriteToNoEmptyWrites pins that WriteTo never issues a zero-length Write,
// even when whole packets are dropped by pre-skip, end trim or a seek discard.
func TestWriteToNoEmptyWrites(t *testing.T) {
	// trimFinal drops whole trailing packets via the end trim, so a packet decodes
	// to zero kept samples without the stream being over.
	stream := repage(t, seekFixture(t, 1, 20, 20000, 64000), &repageOpts{maxSegs: 255, trimFinal: 2000})
	want := streamingDecode(t, stream)
	d := seekDecoder(t, stream)
	var cw countingWriter
	if _, err := d.WriteTo(&cw); err != nil {
		t.Fatal(err)
	}
	if cw.empty != 0 || !bytes.Equal(cw.buf.Bytes(), want) {
		t.Fatalf("sequential: %d empty writes, equal=%v", cw.empty, bytes.Equal(cw.buf.Bytes(), want))
	}
	d = seekDecoder(t, stream)
	length, err := d.Length()
	if err != nil || length != int64(len(want)/2) {
		t.Fatalf("Length = %d, %v; want %d", length, err, len(want)/2)
	}
	n := length - 100
	if _, err := d.SeekToSample(n); err != nil {
		t.Fatal(err)
	}
	cw = countingWriter{}
	if _, err := d.WriteTo(&cw); err != nil {
		t.Fatal(err)
	}
	if cw.empty != 0 || len(cw.buf.Bytes()) != 200 {
		t.Fatalf("after seek: %d empty writes, %d bytes (want 200)", cw.empty, cw.buf.Len())
	}
}

type onlyReader struct{ io.Reader }

// TestSeekErrors covers capability, argument and sticky-error semantics (I7).
func TestSeekErrors(t *testing.T) {
	stream := repage(t, seekFixture(t, 1, 20, 3*48000, 64000), &repageOpts{maxSegs: 3})
	want := streamingDecode(t, stream)

	t.Run("not a seeker", func(t *testing.T) {
		d, err := NewDecoder(onlyReader{bytes.NewReader(stream)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("Length err = %v", err)
		}
		if _, err := d.SeekToSample(10); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("SeekToSample err = %v", err)
		}
		if got := readAll(t, d); !bytes.Equal(got, want) {
			t.Fatal("decode changed")
		}
	})
	t.Run("seek always fails", func(t *testing.T) {
		src := newFlaky(stream)
		src.failAll = true
		d, err := NewDecoder(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("Length err = %v", err)
		}
		if got := readAll(t, d); !bytes.Equal(got, want) {
			t.Fatal("decode changed")
		}
	})
	t.Run("seek from end fails", func(t *testing.T) {
		src := newFlaky(stream)
		src.failEnd = true
		d, err := NewDecoder(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("Length err = %v", err)
		}
		if _, err := d.SeekToSample(1); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("SeekToSample err = %v", err)
		}
		if got := readAll(t, d); !bytes.Equal(got, want) {
			t.Fatal("decode changed")
		}
	})
	t.Run("negative index", func(t *testing.T) {
		d := seekDecoder(t, stream)
		if _, err := io.ReadFull(d, make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SeekToSample(-1); !errors.Is(err, ErrInvalidSeek) {
			t.Fatalf("err = %v, want ErrInvalidSeek", err)
		}
		rest := readAll(t, d)
		if !bytes.Equal(rest, want[1000:]) {
			t.Fatal("negative seek moved the decoder")
		}
	})
	t.Run("zero value", func(t *testing.T) {
		var d Decoder
		if _, err := d.Length(); !errors.Is(err, errUninitialized) {
			t.Fatalf("Length err = %v", err)
		}
		if _, err := d.SeekToSample(0); !errors.Is(err, errUninitialized) {
			t.Fatalf("SeekToSample err = %v", err)
		}
	})
	t.Run("failure at every operation of a seek is sticky and recoverable", func(t *testing.T) {
		for _, target := range []int64{100, 90000, 140000} {
			failed := 0
			for failAt := 0; ; failAt++ {
				src := newFlaky(stream)
				d, err := NewDecoder(src)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.Length(); err != nil {
					t.Fatal(err)
				}
				d.scan = nil // drop the cached window so the landing scan does real I/O
				src.armed, src.failAt = true, failAt
				_, err = d.SeekToSample(target)
				if err == nil {
					break
				}
				failed++
				if !errors.Is(err, errInjected) {
					t.Fatalf("target %d failAt %d: err = %v, want injected", target, failAt, err)
				}
				if _, rerr := d.Read(make([]byte, 8)); !errors.Is(rerr, errInjected) {
					t.Fatalf("Read after failed seek = %v, want the latched error", rerr)
				}
				src.armed = false
				if _, err := d.SeekToSample(0); err != nil {
					t.Fatalf("recovering seek: %v", err)
				}
				if got := readAll(t, d); !bytes.Equal(got, want) {
					t.Fatal("recovered decode differs")
				}
				if failAt > 5000 {
					t.Fatal("seek never completed")
				}
			}
			if failed == 0 {
				t.Fatalf("target %d: no operation was failed; the injection is vacuous", target)
			}
		}
	})
	t.Run("failed cursor restore latches", func(t *testing.T) {
		probe := newFlaky(stream)
		pd, _ := NewDecoder(probe)
		before := probe.seeks
		if _, err := pd.Length(); err != nil {
			t.Fatal(err)
		}
		last := probe.seeks - before
		src := newFlaky(stream)
		d, _ := NewDecoder(src)
		src.failSeek = src.seeks + last
		if _, err := d.Length(); !errors.Is(err, errInjected) {
			t.Fatalf("Length err = %v, want injected", err)
		}
		if _, err := d.Read(make([]byte, 8)); !errors.Is(err, errInjected) {
			t.Fatalf("Read err = %v, want the latched error", err)
		}
	})
}

// TestLengthDoesNotMovePosition pins I2.
func TestLengthDoesNotMovePosition(t *testing.T) {
	stream := repage(t, seekFixture(t, 2, 20, 2*48000, 64000), &repageOpts{maxSegs: 3})
	want := streamingDecode(t, stream)
	for _, k := range []int{0, 1, 7001, len(want) / 2, len(want)} {
		d := seekDecoder(t, stream)
		head := make([]byte, k, len(want))
		if _, err := io.ReadFull(d, head); err != nil {
			t.Fatal(err)
		}
		l1, err := d.Length()
		if err != nil {
			t.Fatal(err)
		}
		l2, err := d.Length()
		if err != nil || l1 != l2 {
			t.Fatalf("second Length = %d, %v; first %d", l2, err, l1)
		}
		got := slices.Concat(head, readAll(t, d))
		if !bytes.Equal(got, want) {
			t.Fatalf("k=%d: Length changed the decode", k)
		}
	}
}

// TestSeekHistoryIndependent pins I5 and I6 over random seek and read sequences.
func TestSeekHistoryIndependent(t *testing.T) {
	base := seekFixture(t, 2, 20, 2*48000+17, 96000)
	for _, o := range []*repageOpts{nil, {maxSegs: 3}, {maxSegs: 2}, {maxSegs: 1}} {
		stream := applyVariant(t, base, o)
		length, err := seekDecoder(t, stream).Length()
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(7, 11))
		for seq := range 12 {
			d := seekDecoder(t, stream)
			for step := range 6 {
				n := rng.Int64N(length + 100)
				if step%3 == 2 {
					n = length - rng.Int64N(6000)
					n = max(n, 0)
				}
				r := rng.IntN(40000)
				got, err := d.SeekToSample(n)
				if err != nil {
					t.Fatalf("seq %d step %d: SeekToSample(%d): %v", seq, step, n, err)
				}
				if got != min(n, length) {
					t.Fatalf("SeekToSample(%d) = %d, want %d", n, got, min(n, length))
				}
				buf := make([]byte, r)
				m, _ := io.ReadFull(d, buf)
				fresh := seekDecoder(t, stream)
				if _, err := fresh.SeekToSample(n); err != nil {
					t.Fatal(err)
				}
				fb := make([]byte, r)
				fm, _ := io.ReadFull(fresh, fb)
				wantN := min(int64(r), max(length-n, 0)*4)
				if int64(m) != wantN || m != fm || !bytes.Equal(buf[:m], fb[:fm]) {
					t.Fatalf("variant=%v seq %d step %d n=%d: read %d (fresh %d, want %d), equal=%v",
						o, seq, step, n, m, fm, wantN, bytes.Equal(buf[:m], fb[:fm]))
				}
			}
		}
	}
}

// TestSeekChainedFirstLinkOnly pins I9 with a second link shorter and longer than
// the scan window.
func TestSeekChainedFirstLinkOnly(t *testing.T) {
	// Link 1 is either short or longer than the scan window; link 2 likewise.
	for _, sizes := range [][2]int{{2 * 48000, 4800}, {2 * 48000, 6 * 48000}, {6 * 48000, 6 * 48000}} {
		link1 := repage(t, seekFixture(t, 1, 20, sizes[0], 510000), &repageOpts{maxSegs: 3, serial: 1})
		want := streamingDecode(t, link1)
		link2 := repage(t, seekFixture(t, 1, 20, sizes[1], 510000), &repageOpts{maxSegs: 3, serial: 2})
		if sizes[1] > 4800 && len(link2) < scanWindow+maxPageLen {
			t.Fatalf("second link only %d bytes; the test would not exceed the scan window", len(link2))
		}
		if sizes[0] > 2*48000 && len(link1) < scanWindow+maxPageLen {
			t.Fatalf("first link only %d bytes; the test would not exceed the scan window", len(link1))
		}
		stream := append(bytes.Clone(link1), link2...)
		d := seekDecoder(t, stream)
		length, err := d.Length()
		if err != nil {
			t.Fatalf("sizes %v: Length: %v", sizes, err)
		}
		if length != int64(len(want)/2) {
			t.Fatalf("sizes %v: Length = %d, want %d", sizes, length, len(want)/2)
		}
		if d.idx.linkEnd != int64(len(link1)) {
			t.Fatalf("sizes %v: first link ends at %d, want %d", sizes, d.idx.linkEnd, len(link1))
		}
		if got, err := d.SeekToSample(length + 50); err != nil || got != length {
			t.Fatalf("seek past link end = %d, %v", got, err)
		}
		n := length - 5000
		if _, err := d.SeekToSample(n); err != nil {
			t.Fatal(err)
		}
		rest := readAll(t, d)
		if int64(len(rest)) != (length-n)*2 {
			t.Fatalf("sizes %v: drained %d bytes, want %d", sizes, len(rest), (length-n)*2)
		}
	}
}

// TestSeekTrailingJunk pins that bytes after the EOS page change nothing.
func TestSeekTrailingJunk(t *testing.T) {
	base := repage(t, seekFixture(t, 1, 20, 2*48000, 64000), &repageOpts{maxSegs: 3})
	want := streamingDecode(t, base)
	rng := rand.New(rand.NewPCG(3, 4))
	rnd := make([]byte, 3000)
	for i := range rnd {
		rnd[i] = byte(rng.Uint32())
	}
	half := base[len(base)-300:]
	for name, junk := range map[string][]byte{"zeros": make([]byte, 70000), "random": rnd, "truncated page": half} {
		t.Run(name, func(t *testing.T) {
			stream := append(bytes.Clone(base), junk...)
			d := seekDecoder(t, stream)
			length, err := d.Length()
			if err != nil || length != int64(len(want)/2) {
				t.Fatalf("Length = %d, %v; want %d", length, err, len(want)/2)
			}
			n := int64(50000)
			if _, err := d.SeekToSample(n); err != nil {
				t.Fatal(err)
			}
			if rest := readAll(t, d); int64(len(rest)) != (length-n)*2 {
				t.Fatalf("drained %d bytes, want %d", len(rest), (length-n)*2)
			}
		})
	}
}

// TestSeekInitialOffsetUnsupported covers the RFC 7845 4.5 refusal.
func TestSeekInitialOffsetUnsupported(t *testing.T) {
	base := seekFixture(t, 1, 20, 48000, 64000)
	stream := repage(t, base, &repageOpts{maxSegs: 3, offset: 1000})
	d := seekDecoder(t, stream)
	if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
		t.Fatalf("Length err = %v, want ErrSeekUnsupported", err)
	}
	if _, err := d.SeekToSample(5000); !errors.Is(err, ErrSeekUnsupported) {
		t.Fatalf("SeekToSample err = %v, want ErrSeekUnsupported", err)
	}
	fresh := seekDecoder(t, stream)
	if !bytes.Equal(readAll(t, d), readAll(t, fresh)) {
		t.Fatal("failed Length/SeekToSample changed the sequential output")
	}

	bad := repage(t, base, &repageOpts{maxSegs: 3, offset: -5})
	d = seekDecoder(t, bad)
	_, err := d.Length()
	if err == nil || errors.Is(err, ErrSeekUnsupported) {
		t.Fatalf("Length err = %v, want a non-nil error other than ErrSeekUnsupported", err)
	}
}

// TestResetReevaluatesSeekability pins I8.
func TestResetReevaluatesSeekability(t *testing.T) {
	a := seekFixture(t, 1, 20, 30000, 64000)
	c := seekFixture(t, 1, 20, 50000, 64000)
	wantA := int64(len(streamingDecode(t, a)) / 2)
	wantC := int64(len(streamingDecode(t, c)) / 2)
	d := seekDecoder(t, a)
	if l, err := d.Length(); err != nil || l != wantA {
		t.Fatalf("A: Length = %d, %v; want %d", l, err, wantA)
	}
	if err := d.Reset(onlyReader{bytes.NewReader(a)}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
		t.Fatalf("B: Length err = %v, want ErrSeekUnsupported", err)
	}
	if err := d.Reset(bytes.NewReader(c)); err != nil {
		t.Fatal(err)
	}
	if l, err := d.Length(); err != nil || l != wantC {
		t.Fatalf("C: Length = %d, %v; want %d", l, err, wantC)
	}
	if err := d.Reset(nil); err == nil {
		t.Fatal("Reset(nil) succeeded")
	}
	if _, err := d.Length(); !errors.Is(err, errUninitialized) {
		t.Fatalf("after failed Reset: Length err = %v, want errUninitialized", err)
	}
}

// TestSeekContinuedPacketLanding pins that a landing on a page whose first
// packet continues from the previous one neither emits nor loses the fragment.
func TestSeekContinuedPacketLanding(t *testing.T) {
	const k = 100000
	// Noise under the impulse keeps packets large at a high bitrate, so they span
	// several lacing values and a two-value page boundary splits them.
	pcm := make([]int16, 3*48000)
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range pcm {
		pcm[i] = int16(rng.IntN(6001) - 3000)
	}
	pcm[k] = 32767
	base := encodeOgg(t, Config{SampleRate: 48000, Channels: 1, Bitrate: 510000}, pcm)
	stream := repage(t, base, &repageOpts{maxSegs: 2})
	offs, grans, _ := pageMetaOf(t, stream)
	conts := 0
	for _, off := range offs {
		if stream[off+5]&byte(flagContinued) != 0 {
			conts++
		}
	}
	if conts < 20 {
		t.Fatalf("only %d continued pages; the layout does not split packets", conts)
	}
	_ = grans
	d := seekDecoder(t, stream)
	length, _ := d.Length()
	for m := 0; m < 6000; m += 211 {
		target := int64(k - m)
		if _, err := d.SeekToSample(target); err != nil {
			t.Fatal(err)
		}
		out := bytesToInt16s(readAll(t, d))
		if int64(len(out)) != length-target || peakIndex(out, 1) != m {
			t.Fatalf("m=%d: %d samples (want %d), peak %d", m, len(out), length-target, peakIndex(out, 1))
		}
	}
}

// TestPcmFacadeSmoke is in the pcm package; this test pins Length on a decoder
// built from a bytes.Reader that has been partly consumed before NewDecoder.
func TestSeekSourceWithPriorOffset(t *testing.T) {
	stream := seekFixture(t, 1, 20, 48000, 64000)
	prefix := []byte("not part of the stream")
	r := bytes.NewReader(append(bytes.Clone(prefix), stream...))
	if _, err := r.Seek(int64(len(prefix)), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	d, err := NewDecoder(r)
	if err != nil {
		t.Fatal(err)
	}
	want := streamingDecode(t, stream)
	l, err := d.Length()
	if err != nil || l != int64(len(want)/2) {
		t.Fatalf("Length = %d, %v; want %d", l, err, len(want)/2)
	}
	if _, err := d.SeekToSample(0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, d), want) {
		t.Fatal("seek to 0 with a prefixed source differs from sequential decode")
	}
}

// TestSeekLandingMatchesLinearScan pins the bisection against a brute-force scan:
// for every target, the chosen landing is the last page whose granule minus the
// pre-skip lies in [0, target-3840], and none when there is none. The stream is
// longer than the scan window, so the bisection loop (not just the final walk) runs.
func TestSeekLandingMatchesLinearScan(t *testing.T) {
	base := seekFixture(t, 1, 20, 8*48000, 256000)
	for _, segs := range []int{1, 3} {
		stream := repage(t, base, &repageOpts{maxSegs: segs})
		if len(stream) < 2*scanWindow {
			t.Fatalf("segs=%d: stream is %d bytes, too short to exercise the bisection", segs, len(stream))
		}
		offs, grans, serials := pageMetaOf(t, stream)
		type cand struct {
			off int64
			pos int64
		}
		var cands []cand
		for i, off := range offs {
			pg, _, err := parsePage(stream[off:])
			if err != nil {
				t.Fatal(err)
			}
			if serials[i] == 0x1234 && grans[i] >= 0 && pg.completesPacket() && grans[i] >= expectedPreSkip {
				cands = append(cands, cand{int64(off), grans[i] - expectedPreSkip})
			}
		}
		d := seekDecoder(t, stream)
		length, err := d.Length()
		if err != nil {
			t.Fatal(err)
		}
		step := int64(97)
		if segs == 3 {
			step = 1013
		}
		bisected := 0
		for n := int64(seekPreRoll); n < length; n += step {
			pre := n - seekPreRoll
			var want *cand
			for i := range cands {
				if cands[i].pos <= pre {
					want = &cands[i]
				}
			}
			got, have, err := d.chooseLanding(pre)
			if err != nil {
				t.Fatalf("segs=%d n=%d: %v", segs, n, err)
			}
			switch {
			case want == nil && !have:
			case want == nil || !have:
				t.Fatalf("segs=%d n=%d: got %v (have %v), want %v", segs, n, got, have, want)
			case got.off != want.off:
				t.Fatalf("segs=%d n=%d: landing at %d, linear scan says %d", segs, n, got.off, want.off)
			default:
				bisected++
			}
		}
		if bisected == 0 {
			t.Fatalf("segs=%d: no target had a landing", segs)
		}
	}
}

// FuzzDecoderSeek feeds arbitrary bytes and a target through NewDecoder, Length,
// SeekToSample and a bounded drain. It must not panic or hang, and when the seek
// succeeds the drain must not exceed what Length says remains.
func FuzzDecoderSeek(f *testing.F) {
	var valid bytes.Buffer
	cw, _ := newContainerWriter(&valid, 1, testHead(1, 312), opusTags{vendor: "v"})
	for range 5 {
		_ = cw.writePacket([]byte{0xFC, 0xFF, 0xFE}, 960)
	}
	_ = cw.close(5*960 - 312 - 40)
	f.Add(valid.Bytes(), int64(0))
	f.Add(valid.Bytes(), int64(500))
	f.Add(valid.Bytes(), int64(1<<40))
	f.Add(goldenHeadPage(), int64(3))
	f.Add([]byte("OggS garbage"), int64(-1))

	f.Fuzz(func(_ *testing.T, data []byte, target int64) {
		d, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			return
		}
		length, lerr := d.Length()
		got, serr := d.SeekToSample(target)
		buf := make([]byte, 4096)
		var drained int64
		for range 4096 {
			n, err := d.Read(buf)
			drained += int64(n)
			if err != nil {
				if errors.Is(err, io.EOF) && lerr == nil && serr == nil {
					if limit := max(length-got, 0) * int64(2*d.Info().Channels); drained > limit {
						panic("drained more than Length - target")
					}
				}
				return
			}
		}
	})
}

// TestSeekIgnoresGranuleOnPagesWithoutPacket pins that a page on which no packet
// completes is never a landing, whatever its granule field says (RFC 7845 section
// 4 and RFC 3533 require -1 there).
func TestSeekIgnoresGranuleOnPagesWithoutPacket(t *testing.T) {
	const k = 100000
	pcm := make([]int16, 3*48000)
	rng := rand.New(rand.NewPCG(8, 9))
	for i := range pcm {
		pcm[i] = int16(rng.IntN(6001) - 3000)
	}
	pcm[k] = 32767
	base := encodeOgg(t, Config{SampleRate: 48000, Channels: 1, Bitrate: 510000}, pcm)
	stream := repage(t, base, &repageOpts{maxSegs: 1, bogusGran: 50000})
	d := seekDecoder(t, stream)
	length, err := d.Length()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []int{0, 100, 5000} {
		target := int64(k - m)
		if _, err := d.SeekToSample(target); err != nil {
			t.Fatal(err)
		}
		out := bytesToInt16s(readAll(t, d))
		if int64(len(out)) != length-target || peakIndex(out, 1) != m {
			t.Fatalf("m=%d: %d samples (want %d), peak %d", m, len(out), length-target, peakIndex(out, 1))
		}
	}
}

// smallPage returns one valid encoded page carrying a 10-byte packet.
func smallPage(t *testing.T, serial uint32) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := writePage(&b, 0, 960, serial, 3, []byte{10}, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeCaptures returns n bytes of zeros with an "OggS" every 997 bytes, so every
// scan sees many candidates that are not pages.
func fakeCaptures(n int) []byte {
	b := make([]byte, n)
	for i := 0; i+4 <= n; i += 997 {
		copy(b[i:], "OggS")
	}
	return b
}

// TestScannerFindsPagesAmongJunk pins the scan primitives across window edges: a
// page that straddles the edge of a scan window is found whole, junk that looks
// like page starts is skipped, and bounds limit start offsets only.
func TestScannerFindsPagesAmongJunk(t *testing.T) {
	page := smallPage(t, 7)
	size := 5 * scanWindow
	for _, anchor := range []int{0, scanWindow, 2 * scanWindow, 3 * scanWindow, size - scanWindow} {
		for delta := -200; delta <= 200; delta += 37 {
			at := anchor + delta
			if at < 0 || at+len(page) > size {
				continue
			}
			buf := fakeCaptures(size)
			copy(buf[at:], page)
			sc := &pageScanner{rs: bytes.NewReader(buf)}

			got, ok, err := sc.findForward(0, int64(size))
			if err != nil || !ok || got.off != int64(at) || got.end != int64(at+len(page)) || got.serial != 7 {
				t.Fatalf("at %d: findForward = %+v, %v, %v", at, got, ok, err)
			}
			if _, ok, _ := sc.findForward(0, int64(at)); ok {
				t.Fatalf("at %d: findForward found a page that starts at the exclusive bound", at)
			}
			if _, ok, _ := sc.findForward(int64(at)+1, int64(size)); ok {
				t.Fatalf("at %d: findForward found a page after skipping past it", at)
			}
			got, ok, err = sc.findBackward(0, int64(size))
			if err != nil || !ok || got.off != int64(at) {
				t.Fatalf("at %d: findBackward = %+v, %v, %v", at, got, ok, err)
			}
			if _, ok, _ := sc.findBackward(0, int64(at+len(page)-1)); ok {
				t.Fatalf("at %d: findBackward returned a page that ends past the bound", at)
			}
			if _, ok, _ := sc.findBackward(int64(at)+1, int64(size)); ok {
				t.Fatalf("at %d: findBackward returned a page that starts before the floor", at)
			}
			if _, ok, _ := sc.pageAt(int64(at) + 1); ok {
				t.Fatalf("at %d: pageAt accepted an offset inside a page", at)
			}
			if p, ok, _ := sc.pageAt(int64(at)); !ok || p.serial != 7 {
				t.Fatalf("at %d: pageAt = %+v, %v", at, p, ok)
			}
		}
	}
	// No page at all.
	sc := &pageScanner{rs: bytes.NewReader(fakeCaptures(3 * scanWindow))}
	if _, ok, err := sc.findForward(0, int64(3*scanWindow)); ok || err != nil {
		t.Fatalf("findForward on junk = %v, %v", ok, err)
	}
	if _, ok, err := sc.findBackward(0, int64(3*scanWindow)); ok || err != nil {
		t.Fatalf("findBackward on junk = %v, %v", ok, err)
	}
}

// TestSeekLongTrailingJunk pins that Length and a seek see through trailing junk
// several windows long that is full of fake page starts.
func TestSeekLongTrailingJunk(t *testing.T) {
	base := repage(t, seekFixture(t, 1, 20, 2*48000, 64000), &repageOpts{maxSegs: 3})
	want := streamingDecode(t, base)
	stream := append(bytes.Clone(base), fakeCaptures(4*scanWindow)...)
	d := seekDecoder(t, stream)
	length, err := d.Length()
	if err != nil || length != int64(len(want)/2) {
		t.Fatalf("Length = %d, %v; want %d", length, err, len(want)/2)
	}
	if got, err := d.SeekToSample(length - 10); err != nil || got != length-10 {
		t.Fatalf("SeekToSample = %d, %v", got, err)
	}
	if rest := readAll(t, d); len(rest) != 20 {
		t.Fatalf("drained %d bytes, want 20", len(rest))
	}
}

// TestSeekOverClaimingGranules pins the landing at end when the stream's final
// granule promises more samples than the packets decode to.
func TestSeekOverClaimingGranules(t *testing.T) {
	base := seekFixture(t, 1, 20, 2*48000, 64000)
	claimed := int64(len(streamingDecode(t, base))/2) + 20000
	stream := repage(t, base, &repageOpts{maxSegs: 3, trimFinal: -20000})
	actual := int64(len(streamingDecode(t, stream)) / 2) // the packets decode to less than the granule claims
	d := seekDecoder(t, stream)
	length, err := d.Length()
	if err != nil || length != claimed || actual >= claimed {
		t.Fatalf("Length = %d, %v; want %d (decodable %d)", length, err, claimed, actual)
	}
	got, err := d.SeekToSample(length - 10)
	if err != nil || got != actual {
		t.Fatalf("SeekToSample = %d, %v; want the decodable end %d", got, err, actual)
	}
	if _, err := d.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after seeking to the decodable end = %v, want io.EOF", err)
	}
}

// TestLengthHeaderOnlyAndTruncated covers streams with no audio page to measure.
func TestLengthHeaderOnlyAndTruncated(t *testing.T) {
	var hdr bytes.Buffer
	writeContainer(t, &hdr, testHead(1, 312), opusTags{vendor: "v"}, nil, 960, 0)
	d := seekDecoder(t, hdr.Bytes())
	if l, err := d.Length(); err != nil || l != 0 {
		t.Fatalf("header-only Length = %d, %v; want 0", l, err)
	}
	if got, err := d.SeekToSample(5); err != nil || got != 0 {
		t.Fatalf("header-only SeekToSample = %d, %v; want 0", got, err)
	}

	full := seekFixture(t, 1, 20, 48000, 64000)
	d2 := seekDecoder(t, full)
	if d2.idx.audioStart <= 0 {
		t.Fatal("audio start not recorded")
	}
	cut := full[:d2.idx.audioStart+10] // header pages plus a fragment of the first audio page
	d3 := seekDecoder(t, cut)
	for range 2 {
		if _, err := d3.Length(); err == nil || errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("truncated Length err = %v, want a non-nil error other than ErrSeekUnsupported", err)
		}
	}
}

// TestLengthRejectsBadFirstAudioPage covers a first audio page without a granule
// and one with a corrupt CRC.
func TestLengthRejectsBadFirstAudioPage(t *testing.T) {
	base := seekFixture(t, 1, 20, 48000, 64000)
	stream := repage(t, base, &repageOpts{maxSegs: 3, firstNone: true})
	d := seekDecoder(t, stream)
	if _, err := d.Length(); err == nil || errors.Is(err, ErrSeekUnsupported) || !strings.Contains(err.Error(), "no granule") {
		t.Fatalf("Length err = %v, want a no-granule error", err)
	}

	stream = repage(t, base, &repageOpts{maxSegs: 3})
	d = seekDecoder(t, stream)
	corrupt := bytes.Clone(stream)
	corrupt[d.idx.audioStart+pageHeaderFixedLen+5] ^= 0xff
	d = seekDecoder(t, corrupt)
	if _, err := d.Length(); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("Length err = %v, want a CRC error", err)
	}
}

// TestSeekThroughDamagedMiddle pins that a corrupt page or a foreign page inside
// the stream makes a landing walk fail with a latched error, as sequential decoding
// does at the same page.
func TestSeekThroughDamagedMiddle(t *testing.T) {
	base := repage(t, seekFixture(t, 1, 20, 3*48000, 64000), &repageOpts{maxSegs: 3})
	offs, _, _ := pageMetaOf(t, base)
	mid := offs[len(offs)/2]
	pageLen := offs[len(offs)/2+1] - mid

	corrupt := bytes.Clone(base)
	corrupt[mid+pageLen-2] ^= 0xff

	var foreign bytes.Buffer
	foreign.Write(base[:mid])
	if err := writePage(&foreign, 0, 5, 99, 0, []byte{4}, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	foreign.Write(base[mid:])

	for name, stream := range map[string][]byte{"corrupt page": corrupt, "foreign page": foreign.Bytes()} {
		t.Run(name, func(t *testing.T) {
			d := seekDecoder(t, stream)
			length, err := d.Length()
			if err != nil {
				t.Fatalf("Length: %v", err)
			}
			if _, err := d.SeekToSample(length - 100); err == nil {
				t.Fatal("SeekToSample succeeded across a damaged page")
			}
			if _, err := d.Read(make([]byte, 8)); err == nil {
				t.Fatal("Read after the failed seek returned no error")
			}
			if _, err := d.SeekToSample(0); err != nil {
				t.Fatalf("recovering SeekToSample(0): %v", err)
			}
		})
	}
}

// TestLengthIOErrorsAreNotCached fails every source operation of the first Length
// call in turn. Each failure surfaces, and a later call measures correctly.
func TestLengthIOErrorsAreNotCached(t *testing.T) {
	cases := map[string][]byte{
		"plain":   repage(t, seekFixture(t, 1, 20, 2*48000, 64000), &repageOpts{maxSegs: 3}),
		"chained": append(repage(t, seekFixture(t, 1, 20, 4800, 64000), &repageOpts{maxSegs: 3, serial: 1}), repage(t, seekFixture(t, 1, 20, 6*48000, 510000), &repageOpts{maxSegs: 3, serial: 2})...),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			want := int64(len(streamingDecode(t, stream)) / 2)
			failed := 0
			for failAt := range 200 {
				src := newFlaky(stream)
				d, err := NewDecoder(src)
				if err != nil {
					t.Fatal(err)
				}
				src.armed, src.failAt = true, failAt
				l, err := d.Length()
				if err == nil {
					if l != want {
						t.Fatalf("Length = %d, want %d", l, want)
					}
					break
				}
				failed++
				if !errors.Is(err, errInjected) {
					t.Fatalf("failAt %d: err = %v, want the injected error", failAt, err)
				}
				src.armed = false
				if l, err := d.Length(); err != nil || l != want {
					t.Fatalf("failAt %d: retry Length = %d, %v; want %d", failAt, l, err, want)
				}
			}
			if failed == 0 {
				t.Fatal("no operation was failed; the injection is vacuous")
			}
		})
	}
}

// TestSeekSurvivesDeterministicScanFailure keeps the rewind path honest when the
// source cannot seek back to its start.
func TestSeekRewindSeekFailure(t *testing.T) {
	stream := repage(t, seekFixture(t, 1, 20, 48000, 64000), &repageOpts{maxSegs: 3})
	src := newFlaky(stream)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Length(); err != nil {
		t.Fatal(err)
	}
	src.failAll = true
	if _, err := d.SeekToSample(10); !errors.Is(err, errInjected) {
		t.Fatalf("SeekToSample err = %v, want the injected error", err)
	}
	src.failAll = false
	if _, err := d.SeekToSample(0); err != nil {
		t.Fatalf("recovering SeekToSample: %v", err)
	}
}

// TestReadWriteToReturnLatchedError pins that WriteTo, like Read, refuses to run
// on a decoder whose seek failed.
func TestReadWriteToReturnLatchedError(t *testing.T) {
	stream := repage(t, seekFixture(t, 1, 20, 48000, 64000), &repageOpts{maxSegs: 3})
	src := newFlaky(stream)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Length(); err != nil {
		t.Fatal(err)
	}
	src.failAll = true
	if _, err := d.SeekToSample(0); err == nil {
		t.Fatal("seek succeeded")
	}
	src.failAll = false
	if n, err := d.WriteTo(io.Discard); !errors.Is(err, errInjected) || n != 0 {
		t.Fatalf("WriteTo = %d, %v; want 0 and the latched error", n, err)
	}
}

func TestPacketSamples48kRejectsEmptyPacket(t *testing.T) {
	if _, err := packetSamples48k(nil); err == nil {
		t.Fatal("packetSamples48k accepted an empty packet")
	}
	if n, err := packetSamples48k([]byte{0xFC, 0, 0}); err != nil || n != 960 {
		t.Fatalf("packetSamples48k(20 ms CELT) = %d, %v; want 960", n, err)
	}
}

// TestLengthRejectsGranuleBelowPreSkip pins that a final granule below the pre-skip
// is refused: sequential decoding keeps every decoded sample of such a stream (a
// negative end-trim limit never trims), so no Length can match it.
func TestLengthRejectsGranuleBelowPreSkip(t *testing.T) {
	base := seekFixture(t, 1, 20, 4800, 64000)
	cr, _ := readContainer(t, bytes.NewReader(base))
	stream := repage(t, base, &repageOpts{maxSegs: 255, trimFinal: cr.finalGranule - 200})
	want := streamingDecode(t, stream)
	if len(want) == 0 {
		t.Fatal("sequential decoding yields nothing; the fixture would not show the mismatch")
	}
	d := seekDecoder(t, stream)
	for range 2 {
		if _, err := d.Length(); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("Length err = %v, want ErrSeekUnsupported", err)
		}
		if _, err := d.SeekToSample(0); !errors.Is(err, ErrSeekUnsupported) {
			t.Fatalf("SeekToSample err = %v, want ErrSeekUnsupported", err)
		}
	}
	if got := readAll(t, d); !bytes.Equal(got, want) {
		t.Fatal("failed Length/SeekToSample changed the sequential output")
	}
}

// TestScannerWindowEdges pins two window-edge cases: a page whose capture pattern
// straddles the end of a full window is still found by findForward, including when
// the bound is exactly the window length, and findBackward does not stop at the end
// of a window left in the cache by an earlier call.
func TestScannerWindowEdges(t *testing.T) {
	page := smallPage(t, 7)
	size := 4 * scanWindow

	for _, at := range []int{scanWindow - 3, scanWindow - 2, scanWindow - 1} {
		buf := make([]byte, size) // no fake captures: they would let the scan re-anchor near the edge
		copy(buf[at:], page)
		for _, bound := range []int64{int64(scanWindow), int64(size)} {
			sc := &pageScanner{rs: bytes.NewReader(buf)}
			got, ok, err := sc.findForward(0, bound)
			if err != nil || !ok || got.off != int64(at) {
				t.Fatalf("page at %d, bound %d: findForward = %+v, %v, %v", at, bound, got, ok, err)
			}
		}
	}

	// Warm the cache with a window that ends before the span findBackward reads.
	buf := make([]byte, size)       // no fake captures: they would let the scan re-anchor near the edge
	at := size - scanWindow/4 - 100 // past the end of the cached window [2.5W, 3.5W)
	copy(buf[at:], page)
	sc := &pageScanner{rs: bytes.NewReader(buf)}
	if _, err := sc.window(int64(2*scanWindow + scanWindow/2)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := sc.findBackward(0, int64(size))
	if err != nil || !ok || got.off != int64(at) {
		t.Fatalf("findBackward after a warm cache = %+v, %v, %v; want the page at %d", got, ok, err, at)
	}
}

// TestSeekLeavesAtLeastPreRoll pins the pre-roll against the literal 3840: the page
// SeekToSample lands on (the last source seek it issues, since decoding only reads)
// is the last page whose first new sample lies at most 3840 samples before the
// target, found here by walking every page of a small-page stream.
func TestSeekLeavesAtLeastPreRoll(t *testing.T) {
	stream := repage(t, seekFixture(t, 1, 20, 6*48000, 128000), &repageOpts{maxSegs: 1})
	offs, grans, _ := pageMetaOf(t, stream)
	src := newFlaky(stream)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Length(); err != nil {
		t.Fatal(err)
	}
	// Targets on and next to the page grid: a target whose n-3840 equals a page's
	// first new sample must land on that page, and one sample earlier on the page
	// before it.
	targets := make([]int64, 0, 12)
	targets = append(targets, 3840, 3841, 4800, 30000, 100001, 200000)
	for _, i := range []int{20, 100} {
		pos := grans[i] - expectedPreSkip
		targets = append(targets, pos+3840, pos+3840-1, pos+3840+1)
	}
	for _, n := range targets {
		src.startSeek = nil
		if _, err := d.SeekToSample(n); err != nil {
			t.Fatal(err)
		}
		want := int64(-1)
		for i, off := range offs {
			pg, _, err := parsePage(stream[off:])
			if err != nil {
				t.Fatal(err)
			}
			pos := grans[i] - expectedPreSkip
			if grans[i] >= 0 && pg.completesPacket() && pos >= 0 && pos <= n-3840 {
				want = int64(off)
			}
		}
		if want < 0 {
			// No page is far enough back: the decoder restarts at the stream start.
			if got := src.startSeek[len(src.startSeek)-1]; got != 0 {
				t.Fatalf("n=%d: no landing page exists but the last seek was to %d, want the stream start", n, got)
			}
			continue
		}
		if got := src.startSeek[len(src.startSeek)-1]; got != want {
			t.Fatalf("n=%d: landed at offset %d, want %d (last page at least 3840 samples before the target)", n, got, want)
		}
	}
}

// TestLengthReadsOnlyTheTail pins that Length measures a large stream by reading
// the tail, not the whole source.
func TestLengthReadsOnlyTheTail(t *testing.T) {
	stream := seekFixture(t, 1, 20, 24*48000, 510000)
	if len(stream) < 8*scanWindow {
		t.Fatalf("stream is %d bytes; the test needs more than 8 scan windows (%d)", len(stream), 8*scanWindow)
	}
	src := newFlaky(stream)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatal(err)
	}
	src.bytesRead = 0
	if _, err := d.Length(); err != nil {
		t.Fatal(err)
	}
	if limit := 3 * scanWindow; src.bytesRead > limit {
		t.Fatalf("Length read %d bytes of a %d byte stream, want at most %d", src.bytesRead, len(stream), limit)
	}
}
