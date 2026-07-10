package packet

import (
	"bytes"
	"errors"
	"testing"
)

// framesEqual reports whether got is exactly the sequence of frame payloads in
// want.
func framesEqual(got, want [][]byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			return false
		}
	}
	return true
}

func TestParseCode0(t *testing.T) {
	// config 0, mono, code 0: a single frame "abc".
	pkt := []byte{0x00, 'a', 'b', 'c'}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.TOC.FrameCountCode() != 0 {
		t.Errorf("code = %d want 0", p.TOC.FrameCountCode())
	}
	if !framesEqual(p.Frames, [][]byte{[]byte("abc")}) {
		t.Errorf("frames = %q want [abc]", p.Frames)
	}
	if p.Padding != nil {
		t.Errorf("padding = %v want nil", p.Padding)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode0Empty(t *testing.T) {
	// A code 0 packet may carry a zero-length frame (DTX): just the TOC byte.
	p, err := Parse([]byte{0x00})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Frames) != 1 || len(p.Frames[0]) != 0 {
		t.Fatalf("frames = %v want one empty frame", p.Frames)
	}
	if p.Consumed != 1 {
		t.Errorf("consumed = %d want 1", p.Consumed)
	}
}

func TestParseCode1(t *testing.T) {
	// code 1: two equal frames; payload length must be even.
	pkt := []byte{0x01, 1, 2, 3, 4}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4}}) {
		t.Errorf("frames = %v want [[1 2] [3 4]]", p.Frames)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode1OddLength(t *testing.T) {
	// code 1 with an odd number of payload bytes is invalid.
	if _, err := Parse([]byte{0x01, 1, 2, 3}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode2(t *testing.T) {
	// code 2: two frames, first length signalled (2 bytes), rest is frame 1.
	pkt := []byte{0x02, 0x02, 'a', 'b', 'c', 'd', 'e'}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{[]byte("ab"), []byte("cde")}) {
		t.Errorf("frames = %q want [ab cde]", p.Frames)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode2TwoByteSize(t *testing.T) {
	// A first-frame length of 300 needs a two-byte size field:
	// encode_size(300) = {252+(300&3), (300-252)>>2} = {0, 12}? check: 300&3=0
	// so byte0=252, byte1=(300-252)>>2=12. Decodes to 4*12+252 = 300.
	first := make([]byte, 300)
	for i := range first {
		first[i] = byte(i)
	}
	second := []byte{9, 8, 7}
	pkt := append([]byte{0x02, 252, 12}, first...)
	pkt = append(pkt, second...)
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{first, second}) {
		t.Errorf("frames mismatch for two-byte size field")
	}
}

func TestParseCode2SizeOverflow(t *testing.T) {
	// First-frame length larger than the remaining bytes is invalid.
	if _, err := Parse([]byte{0x02, 200, 1, 2, 3}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode3CBR(t *testing.T) {
	// code 3 CBR: count byte = 3 (no VBR bit, no padding bit); three frames of
	// equal length. config 0 (SILK 10 ms) so 3 frames = 30 ms, within limits.
	pkt := []byte{0x03, 0x03, 1, 2, 3, 4, 5, 6}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2}, {3, 4}, {5, 6}}) {
		t.Errorf("frames = %v", p.Frames)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode3CBRNotDivisible(t *testing.T) {
	// CBR payload that does not divide evenly across the frame count is invalid.
	if _, err := Parse([]byte{0x03, 0x03, 1, 2, 3, 4, 5}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode3VBR(t *testing.T) {
	// code 3 VBR: count byte = 0x83 (VBR bit + count 3). Sizes 1 and 2 for the
	// first two frames; the third is implicit (3 bytes here).
	pkt := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := [][]byte{{10}, {20, 21}, {30, 31, 32}}
	if !framesEqual(p.Frames, want) {
		t.Errorf("frames = %v want %v", p.Frames, want)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode3ZeroCount(t *testing.T) {
	// A code 3 count byte of 0 frames is invalid.
	if _, err := Parse([]byte{0x03, 0x00}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode3TooManySamples(t *testing.T) {
	// config 3 is SILK 60 ms; 3 frames would be 180 ms, over the 120 ms cap.
	if _, err := Parse([]byte{0x03 | (3 << 3), 0x03, 1, 2, 3}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode3Padding(t *testing.T) {
	// code 3, one frame, padding flag set. Padding descriptor 0x03 means three
	// padding data bytes trail the single 4-byte frame.
	frame := []byte{10, 11, 12, 13}
	pkt := make([]byte, 0, 3+len(frame)+3)
	pkt = append(pkt, 0x03, 0x41, 0x03)
	pkt = append(pkt, frame...)
	pkt = append(pkt, 0, 0, 0) // three padding bytes
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{frame}) {
		t.Errorf("frames = %v want [%v]", p.Frames, frame)
	}
	if len(p.Padding) != 3 {
		t.Errorf("padding len = %d want 3", len(p.Padding))
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode3PaddingMultiByte(t *testing.T) {
	// A padding descriptor of 255 contributes 254 bytes and continues; the
	// following byte ends the run. 255 then 10 => 254+10 = 264 padding bytes.
	frame := []byte{1, 2}
	pkt := make([]byte, 0, 4+len(frame)+264)
	pkt = append(pkt, 0x03, 0x41, 255, 10)
	pkt = append(pkt, frame...)
	pkt = append(pkt, make([]byte, 264)...)
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{frame}) {
		t.Errorf("frames = %v", p.Frames)
	}
	if len(p.Padding) != 264 {
		t.Errorf("padding len = %d want 264", len(p.Padding))
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

func TestParseCode3PaddingTruncated(t *testing.T) {
	// Padding descriptor claims more bytes than remain: invalid.
	if _, err := Parse([]byte{0x03, 0x41, 200, 1, 2}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("Parse(nil) err = %v want ErrInvalidPacket", err)
	}
	if _, err := Parse([]byte{}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("Parse(empty) err = %v want ErrInvalidPacket", err)
	}
}

func TestParseCode3MissingCountByte(t *testing.T) {
	// code 3 with no count byte at all.
	if _, err := Parse([]byte{0x03}); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestFramesAliasWithoutOverlap(t *testing.T) {
	// Frame sub-slices are capped so an append to one cannot reach into the next.
	pkt := []byte{0x01, 1, 2, 3, 4}
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cap(p.Frames[0]) != len(p.Frames[0]) {
		t.Errorf("frame 0 cap=%d len=%d, want equal (three-index slice)", cap(p.Frames[0]), len(p.Frames[0]))
	}
	p.Frames[0] = append(p.Frames[0], 0xFF)
	if p.Frames[1][0] != 3 {
		t.Errorf("append to frame 0 clobbered frame 1: %v", p.Frames[1])
	}
}

func TestCountFrames(t *testing.T) {
	cases := []struct {
		pkt  []byte
		want int
	}{
		{[]byte{0x00, 1}, 1},
		{[]byte{0x01, 1, 2}, 2},
		{[]byte{0x02, 1, 2}, 2},
		{[]byte{0x03, 0x05, 1}, 5},  // code 3 count byte 5
		{[]byte{0x03, 0x83, 1}, 3},  // VBR bit ignored by the count
		{[]byte{0x03, 0x40 | 7}, 7}, // padding bit ignored by the count
	}
	for _, c := range cases {
		got, err := CountFrames(c.pkt)
		if err != nil {
			t.Errorf("CountFrames(%v): %v", c.pkt, err)
			continue
		}
		if got != c.want {
			t.Errorf("CountFrames(%v) = %d want %d", c.pkt, got, c.want)
		}
	}
}

func TestCountFramesErrors(t *testing.T) {
	if _, err := CountFrames(nil); !errors.Is(err, ErrBadArg) {
		t.Errorf("CountFrames(nil) err = %v want ErrBadArg", err)
	}
	if _, err := CountFrames([]byte{0x03}); !errors.Is(err, ErrInvalidPacket) {
		t.Errorf("CountFrames(code3 truncated) err = %v want ErrInvalidPacket", err)
	}
}

func TestSamples(t *testing.T) {
	// config 1 (SILK NB 20 ms = 960 @ 48 kHz), code 1 -> 2 frames -> 1920.
	pkt := []byte{0x01 | (1 << 3), 1, 2}
	n, err := Samples(pkt, 48000)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if n != 1920 {
		t.Errorf("Samples = %d want 1920", n)
	}
	// At 16 kHz the same packet is 2 * 320 = 640 samples.
	if n16, err := Samples(pkt, 16000); err != nil || n16 != 640 {
		t.Errorf("Samples@16k = %d, %v want 640", n16, err)
	}
}

func TestSamplesOver120ms(t *testing.T) {
	// config 3 (SILK 60 ms), a code 3 count byte of 3 => 180 ms.
	pkt := []byte{0x03 | (3 << 3), 0x03}
	if _, err := Samples(pkt, 48000); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("err = %v want ErrInvalidPacket", err)
	}
}

func TestPacketDuration(t *testing.T) {
	pkt := []byte{0x03, 0x03, 1, 2, 3, 4, 5, 6} // config 0 SILK 10 ms, 3 frames
	p, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := p.Duration(48000); got != 3*480 {
		t.Errorf("Duration = %d want %d", got, 3*480)
	}
	// Duration must agree with the header-only Samples helper.
	s, err := Samples(pkt, 48000)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if s != p.Duration(48000) {
		t.Errorf("Samples=%d != Duration=%d", s, p.Duration(48000))
	}
}

func TestParseSelfDelimited(t *testing.T) {
	// Two self-delimited code 0 packets concatenated. Each is TOC + size + frame.
	a := []byte{0x00, 0x03, 'a', 'b', 'c'}
	b := []byte{0x00, 0x02, 'x', 'y'}
	stream := append(bytes.Clone(a), b...)

	p1, err := ParseSelfDelimited(stream)
	if err != nil {
		t.Fatalf("ParseSelfDelimited #1: %v", err)
	}
	if !framesEqual(p1.Frames, [][]byte{[]byte("abc")}) {
		t.Errorf("packet 1 frames = %q", p1.Frames)
	}
	if p1.Consumed != len(a) {
		t.Fatalf("packet 1 consumed = %d want %d", p1.Consumed, len(a))
	}

	p2, err := ParseSelfDelimited(stream[p1.Consumed:])
	if err != nil {
		t.Fatalf("ParseSelfDelimited #2: %v", err)
	}
	if !framesEqual(p2.Frames, [][]byte{[]byte("xy")}) {
		t.Errorf("packet 2 frames = %q", p2.Frames)
	}
	if p2.Consumed != len(b) {
		t.Errorf("packet 2 consumed = %d want %d", p2.Consumed, len(b))
	}
}

func TestParseSelfDelimitedCode1(t *testing.T) {
	// Self-delimited code 1: two equal frames, last length signalled explicitly.
	// TOC + sizeByte(3) + frame0(3) + frame1(3).
	pkt := []byte{0x01, 0x03, 1, 2, 3, 4, 5, 6}
	p, err := ParseSelfDelimited(pkt)
	if err != nil {
		t.Fatalf("ParseSelfDelimited: %v", err)
	}
	if !framesEqual(p.Frames, [][]byte{{1, 2, 3}, {4, 5, 6}}) {
		t.Errorf("frames = %v", p.Frames)
	}
	if p.Consumed != len(pkt) {
		t.Errorf("consumed = %d want %d", p.Consumed, len(pkt))
	}
}

// seedFuzzCorpus adds hand-built packets exercising every frame code plus a
// padded packet.
func seedFuzzCorpus(f *testing.F) {
	f.Helper()
	seeds := [][]byte{
		nil,
		{0x00},
		{0x00, 1, 2, 3},
		{0x01, 1, 2, 3, 4},
		{0x02, 0x02, 1, 2, 3, 4, 5},
		{0x03, 0x03, 1, 2, 3, 4, 5, 6},
		{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32},
		{0x03, 0x41, 0x03, 10, 11, 12, 13, 0, 0, 0},
		{0xff, 0x41, 3, 1, 2, 3, 0, 0, 0},
		{0x01, 0x03, 1, 2, 3, 4, 5, 6},
	}
	for _, s := range seeds {
		f.Add(s)
	}
}

// FuzzParsePacket asserts the invariant from the plan test strategy: the packet
// layer never panics on arbitrary input, and any packet it accepts is
// self-consistent (frames and padding lie within the buffer, Consumed is
// bounded). The repacketizer is driven too so its parse/serialize paths are
// fuzzed.
func FuzzParsePacket(f *testing.F) {
	seedFuzzCorpus(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err == nil {
			checkPacketInvariants(t, data, p, false)
		}

		if sp, err := ParseSelfDelimited(data); err == nil {
			checkPacketInvariants(t, data, sp, true)
		}

		// These helpers must also never panic on arbitrary input.
		_, _ = CountFrames(data)
		_, _ = Samples(data, 48000)

		// The repacketizer must survive hostile input and round-trip its own
		// output when it accepts a packet.
		rp := NewRepacketizer()
		if rp.Cat(data) == nil && rp.GetNbFrames() > 0 {
			buf := make([]byte, len(data)+2*MaxFrames+4)
			n, err := rp.Out(buf)
			if err == nil {
				if _, err := Parse(buf[:n]); err != nil {
					t.Fatalf("repacketizer output failed to re-parse: %v", err)
				}
			}
		}
	})
}

func checkPacketInvariants(t *testing.T, data []byte, p *Packet, selfDelimited bool) {
	t.Helper()
	if p.Consumed < 0 || p.Consumed > len(data) {
		t.Fatalf("Consumed=%d out of range [0,%d]", p.Consumed, len(data))
	}
	if len(p.Frames) < 1 || len(p.Frames) > MaxFrames {
		t.Fatalf("frame count %d out of range [1,%d]", len(p.Frames), MaxFrames)
	}
	total := 0
	for _, fr := range p.Frames {
		if len(fr) > 1275 && !selfDelimited {
			t.Fatalf("frame length %d exceeds 1275", len(fr))
		}
		total += len(fr)
	}
	if total+len(p.Padding) > len(data) {
		t.Fatalf("frames+padding = %d exceed input %d", total+len(p.Padding), len(data))
	}
}
