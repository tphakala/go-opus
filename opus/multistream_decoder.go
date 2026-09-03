package opus

import (
	"fmt"

	"github.com/tphakala/go-opus/internal/opusdec"
)

// MultistreamDecoder decodes multistream Opus packets (RFC 7845 channel mapping
// families 0 and 1) to interleaved 16-bit PCM. A multistream packet concatenates
// one Opus sub-packet per stream; the first CoupledStreams streams are stereo,
// the rest mono, and each decoded stream channel is routed to the API channels a
// mapping table names. This is the surround-capable counterpart to Decoder and
// wraps internal/opusdec's transliteration of libopus opus_multistream_decoder.
//
// Like Decoder it is stateful (every sub-decoder carries cross-packet memory) and
// not safe for concurrent use; feed one stream's packets to one decoder in order,
// and use one MultistreamDecoder per goroutine.
type MultistreamDecoder struct {
	sampleRate int
	channels   int
	dec        *opusdec.OpusMSDecoder
}

// NewMultistreamDecoder returns a decoder producing sampleRate-Hz, channels-channel
// interleaved PCM from multistream Opus packets. sampleRate must be one of 8000,
// 12000, 16000, 24000, or 48000. streams is the number of Opus streams in each
// packet (1..255) and coupledStreams how many of them are stereo (0..streams,
// with streams+coupledStreams <= 255). mapping has exactly channels entries; entry
// i names the decoded stream channel feeding API channel i, where the coupled
// streams occupy indices 0..2*coupledStreams-1 (left/right interleaved) and the
// mono streams the indices above them, or 255 to leave the channel muted. Anything
// out of range returns ErrBadArg.
//
// For the standard family-1 surround layouts, obtain streams, coupledStreams, and
// mapping from the stream's OpusHead (the container carries them); this decoder
// does not infer them from the channel count.
func NewMultistreamDecoder(sampleRate, channels, streams, coupledStreams int, mapping []byte) (*MultistreamDecoder, error) {
	switch sampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return nil, fmt.Errorf("%w: sample rate %d (want 8000, 12000, 16000, 24000, or 48000)", ErrBadArg, sampleRate)
	}
	if channels < 1 || channels > 255 {
		return nil, fmt.Errorf("%w: channels %d (want 1..255)", ErrBadArg, channels)
	}
	if streams < 1 || streams > 255 {
		return nil, fmt.Errorf("%w: streams %d (want 1..255)", ErrBadArg, streams)
	}
	if coupledStreams < 0 || coupledStreams > streams || streams > 255-coupledStreams {
		return nil, fmt.Errorf("%w: coupled streams %d (want 0..%d with streams+coupled<=255)", ErrBadArg, coupledStreams, streams)
	}
	if len(mapping) != channels {
		return nil, fmt.Errorf("%w: mapping has %d entries (want channels=%d)", ErrBadArg, len(mapping), channels)
	}
	maxChan := streams + coupledStreams
	for i, m := range mapping {
		if int(m) >= maxChan && m != 255 {
			return nil, fmt.Errorf("%w: mapping[%d]=%d out of range (want <%d or 255)", ErrBadArg, i, m, maxChan)
		}
	}

	dec, err := opusdec.NewMSDecoder(int32(sampleRate), channels, streams, coupledStreams, mapping)
	if err != nil {
		return nil, mapDecErr(err)
	}
	return &MultistreamDecoder{
		sampleRate: sampleRate,
		channels:   channels,
		dec:        dec,
	}, nil
}

// Decode decodes one multistream Opus packet into interleaved PCM and returns the
// number of samples produced per channel. pcm must hold at least that many samples
// times the channel count; a too-small buffer returns ErrBufferTooSmall. A nil or
// empty packet requests packet-loss concealment for the frame duration implied by
// len(pcm), which must be a multiple of 2.5 ms per channel. A malformed packet
// returns ErrInvalidPacket; the decoder never panics on hostile input.
func (d *MultistreamDecoder) Decode(pkt []byte, pcm []int16) (int, error) {
	return d.decode(pkt, pcm, 0)
}

// DecodeFEC reconstructs a lost frame from the in-band FEC carried by the next
// received packet, mirroring opus_multistream_decode with decode_fec=1. pkt is
// that next packet; pcm receives the recovered frame and its length per channel
// must be a multiple of 2.5 ms. Streams without FEC for the lost duration fall
// back to PLC. Returns the per-channel sample count.
func (d *MultistreamDecoder) DecodeFEC(pkt []byte, pcm []int16) (int, error) {
	return d.decode(pkt, pcm, 1)
}

// decode is the shared body of Decode / DecodeFEC. frameSize is the caller's
// per-channel buffer capacity, exactly the frame_size opus_multistream_decode
// passes down.
func (d *MultistreamDecoder) decode(pkt []byte, pcm []int16, decodeFec int) (int, error) {
	frameSize := len(pcm) / d.channels
	n, err := d.dec.Decode(pkt, pcm, frameSize, decodeFec)
	if err != nil {
		return 0, mapDecErr(err)
	}
	return n, nil
}

// Channels returns the number of interleaved output channels the decoder produces.
func (d *MultistreamDecoder) Channels() int { return d.channels }

// Streams returns the number of Opus streams each packet carries.
func (d *MultistreamDecoder) Streams() int { return d.dec.Streams() }

// CoupledStreams returns the number of stereo (coupled) streams; the remaining
// Streams()-CoupledStreams() streams are mono.
func (d *MultistreamDecoder) CoupledStreams() int { return d.dec.CoupledStreams() }

// Mapping returns a copy of the channel mapping table passed to
// NewMultistreamDecoder: one entry per output channel, naming the decoded stream
// channel that feeds it, or 255 for a muted channel. The returned slice is a
// fresh copy, so mutating it does not affect the decoder.
func (d *MultistreamDecoder) Mapping() []byte { return d.dec.Mapping() }

// FinalRange returns the XOR of every stream's post-decode range-coder state
// (OPUS_GET_FINAL_RANGE), the strong per-packet bit-exactness check against the
// encoder's recorded value.
func (d *MultistreamDecoder) FinalRange() uint32 { return d.dec.FinalRange() }

// LastPacketDuration returns the number of samples per channel produced by the
// most recent successful Decode call.
func (d *MultistreamDecoder) LastPacketDuration() int { return d.dec.LastPacketDuration() }

// Reset clears cross-packet state in every stream decoder while keeping the
// configured layout, mirroring opus_multistream_decoder_ctl(OPUS_RESET_STATE).
func (d *MultistreamDecoder) Reset() { d.dec.ResetState() }
