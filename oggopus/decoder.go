package oggopus

import (
	"encoding/binary"
	"errors"
	"io"
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
// works (the current position is queried once, when the Decoder is bound to the
// source), Length reports the exact sample count and SeekToSample repositions the
// decoder at a sample index. Both cover the first logical stream only, matching
// sequential decoding, which stops at the first end-of-stream page. For any other
// source they return ErrSeekUnsupported and the Decoder decodes sequentially as
// before. Callers that never call them see no change.
type Decoder struct {
	cr   *containerReader
	dec  frameDecoder // nil until the codec seam is wired
	info Info

	pcm         []byte // decoded, trimmed PCM awaiting delivery
	preSkipLeft int    // pre-skip samples per channel still to drop
	delivered   int64  // samples per channel already produced
	limit       int64  // total output samples per channel, or -1 until known

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
func NewDecoder(r io.Reader) (*Decoder, error) {
	d := &Decoder{}
	if err := d.init(r); err != nil {
		return nil, err
	}
	return d, nil
}

// Reset rebinds the Decoder to a new source r, re-parsing its OpusHead and
// OpusTags headers, so one Decoder can decode many independent streams without
// re-allocating. It mirrors Encoder.Reset and the sibling pcm.Decoder.Reset in
// go-wav, go-flac, and go-aac: the pooling path for a "many short clips" workload.
//
// The codec and container reader are cleared first, so a Reset that fails partway
// (a nil source, or headers that will not parse) leaves a Decoder that reports its
// uninitialized error on the next Read rather than one still bound to the previous
// stream. The decoded-PCM scratch buffer keeps its capacity for reuse. A stream
// whose first packet is not an OpusHead returns an error wrapping ErrNotOpus.
//
// Seekability is re-evaluated for the new source and everything cached about the
// previous stream (its length and page layout) is discarded, as is any error
// latched by a failed seek.
func (d *Decoder) Reset(r io.Reader) error {
	d.cr = nil
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
	return d.init(r)
}

// init parses and validates the container headers from r and populates the
// decoder's per-stream state. It backs both NewDecoder and Reset, so the two agree
// on header handling and on rejecting a nil source before any read. It does not
// touch d.pcm, so Reset can retain that buffer's capacity across streams.
//
// It also decides seekability: r is kept for Length and SeekToSample only when it
// is an io.ReadSeeker that reports its position, and the first audio page's offset
// is recorded from the header pages the container reader consumed.
func (d *Decoder) init(r io.Reader) error {
	// Reject a nil source up front; otherwise the first container read dereferences
	// a nil io.Reader and panics.
	if r == nil {
		return errNilReader
	}
	// Capture seekability before the container reader wraps r in a bufio.Reader.
	// An io.Seeker that cannot report its position (a pipe behind *os.File) is not
	// seekable for our purposes.
	var rs io.ReadSeeker
	var base int64
	if s, ok := r.(io.ReadSeeker); ok {
		if pos, err := s.Seek(0, io.SeekCurrent); err == nil {
			rs, base = s, pos
		}
	}
	cr, err := newContainerReader(r)
	if err != nil {
		return err
	}
	dec, err := newFrameDecoder(cr.head)
	if err != nil {
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
	// End-trim so the total delivered equals finalGranule - preSkip.
	if d.limit >= 0 && d.delivered+int64(perChan) > d.limit {
		keep := max(int(d.limit-d.delivered), 0)
		samples = samples[:keep*d.info.Channels]
		perChan = keep
	}
	d.delivered += int64(perChan)

	d.pcm = d.pcm[:0]
	for _, s := range samples {
		d.pcm = binary.LittleEndian.AppendUint16(d.pcm, uint16(s))
	}
	return false, nil
}
