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
	// discards before the target: 1.28 s. RFC 7845 section 4.6 recommends at least
	// 80 ms, but the codec state rebuilt from 80 ms is still far from the sequential
	// state on tonal content. Measured on this library's CELT output, the residual
	// stops shrinking at about 1.28 s, so a longer pre-roll only costs time.
	seekPreRoll = 61440
)

// seekIndex is what the first Length or seek call learns about the bound
// stream. It is valid only for the source passed to the latest NewDecoder or Reset.
type seekIndex struct {
	ready      bool
	audioStart int64 // absolute offset of the first audio page
	linkEnd    int64 // absolute offset just past the last page of the first logical stream
	length     int64 // samples per channel sequential decoding yields
	eos        bool  // the last page of the first link carries the end-of-stream flag
	err        error // a deterministic rejection of the stream, returned on every call
}

// pageRef locates one CRC-valid page in the source.
type pageRef struct {
	off, end   int64
	granule    int64
	serial     uint32
	hasGranule bool // granule >= 0 and at least one packet completes on the page
	eos        bool // the page carries the end-of-stream flag
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
		eos:        pg.flags&flagEOS != 0,
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
			adv := lim
			if lim == len(buf) && !s.eof {
				// A capture pattern may start in the last bytes of a full window and
				// end in the next one, so the next window starts a pattern early.
				adv = max(lim-(len(capture)-1), 1)
			}
			pos += int64(adv)
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
		if int64(len(buf)) < end-start && !s.eof {
			// A cached window can end before end; read afresh from start.
			s.valid = false
			if buf, err = s.window(start); err != nil {
				return pageRef{}, false, err
			}
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
// sequential decoding. Links are told apart by serial number: the first link ends
// at the first page of a different serial. A chain whose links share one serial is
// treated as one stream, so Length is derived from the final granule position of
// its last link while Read stops at the first end-of-stream page, and seeks can
// land in a later link.
//
// It needs the reader given to NewDecoder or Reset to be an io.ReadSeeker whose
// Seek works, including Seek relative to the end; otherwise it returns an error
// wrapping ErrSeekUnsupported. It also returns ErrSeekUnsupported for a stream
// whose first granule position is nonzero (RFC 7845 section 4.5) and for one with
// audio data on the OpusTags page, and for one whose final granule position is
// below the pre-skip. A zero-value Decoder returns an error.
//
// The first call finds the last page of the stream with a CRC-verified backward
// scan, through the source directly: the read position is restored afterwards (a failed restore is latched as described for SeekToSample), so
// Read output is the same whether or not Length was called. The result is cached
// for the bound stream; a source that grows afterwards is not re-measured. Length
// trusts the stream's final granule position, so a malformed stream whose granule
// exceeds what it can decode reports more than sequential decoding yields.
// A stream whose last page has no end-of-stream flag is not end-trimmed by
// sequential decoding, so for it Length is the decoded total (the last granule
// plus the packets that complete after it) and SeekToSample does not trim either.
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
// on the last page that ends at least 1.28 s (61440 samples) before the target,
// resets the codec and decodes and discards up to the target, all before
// returning. The next Read starts at exactly sampleIndex. For a target too close
// to the start for such a page to exist, the decoder restarts at the beginning and
// the output is bit-identical to sequential decoding.
//
// Otherwise the codec state is rebuilt from the pre-roll alone. The output usually
// equals sequential decoding exactly, but this is not guaranteed: on some
// stationary content (steady tones, stereo especially) small differences can
// persist for as long as the stream is read. Use SeekToSampleExact when the output
// must match sequential decoding exactly. Streams written by this package use
// large pages (up to about 255 packets), so a seek can decode and discard several
// seconds.
//
// It needs a seekable source as described for Length and returns the same errors
// for the same reasons; a negative sampleIndex returns ErrInvalidSeek. Those
// errors, and any error from reading the stream's tail while indexing it, leave
// the decoder unchanged, except that a failure to restore the read position is
// latched like the failures below. A failure after the source has been
// repositioned (an I/O error, a corrupt page or packet, a packet over the size
// limit) is latched: Read and WriteTo return it until the next successful
// SeekToSample, SeekToSampleExact or Reset. Seeks trust the stream's granule
// positions; a malformed stream can land on the wrong sample. Only the first
// logical stream is covered, and links are told apart by serial as described for
// Length.
func (d *Decoder) SeekToSample(sampleIndex int64) (int64, error) {
	if err := d.seekCheck(sampleIndex); err != nil {
		return 0, err
	}
	d.inSequence = false
	if sampleIndex >= d.idx.length {
		return d.seekToEnd(d.idx.length), nil
	}
	// Sequential decoding trims only after an end-of-stream page.
	limit := int64(-1)
	if d.idx.eos {
		limit = d.idx.length
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
		if err := d.rewind(); err != nil {
			return d.seekFailed(err)
		}
		d.resetForSeek(sampleIndex, d.info.PreSkip, 0, limit)
	} else {
		if _, err := d.rs.Seek(land.off, io.SeekStart); err != nil {
			return d.seekFailed(err)
		}
		d.cr.repositionAt(d.rs)
		if err := readPage(d.cr.br, &d.cr.pg); err != nil {
			return d.seekFailed(err)
		}
		if err := d.cr.processPage(&d.cr.pg); err != nil {
			return d.seekFailed(err)
		}
		// The packets that complete on this page start before its granule; the
		// partial packet at its end is the first one the decoder needs.
		d.cr.queue = d.cr.queue[:0]
		d.cr.qi = 0
		d.resetForSeek(sampleIndex, 0, land.granule-int64(d.info.PreSkip), limit)
	}

	return d.decodeToTarget(sampleIndex)
}

// SeekToSampleExact positions the decoder so the next Read returns the sample at
// index sampleIndex, as SeekToSample does, but the bytes Read and WriteTo yield
// from there on are identical to those of decoding the stream sequentially from
// its start. It returns that index. A sampleIndex at or past Length lands at the
// end of the stream and returns Length; the next Read returns io.EOF.
//
// It decodes forward from the current position when the decoder is at or before
// sampleIndex and nothing has disturbed its codec state since it was bound or last
// seeked exactly: no SeekToSample, no seek to the end of the stream, no failed seek
// and no read error. Otherwise it restarts at the first audio page and decodes
// from there. Either way the cost is
// linear in the samples decoded, up to everything before sampleIndex, unlike
// SeekToSample, which decodes a bounded pre-roll. The result, whether output,
// error or position, does not depend on earlier calls.
//
// It needs a seekable source as described for Length and returns the same errors
// for the same reasons; a negative sampleIndex returns ErrInvalidSeek and leaves
// the decoder unchanged. A failure after the decoder has started moving (an I/O
// error, a corrupt page or packet) is latched like those of SeekToSample. Only the
// first logical stream is covered, and a Decoder must not be used concurrently.
func (d *Decoder) SeekToSampleExact(sampleIndex int64) (int64, error) {
	if err := d.seekCheck(sampleIndex); err != nil {
		return 0, err
	}
	if sampleIndex >= d.idx.length {
		return d.seekToEnd(d.idx.length), nil
	}

	// delivered is the index just past the samples in d.pcm. A Read that stopped
	// inside a sample leaves its tail first in d.pcm; the whole samples after it
	// start at delivered-whole, so earlier indexes are out of reach.
	frame := int64(2 * d.info.Channels)
	whole := int64(len(d.pcm)) / frame
	if d.inSequence && sampleIndex >= d.delivered-whole {
		if sampleIndex <= d.delivered {
			// Inside the buffered samples: drop the bytes before sampleIndex.
			d.pcm = d.pcm[int64(len(d.pcm))-(d.delivered-sampleIndex)*frame:]
			return sampleIndex, nil
		}
		d.pcm = d.pcm[:0]
		d.skipTo = sampleIndex
	} else {
		if err := d.rewind(); err != nil {
			return d.seekFailed(err)
		}
		// A limit of -1 lets fill learn the end trim from the container, as
		// sequential decoding does, so the decoder is in sequence again.
		d.resetForSeek(sampleIndex, d.info.PreSkip, 0, -1)
		d.inSequence = true
	}

	return d.decodeToTarget(sampleIndex)
}

// rewind points the container reader at the first audio page of the bound source,
// as a decoder that has just been bound to it.
func (d *Decoder) rewind() error {
	if _, err := d.rs.Seek(d.base, io.SeekStart); err != nil {
		return err
	}
	d.br.Reset(d.rs)
	// A fresh reader, not d.cr reset in place: a failed rebuild must leave d.cr
	// intact for the landing seek that recovers from it.
	cr, err := newContainerReader(d.br, nil)
	if err != nil {
		return err
	}
	d.cr = cr
	return nil
}

// seekCheck returns the error a seek reports before it moves anything: no codec,
// a source that cannot seek, a negative index, or a failure to index the stream.
func (d *Decoder) seekCheck(sampleIndex int64) error {
	if d.dec == nil {
		return errUninitialized
	}
	if d.rs == nil {
		return ErrSeekUnsupported
	}
	if sampleIndex < 0 {
		return ErrInvalidSeek
	}
	return d.ensureIndex()
}

// decodeToTarget decodes and discards until the next sample is d.skipTo and
// returns the index the next Read starts at: sampleIndex, or the decoded end when
// the stream ends first because its granules over-claimed.
func (d *Decoder) decodeToTarget(sampleIndex int64) (int64, error) {
	for d.delivered < d.skipTo {
		done, err := d.fill()
		if err != nil {
			return d.seekFailed(err)
		}
		if done {
			return d.seekToEnd(d.delivered), nil
		}
	}
	return sampleIndex, nil
}

// resetForSeek puts every piece of decode state in the condition of a decoder
// that has just been bound to the stream and is about to decode from the packet
// at sample index delivered, with preSkip samples still to drop. limit is the
// end-trim limit, or -1 for fill to learn it from the container.
func (d *Decoder) resetForSeek(target int64, preSkip int, delivered, limit int64) {
	d.dec.reset()
	d.preSkipLeft = preSkip
	d.delivered = delivered
	d.skipTo = target
	d.limit = limit
	d.pcm = d.pcm[:0]
	d.atEnd = false
	d.err = nil
}

// seekToEnd positions the decoder at end of stream and returns n.
func (d *Decoder) seekToEnd(n int64) int64 {
	d.inSequence = false
	d.atEnd = true
	d.pcm = d.pcm[:0]
	d.err = nil
	return n
}

// seekFailed latches err, which Read and WriteTo return until the next successful
// seek, because the source and container reader are no longer in step.
func (d *Decoder) seekFailed(err error) (int64, error) {
	d.inSequence = false
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
		d.inSequence = false
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
// I/O. A stream sequential decoding cannot be described by a length (audio on the
// OpusTags page, an initial granule offset, a final granule below the pre-skip)
// returns a cached error wrapping ErrSeekUnsupported. When the last page of the
// link lacks the end-of-stream flag, length is the decoded total instead of the
// last granule position, and idx.eos is false.
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
	var prev pageRef // the granule-bearing page before the final one
	havePrev := false
	for end, first := idx.linkEnd, true; end > idx.audioStart; {
		p, ok, err := sc.findBackward(idx.audioStart, end)
		if err != nil {
			return idx, false, err
		}
		if !ok {
			break
		}
		if first && p.serial == serial {
			idx.eos = p.eos
			first = false
		}
		if p.serial == serial && p.hasGranule {
			if final < 0 {
				final = p.granule
				if idx.eos {
					break
				}
			} else {
				prev, havePrev = p, true
				break
			}
		}
		end = p.off
	}
	if final < 0 {
		return idx, false, nil
	}
	if !idx.eos {
		// Without an end-of-stream page sequential decoding never trims, so the
		// length is the decoded total: the last earlier granule plus the packets
		// that complete after it. A granule that disagrees with the samples
		// (a malformed stream) is not trusted.
		total, c, err := d.decodedTotal(idx, prev, havePrev)
		if err != nil {
			return idx, c, err
		}
		idx.length = max(total-preSkip, 0)
		cache, err = d.checkInitialGranule(idx)
		return idx, cache, err
	}
	if final < preSkip {
		// Sequential decoding does not trim when the end-trim limit is negative, so
		// no length can describe what it yields for this stream.
		return idx, true, fmt.Errorf("%w: final granule position %d is below the pre-skip %d", ErrSeekUnsupported, final, preSkip)
	}
	idx.length = final - preSkip
	cache, err = d.checkInitialGranule(idx)
	return idx, cache, err
}

// decodedTotal returns the samples per channel at 48 kHz that decoding every packet
// of the first link yields before the pre-skip is dropped. It starts from the
// granule of prev (the last granule-bearing page before the final one), or from
// zero at the first audio page when there is none, and adds the duration of every
// packet that completes after that point. cache has the meaning it has for
// buildIndex.
func (d *Decoder) decodedTotal(idx seekIndex, prev pageRef, havePrev bool) (total int64, cache bool, err error) {
	start := idx.audioStart
	if havePrev {
		start = prev.off
	}
	if _, err := d.rs.Seek(start, io.SeekStart); err != nil {
		return 0, false, err
	}
	// This pass and checkInitialGranule read through their own small buffers, not
	// d.br: d.br holds the sequential read-ahead that ensureIndex must leave intact.
	br := bufio.NewReader(io.LimitReader(d.rs, idx.linkEnd-start))
	cr := d.cr.auxReader(br)
	if havePrev {
		total = prev.granule
	}
	for first := true; ; first = false {
		pg := &cr.pg
		if err := readPage(br, pg); err != nil {
			if errors.Is(err, io.EOF) {
				return total, false, nil
			}
			return 0, false, err
		}
		if err := cr.processPage(pg); err != nil {
			return 0, false, err
		}
		if first && havePrev {
			// The packets completing on prev are already in its granule; the tail
			// fragment it leaves in cr.partial belongs to the next page.
			cr.queue = cr.queue[:0]
			continue
		}
		for _, pkt := range cr.queue {
			n, err := packetSamples48k(pkt)
			if err != nil {
				return 0, true, fmt.Errorf("%w: %w", ErrSeekUnsupported, err)
			}
			total += int64(n)
		}
	}
}

// checkInitialGranule rejects streams whose granule positions do not count from
// zero at the first audio packet, because the seek arithmetic assumes they do.
func (d *Decoder) checkInitialGranule(idx seekIndex) (cache bool, err error) {
	if _, err := d.rs.Seek(idx.audioStart, io.SeekStart); err != nil {
		return false, err
	}
	br := bufio.NewReader(io.LimitReader(d.rs, idx.linkEnd-idx.audioStart))
	cr := d.cr.auxReader(br)
	for {
		pg := &cr.pg
		if err := readPage(br, pg); err != nil {
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
