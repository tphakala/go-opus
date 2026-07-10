package opus

import (
	"fmt"

	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/packet"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// maxPacketSamples is the largest per-channel sample count a single Opus packet
// can decode to: 120 ms at 48 kHz (RFC 6716 section 3.2.5).
const maxPacketSamples = 5760

// Decoder decodes Opus packets to interleaved 16-bit PCM, mirroring the CELT
// branch of libopus opus_decode_frame. In phase 2 it handles CELT-only packets;
// SILK-only and hybrid packets return ErrUnsupportedMode until phase 3 adds
// those decoders.
//
// A Decoder is stateful: the underlying CELT decoder carries overlap-add,
// energy prediction, and post-filter memory across packets, so packets from one
// stream must be fed to one Decoder in order. It is not safe for concurrent use;
// use one Decoder per goroutine.
type Decoder struct {
	sampleRate int
	channels   int
	celt       *celt.Decoder

	lastPacketDuration int
	finalRange         uint32
}

// NewDecoder returns a decoder producing sampleRate-Hz, channels-channel PCM.
// sampleRate must be one of 8000, 12000, 16000, 24000, or 48000; channels must
// be 1 or 2. Anything else returns ErrBadArg. The RFC 6716 conformance vectors
// decode at 48000 Hz.
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	switch sampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return nil, fmt.Errorf("%w: sample rate %d (want 8000, 12000, 16000, 24000, or 48000)", ErrBadArg, sampleRate)
	}
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("%w: channels %d (want 1 or 2)", ErrBadArg, channels)
	}

	cd := celt.NewDecoder(channels)
	if cd == nil {
		return nil, fmt.Errorf("%w: celt decoder init", ErrInternal)
	}
	// The CELT decoder runs the 48 kHz mode and downsamples to the output rate,
	// exactly as celt_decoder_init derives resampling_factor(Fs). 48000 keeps the
	// default factor 1; the lower rates all divide 48000 evenly.
	if ds := 48000 / sampleRate; ds != 1 {
		if err := cd.SetDownsample(ds); err != nil {
			return nil, fmt.Errorf("%w: celt downsample %d: %w", ErrInternal, ds, err)
		}
	}

	return &Decoder{
		sampleRate: sampleRate,
		channels:   channels,
		celt:       cd,
	}, nil
}

// Decode decodes one Opus packet into interleaved PCM and returns the number of
// samples produced per channel. pcm must hold at least that many samples times
// the channel count; a too-small buffer returns ErrBufferTooSmall without
// modifying decoder state visible to the caller.
//
// Phase 2 supports CELT-only packets. A SILK-only or hybrid packet returns
// ErrUnsupportedMode. A malformed packet returns ErrInvalidPacket; the decoder
// never panics on hostile input. Like opus_decode, this does not drop the
// codec pre-skip: dropping it is the container (oggopus) decoder's job.
func (d *Decoder) Decode(pkt []byte, pcm []int16) (int, error) {
	if len(pkt) == 0 {
		// A nil or empty packet requests packet-loss concealment in the full API
		// (see docs/api-design.md). The CELT-only phase 2 decoder does not run PLC
		// through the public entry point.
		// TODO(phase 3): conceal a lost frame of the duration implied by len(pcm).
		return 0, fmt.Errorf("%w: empty packet (PLC not supported in phase 2)", ErrInvalidPacket)
	}

	toc := packet.ParseTOC(pkt[0])
	if toc.Mode() != packet.ModeCELTOnly {
		// TODO(phase 3): decode SILK-only and hybrid packets.
		return 0, fmt.Errorf("%w: %s", ErrUnsupportedMode, toc.Mode())
	}

	p, err := packet.Parse(pkt)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidPacket, err)
	}

	// Configure the CELT decoder for this packet, mirroring opus_decode_frame:
	// CELT-only always starts at band 0 (hybrid would start at 17); the end band
	// follows the TOC bandwidth; the stream channel count follows the TOC stereo
	// flag. The physical output channels stay fixed at the decoder's channel
	// count, so a mono packet in a stereo decoder is upmixed by CELT.
	end := endBandForBandwidth(toc.Bandwidth())
	if err := d.celt.SetStartBand(0); err != nil {
		return 0, fmt.Errorf("%w: set start band: %w", ErrInternal, err)
	}
	if err := d.celt.SetEndBand(end); err != nil {
		return 0, fmt.Errorf("%w: set end band %d: %w", ErrInternal, end, err)
	}
	if err := d.celt.SetStreamChannels(toc.Channels()); err != nil {
		return 0, fmt.Errorf("%w: set stream channels %d: %w", ErrInternal, toc.Channels(), err)
	}

	// Every frame in the packet has the same duration; decode them back to back
	// into pcm, each frame driven by its own range decoder over its own bytes,
	// exactly as opus_decode_native loops opus_decode_frame per frame.
	frameSize := toc.SamplesPerFrame(d.sampleRate)
	total := 0
	var rd rangecoding.Decoder
	for i, frame := range p.Frames {
		if (total+frameSize)*d.channels > len(pcm) {
			return 0, fmt.Errorf("%w: pcm holds %d samples, need %d through frame %d",
				ErrBufferTooSmall, len(pcm), (total+frameSize)*d.channels, i)
		}
		rd.Init(frame)
		n, err := d.celt.DecodeWithEC(frame, pcm[total*d.channels:], frameSize, &rd, 0)
		if err != nil {
			return 0, fmt.Errorf("%w: celt frame %d: %w", ErrInvalidPacket, i, err)
		}
		total += n
		// The packet final range is the CELT range-coder state after its last
		// frame (OPUS_GET_FINAL_RANGE), the value conformance compares.
		d.finalRange = d.celt.Rng()
	}

	d.lastPacketDuration = total
	return total, nil
}

// FinalRange returns the range-coder state after the last decoded frame
// (OPUS_GET_FINAL_RANGE). For a conformant decode it equals the encoder's final
// range recorded in the test bitstream, the strong per-packet bit-exactness
// check.
func (d *Decoder) FinalRange() uint32 { return d.finalRange }

// LastPacketDuration returns the number of samples per channel produced by the
// most recent successful Decode call.
func (d *Decoder) LastPacketDuration() int { return d.lastPacketDuration }

// Reset clears cross-packet decoder state (overlap-add, energy prediction,
// post-filter memory) while keeping the configured sample rate and channel
// count, mirroring opus_decoder_ctl(OPUS_RESET_STATE).
func (d *Decoder) Reset() {
	d.celt.ResetState()
	d.lastPacketDuration = 0
	d.finalRange = 0
}

// endBandForBandwidth maps a TOC bandwidth to the CELT end band, mirroring the
// switch in opus_decode_frame (opus_decoder.c): NB->13, MB and WB->17, SWB->19,
// FB->21.
func endBandForBandwidth(bw packet.Bandwidth) int {
	switch bw {
	case packet.BandwidthNarrowband:
		return 13
	case packet.BandwidthMediumband, packet.BandwidthWideband:
		return 17
	case packet.BandwidthSuperwideband:
		return 19
	default: // packet.BandwidthFullband
		return 21
	}
}
