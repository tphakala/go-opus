package oggopus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"
)

// repageOpts selects how repage rewrites a stream's pages.
type repageOpts struct {
	maxSegs     int    // lacing values per page; 0 means the full 255
	serial      uint32 // logical stream serial; 0 selects 0x1234
	offset      int64  // added to every granule >= 0 (an RFC 7845 4.5 initial offset)
	chain       []byte // a complete second stream appended after this one
	trailing    []byte // raw bytes appended after everything else
	eosDangling bool   // end with an EOS page of granule -1 holding only a dangling fragment
	audioOnTags bool   // put the first audio packet on the OpusTags page (RFC 7845 section 3 violation)
	trimFinal   int64  // subtract from the final granule, so whole trailing packets are end-trimmed away
	bogusGran   int64  // granule written on non-final pages that complete no packet (RFC 3533 says -1)
	firstNone   bool   // write granule -1 on the first audio page although packets complete on it
	noEOS       bool   // end the stream without an end-of-stream page; the last granule is the cumulative total minus trimFinal
}

type pager struct {
	out    bytes.Buffer
	t      *testing.T
	serial uint32
	seq    uint32
	max    int
	segs   []byte
	body   []byte
	cont   bool
	gran   int64
	bogus  int64
	first  bool
}

func (p *pager) flush(eos bool) {
	p.t.Helper()
	if len(p.segs) == 0 {
		return
	}
	var flags headerType
	if p.seq == 0 {
		flags |= flagBOS
	}
	if p.cont {
		flags |= flagContinued
	}
	if eos {
		flags |= flagEOS
	}
	if p.first && p.seq == 2 {
		p.gran = granuleNone
	}
	if p.gran == granuleNone && p.bogus != 0 && !eos {
		p.gran = p.bogus
	}
	if err := writePage(&p.out, flags, p.gran, p.serial, p.seq, p.segs, p.body); err != nil {
		p.t.Fatalf("writePage: %v", err)
	}
	p.cont = p.segs[len(p.segs)-1] == maxSegmentLen
	p.seq++
	p.segs, p.body, p.gran = p.segs[:0], p.body[:0], granuleNone
}

func (p *pager) add(pkt []byte, granAfter int64) {
	segs := lacing(len(pkt))
	off := 0
	for i, s := range segs {
		if len(p.segs) == p.max {
			p.flush(false)
		}
		p.segs = append(p.segs, s)
		p.body = append(p.body, pkt[off:off+int(s)]...)
		off += int(s)
		if i == len(segs)-1 {
			p.gran = granAfter
		}
	}
}

// repage rewrites the packets of a stream onto pages of at most o.maxSegs lacing
// values. The granule of each page is the cumulative duration of the packets that
// complete on it (that total already includes the pre-skip, as the encoder's
// granules do), or -1 when none does. The final packet keeps the stream's original
// end-trimmed granule unless o.eosDangling asks for an untrimmed one.
func repage(t *testing.T, stream []byte, o *repageOpts) []byte {
	t.Helper()
	cr, pkts := readContainer(t, bytes.NewReader(stream))
	finalG := cr.finalGranule
	p := &pager{t: t, serial: o.serial, max: o.maxSegs, bogus: o.bogusGran, first: o.firstNone}
	if p.serial == 0 {
		p.serial = 0x1234
	}
	if p.max == 0 {
		p.max = maxPageSegments
	}
	addHeader := func(pkt []byte) {
		saved := p.max
		p.max = maxPageSegments
		p.add(pkt, 0)
		p.max = saved
	}
	addHeader(cr.head.marshal())
	p.flush(false)
	addHeader(cr.tags.marshal())
	if !o.audioOnTags {
		p.flush(false)
	}
	var cum int64
	for i, pkt := range pkts {
		d, err := packetSamples48k(pkt)
		if err != nil {
			t.Fatalf("packetSamples48k: %v", err)
		}
		cum += int64(d)
		g := cum
		if i == len(pkts)-1 && !o.eosDangling {
			g = finalG - o.trimFinal
			if o.noEOS {
				g = cum - o.trimFinal
			}
		}
		if g >= 0 {
			g += o.offset
		}
		if i == 0 && o.audioOnTags {
			saved := p.max
			p.max = maxPageSegments
			p.add(pkt, g)
			p.max = saved
			p.flush(false)
			continue
		}
		p.add(pkt, g)
	}
	if o.eosDangling {
		p.flush(false)
		p.segs = append(p.segs, maxSegmentLen)
		p.body = append(p.body, make([]byte, maxSegmentLen)...)
		p.flush(true)
	} else {
		p.flush(!o.noEOS)
	}
	out := p.out.Bytes()
	out = append(out, o.chain...)
	out = append(out, o.trailing...)
	return out
}

// seekFixture encodes n samples per channel of tone-plus-chirp content.
func seekFixture(t *testing.T, ch, frameMS, n, bitrate int) []byte {
	t.Helper()
	pcm := make([]int16, n*ch)
	for i := range n {
		f := 300 + 2000*float64(i)/48000
		v := int16(9000*math.Sin(2*math.Pi*f*float64(i)/48000) + 5000*math.Sin(2*math.Pi*1234*float64(i)/48000))
		for c := range ch {
			pcm[i*ch+c] = v / int16(c+1)
		}
	}
	return encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: bitrate, FrameDurationMS: frameMS}, pcm)
}

func seekDecoder(t *testing.T, stream []byte) *Decoder {
	t.Helper()
	d, err := NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	return d
}

// pageMetaOf returns the offsets and granules of every page of a stream.
func pageMetaOf(t *testing.T, stream []byte) (offs []int, grans []int64, serials []uint32) {
	t.Helper()
	off := 0
	for off < len(stream) {
		pg, n, err := parsePage(stream[off:])
		if err != nil {
			break
		}
		offs = append(offs, off)
		grans = append(grans, pg.granule)
		serials = append(serials, pg.serial)
		off += n
	}
	return offs, grans, serials
}

// flakySource is an io.ReadSeeker with injectable failures, counting operations
// once armed so a test can fail the Nth Read or Seek.
type flakySource struct {
	r        *bytes.Reader
	armed    bool
	ops      int
	failAt   int // fail the op with this 0-based index after arming; -1 disables
	failEnd  bool
	failAll  bool
	seeks    int
	failSeek int // fail the Nth Seek (1-based) overall; 0 disables

	bytesRead int     // bytes returned by Read
	startSeek []int64 // offsets of every Seek(off, io.SeekStart)
}

var errInjected = errors.New("injected I/O failure")

func newFlaky(stream []byte) *flakySource {
	return &flakySource{r: bytes.NewReader(stream), failAt: -1}
}

func (f *flakySource) tick() error {
	if !f.armed {
		return nil
	}
	n := f.ops
	f.ops++
	if n == f.failAt {
		return errInjected
	}
	return nil
}

func (f *flakySource) Read(p []byte) (int, error) {
	if err := f.tick(); err != nil {
		return 0, err
	}
	n, err := f.r.Read(p)
	f.bytesRead += n
	return n, err
}

func (f *flakySource) Seek(off int64, whence int) (int64, error) {
	f.seeks++
	if whence == io.SeekStart {
		f.startSeek = append(f.startSeek, off)
	}
	if f.failAll || (f.failEnd && whence == io.SeekEnd) || (f.failSeek != 0 && f.seeks == f.failSeek) {
		return 0, errInjected
	}
	if err := f.tick(); err != nil {
		return 0, err
	}
	return f.r.Seek(off, whence)
}

func snr(ref, got []int16) float64 {
	var sig, noise float64
	for i := range ref {
		r := float64(ref[i])
		d := r - float64(got[i])
		sig += r * r
		noise += d * d
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}

func variantName(o *repageOpts) string {
	return fmt.Sprintf("segs%d_off%d_chain%d_junk%d_dangle%v_tags%v_trim%d_bogus%d", o.maxSegs, o.offset, len(o.chain), len(o.trailing), o.eosDangling, o.audioOnTags, o.trimFinal, o.bogusGran)
}
