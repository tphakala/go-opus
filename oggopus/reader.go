package oggopus

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
)

// pageInfo records the structural facts of one parsed page. It exists so tests
// (and future diagnostics) can assert granule progression and paging without
// re-decoding, the container-structure check the go-flac seekability regression
// argues for.
type pageInfo struct {
	granule   int64
	flags     headerType
	sequence  uint32
	packets   int  // packets that completed on this page
	continued bool // page begins with the tail of a prior packet
}

// containerReader parses an Ogg Opus stream from an io.Reader: it reassembles
// packets across page boundaries, exposes the OpusHead and OpusTags, and tracks
// the granule accounting needed for pre-skip and end-trim. It is
// codec-independent; turning packets into PCM is the decoder's job.
type containerReader struct {
	br *bufio.Reader

	serial    uint32
	serialSet bool
	head      opusHead
	tags      opusTags

	partial []byte   // bytes of a packet still spanning pages
	queue   [][]byte // completed packets from the current page; each aliases arena
	qi      int      // index of the next packet to serve from queue

	pg    page   // readPage scratch, reused for every page
	arena []byte // backing store for the packets in queue, rewritten for every page

	lastGranule  int64 // granule of the most recent page that completed a packet
	finalGranule int64 // granule of the EOS page, or -1 if not yet seen
	sawEOS       bool

	pages []pageInfo

	// consumed counts the bytes of every page read so far (header, segment table
	// and body). After newContainerReader it is the length of the header pages, so
	// the first audio page begins at the stream's starting offset plus consumed.
	consumed int64
}

// maxReusedBuf is the largest capacity of the packet arena or the partial-packet
// buffer that a containerReader keeps. With packets no longer than a page body
// (an Opus packet is at most 48 frames of 1275 bytes), either buffer needs at most
// two page bodies: the arena holds one page body plus a carried fragment. Growing
// a slice can overshoot its need by a quarter plus 192 bytes (nextslicecap,
// runtime/slice.go:341, Go 1.27.3) and a large allocation is then rounded up to an
// 8 KiB page (roundupsize, runtime/msize.go:31), so the limit adds that headroom
// and such a buffer is never dropped. A larger one comes from a packet that spans
// several pages, such as a huge OpusTags. processPage drops it once that packet is
// done, and newContainerReader does not carry it over from a previous reader, so
// one such packet does not pin its memory for the rest of the stream or in a
// pooled Decoder.
const maxReusedBuf = 2*maxPageBodyLen + 2*maxPageBodyLen/4 + 16<<10

// newContainerReader reads and validates the OpusHead and OpusTags header
// packets, leaving the reader positioned at the first audio packet. A first
// packet without the OpusHead magic returns an error wrapping ErrNotOpus.
//
// br is the buffered source. reuse, when not nil, is a reader of an earlier stream
// whose page, packet and queue buffers are recycled and whose struct is
// overwritten; the caller must not use it afterwards.
func newContainerReader(br *bufio.Reader, reuse *containerReader) (*containerReader, error) {
	cr := reuse
	if cr == nil {
		cr = new(containerReader)
	}
	arena, partial := cr.arena[:0], cr.partial[:0]
	if cap(arena) > maxReusedBuf {
		arena = nil
	}
	if cap(partial) > maxReusedBuf {
		partial = nil
	}
	// Stale queue slots would keep a dropped arena reachable.
	queue := cr.queue[:cap(cr.queue)]
	clear(queue)
	*cr = containerReader{br: br, finalGranule: -1, pg: page{raw: cr.pg.raw}, arena: arena, queue: queue[:0], partial: partial}
	hp, err := cr.nextRawPacket()
	if err != nil {
		return nil, headerErr("OpusHead", err)
	}
	head, err := parseOpusHead(hp)
	if err != nil {
		return nil, err
	}
	tp, err := cr.nextRawPacket()
	if err != nil {
		return nil, headerErr("OpusTags", err)
	}
	tags, err := parseOpusTags(tp)
	if err != nil {
		return nil, err
	}
	cr.head = head
	cr.tags = tags
	return cr, nil
}

// nextPacket returns the next audio packet, or io.EOF when the stream ends. The
// packet aliases the reader's arena and is valid until the next call to nextPacket
// or nextRawPacket, which may read a page into that arena.
func (cr *containerReader) nextPacket() ([]byte, error) {
	return cr.nextRawPacket()
}

// totalSamples reports the playable sample count per channel at 48 kHz derived
// from the granule accounting (final granule minus pre-skip). The boolean is
// false until the EOS page has been read, i.e. until nextPacket has returned
// io.EOF on a stream that carried an end-of-stream page.
func (cr *containerReader) totalSamples() (int64, bool) {
	if cr.finalGranule < 0 {
		return 0, false
	}
	return cr.finalGranule - int64(cr.head.preSkip), true
}

// nextRawPacket returns the next reassembled packet from the logical stream,
// reading pages as needed and adding the size of each page read to consumed. It
// serves the OpusHead and OpusTags packets too; newContainerReader consumes those
// before returning. Like nextPacket, the returned packet is valid only until the
// next call.
func (cr *containerReader) nextRawPacket() ([]byte, error) {
	for {
		if cr.qi < len(cr.queue) {
			p := cr.queue[cr.qi]
			cr.qi++
			return p, nil
		}
		if cr.sawEOS {
			return nil, io.EOF
		}
		pg := &cr.pg
		if err := readPage(cr.br, pg); err != nil {
			return nil, err // io.EOF here means a clean end with no EOS page
		}
		cr.consumed += int64(pageHeaderFixedLen + len(pg.segTable) + len(pg.body))
		if err := cr.processPage(pg); err != nil {
			return nil, err
		}
	}
}

// repositionAt rebinds the reader to r, which must be positioned at a page
// boundary, and drops all packet state so the next page read starts clean. It
// keeps the parsed headers and serial. lastGranule is left for the caller to set
// by processing the page it lands on.
func (cr *containerReader) repositionAt(r io.Reader) {
	cr.br.Reset(r)
	cr.queue = cr.queue[:0]
	cr.qi = 0
	cr.partial = cr.partial[:0]
	cr.sawEOS = false
	cr.finalGranule = -1
}

// processPage reassembles the packets on one page into the queue and updates the
// granule and end-of-stream accounting.
func (cr *containerReader) processPage(pg *page) error {
	if !cr.serialSet {
		cr.serial = pg.serial
		cr.serialSet = true
		if pg.flags&flagBOS == 0 {
			return errors.New("oggopus: first page missing BOS flag")
		}
	} else if pg.serial != cr.serial {
		return fmt.Errorf("oggopus: unexpected serial %#08x (chained or multiplexed streams are unsupported)", pg.serial)
	}

	// If this page does not continue a packet but one was left dangling, the
	// previous packet was truncated; drop it rather than fuse across a gap.
	if pg.flags&flagContinued == 0 && len(cr.partial) > 0 {
		cr.partial = cr.partial[:0]
	}

	cr.queue = cr.queue[:0]
	cr.qi = 0
	// Packets are copied into the arena, which is rewritten for every page. Growing
	// it once up front keeps it from reallocating mid-page, and only a page that
	// completes a packet writes to it, so a run of continuation-only pages grows
	// partial alone. A packet that spanned several pages can leave the arena far
	// larger than a page; such an arena is dropped rather than held for the rest of
	// the stream.
	if cap(cr.arena) > maxReusedBuf {
		cr.arena = nil
	} else {
		cr.arena = cr.arena[:0]
	}
	if pg.completesPacket() {
		cr.arena = slices.Grow(cr.arena, len(cr.partial)+len(pg.body))
	}
	off := 0
	for _, l := range pg.segTable {
		cr.partial = append(cr.partial, pg.body[off:off+int(l)]...)
		off += int(l)
		if l < maxSegmentLen {
			start := len(cr.arena)
			cr.arena = append(cr.arena, cr.partial...)
			cr.queue = append(cr.queue, cr.arena[start:len(cr.arena):len(cr.arena)])
			if cap(cr.partial) > maxReusedBuf {
				cr.partial = nil // the multi-page packet it held is complete
			} else {
				cr.partial = cr.partial[:0]
			}
		}
	}

	if pg.granule != granuleNone {
		cr.lastGranule = pg.granule
	}
	if pg.flags&flagEOS != 0 {
		cr.sawEOS = true
		cr.finalGranule = cr.lastGranule
	}
	cr.pages = append(cr.pages, pageInfo{
		granule:   pg.granule,
		flags:     pg.flags,
		sequence:  pg.sequence,
		packets:   len(cr.queue),
		continued: pg.flags&flagContinued != 0,
	})
	return nil
}

// headerErr wraps an error from reading a header packet, mapping a premature EOF
// to a clearer "truncated" message.
func headerErr(which string, err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("oggopus: stream ended before %s", which)
	}
	return fmt.Errorf("oggopus: reading %s: %w", which, err)
}
