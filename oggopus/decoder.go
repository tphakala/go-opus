package oggopus

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"slices"
)

// Decoder implements io.Reader and io.WriterTo, the go-flac pcm.Decoder shape.
var (
	_ io.Reader   = (*Decoder)(nil)
	_ io.WriterTo = (*Decoder)(nil)
)

// OutputSampleRate is the sample rate of all decoded PCM, in Hz. RFC 7845
// defines the pre-skip, every granule position, and the decode output at 48 kHz
// regardless of the informational input sample rate recorded in OpusHead, so
// Read and WriteTo always produce 48 kHz PCM. The constant is untyped so it
// compares directly with Info.OutputSampleRate (uint32) and with int sample
// rates. Label decoded PCM with this, never with Info.InputSampleRate.
const OutputSampleRate = 48000

// sourceBufSize is the size of the buffer a Decoder reads a source other than a
// *bufio.Reader through. It matches the 64 KiB stream buffers of go-wav (v1.1.0)
// and go-mp3 (v0.2.0) and is larger than the biggest
// Ogg page (maxPageLen), so a page is never read around the buffer. A Decoder
// allocates it once and keeps it for its life; Reset only re-points it.
const sourceBufSize = 64 << 10

// Info reports the stream parameters parsed from OpusHead.
type Info struct {
	Channels         int    // decoded channel count (mapping family 0: 1 or 2)
	InputSampleRate  uint32 // original source rate recorded in OpusHead (informational; 0 if unspecified); NOT the decode output rate
	OutputSampleRate uint32 // rate of the decoded PCM in Hz; always 48000 (OutputSampleRate)
	PreSkip          int    // samples to discard at the start, at 48 kHz
	OutputGain       int16  // Q7.8 gain in dB applied on decode
}

// Decoder reads an Ogg Opus stream from an io.Reader and yields interleaved
// little-endian int16 PCM at 48 kHz, implementing io.Reader and io.WriterTo. It
// is shaped like go-flac's pcm.Decoder.
//
// NewDecoder parses and validates the headers, and Info reports the stream
// parameters. The packet->PCM conversion goes through the codec seam (codec.go),
// which wraps the public opus.Decoder. Output is always 48 kHz, the rate RFC 7845
// defines the pre-skip and every granule position at, whatever OpusHead's
// informational input-sample-rate field says. A Decoder is not safe for
// concurrent use.
//
// # Length and seeking
//
// When the reader given to NewDecoder or Reset is an io.ReadSeeker whose Seek
// works (the position is queried when the Decoder is bound to the source),
// Length reports the exact sample count and SeekToSample repositions the decoder
// at a sample index. Both cover the first logical stream only, matching
// sequential decoding, which stops at the first end-of-stream page; links are
// told apart by serial number, as described for Length. For any other source
// they return ErrSeekUnsupported and the Decoder decodes sequentially as before.
// Callers that never call them see no change.
type Decoder struct {
	cr   *containerReader
	dec  frameDecoder // nil until the codec seam is wired
	info Info

	pcmBuf      []byte        // backing array fill writes into; kept across Reset
	pcm         []byte        // view of pcmBuf awaiting delivery
	br          *bufio.Reader // source read buffer, kept across Reset and re-pointed at each source
	preSkipLeft int           // pre-skip samples per channel still to drop
	delivered   int64         // samples per channel already produced
	limit       int64         // total output samples per channel, or -1 until known

	// Seeking state. rs is nil unless the source is a working io.ReadSeeker; base is
	// its offset at bind time. idx is built lazily by Length or SeekToSample and is
	// valid only for the bound source (Reset clears it). scan is the scratch window
	// for page scans.
	rs      io.ReadSeeker
	base    int64
	idx     seekIndex
	scan    *pageScanner
	skipTo  int64 // per-channel index below which decoded samples are discarded
	atEnd   bool  // a seek positioned the decoder at end of stream
	err     error // sticky error latched by a failed seek, cleared by the next successful one
	tagsBad bool  // audio data shares the OpusTags page (RFC 7845 section 3 violation)
}

// NewDecoder reads and validates the OpusHead and OpusTags headers from r and
// returns a Decoder positioned at the first audio packet. Header parsing is
// real; the PCM output path is stubbed at the codec seam. A stream whose first
// packet is not an OpusHead returns an error wrapping ErrNotOpus.
//
// Whether r supports Length and SeekToSample is decided here: r must implement
// io.Seeker and answer Seek(0, io.SeekCurrent). Offsets are then relative to the
// position of r at this call, so a stream embedded in a larger source works.
//
// A Decoder keeps its page and packet buffers between packets and across Reset;
// they stay under about 450 KiB. A packet that spans several pages, such as a
// large OpusTags, needs buffers of its own size until the Decoder has read past
// it.
//
// Unless r is a *bufio.Reader, it is also read through a 64 KiB buffer that the
// Decoder allocates once and that Reset reuses, so a program holding many Decoders
// should budget that much more for each. That buffer reads ahead: the Decoder may
// take up to 64 KiB from r beyond the pages it has parsed, including bytes after
// the end of the stream. To decode a stream that other data follows in r, such as
// a chained stream for a later Reset, pass a *bufio.Reader: the Decoder reads it
// page by page, so once it has decoded a stream that ends with an end-of-stream
// page, the bytes after that page are still in the reader. Without that page the
// Decoder may read into whatever follows. A NewDecoder or Reset that fails may
// already have consumed bytes from r.
func NewDecoder(r io.Reader) (*Decoder, error) {
	d := &Decoder{}
	if err := d.init(r, nil); err != nil {
		return nil, err
	}
	return d, nil
}

// Reset rebinds the Decoder to a new source r, re-parsing its OpusHead and
// OpusTags headers, so one Decoder can decode many independent streams. Its source
// read buffer (used when r is not a *bufio.Reader), its decoded-PCM buffer and the
// container's page and packet buffers are reused; the codec is rebuilt for each
// stream. It mirrors Encoder.Reset and
// the sibling pcm.Decoder.Reset in go-wav, go-flac, and go-aac: the pooling path
// for a "many short clips" workload.
//
// The codec and container reader are cleared first, so a Reset that fails partway
// (a nil source, or headers that will not parse) leaves a Decoder that reports its
// uninitialized error on the next Read rather than one still bound to the previous
// stream. A stream whose first packet is not an OpusHead returns an error wrapping
// ErrNotOpus.
//
// Seekability is re-evaluated for the new source and everything cached about the
// previous stream (its length and page layout) is discarded, as is any error
// latched by a failed seek.
func (d *Decoder) Reset(r io.Reader) error {
	prev := d.cr
	d.cr = nil
	if d.br != nil {
		d.br.Reset(nil) // drop the previous source now; init binds the new one
	}
	d.dec = nil
	d.rs = nil
	d.base = 0
	d.idx = seekIndex{}
	d.skipTo = 0
	d.atEnd = false
	d.err = nil
	d.scan = nil
	d.tagsBad = false
	// Clear info too, so a Reset that fails partway does not leave Info reporting
	// the previous stream's parameters (init only repopulates it on success). This
	// matches go-flac and go-aac, whose decoders zero their info up front.
	d.info = Info{}
	d.pcm = d.pcm[:0]
	return d.init(r, prev)
}

// init parses and validates the container headers from r and populates the
// decoder's per-stream state. It backs both NewDecoder and Reset, so the two agree
// on header handling and on rejecting a nil source before any read. It leaves
// d.pcmBuf in place, so Reset retains that buffer across streams, and binds d.br
// (allocated on first use) to r unless r is already a *bufio.Reader. reuse, when
// not nil, is the previous stream's container reader, whose buffers the new one
// recycles.
//
// It also decides seekability: r is kept for Length and SeekToSample only when it
// is an io.ReadSeeker that reports its position, and the first audio page's offset
// is recorded from the header pages the container reader consumed.
func (d *Decoder) init(r io.Reader, reuse *containerReader) error {
	// Reject a nil source up front; otherwise the first container read dereferences
	// a nil io.Reader and panics.
	if r == nil {
		return errNilReader
	}
	// Capture seekability from r itself, before it may be wrapped in the Decoder's
	// read buffer.
	// An io.Seeker that cannot report its position (a pipe behind *os.File) is not
	// seekable for our purposes.
	var rs io.ReadSeeker
	var base int64
	if s, ok := r.(io.ReadSeeker); ok {
		if pos, err := s.Seek(0, io.SeekCurrent); err == nil {
			rs, base = s, pos
		}
	}
	// A caller's *bufio.Reader is read directly, so page reads stop after the
	// end-of-stream page and whatever follows stays in the caller's reader (a chained
	// stream, a trailer). Any other source is read through d.br. It is made with a
	// nil source and pointed at r by Reset, so it is always the Decoder's own:
	// bufio.NewReaderSize(r, n) would hand back a caller's *bufio.Reader that is
	// already large enough, and a later Reset would rebind it.
	src, ok := r.(*bufio.Reader)
	if ok && src == nil {
		return errNilReader
	}
	if !ok {
		if d.br == nil {
			d.br = bufio.NewReaderSize(nil, sourceBufSize)
		}
		d.br.Reset(r)
		src = d.br
	}
	cr, err := newContainerReader(src, reuse)
	if err != nil {
		if src == d.br {
			d.br.Reset(nil) // do not keep a source the Decoder failed to bind reachable
		}
		return err
	}
	dec, err := newFrameDecoder(cr.head)
	if err != nil {
		if src == d.br {
			d.br.Reset(nil)
		}
		return err
	}
	d.rs = rs
	d.base = base
	d.idx = seekIndex{audioStart: base + cr.consumed}
	// Audio packets sharing the OpusTags page break the granule arithmetic the
	// seek path relies on (RFC 7845 section 3 requires the comment header to end
	// its page). Sequential decoding is unaffected.
	d.tagsBad = cr.qi < len(cr.queue) || len(cr.partial) > 0
	d.skipTo = 0
	d.atEnd = false
	d.err = nil
	d.cr = cr
	d.dec = dec
	d.preSkipLeft = int(cr.head.preSkip)
	d.delivered = 0
	d.limit = -1
	d.info = Info{
		Channels:         int(cr.head.channels),
		InputSampleRate:  cr.head.inputSampleRate,
		OutputSampleRate: OutputSampleRate,
		PreSkip:          int(cr.head.preSkip),
		OutputGain:       cr.head.outputGain,
	}
	return nil
}

// Info returns the stream parameters parsed from OpusHead.
func (d *Decoder) Info() Info { return d.info }

// Read fills p with interleaved little-endian int16 PCM at 48 kHz, applying the
// pre-skip drop, the end-trim, and the OpusHead output gain. It returns io.EOF
// when the stream is exhausted.
//
// The output rate is always 48000 (OutputSampleRate), whatever OpusHead's
// informational input rate says; label decoded PCM with Info().OutputSampleRate,
// never with Info().InputSampleRate.
//
// After a failed SeekToSample, Read returns the error that seek latched until the
// next successful SeekToSample or Reset. After a SeekToSample to the end of the
// stream it returns io.EOF.
func (d *Decoder) Read(p []byte) (int, error) {
	if d.dec == nil {
		return 0, errUninitialized
	}
	if d.err != nil {
		return 0, d.err
	}
	for len(d.pcm) == 0 {
		done, err := d.fill()
		if err != nil {
			return 0, err
		}
		if done {
			return 0, io.EOF
		}
	}
	n := copy(p, d.pcm)
	d.pcm = d.pcm[n:]
	return n, nil
}

// WriteTo streams the whole decoded output to w, implementing io.WriterTo so a
// caller can io.Copy a Decoder to a sink. The PCM written to w is interleaved
// little-endian int16 at 48 kHz (OutputSampleRate); see Read, which also describes
// the error a failed SeekToSample leaves behind. It never issues a zero-length
// Write: a packet whose samples are all dropped is skipped.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	if d.dec == nil {
		return 0, errUninitialized
	}
	if d.err != nil {
		return 0, d.err
	}
	var written int64
	for {
		for len(d.pcm) == 0 {
			done, err := d.fill()
			if err != nil {
				return written, err
			}
			if done {
				return written, nil
			}
		}
		n, err := w.Write(d.pcm)
		written += int64(n)
		d.pcm = d.pcm[n:]
		if err != nil {
			return written, err
		}
	}
}

// fill decodes the next packet into d.pcm, applying the pre-skip drop at the
// start and the granule end-trim at the end. After a seek it also discards the
// pre-roll samples before the target index (skipTo). It reports done when the
// stream is fully consumed or a seek positioned the decoder at its end.
func (d *Decoder) fill() (done bool, err error) {
	if d.atEnd {
		return true, nil
	}
	pkt, err := d.cr.nextPacket()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, err
	}
	samples, err := d.dec.decodeFrame(pkt)
	if err != nil {
		return false, err
	}
	if d.limit < 0 {
		if total, ok := d.cr.totalSamples(); ok {
			d.limit = total
		}
	}
	perChan := len(samples) / d.info.Channels

	// Drop leading pre-skip samples spread across the first packets.
	if d.preSkipLeft > 0 {
		drop := min(d.preSkipLeft, perChan)
		samples = samples[drop*d.info.Channels:]
		perChan -= drop
		d.preSkipLeft -= drop
	}
	// Discard the pre-roll after a seek: decoded samples before the target index
	// only warm the codec up. The minimum is taken in int64 before converting.
	if d.delivered < d.skipTo {
		drop := int(min(d.skipTo-d.delivered, int64(perChan)))
		samples = samples[drop*d.info.Channels:]
		perChan -= drop
		d.delivered += int64(drop)
	}
	// End-trim so the total delivered equals finalGranule - preSkip. The floor is
	// taken in int64 before converting: a limit learned only at the EOS page can sit
	// further below delivered than a 32-bit int holds.
	if d.limit >= 0 && d.delivered+int64(perChan) > d.limit {
		keep := int(max(d.limit-d.delivered, 0))
		samples = samples[:keep*d.info.Channels]
		perChan = keep
	}
	d.delivered += int64(perChan)

	// Build the bytes from the start of pcmBuf, not of d.pcm: Read and WriteTo
	// advance d.pcm, which would shrink its capacity to the undelivered tail and
	// reallocate on every packet.
	buf := slices.Grow(d.pcmBuf[:0], 2*len(samples))
	for _, s := range samples {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(s))
	}
	d.pcmBuf = buf
	d.pcm = buf
	return false, nil
}
