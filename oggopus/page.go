package oggopus

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Ogg physical bitstream constants (RFC 3533 section 6).
const (
	// capturePattern begins every page header.
	capturePattern = "OggS"
	// pageVersion is the only defined Ogg stream structure version.
	pageVersion = 0
	// pageHeaderFixedLen is the size of a page header up to and including the
	// segment count byte, before the variable segment table: capture pattern (4)
	// + version (1) + header type (1) + granule position (8) + serial (4) +
	// page sequence (4) + CRC (4) + segment count (1).
	pageHeaderFixedLen = 27
	// crcFieldOffset is the byte offset of the 4-byte CRC field within the page
	// header. The checksum is computed with this field zeroed.
	crcFieldOffset = 22
	// maxPageSegments is the largest segment table (the segment count is one
	// byte).
	maxPageSegments = 255
	// maxSegmentLen is the largest lacing value; a value of 255 means the packet
	// continues into the following segment.
	maxSegmentLen = 255
	// maxPageBodyLen is the largest possible page body.
	maxPageBodyLen = maxPageSegments * maxSegmentLen
)

// headerType carries the three page flags from the header type byte.
type headerType byte

const (
	// flagContinued marks a page whose first packet continues a packet left
	// unfinished at the end of the previous page.
	flagContinued headerType = 0x01
	// flagBOS marks the first page of the logical bitstream (beginning of
	// stream).
	flagBOS headerType = 0x02
	// flagEOS marks the last page of the logical bitstream (end of stream).
	flagEOS headerType = 0x04
)

// granuleNone is the granule position of a page on which no packet completes
// (RFC 3533: the special value -1). It is written as 0xffffffffffffffff.
const granuleNone int64 = -1

// page is a parsed Ogg page. body is the concatenation of all segment payloads;
// segTable holds the lacing values that split body back into packet fragments.
type page struct {
	flags    headerType
	granule  int64
	serial   uint32
	sequence uint32
	segTable []byte
	body     []byte

	raw []byte // readPage's reusable backing store; segTable and body alias it. parsePage leaves it nil.
}

// writePage serialises a single Ogg page to w. segTable must hold 1..255 lacing
// values whose sum equals len(body). The CRC is computed over the fully
// assembled header and body with the CRC field zeroed, so the page is valid on a
// plain io.Writer with no seeking: nothing is patched after the fact.
func writePage(w io.Writer, flags headerType, granule int64, serial, sequence uint32, segTable, body []byte) error {
	if len(segTable) == 0 || len(segTable) > maxPageSegments {
		return fmt.Errorf("oggopus: invalid segment count %d", len(segTable))
	}
	sum := 0
	for _, l := range segTable {
		sum += int(l)
	}
	if sum != len(body) {
		return fmt.Errorf("oggopus: segment table sum %d does not match body length %d", sum, len(body))
	}

	buf := make([]byte, pageHeaderFixedLen+len(segTable)+len(body))
	copy(buf, capturePattern)
	buf[4] = pageVersion
	buf[5] = byte(flags)
	binary.LittleEndian.PutUint64(buf[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(buf[14:18], serial)
	binary.LittleEndian.PutUint32(buf[18:22], sequence)
	// buf[22:26] is the CRC field: left zero while the checksum is computed.
	buf[26] = byte(len(segTable))
	copy(buf[pageHeaderFixedLen:], segTable)
	copy(buf[pageHeaderFixedLen+len(segTable):], body)

	sum32 := crc32(0, buf)
	binary.LittleEndian.PutUint32(buf[crcFieldOffset:crcFieldOffset+4], sum32)

	_, err := w.Write(buf)
	return err
}

// errBadCapture reports a page that does not begin with the Ogg capture pattern.
// It is unwrapped by the reader to distinguish resync-worthy framing errors.
var errBadCapture = errors.New("oggopus: missing OggS capture pattern")

// readPage reads and validates one Ogg page from r into pg. It returns io.EOF only
// when r is exhausted at a clean page boundary (no bytes buffered for a new page);
// a truncated page in progress returns io.ErrUnexpectedEOF. The CRC is verified
// with pageCRC, the same check parsePage applies to an in-memory page. It accepts
// exactly the pages parsePage accepts.
//
// The page is read into pg.raw, which is reused from call to call, so pg.segTable
// and pg.body alias it and stay valid only until the next readPage into the same
// pg. On an error pg.segTable and pg.body are nil.
func readPage(r *bufio.Reader, pg *page) error {
	pg.segTable, pg.body = nil, nil
	// Peek before reading so a clean end-of-stream surfaces as io.EOF rather
	// than io.ErrUnexpectedEOF.
	if _, err := r.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return err
	}
	// The header, segment table and body are read back to back into one buffer,
	// the layout pageCRC and the writer use. Capacity for the fixed header and the
	// largest segment table comes first, so the table never reallocates the buffer.
	buf := slices.Grow(pg.raw[:0], pageHeaderFixedLen+maxPageSegments)[:pageHeaderFixedLen]
	pg.raw = buf
	if _, err := io.ReadFull(r, buf); err != nil {
		return unexpectedEOF(err)
	}
	if string(buf[0:4]) != capturePattern {
		return errBadCapture
	}
	if buf[4] != pageVersion {
		return fmt.Errorf("oggopus: unsupported page version %d", buf[4])
	}

	segCount := int(buf[26])
	if segCount == 0 {
		return errors.New("oggopus: page has empty segment table")
	}
	hdrLen := pageHeaderFixedLen + segCount
	buf = buf[:hdrLen]
	if _, err := io.ReadFull(r, buf[pageHeaderFixedLen:]); err != nil {
		return unexpectedEOF(err)
	}
	bodyLen := 0
	for _, l := range buf[pageHeaderFixedLen:] {
		bodyLen += int(l)
	}
	buf = slices.Grow(buf, bodyLen)[:hdrLen+bodyLen]
	pg.raw = buf
	if _, err := io.ReadFull(r, buf[hdrLen:]); err != nil {
		return unexpectedEOF(err)
	}

	// Verify the CRC by recomputing over the assembled page with the CRC field
	// zeroed, exactly as the writer produced it.
	stored := binary.LittleEndian.Uint32(buf[crcFieldOffset : crcFieldOffset+4])
	if sum := pageCRC(buf[:pageHeaderFixedLen], buf[pageHeaderFixedLen:hdrLen], buf[hdrLen:]); sum != stored {
		return fmt.Errorf("oggopus: page CRC mismatch (stored %#08x, computed %#08x)", stored, sum)
	}

	pg.flags = headerType(buf[5])
	pg.granule = int64(binary.LittleEndian.Uint64(buf[6:14]))
	pg.serial = binary.LittleEndian.Uint32(buf[14:18])
	pg.sequence = binary.LittleEndian.Uint32(buf[18:22])
	pg.segTable = buf[pageHeaderFixedLen:hdrLen]
	pg.body = buf[hdrLen:]
	return nil
}

// pageCRC computes the Ogg CRC of a page from its 27-byte fixed header (whose CRC
// field may hold any value; it is treated as zero), segment table and body.
func pageCRC(hdr, segTable, body []byte) uint32 {
	var zeroed [pageHeaderFixedLen]byte
	copy(zeroed[:], hdr[:pageHeaderFixedLen])
	for i := crcFieldOffset; i < crcFieldOffset+4; i++ {
		zeroed[i] = 0
	}
	sum := crc32(0, zeroed[:])
	sum = crc32(sum, segTable)
	return crc32(sum, body)
}

// errShortPage reports that the buffer given to parsePage ends before the page
// does, so the caller may retry with more bytes.
var errShortPage = errors.New("oggopus: buffer ends inside a page")

// parsePage parses and CRC-verifies the page that begins at b[0], returning it and
// its total encoded length. A buffer that ends before the page does returns
// errShortPage; a buffer that does not begin with a valid page returns another
// error. It accepts exactly the pages readPage accepts. The returned page's
// segTable and body alias b.
func parsePage(b []byte) (*page, int, error) {
	if len(b) < pageHeaderFixedLen {
		return nil, 0, errShortPage
	}
	if string(b[0:4]) != capturePattern {
		return nil, 0, errBadCapture
	}
	if b[4] != pageVersion {
		return nil, 0, fmt.Errorf("oggopus: unsupported page version %d", b[4])
	}
	segCount := int(b[26])
	if segCount == 0 {
		return nil, 0, errors.New("oggopus: page has empty segment table")
	}
	hdrLen := pageHeaderFixedLen + segCount
	if len(b) < hdrLen {
		return nil, 0, errShortPage
	}
	segTable := b[pageHeaderFixedLen:hdrLen]
	bodyLen := 0
	for _, l := range segTable {
		bodyLen += int(l)
	}
	total := hdrLen + bodyLen
	if len(b) < total {
		return nil, 0, errShortPage
	}
	body := b[hdrLen:total]
	stored := binary.LittleEndian.Uint32(b[crcFieldOffset : crcFieldOffset+4])
	if sum := pageCRC(b[:pageHeaderFixedLen], segTable, body); sum != stored {
		return nil, 0, fmt.Errorf("oggopus: page CRC mismatch (stored %#08x, computed %#08x)", stored, sum)
	}
	return &page{
		flags:    headerType(b[5]),
		granule:  int64(binary.LittleEndian.Uint64(b[6:14])),
		serial:   binary.LittleEndian.Uint32(b[14:18]),
		sequence: binary.LittleEndian.Uint32(b[18:22]),
		segTable: segTable,
		body:     body,
	}, total, nil
}

// completesPacket reports whether any packet ends on the page, that is, whether
// some lacing value is below 255. Only such a page carries a meaningful granule
// position (RFC 7845 section 4).
func (p *page) completesPacket() bool {
	for _, l := range p.segTable {
		if l < maxSegmentLen {
			return true
		}
	}
	return false
}

// unexpectedEOF maps a mid-structure io.EOF to io.ErrUnexpectedEOF so a
// truncated page is distinguishable from a clean end of stream.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// lacing returns the lacing values that encode a packet of the given length in
// an Ogg segment table: floor(n/255) segments of 255 followed by one segment of
// n mod 255. A packet whose length is an exact multiple of 255 therefore ends
// with a terminating zero-length segment, which is how the reader knows the
// packet finished rather than continuing.
func lacing(n int) []byte {
	segs := make([]byte, 0, n/maxSegmentLen+1)
	for n >= maxSegmentLen {
		segs = append(segs, maxSegmentLen)
		n -= maxSegmentLen
	}
	segs = append(segs, byte(n))
	return segs
}
