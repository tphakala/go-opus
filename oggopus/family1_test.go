package oggopus

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/tphakala/go-opus/internal/packet"
	"github.com/tphakala/go-opus/opus"
)

// TestDecodeFamily1SurroundRoundTrip verifies the container decode wiring for
// channel mapping family 1 (surround). It hand-builds a family-1 Ogg Opus stream
// whose audio packets are real multistream packets (each sub-stream encoded with
// the single-stream encoder, then concatenated with self-delimited framing for
// every stream but the last, the RFC 7845 multistream payload layout), decodes it
// through oggopus.Decoder, and checks the PCM equals what opus.MultistreamDecoder
// produces from the same packets. Both paths share the same multistream decoder,
// so any difference is in the container wiring: the family-1 OpusHead parse,
// newFrameDecoder routing to the multistream decoder, and the pre-skip drop.
// End-trim is zero here (sourceSamples is the full post-pre-skip count); the
// mapping-family-independent end-trim path is covered by TestContainerRoundTrip.
func TestDecodeFamily1SurroundRoundTrip(t *testing.T) {
	const (
		frameSize = 960 // 20 ms at 48 kHz
		nFrames   = 8
		preSkip   = 312
		channels  = 3
		streams   = 2
		coupled   = 1
	)
	// mapping[apiCh] names the decoded stream channel feeding that API channel:
	// coupled stream 0 provides decode channels 0 (L) and 1 (R); mono stream 1
	// provides decode channel 2. So API channels 0 and 1 come from the stereo
	// stream and channel 2 from the mono stream.
	mapping := []byte{0, 1, 2}

	packets := encodeMultistreamSeq(t, channels, streams, coupled, mapping, frameSize, nFrames)

	head := opusHead{
		version:         opusHeadVersion,
		channels:        channels,
		preSkip:         preSkip,
		inputSampleRate: 48000,
		mappingFamily:   1,
		streamCount:     streams,
		coupledCount:    coupled,
		channelMapping:  mapping,
	}
	var buf bytes.Buffer
	writeContainer(t, &buf, head, opusTags{vendor: "go-opus"}, packets, frameSize, int64(nFrames*frameSize-preSkip))

	dec, err := NewDecoder(&buf)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := dec.Info().Channels; got != channels {
		t.Fatalf("Info().Channels = %d want %d", got, channels)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	want := referenceMSDecode(t, channels, streams, coupled, mapping, frameSize, packets, preSkip)
	if len(want) == 0 {
		t.Fatal("reference produced no PCM; the test would be vacuous")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("family-1 container decode mismatch: got %d bytes, want %d bytes, first diff at byte %d",
			len(got), len(want), firstDiffByte(got, want))
	}
}

// TestNewFrameDecoderRejectsBadFamily1Header confirms a family-1 OpusHead with an
// inconsistent mapping table is rejected with an error (not a panic) at decoder
// construction, exercising the family-1 branch of newFrameDecoder on the error path
// in the default (non-reference-codec) test run.
func TestNewFrameDecoderRejectsBadFamily1Header(t *testing.T) {
	// streams=1, coupled=0 gives one decode channel (index 0); the mapping value 5
	// names a nonexistent stream channel, so NewMultistreamDecoder must reject it.
	head := opusHead{
		version:        opusHeadVersion,
		channels:       2,
		preSkip:        312,
		mappingFamily:  1,
		streamCount:    1,
		coupledCount:   0,
		channelMapping: []byte{0, 5},
	}
	if _, err := newFrameDecoder(head); err == nil {
		t.Fatal("newFrameDecoder accepted an out-of-range family-1 mapping; want an error")
	}
}

// TestNewFrameDecoderRejectsFamily1Over8Channels rejects a family-1 header that
// declares more than the 8 channels RFC 7845 permits for family 1, even when the
// stream layout and mapping are otherwise self-consistent.
func TestNewFrameDecoderRejectsFamily1Over8Channels(t *testing.T) {
	// 9 mono streams, mapping 0..8: a valid multistream layout on its own, but
	// family 1 caps at 8 channels.
	head := opusHead{
		version:        opusHeadVersion,
		channels:       9,
		preSkip:        312,
		mappingFamily:  1,
		streamCount:    9,
		coupledCount:   0,
		channelMapping: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8},
	}
	if _, err := newFrameDecoder(head); err == nil {
		t.Fatal("newFrameDecoder accepted a 9-channel family-1 header; want an error (RFC 7845 caps family 1 at 8)")
	}
}

// encodeMultistreamSeq encodes nFrames frames of a (streams, coupled, mapping)
// family-1 layout, returning one multistream packet per frame. Each sub-stream is
// encoded with the single-stream encoder from the API channels the mapping routes
// to it, then the sub-packets are concatenated with self-delimited framing for
// every stream but the last.
func encodeMultistreamSeq(t *testing.T, channels, streams, coupled int, mapping []byte, frameSize, nFrames int) [][]byte {
	t.Helper()

	// Invert the decode mapping to the API channels that feed each stream. A coupled
	// stream s draws decode channels 2s (L) and 2s+1 (R); a mono stream s draws
	// decode channel s+coupled. The standard surround mapping is a permutation, so
	// each decode channel has exactly one API source here.
	find := func(decIdx int) int {
		for i, m := range mapping {
			if int(m) == decIdx {
				return i
			}
		}
		return -1
	}
	srcCh := make([][]int, streams)
	for s := range streams {
		if s < coupled {
			srcCh[s] = []int{find(2 * s), find(2*s + 1)}
		} else {
			srcCh[s] = []int{find(s + coupled)}
		}
		for _, c := range srcCh[s] {
			if c < 0 {
				t.Fatalf("stream %d has an unmapped decode channel (mapping %v is not a permutation)", s, mapping)
			}
		}
	}

	encs := make([]*opus.Encoder, streams)
	for s := range streams {
		enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: len(srcCh[s]), Bitrate: 64000})
		if err != nil {
			t.Fatalf("NewEncoder stream %d: %v", s, err)
		}
		encs[s] = enc
	}

	apiPCM := genInterleavedPCM(nFrames*frameSize, channels)
	streamBuf := make([]int16, 2*frameSize) // up to stereo per stream
	encBuf := make([]byte, maxPacketBytes)
	sd := make([]byte, maxPacketBytes+8) // self-delimited framing adds a length prefix
	rp := packet.NewRepacketizer()

	out := make([][]byte, nFrames)
	for f := range nFrames {
		var ms []byte
		for s := range streams {
			ch := len(srcCh[s])
			// Deinterleave this stream's API channels for frame f into streamBuf.
			for i := range frameSize {
				sample := (f*frameSize + i) * channels
				for j, api := range srcCh[s] {
					streamBuf[i*ch+j] = apiPCM[sample+api]
				}
			}
			n, err := encs[s].Encode(streamBuf[:frameSize*ch], encBuf)
			if err != nil {
				t.Fatalf("encode stream %d frame %d: %v", s, f, err)
			}
			if s != streams-1 {
				// Leading stream: frame the sub-packet self-delimited so the decoder
				// can find where the next stream begins.
				rp.Init()
				if err := rp.Cat(encBuf[:n]); err != nil {
					t.Fatalf("repacketizer cat stream %d frame %d: %v", s, f, err)
				}
				m, err := rp.OutSelfDelimited(sd)
				if err != nil {
					t.Fatalf("OutSelfDelimited stream %d frame %d: %v", s, f, err)
				}
				ms = append(ms, sd[:m]...)
			} else {
				// Final stream: a plain (non-self-delimited) packet.
				ms = append(ms, encBuf[:n]...)
			}
		}
		out[f] = ms
	}
	return out
}

// referenceMSDecode decodes packets through opus.MultistreamDecoder and returns the
// interleaved little-endian int16 PCM with the leading preSkip samples per channel
// dropped, the exact output the container decoder must reproduce (the container
// applies the same pre-skip drop and, with sourceSamples set to the post-pre-skip
// count, no end-trim).
func referenceMSDecode(t *testing.T, channels, streams, coupled int, mapping []byte, frameSize int, packets [][]byte, preSkip int) []byte {
	t.Helper()
	dec, err := opus.NewMultistreamDecoder(48000, channels, streams, coupled, mapping)
	if err != nil {
		t.Fatalf("NewMultistreamDecoder: %v", err)
	}
	buf := make([]int16, frameSize*channels)
	var all []int16
	for i, p := range packets {
		n, err := dec.Decode(p, buf)
		if err != nil {
			t.Fatalf("reference decode packet %d: %v", i, err)
		}
		all = append(all, buf[:n*channels]...)
	}
	if preSkip*channels > len(all) {
		t.Fatalf("pre-skip %d samples exceeds decoded %d", preSkip, len(all)/channels)
	}
	all = all[preSkip*channels:]
	out := make([]byte, 0, len(all)*2)
	for _, s := range all {
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// genInterleavedPCM builds n samples per channel of deterministic, lively
// interleaved int16 content, decorrelated per channel so the coupled stream has
// real stereo work to do.
func genInterleavedPCM(n, channels int) []int16 {
	out := make([]int16, n*channels)
	var lcg uint32 = 0x2545f491
	for i := range n {
		lcg = lcg*1664525 + 1013904223
		base := int32(int16(lcg>>16)) / 3
		for c := range channels {
			v := base + int32(1500*(c+1))*int32(i%11-5)/16
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			out[i*channels+c] = int16(v)
		}
	}
	return out
}

// firstDiffByte returns the index of the first differing byte, or the shorter
// length when one is a prefix of the other.
func firstDiffByte(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
