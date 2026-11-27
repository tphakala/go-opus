package oggopus

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
)

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

	partial []byte // bytes of a packet still spanning pages
	// queue holds the packets completed on the current page. A packet that starts
	// and ends on the page aliases pg.body; any other aliases arena.
	queue [][]byte
	qi    int // index of the next packet to serve from queue

	pg page // readPage scratch, reused for every page
	// arena holds owned copies of the packets in queue that pg.body cannot serve:
	// the header packets and a packet completed from earlier pages. It is
	// rewritten for every page.
	arena []byte

	lastGranule  int64 // granule of the most recent page that completed a packet
	finalGranule int64 // granule of the EOS page, or -1 if not yet seen
	sawEOS       bool

	// limit is the largest audio packet processPage assembles. newContainerReader
	// starts it at one stream's limit, which no single page reaches, and raises it
	// from OpusHead. Zero rejects every non-empty packet, so a reader built without
	// it fails rather than growing without bound.
	limit int
	// headers counts the header packets (OpusHead, then OpusTags) still to
	// complete. processPage keeps at most maxHeaderPacketLen bytes of each and drops
	// the rest, up to maxHeaderSpan, instead of failing. Zero means every packet is
	// audio.
	headers int
	// headerSpan counts the header bytes seen, kept or dropped, since the previous
	// header packet completed. It is reset only on completion, not when a page that
	// does not continue the packet drops a dangling fragment: resetting there would
	// let pages that each drop the last restart the count forever.
	headerSpan int

	// consumed counts the bytes of every page read so far (header, segment table
	// and body). After newContainerReader it is the length of the header pages, so
	// the first audio page begins at the stream's starting offset plus consumed.
	consumed int64

	// afterHeaders is the granule and end-of-stream state newContainerReader left
	// once the header packets were read, which restart restores.
	afterHeaders struct {
		lastGranule, finalGranule int64
		sawEOS                    bool
	}
}

// Packet size limits. An Opus packet without padding is at most 61,298*N - 2
// bytes for N streams (RFC 7845 section 6): per stream, 48 frames of at most 1275
// bytes (internal/packet/parse.go:226, :330), a TOC byte, a code-3 count byte, 47
// two-byte frame lengths and a two-byte self-delimited last length. Section 6 also
// says a demuxer should treat a packet over rfcPacketLen bytes per stream as
// invalid unless it has a specific reason to allow extra padding. Code-3 padding
// is otherwise unbounded (internal/packet/parse.go:251-271), so the audio limit
// allows as much padding again.
const (
	rfcPacketLen = 61440
	// streamPacketLimit is the audio packet limit per Opus stream.
	streamPacketLimit = 2 * rfcPacketLen
	// maxHeaderPacketLen is how much of OpusHead or OpusTags is kept; the rest of
	// a longer one is read and dropped, up to maxHeaderSpan. RFC 7845 section 5.2
	// lets a reader ignore comments not fully inside the first 61,440 bytes of the
	// comment header.
	maxHeaderPacketLen = rfcPacketLen
	// maxHeaderSpan is how many bytes one header packet may span before reading
	// fails. It bounds read time, not memory (memory stays at maxHeaderPacketLen):
	// without it a header continued by endless full pages is read forever. Cover
	// art in OpusTags is a base64 METADATA_BLOCK_PICTURE, and a FLAC picture block
	// has a 24-bit length (under 16 MiB), which base64-encodes to under 22.4 MB, so
	// a picture copied from a FLAC file fits. opusfile and ffmpeg set no fixed
	// ceiling (ffmpeg libavformat/oggdec.c:306-314 doubles a buffer up to
	// UINT_MAX/2), so this is a policy bound above realistic tags. Dropped fragments
	// of a truncated header count toward it (see containerReader.headerSpan).
	maxHeaderSpan = 32 << 20
)

// audioPacketLimit returns the largest audio packet processPage accepts for a
// stream with the given OpusHead: streamPacketLimit per Opus stream. A multistream
// packet is the concatenation of one sub-packet per stream, so the bound scales
// with the stream count. parseOpusHead leaves the count at zero for mapping family
// 0, which has one stream; the max also keeps a malformed head with a count of
// zero from producing a zero limit before newFrameDecoder rejects it.
func audioPacketLimit(h opusHead) int {
	return max(int(h.streamCount), 1) * streamPacketLimit
}

// maxReusedBuf is the largest capacity of the packet arena or the partial-packet
// buffer that a containerReader keeps. Each holds at most one audio packet at a
// time (partial the packet still spanning pages, the arena the one completed from
// them), so the limit keeps a buffer for a packet of up to two page bodies, which
// covers an unpadded packet of up to two Opus streams (at most 61,298*2 - 2 =
// 122,594 bytes, see the packet size limits above). Growing a slice can overshoot
// its need by a quarter plus 192 bytes (nextslicecap, runtime/slice.go:341, Go
// 1.27.3) and a large allocation is then rounded up to an 8 KiB page
// (roundupsize, runtime/msize.go:31), so the limit adds that headroom and such a
// buffer is never dropped. A larger one comes from a multistream or padded audio
// packet. processPage drops it once that packet is done, repositionAt drops it
// when a seek moves the reader, and newContainerReader does not carry it over from
// a previous reader, so one such packet does not pin its memory for the rest of
// the stream or in a pooled Decoder.
const maxReusedBuf = 2*maxPageBodyLen + 2*maxPageBodyLen/4 + 16<<10

// minPacketBuf is the capacity a containerReader gives its arena and its
// partial-packet buffer up front. Only header packets and packets continued from
// an earlier page are copied into them, and 8 KiB holds every packet this
// package's encoder writes (at most maxPacketBytes, 7656 bytes, codec.go:66), so
// reading the audio of such a stream does not grow them. A longer packet grows
// them as needed.
const minPacketBuf = 8 << 10

// newContainerReader reads and validates the OpusHead and OpusTags header
// packets, leaving the reader positioned at the first audio packet. A first
// packet without the OpusHead magic returns an error wrapping ErrNotOpus. A header
// packet spanning more than maxHeaderSpan bytes returns an error wrapping
// ErrPacketTooLarge.
//
// br is the buffered source. The reader's buffers are sized here for the largest
// page, the most packets a page can complete and minPacketBuf bytes of carried
// packet (see presize). reuse, when not nil, is a reader of an earlier stream
// whose page, packet and queue buffers are recycled when they are within
// maxReusedBuf and whose struct is overwritten; the caller must not use it
// afterwards.
func newContainerReader(br *bufio.Reader, reuse *containerReader) (*containerReader, error) {
	cr := reuse
	if cr == nil {
		cr = new(containerReader)
	}
	cr.resetArena()
	cr.resetPartial()
	*cr = containerReader{br: br, finalGranule: -1, pg: page{raw: cr.pg.raw}, arena: cr.arena, queue: cr.queue[:0], partial: cr.partial,
		limit: streamPacketLimit, headers: 2}
	cr.presize()
	hp, err := cr.nextRawPacket()
	if err != nil {
		return nil, headerErr("OpusHead", err)
	}
	head, err := parseOpusHead(hp)
	if err != nil {
		return nil, err
	}
	// Set before OpusTags is read, so audio sharing its page (which RFC 7845
	// section 3 forbids) is held to the audio limit too.
	cr.limit = audioPacketLimit(head)
	tp, err := cr.nextRawPacket()
	if err != nil {
		return nil, headerErr("OpusTags", err)
	}
	var tags opusTags
	if len(tp) >= maxHeaderPacketLen {
		// The packet may have been cut, so only the magic is checked: the comments
		// are not exposed, and RFC 7845 section 5.2 lets a reader ignore those past
		// the first 61,440 bytes.
		err = checkOpusTagsMagic(tp)
	} else {
		tags, err = parseOpusTags(tp)
	}
	if err != nil {
		return nil, err
	}
	cr.head = head
	cr.tags = tags
	cr.afterHeaders.lastGranule, cr.afterHeaders.finalGranule, cr.afterHeaders.sawEOS = cr.lastGranule, cr.finalGranule, cr.sawEOS
	return cr, nil
}

// presize grows the reader's buffers, which must be empty, so that reading pages
// does not grow them: the page buffer to the largest page, the queue to the most
// packets one page completes (each ends at a lacing value below 255, so at most
// one per segment), and the arena and partial to minPacketBuf. slices.Grow keeps
// a buffer that is already large enough.
func (cr *containerReader) presize() {
	cr.pg.raw = slices.Grow(cr.pg.raw[:0], maxPageLen)
	cr.queue = slices.Grow(cr.queue[:0], maxPageSegments)
	cr.arena = slices.Grow(cr.arena[:0], minPacketBuf)
	cr.partial = slices.Grow(cr.partial[:0], minPacketBuf)
}

// resetPartial empties partial, dropping it when a packet that spanned several
// pages grew it past maxReusedBuf.
func (cr *containerReader) resetPartial() {
	if cap(cr.partial) > maxReusedBuf {
		cr.partial = nil
	} else {
		cr.partial = cr.partial[:0]
	}
}

// resetArena empties the arena, dropping it when a packet that spanned several
// pages grew it past maxReusedBuf. Queue slots past the current length may still
// alias a dropped arena, so they are cleared too.
func (cr *containerReader) resetArena() {
	if cap(cr.arena) > maxReusedBuf {
		clear(cr.queue[:cap(cr.queue)])
		cr.arena = nil
	} else {
		cr.arena = cr.arena[:0]
	}
}

// restart rebinds the reader to r, which must be positioned at the first audio
// page, in the state newContainerReader left it in. That state has no queued
// packet and no partial one only when the header pages hold no audio, which the
// callers guarantee: seeking, the only caller, refuses a stream whose OpusTags
// page carries audio (Decoder.tagsBad).
func (cr *containerReader) restart(r io.Reader) {
	cr.repositionAt(r)
	cr.lastGranule = cr.afterHeaders.lastGranule
	cr.finalGranule = cr.afterHeaders.finalGranule
	cr.sawEOS = cr.afterHeaders.sawEOS
}

// nextPacket returns the next audio packet, or io.EOF when the stream ends. The
// packet aliases the reader's page buffer or arena and is valid until the next
// call to nextPacket or nextRawPacket, which may read a page into them.
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
// next call. After an error the reader is not resumable: a later call could read
// on from the middle of the failed page or packet.
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
// by processing the page it lands on. It also releases packet buffers grown past
// maxReusedBuf and restores the sizes newContainerReader gave the buffers.
func (cr *containerReader) repositionAt(r io.Reader) {
	cr.br.Reset(r)
	cr.queue = cr.queue[:0]
	cr.qi = 0
	cr.resetPartial()
	cr.resetArena()
	cr.presize()
	cr.sawEOS = false
	cr.finalGranule = -1
}

// processPage reassembles the packets that complete on one page into the queue
// and updates the granule and end-of-stream accounting. A packet that starts and
// ends on pg aliases pg.body, so pg must not be rewritten while the queue is in
// use; the packet that continues to the next page is copied into partial. An
// audio packet longer than cr.limit fails with ErrPacketTooLarge; a header packet
// is cut to maxHeaderPacketLen instead, and fails with ErrPacketTooLarge only once
// it spans more than maxHeaderSpan.
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
		cr.resetPartial()
	}

	cr.queue = cr.queue[:0]
	cr.qi = 0
	// The arena is rewritten for every page. One that a packet spanning several
	// pages grew far larger than a page is dropped rather than held for the rest
	// of the stream.
	cr.resetArena()
	// An audio packet that starts and ends on this page is queued as a slice of
	// pg.body, which stays valid until the next readPage. Header packets and a
	// packet continued from an earlier page are assembled in partial and copied
	// into the arena. start is the first body byte not yet in partial or the
	// queue.
	off, start := 0, 0
	for _, l := range pg.segTable {
		seg := pg.body[off : off+int(l)]
		off += int(l)
		// The check runs on the packet's length so far, partial plus its bytes on
		// this page, before anything is kept, so partial never exceeds the limit.
		// Acceptance of an audio packet depends on its length alone, not on how pages
		// split it; a header packet is cut instead, and only its total span
		// (headerSpan) can fail it.
		limit := cr.limit
		if cr.headers > 0 {
			limit = maxHeaderPacketLen
			cr.headerSpan += len(seg)
			if cr.headerSpan > maxHeaderSpan {
				return fmt.Errorf("%w: a header packet spans more than %d bytes", ErrPacketTooLarge, maxHeaderSpan)
			}
		}
		if len(cr.partial) > limit-(off-start) {
			if cr.headers == 0 {
				return cr.packetTooLarge()
			}
			seg = seg[:limit-len(cr.partial)] // a header packet is cut; only its span can fail it
		}
		if cr.headers == 0 && len(cr.partial) == 0 {
			// An audio packet that started on this page.
			if l < maxSegmentLen {
				cr.queue = append(cr.queue, pg.body[start:off:off])
				start = off
			}
			continue
		}
		cr.partial = append(cr.partial, seg...)
		start = off
		if l < maxSegmentLen {
			if cr.headers > 0 {
				cr.headers--
				cr.headerSpan = 0
			}
			// Copied, not aliased: a packet that starts later on this page and
			// continues to the next is written into partial.
			n := len(cr.arena)
			cr.arena = append(cr.arena, cr.partial...)
			cr.queue = append(cr.queue, cr.arena[n:len(cr.arena):len(cr.arena)])
			cr.resetPartial() // the multi-page packet it held is complete
		}
	}
	// The packet that started on this page and continues to the next moves to
	// partial, because the next readPage overwrites pg.body. Its length passed the
	// limit check above.
	if start < len(pg.body) {
		cr.partial = append(cr.partial, pg.body[start:]...)
	}

	if pg.granule != granuleNone {
		cr.lastGranule = pg.granule
	}
	if pg.flags&flagEOS != 0 {
		cr.sawEOS = true
		cr.finalGranule = cr.lastGranule
	}
	return nil
}

// auxReader returns a reader over br for the same logical stream as cr, for a pass
// that must not disturb cr's buffers or position. It holds audio packets to cr's
// limit and expects br to start at an audio page.
func (cr *containerReader) auxReader(br *bufio.Reader) *containerReader {
	return &containerReader{br: br, serial: cr.serial, serialSet: true, finalGranule: -1, limit: cr.limit}
}

// packetTooLarge returns the limit error and releases the partial packet, so a
// pooled Decoder does not pin it. The Decoder latches the error, so the reader is
// not read on from the middle of the packet.
func (cr *containerReader) packetTooLarge() error {
	cr.partial = nil
	return fmt.Errorf("%w: a packet spanning pages exceeds %d bytes", ErrPacketTooLarge, cr.limit)
}

// headerErr wraps an error from reading a header packet, mapping a premature EOF
// to a clearer "truncated" message.
func headerErr(which string, err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("oggopus: stream ended before %s", which)
	}
	return fmt.Errorf("oggopus: reading %s: %w", which, err)
}
