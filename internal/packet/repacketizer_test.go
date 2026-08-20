package packet

import (
	"errors"
	"testing"
)

// catAll cats every packet into a fresh repacketizer, failing on any error.
func catAll(t *testing.T, pkts ...[]byte) *Repacketizer {
	t.Helper()
	rp := NewRepacketizer()
	for i, pkt := range pkts {
		if err := rp.Cat(pkt); err != nil {
			t.Fatalf("Cat(#%d): %v", i, err)
		}
	}
	return rp
}

func TestRepacketizerRoundTripVBR(t *testing.T) {
	// A code 3 VBR packet with three frames of sizes 1, 2, 3.
	in := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32}
	rp := catAll(t, in)
	if rp.GetNbFrames() != 3 {
		t.Fatalf("GetNbFrames = %d want 3", rp.GetNbFrames())
	}

	buf := make([]byte, 64)
	n, err := rp.Out(buf)
	if err != nil {
		t.Fatalf("Out: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse output: %v", err)
	}
	want := [][]byte{{10}, {20, 21}, {30, 31, 32}}
	if !framesEqual(p.Frames, want) {
		t.Errorf("frames = %v want %v", p.Frames, want)
	}
	if p.TOC.Byte()&0xFC != in[0]&0xFC {
		t.Errorf("TOC config changed: %#02x vs %#02x", p.TOC.Byte(), in[0])
	}
}

func TestRepacketizerMergeEqual(t *testing.T) {
	// Two single-frame packets of equal length merge into a code 1 packet.
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4})
	buf := make([]byte, 16)
	n, err := rp.Out(buf)
	if err != nil {
		t.Fatalf("Out: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.TOC.FrameCountCode() != 1 {
		t.Errorf("code = %d want 1", p.TOC.FrameCountCode())
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4}}) {
		t.Errorf("frames = %v", p.Frames)
	}
}

func TestRepacketizerMergeUnequal(t *testing.T) {
	// Two single-frame packets of unequal length merge into a code 2 packet.
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4, 5})
	buf := make([]byte, 16)
	n, err := rp.Out(buf)
	if err != nil {
		t.Fatalf("Out: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.TOC.FrameCountCode() != 2 {
		t.Errorf("code = %d want 2", p.TOC.FrameCountCode())
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4, 5}}) {
		t.Errorf("frames = %v", p.Frames)
	}
}

func TestRepacketizerMergeMixedCodes(t *testing.T) {
	// Same config, differing frame-count codes (code 0 then code 1) is allowed;
	// only the top six TOC bits must agree.
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x01, 3, 4, 5, 6})
	if rp.GetNbFrames() != 3 {
		t.Fatalf("GetNbFrames = %d want 3", rp.GetNbFrames())
	}
	buf := make([]byte, 32)
	n, err := rp.Out(buf)
	if err != nil {
		t.Fatalf("Out: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4}, {5, 6}}) {
		t.Errorf("frames = %v", p.Frames)
	}
}

func TestRepacketizerOutRange(t *testing.T) {
	in := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32} // frames [10] [20 21] [30 31 32]
	rp := catAll(t, in)
	buf := make([]byte, 64)

	cases := []struct {
		begin, end int
		want       [][]byte
	}{
		{0, 1, [][]byte{{10}}},
		{0, 2, [][]byte{{10}, {20, 21}}},
		{1, 3, [][]byte{{20, 21}, {30, 31, 32}}},
		{0, 3, [][]byte{{10}, {20, 21}, {30, 31, 32}}},
	}
	for _, c := range cases {
		n, err := rp.OutRange(c.begin, c.end, buf)
		if err != nil {
			t.Fatalf("OutRange(%d,%d): %v", c.begin, c.end, err)
		}
		p, err := Parse(buf[:n])
		if err != nil {
			t.Fatalf("Parse OutRange(%d,%d): %v", c.begin, c.end, err)
		}
		if !framesEqual(p.Frames, c.want) {
			t.Errorf("OutRange(%d,%d) frames = %v want %v", c.begin, c.end, p.Frames, c.want)
		}
	}
}

func TestRepacketizerOutRangeBadArgs(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2})
	buf := make([]byte, 16)
	bad := [][2]int{{0, 0}, {-1, 1}, {1, 1}, {0, 2}, {2, 1}}
	for _, r := range bad {
		if _, err := rp.OutRange(r[0], r[1], buf); !errors.Is(err, ErrBadArg) {
			t.Errorf("OutRange(%d,%d) err = %v want ErrBadArg", r[0], r[1], err)
		}
	}
}

func TestRepacketizerConfigMismatch(t *testing.T) {
	rp := NewRepacketizer()
	if err := rp.Cat([]byte{0x00, 1, 2}); err != nil {
		t.Fatalf("Cat #1: %v", err)
	}
	// config 1 (byte 0x08) differs in the top six bits from config 0.
	if err := rp.Cat([]byte{0x08, 3, 4}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("Cat mismatch err = %v want ErrInvalidPacket", err)
	}
}

func TestRepacketizerDurationCap(t *testing.T) {
	// config 3 is SILK 60 ms (480 samples at 8 kHz). Two frames reach 120 ms;
	// a third exceeds the cap.
	pkt := []byte{0x18, 1} // config 3, code 0, one frame
	rp := NewRepacketizer()
	if err := rp.Cat(pkt); err != nil {
		t.Fatalf("Cat #1: %v", err)
	}
	if err := rp.Cat(pkt); err != nil {
		t.Fatalf("Cat #2: %v", err)
	}
	if err := rp.Cat(pkt); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("Cat #3 err = %v want ErrInvalidPacket", err)
	}
}

func TestRepacketizerBufferTooSmall(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2, 3})
	if _, err := rp.Out(make([]byte, 2)); !errors.Is(err, ErrBufferTooSmall) {
		t.Fatalf("Out err = %v want ErrBufferTooSmall", err)
	}
}

func TestRepacketizerInitReuse(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4})
	if rp.GetNbFrames() != 2 {
		t.Fatalf("GetNbFrames = %d want 2", rp.GetNbFrames())
	}
	rp.Init()
	if rp.GetNbFrames() != 0 {
		t.Fatalf("after Init GetNbFrames = %d want 0", rp.GetNbFrames())
	}
	// Reusing after Init picks up a fresh (possibly different) configuration.
	if err := rp.Cat([]byte{0x08, 9, 9}); err != nil {
		t.Fatalf("Cat after Init: %v", err)
	}
	if rp.GetNbFrames() != 1 {
		t.Fatalf("GetNbFrames = %d want 1", rp.GetNbFrames())
	}
}

// TestRepacketizerPad exercises the (internal) padding path: promoting a short
// packet to code 3 and filling the buffer with padding to a target length.
func TestRepacketizerPad(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2, 3})
	buf := make([]byte, 20)
	n, err := rp.outRangeImpl(0, rp.nbFrames, buf, false, true)
	if err != nil {
		t.Fatalf("outRangeImpl pad: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("padded length = %d want %d", n, len(buf))
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse padded: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2, 3}}) {
		t.Errorf("frames = %v want [[1 2 3]]", p.Frames)
	}
	if len(p.Padding) == 0 {
		t.Errorf("expected non-empty padding")
	}
	if p.Consumed != n {
		t.Errorf("consumed = %d want %d", p.Consumed, n)
	}
}

// TestRepacketizerPadLargeDescriptor forces a padding amount that needs a
// multi-byte descriptor (a run of 0xFF bytes).
func TestRepacketizerPadLargeDescriptor(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2, 3})
	buf := make([]byte, 600) // padding well over 255 bytes
	n, err := rp.outRangeImpl(0, rp.nbFrames, buf, false, true)
	if err != nil {
		t.Fatalf("outRangeImpl pad: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse padded: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2, 3}}) {
		t.Errorf("frames = %v", p.Frames)
	}
	if p.Consumed != n {
		t.Errorf("consumed = %d want %d", p.Consumed, n)
	}
}

// TestRepacketizerSelfDelimited round-trips the internal self-delimited output
// path through ParseSelfDelimited.
func TestRepacketizerSelfDelimited(t *testing.T) {
	in := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32}
	rp := catAll(t, in)
	buf := make([]byte, 64)
	n, err := rp.outRangeImpl(0, rp.nbFrames, buf, true, false)
	if err != nil {
		t.Fatalf("outRangeImpl self-delimited: %v", err)
	}
	p, err := ParseSelfDelimited(buf[:n])
	if err != nil {
		t.Fatalf("ParseSelfDelimited: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{10}, {20, 21}, {30, 31, 32}}) {
		t.Errorf("frames = %v", p.Frames)
	}
	if p.Consumed != n {
		t.Errorf("consumed = %d want %d", p.Consumed, n)
	}
}

// TestRepacketizerCatSelfDelimited drives the internal self-delimited cat path
// used by multistream handling: build a self-delimited packet, cat it back, and
// re-emit as a standard packet.
func TestRepacketizerCatSelfDelimited(t *testing.T) {
	in := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32}
	src := catAll(t, in)
	sd := make([]byte, 64)
	n, err := src.outRangeImpl(0, src.nbFrames, sd, true, false)
	if err != nil {
		t.Fatalf("build self-delimited: %v", err)
	}

	rp := NewRepacketizer()
	if err := rp.catImpl(sd[:n], true); err != nil {
		t.Fatalf("catImpl self-delimited: %v", err)
	}
	if rp.GetNbFrames() != 3 {
		t.Fatalf("GetNbFrames = %d want 3", rp.GetNbFrames())
	}
	out := make([]byte, 64)
	m, err := rp.Out(out)
	if err != nil {
		t.Fatalf("Out: %v", err)
	}
	p, err := Parse(out[:m])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{10}, {20, 21}, {30, 31, 32}}) {
		t.Errorf("frames = %v", p.Frames)
	}
}

func TestRepacketizerCatInvalid(t *testing.T) {
	rp := NewRepacketizer()
	if err := rp.Cat(nil); !errors.Is(err, ErrInvalidPacket) {
		t.Errorf("Cat(nil) err = %v want ErrInvalidPacket", err)
	}
	if err := rp.Cat([]byte{0x03}); err == nil {
		t.Errorf("Cat(truncated code3) err = nil want error")
	}
}

// TestOutRangePaddedBounds checks the maxlen contract of the exported wrapper the
// Opus encoder drives (opus_encoder.c:1831): maxlen must be within [0, len(data)].
func TestOutRangePaddedBounds(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4})
	buf := make([]byte, 20)
	for _, maxlen := range []int{-1, 21, 100} {
		if _, err := rp.OutRangePadded(0, rp.nbFrames, buf, maxlen, false); !errors.Is(err, ErrBadArg) {
			t.Errorf("OutRangePadded(maxlen=%d) err = %v, want ErrBadArg", maxlen, err)
		}
	}
}

// TestOutRangePaddedCBRPromotesCode3 pins the behaviour the multiframe CBR path
// relies on: with pad set and the assembled frames shorter than maxlen, a two-frame
// packet is promoted to code 3 to carry the padding, the return value is maxlen, and
// it still parses back as the two original frames.
func TestOutRangePaddedCBRPromotesCode3(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4}) // two equal frames -> code 1 unpadded
	buf := make([]byte, 20)
	const maxlen = 12
	n, err := rp.OutRangePadded(0, rp.nbFrames, buf, maxlen, true)
	if err != nil {
		t.Fatalf("OutRangePadded pad: %v", err)
	}
	if n != maxlen {
		t.Fatalf("padded length = %d, want %d", n, maxlen)
	}
	if code := buf[0] & 0x3; code != 3 {
		t.Errorf("TOC code = %d, want 3 (padding promotes the two-frame packet)", code)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse padded: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4}}) {
		t.Errorf("frames = %v, want [[1 2] [3 4]]", p.Frames)
	}
	if len(p.Padding) == 0 {
		t.Error("expected non-empty padding")
	}
}

// TestOutRangePaddedUnpaddedNatural confirms that without pad the wrapper returns the
// natural assembled length (the VBR multiframe case), even when maxlen leaves slack.
func TestOutRangePaddedUnpaddedNatural(t *testing.T) {
	rp := catAll(t, []byte{0x00, 1, 2}, []byte{0x00, 3, 4}) // code 1: 1 TOC + 4 payload = 5 bytes
	buf := make([]byte, 20)
	n, err := rp.OutRangePadded(0, rp.nbFrames, buf, 20, false)
	if err != nil {
		t.Fatalf("OutRangePadded: %v", err)
	}
	if n != 5 {
		t.Fatalf("unpadded length = %d, want 5", n)
	}
	if code := buf[0] & 0x3; code != 1 {
		t.Errorf("TOC code = %d, want 1 (two equal frames, no padding)", code)
	}
}

// TestOutRangePaddedSixFrames covers the 120 ms boundary: six 20 ms sub-frames
// assemble as a single code-3 packet that parses back to six frames.
func TestOutRangePaddedSixFrames(t *testing.T) {
	pkts := make([][]byte, 0, 6)
	for i := range 6 {
		pkts = append(pkts, []byte{0x00, byte(i), byte(i + 1)})
	}
	rp := catAll(t, pkts...)
	if rp.nbFrames != 6 {
		t.Fatalf("nbFrames = %d, want 6", rp.nbFrames)
	}
	buf := make([]byte, 64)
	n, err := rp.OutRangePadded(0, rp.nbFrames, buf, len(buf), false)
	if err != nil {
		t.Fatalf("OutRangePadded: %v", err)
	}
	if code := buf[0] & 0x3; code != 3 {
		t.Errorf("TOC code = %d, want 3 (six frames)", code)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Frames) != 6 {
		t.Errorf("parsed %d frames, want 6", len(p.Frames))
	}
}
