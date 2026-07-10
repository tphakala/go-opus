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

// Info reports the stream parameters parsed from OpusHead.
type Info struct {
	Channels        int    // decoded channel count (mapping family 0: 1 or 2)
	InputSampleRate uint32 // original source rate recorded in OpusHead (informational; 0 if unspecified)
	PreSkip         int    // samples to discard at the start, at 48 kHz
	OutputGain      int16  // Q7.8 gain in dB applied on decode
}

// Decoder reads an Ogg Opus stream from an io.Reader and yields interleaved
// little-endian int16 PCM at 48 kHz, implementing io.Reader and io.WriterTo. It
// is shaped like go-flac's pcm.Decoder.
//
// The container parsing (pages, headers, packet reassembly, granule accounting)
// is complete: NewDecoder parses and validates the headers, and Info reports the
// stream parameters. The packet->PCM conversion is the codec seam (codec.go):
// until the phase-2/3 decoder is wired, Read and WriteTo return errCodecNotWired.
// A Decoder is not safe for concurrent use.
type Decoder struct {
	cr   *containerReader
	dec  frameDecoder // nil until the codec seam is wired
	info Info

	pcm         []byte // decoded, trimmed PCM awaiting delivery
	preSkipLeft int    // pre-skip samples per channel still to drop
	delivered   int64  // samples per channel already produced
	limit       int64  // total output samples per channel, or -1 until known
}

// NewDecoder reads and validates the OpusHead and OpusTags headers from r and
// returns a Decoder positioned at the first audio packet. Header parsing is
// real; the PCM output path is stubbed at the codec seam.
func NewDecoder(r io.Reader) (*Decoder, error) {
	cr, err := newContainerReader(r)
	if err != nil {
		return nil, err
	}
	d := &Decoder{
		cr:          cr,
		limit:       -1,
		preSkipLeft: int(cr.head.preSkip),
		info: Info{
			Channels:        int(cr.head.channels),
			InputSampleRate: cr.head.inputSampleRate,
			PreSkip:         int(cr.head.preSkip),
			OutputGain:      cr.head.outputGain,
		},
	}
	dec, err := newFrameDecoder(cr.head)
	if err != nil {
		if !errors.Is(err, errCodecNotWired) {
			return nil, err
		}
		d.dec = nil // PCM path stubbed at the seam
	} else {
		d.dec = dec
	}
	return d, nil
}

// Info returns the stream parameters parsed from OpusHead.
func (d *Decoder) Info() Info { return d.info }

// Read fills p with interleaved little-endian int16 PCM at 48 kHz, applying the
// pre-skip drop, the end-trim, and the OpusHead output gain. It returns io.EOF
// when the stream is exhausted.
func (d *Decoder) Read(p []byte) (int, error) {
	if d.dec == nil {
		return 0, errCodecNotWired
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
// caller can io.Copy a Decoder to a sink.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	if d.dec == nil {
		return 0, errCodecNotWired
	}
	var written int64
	for {
		if len(d.pcm) == 0 {
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
// start and the granule end-trim at the end. It reports done when the stream is
// fully consumed.
func (d *Decoder) fill() (done bool, err error) {
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
	// End-trim so the total delivered equals finalGranule - preSkip.
	if d.limit >= 0 && d.delivered+int64(perChan) > d.limit {
		keep := int(d.limit - d.delivered)
		if keep < 0 {
			keep = 0
		}
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
