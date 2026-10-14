package oggopus

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const (
	// maxPageLen is the largest possible encoded page: fixed header, a full segment
	// table and a full body.
	maxPageLen = pageHeaderFixedLen + maxPageSegments + maxPageBodyLen
	// scanWindow is the size of the scratch window page scans read through. It
	// holds at least one whole page wherever a page starts inside its first half.
	scanWindow = 2 * maxPageLen
	// seekPreRoll is the number of samples per channel the codec decodes and
	// discards before the target (RFC 7845 section 4.6 recommends at least 80 ms).
	seekPreRoll = 3840
)

// seekIndex is what the first Length or SeekToSample call learns about the bound
// stream. It is valid only for the source passed to the latest NewDecoder or Reset.
type seekIndex struct {
	ready      bool
	audioStart int64 // absolute offset of the first audio page
	linkEnd    int64 // absolute offset just past the last page of the first logical stream
	length     int64 // samples per channel sequential decoding yields
	err        error // a deterministic rejection of the stream, returned on every call
}

// pageRef locates one CRC-valid page in the source.
type pageRef struct {
	off, end   int64
	granule    int64
	serial     uint32
	hasGranule bool // granule >= 0 and at least one packet completes on the page
}

// pageScanner reads and parses pages from the raw source through one reused
// window, remembering the last window so neighbouring pages cost no extra reads.
type pageScanner struct {
	rs    io.ReadSeeker
	buf   []byte
	off   int64
	n     int
	eof   bool // the window reaches the end of the source
	valid bool
}

func (d *Decoder) scanner() *pageScanner {
	if d.scan == nil {
		d.scan = &pageScanner{rs: d.rs}
	}
	return d.scan
}

// window returns the bytes starting at off: up to scanWindow of them, and at least
// maxPageLen unless the source ends first.
func (s *pageScanner) window(off int64) ([]byte, error) {
	if s.valid && off >= s.off && off < s.off+int64(s.n) {
		if rem := s.off + int64(s.n) - off; rem >= maxPageLen || s.eof {
			return s.buf[off-s.off : s.n], nil
		}
	}
	if s.buf == nil {
		s.buf = make([]byte, scanWindow)
	}
	s.valid = false
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := io.ReadFull(s.rs, s.buf)
	switch {
	case err == nil:
		s.eof = false
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		s.eof = true
	default:
		return nil, err
	}
	s.off, s.n, s.valid = off, n, true
	return s.buf[:n], nil
}

func refOf(pg *page, off int64, n int) pageRef {
	return pageRef{
		off:        off,
		end:        off + int64(n),
		granule:    pg.granule,
		serial:     pg.serial,
		hasGranule: pg.granule >= 0 && pg.completesPacket(),
	}
}

// pageAt parses the page that starts exactly at off. It returns nil, nil when no
// valid page starts there.
func (s *pageScanner) pageAt(off int64) (ref pageRef, ok bool, err error) {
	buf, err := s.window(off)
	if err != nil {
		return pageRef{}, false, err
	}
	pg, n, perr := parsePage(buf)
	if perr != nil {
		return pageRef{}, false, nil //nolint:nilerr // not a page at this offset is a result, not a failure
	}
	return refOf(pg, off, n), true, nil
}

var capture = []byte(capturePattern)

// findForward returns the first CRC-valid page whose start offset is in
// [from, bound), or nil. The page itself may extend past bound.
func (s *pageScanner) findForward(from, bound int64) (ref pageRef, ok bool, err error) {
	pos := from
	for pos < bound {
		buf, err := s.window(pos)
		if err != nil {
			return pageRef{}, false, err
		}
		if len(buf) == 0 {
			return pageRef{}, false, nil
		}
		lim := len(buf)
		if int64(lim) > bound-pos {
			lim = int(bound - pos)
		}
		i := 0
		moved := false
		for i < lim {
			j := bytes.Index(buf[i:min(len(buf), lim+len(capture)-1)], capture)
			if j < 0 {
				break
			}
			c := i + j
			if c >= lim {
				break
			}
			pg, n, perr := parsePage(buf[c:])
			if perr == nil {
				return refOf(pg, pos+int64(c), n), true, nil
			}
			if errors.Is(perr, errShortPage) && c > 0 && len(buf)-c < maxPageLen && !s.eof {
				// The page may fit once the window starts here.
				pos += int64(c)
				moved = true
				break
			}
			i = c + 1
		}
		if !moved {
			pos += int64(lim)
		}
	}
	return pageRef{}, false, nil
}

// findBackward returns the last CRC-valid page that starts at or after floor and
// ends at or before end, or nil.
func (s *pageScanner) findBackward(floor, end int64) (ref pageRef, ok bool, err error) {
	for end > floor {
		start := max(floor, end-scanWindow)
		buf, err := s.window(start)
		if err != nil {
			return pageRef{}, false, err
		}
		n := min(int64(len(buf)), end-start)
		buf = buf[:n]
		i := len(buf)
		for i >= len(capture) {
			c := bytes.LastIndex(buf[:i], capture)
			if c < 0 {
				break
			}
			if pg, pn, perr := parsePage(buf[c:]); perr == nil {
				return refOf(pg, start+int64(c), pn), true, nil
			}
			i = c + len(capture) - 1
		}
		if start == floor {
			return pageRef{}, false, nil
		}
		// Step back, overlapping by the largest page so a page straddling the
		// window edge is seen whole by the next window.
		end = start + maxPageLen - 1
	}
	return pageRef{}, false, nil
}

// Length returns the number of samples per channel at 48 kHz that Read and WriteTo
// yield over the whole first logical stream: the pre-skip is excluded and the
// granule end-trim applied, so the result equals the sequential decode length
// exactly. For a stream with several chained links only the first counts, as in
// sequential decoding.
//
// It needs the reader given to NewDecoder or Reset to be an io.ReadSeeker whose
// Seek works, including Seek relative to the end; otherwise it returns an error
// wrapping ErrSeekUnsupported. It also returns ErrSeekUnsupported for a stream
// whose first granule position is nonzero (RFC 7845 section 4.5) and for one with
// audio data on the OpusTags page. A zero-value Decoder returns an error.
//
// The first call finds the last page of the stream with a CRC-verified backward
// scan, through the source directly: the read position is left where it was, so
// Read output is the same whether or not Length was called. The result is cached
// for the bound stream; a source that grows afterwards is not re-measured. Length
// trusts the stream's final granule position, so a malformed stream whose granule
// exceeds what it can decode reports more than sequential decoding yields.
func (d *Decoder) Length() (int64, error) {
	if d.dec == nil {
		return 0, errUninitialized
	}
	if d.rs == nil {
		return 0, ErrSeekUnsupported
	}
	if err := d.ensureIndex(); err != nil {
		return 0, err
	}
	return d.idx.length, nil
}

// SeekToSample positions the decoder so the next Read returns the sample at index
// sampleIndex (per channel, counted from the first sample after the pre-skip) and
// returns that index. A sampleIndex at or past Length lands at the end of the
// stream and returns Length; the next Read returns io.EOF.
//
// It bisects the stream's pages by granule position (RFC 7845 section 4.6), lands
// on a page that ends at least 80 ms (3840 samples) before the target, resets the
// codec and decodes and discards up to the target, all before returning. Output
// is aligned to the sample exactly. For a target below 3840 samples the decoder
// restarts at the beginning and the output is bit-identical to sequential
// decoding; for a later target the codec has had only the pre-roll to converge,
// so the first samples can differ slightly from sequential decoding. Streams
// written by this package use large pages (about 255 packets), so the pre-roll
// can be several seconds of decode-and-discard.
//
// It needs a seekable source as described for Length and returns the same errors
// for the same reasons; a negative sampleIndex returns ErrInvalidSeek. Those
// errors, and any error from reading the stream's tail while indexing it, leave
// the decoder unchanged. A failure after the source has been repositioned (an I/O
// error, a corrupt page or packet) is latched: Read and WriteTo return it until
// the next successful SeekToSample or Reset. Seeks trust the stream's granule
// positions; a malformed stream can land on the wrong sample. Only the first
// logical stream is covered.
func (d *Decoder) SeekToSample(sampleIndex int64) (int64, error) {
	if d.dec == nil {
		return 0, errUninitialized
	}
	if d.rs == nil {
		return 0, ErrSeekUnsupported
	}
	if sampleIndex < 0 {
		return 0, ErrInvalidSeek
	}
	if err := d.ensureIndex(); err != nil {
		return 0, err
	}
	if sampleIndex >= d.idx.length {
		return d.seekToEnd(d.idx.length), nil
	}

	var land pageRef
	var haveLanding bool
	if pre := sampleIndex - seekPreRoll; pre >= 0 {
		var err error
		if land, haveLanding, err = d.chooseLanding(pre); err != nil {
			return d.seekFailed(err)
		}
	}
	if !haveLanding {
		// Decode from the start, applying the pre-skip, exactly as sequential
		// decoding does.
		if _, err := d.rs.Seek(d.base, io.SeekStart); err != nil {
			return d.seekFailed(err)
		}
		cr, err := newContainerReader(d.rs)
		if err != nil {
			return d.seekFailed(err)
		}
		d.cr = cr
		d.resetForSeek(sampleIndex, d.info.PreSkip, 0)
	} else {
		if _, err := d.rs.Seek(land.off, io.SeekStart); err != nil {
			return d.seekFailed(err)
		}
		d.cr.repositionAt(d.rs)
		pg, err := readPage(d.cr.br)
		if err != nil {
			return d.seekFailed(err)
		}
		if err := d.cr.processPage(pg); err != nil {
			return d.seekFailed(err)
		}
		// The packets that complete on this page start before its granule; the
		// partial packet at its end is the first one the decoder needs.
		d.cr.queue = d.cr.queue[:0]
		d.cr.qi = 0
		d.resetForSeek(sampleIndex, 0, land.granule-int64(d.info.PreSkip))
	}

	for d.delivered < d.skipTo {
		done, err := d.fill()
		if err != nil {
			return d.seekFailed(err)
		}
		if done {
			// The stream ended before the target: its granules over-claimed.
			return d.seekToEnd(d.delivered), nil
		}
	}
	return sampleIndex, nil
}

// resetForSeek puts every piece of decode state in the condition of a decoder
// that has just been bound to the stream and is about to decode from the packet
// at sample index delivered, with preSkip samples still to drop.
func (d *Decoder) resetForSeek(target int64, preSkip int, delivered int64) {
	d.dec.reset()
	d.preSkipLeft = preSkip
	d.delivered = delivered
	d.skipTo = target
	d.limit = d.idx.length
	d.pcm = d.pcm[:0]
	d.atEnd = false
	d.err = nil
}

// seekToEnd positions the decoder at end of stream and returns n.
func (d *Decoder) seekToEnd(n int64) int64 {
	d.atEnd = true
	d.pcm = d.pcm[:0]
	d.err = nil
	return n
}

// seekFailed latches err, which Read and WriteTo return until the next successful
// seek, because the source and container reader are no longer in step.
func (d *Decoder) seekFailed(err error) (int64, error) {
	d.err = err
	d.pcm = d.pcm[:0]
	return 0, err
}

// ensureIndex builds the stream index on first use. The build moves the source,
// so it records the position first and restores it on every path. A deterministic
// rejection is cached; an I/O error is not, and the next call retries.
func (d *Decoder) ensureIndex() error {
	if d.idx.ready {
		return d.idx.err
	}
	cur, err := d.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSeekUnsupported, err)
	}
	idx, cache, buildErr := d.buildIndex()
	if _, err := d.rs.Seek(cur, io.SeekStart); err != nil {
		d.err = fmt.Errorf("oggopus: restoring the read position: %w", err)
		d.pcm = d.pcm[:0]
		return d.err
	}
	if buildErr != nil {
		if cache {
			idx.ready, idx.err = true, buildErr
			d.idx = idx
		}
		return buildErr
	}
	idx.ready = true
	d.idx = idx
	return nil
}

// buildIndex measures the first logical stream. cache reports whether a returned
// error is a property of the stream (and so worth remembering) rather than of the
// I/O.
func (d *Decoder) buildIndex() (idx seekIndex, cache bool, err error) {
	idx = d.idx
	if d.tagsBad {
		return idx, true, fmt.Errorf("%w: audio data shares the OpusTags page (RFC 7845 section 3)", ErrSeekUnsupported)
	}
	sc := d.scanner()
	serial := d.cr.serial
	preSkip := int64(d.info.PreSkip)

	size, err := d.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return idx, false, fmt.Errorf("%w: %w", ErrSeekUnsupported, err)
	}
	if size <= idx.audioStart {
		idx.linkEnd = idx.audioStart
		return idx, false, nil
	}
	last, found, err := sc.findBackward(idx.audioStart, size)
	if err != nil {
		return idx, false, err
	}
	if !found {
		return idx, true, errors.New("oggopus: no complete audio page found after the headers")
	}

	if last.serial == serial {
		idx.linkEnd = last.end
	} else {
		// A chained file: find the first page of another stream. Pages of the
		// first stream come first, so bisect on the serial.
		lo, hi := idx.audioStart, last.off
		for hi-lo >= scanWindow {
			mid := lo + (hi-lo)/2
			p, ok, err := sc.findForward(mid, hi)
			if err != nil {
				return idx, false, err
			}
			if !ok || p.serial != serial {
				// A page of ours that starts before mid may still extend past it,
				// so mid bounds starts only; the walk below settles the boundary.
				hi = mid
				continue
			}
			lo = p.end
		}
		off := lo
		for off < size {
			p, ok, err := sc.pageAt(off)
			if err != nil {
				return idx, false, err
			}
			if !ok || p.serial != serial {
				break
			}
			off = p.end
		}
		idx.linkEnd = off
	}

	final := int64(-1)
	for end := idx.linkEnd; end > idx.audioStart; {
		p, ok, err := sc.findBackward(idx.audioStart, end)
		if err != nil {
			return idx, false, err
		}
		if !ok {
			break
		}
		if p.serial == serial && p.hasGranule {
			final = p.granule
			break
		}
		end = p.off
	}
	if final < 0 {
		return idx, false, nil
	}
	idx.length = max(final-preSkip, 0)
	cache, err = d.checkInitialGranule(idx)
	return idx, cache, err
}

// checkInitialGranule rejects streams whose granule positions do not count from
// zero at the first audio packet, because the seek arithmetic assumes they do.
func (d *Decoder) checkInitialGranule(idx seekIndex) (cache bool, err error) {
	if _, err := d.rs.Seek(idx.audioStart, io.SeekStart); err != nil {
		return false, err
	}
	br := bufio.NewReader(io.LimitReader(d.rs, idx.linkEnd-idx.audioStart))
	cr := &containerReader{br: br, serial: d.cr.serial, serialSet: true, finalGranule: -1}
	for {
		pg, err := readPage(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		if err := cr.processPage(pg); err != nil {
			return false, err
		}
		if len(cr.queue) == 0 {
			continue
		}
		if pg.granule == granuleNone {
			return true, errors.New("oggopus: first audio page completes a packet but has no granule position")
		}
		var sum int64
		for _, pkt := range cr.queue {
			n, err := packetSamples48k(pkt)
			if err != nil {
				return true, fmt.Errorf("%w: %w", ErrSeekUnsupported, err)
			}
			sum += int64(n)
		}
		switch {
		case pg.granule > sum:
			return true, fmt.Errorf("%w: stream starts at a nonzero granule offset (RFC 7845 section 4.5)", ErrSeekUnsupported)
		case pg.granule < sum && pg.flags&flagEOS == 0:
			return true, fmt.Errorf("oggopus: invalid initial granule position %d for %d samples on the first audio page (RFC 7845 section 4.5)", pg.granule, sum)
		}
		return false, nil
	}
}

// findGranulePage returns the first page of the stream that starts in [from, bound)
// and carries a granule position.
func (d *Decoder) findGranulePage(from, bound int64) (ref pageRef, ok bool, err error) {
	sc := d.scanner()
	for from < bound {
		p, ok, err := sc.findForward(from, bound)
		if err != nil || !ok {
			return pageRef{}, false, err
		}
		if p.serial == d.cr.serial && p.hasGranule {
			return p, true, nil
		}
		from = p.end
	}
	return pageRef{}, false, nil
}

// chooseLanding returns the last page of the stream whose first new sample, the
// granule position minus the pre-skip, lies in [0, pre], or nil when there is
// none and decoding must restart at the beginning.
func (d *Decoder) chooseLanding(pre int64) (best pageRef, have bool, err error) {
	sc := d.scanner()
	preSkip := int64(d.info.PreSkip)
	lo, hi := d.idx.audioStart, d.idx.linkEnd
	for hi-lo >= scanWindow {
		mid := lo + (hi-lo)/2
		p, ok, err := d.findGranulePage(mid, hi)
		if err != nil {
			return pageRef{}, false, err
		}
		if !ok || p.granule-preSkip > pre {
			hi = mid
			continue
		}
		if p.granule-preSkip >= 0 {
			best, have = p, true
		}
		lo = p.end
	}
	for off := lo; off < hi; {
		p, found, err := sc.pageAt(off)
		if err != nil {
			return pageRef{}, false, err
		}
		if !found {
			return pageRef{}, false, fmt.Errorf("oggopus: no valid page at offset %d", off)
		}
		if p.serial != d.cr.serial {
			return pageRef{}, false, fmt.Errorf("oggopus: unexpected serial %#08x (chained or multiplexed streams are unsupported)", p.serial)
		}
		if p.hasGranule {
			pos := p.granule - preSkip
			if pos > pre {
				break
			}
			if pos >= 0 {
				best, have = p, true
			}
		}
		off = p.end
	}
	return best, have, nil
}
