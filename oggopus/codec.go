package oggopus

import (
	"fmt"

	"github.com/tphakala/go-opus/opus"
)

// This file is the codec seam: the single, clearly marked boundary where the
// opus codec attaches to the container layer. Everything else in oggopus (the
// page layer, the RFC 7845 headers, the packet-level writer and reader) is
// codec-independent and is tested without one. The two constructor variables
// below are the only place oggopus depends on the codec, and they depend on it
// only through the public opus package, never on its internals.

// frameEncoder turns one frame of interleaved int16 PCM into one opus packet
// (a multiframe packet for the 40 ms and longer durations). It is the minimal
// surface the container writer needs from the codec.
// The interface is unexported, so the adapter that satisfies it lives inside
// oggopus and wraps the public opus.Encoder; the container never depends on the
// opus package's concrete types.
type frameEncoder interface {
	// encodeFrame encodes one frame of interleaved int16 PCM (channels
	// interleaved; len(pcm) == samplesPerChannel*channels) and returns the opus
	// packet bytes and the frame's duration in samples per channel at 48 kHz.
	//
	// The returned packet aliases an internal buffer and stays valid only until
	// the next encodeFrame call; containerWriter.writePacket copies it.
	encodeFrame(pcm []int16) (packet []byte, samples48k int, err error)
	// lookahead reports the encoder pre-skip in 48 kHz samples, written into
	// OpusHead.
	lookahead() int
	// close releases codec resources.
	close()
}

// frameDecoder turns one opus packet into interleaved int16 PCM at 48 kHz. It is
// the minimal surface the container reader needs from the codec.
type frameDecoder interface {
	// decodeFrame decodes one opus packet into interleaved int16 PCM at 48 kHz
	// (channels interleaved). The returned PCM aliases an internal buffer and
	// stays valid only until the next decodeFrame call; Decoder.fill copies it.
	decodeFrame(packet []byte) (pcm []int16, err error)
	// close releases codec resources.
	close()
}

// maxPacketBytes sizes the encoder's packet scratch buffer. opus_encode clamps
// its own output window to IMIN(1276*6, out_data_bytes) (opus_encoder.c:1221), so
// 1276*6 is the hard ceiling on what one Encode call can ever write and the
// buffer can never be the binding constraint. A VBR packet fits in 1276; only
// hard CBR above roughly 510 kbps at 20 ms pads past that (internal/opusenc
// rates.go:405), and sizing to the libopus ceiling means even the maximum bitrate
// is never silently rate-limited by a short buffer.
const maxPacketBytes = 1276 * 6

// maxFrameSamples48k is the longest Opus frame, 120 ms at 48 kHz. The container's
// own encoder writes frames of the configured duration (20 ms by default, up to
// 120 ms), and the decoder must accept any stream, so its scratch buffer is sized
// for the format's maximum.
const maxFrameSamples48k = 5760

// opusFrameEncoder adapts the public opus.Encoder to the frameEncoder seam.
type opusFrameEncoder struct {
	enc        *opus.Encoder
	sampleRate int
	channels   int
	preSkip48k int
	buf        []byte
}

// encodeFrame encodes one frame and reports its duration at 48 kHz. The duration
// is derived from the frame's own length rather than assumed, so it stays correct
// if the container's frame duration ever changes.
func (e *opusFrameEncoder) encodeFrame(pcm []int16) (packet []byte, samples48k int, err error) {
	n, err := e.enc.Encode(pcm, e.buf)
	if err != nil {
		return nil, 0, fmt.Errorf("oggopus: encoding a frame: %w", err)
	}
	// Every rate opus.NewEncoder accepts divides 48000 exactly, so this is exact.
	return e.buf[:n], len(pcm) / e.channels * sampleRate48k / e.sampleRate, nil
}

// lookahead reports the pre-skip in 48 kHz samples, which is what OpusHead
// carries. opus.Encoder.Lookahead mirrors OPUS_GET_LOOKAHEAD and returns the
// delay AT THE CODING RATE (Fs/400 + Fs/250); RFC 7845 section 5.1 defines the
// pre-skip field at 48 kHz regardless of the coding rate, so it must be scaled.
// This is the same scaling opus-tools' opusenc.c applies
// ("header.preskip = lookahead * (48000. / coding_rate)").
//
// Both terms of the lookahead are proportional to the sample rate and divide
// exactly, so the scaled value is 312 at every rate the encoder accepts. Writing
// the unscaled figure would misalign every decoded stream by the difference: 120
// samples at 48 kHz if the Fs/400 term were dropped, and far more at the low
// rates (52 instead of 312 at 8 kHz). TestOggOpusPreSkipIs312 pins it.
func (e *opusFrameEncoder) lookahead() int { return e.preSkip48k }

// close releases codec resources. opus.Encoder holds only Go memory, so there is
// nothing to release; the method exists because the seam has to work for a codec
// that does, and it is a no-op rather than absent so that double-close (Encoder
// .Close followed by a pooled Reset) is safe.
func (e *opusFrameEncoder) close() {}

// newFrameEncoder builds the per-stream encoder. It is a package variable (not a
// plain function) so the codec is wired in at exactly one site, and so tests can
// substitute a stub encoder for the container without a codec.
var newFrameEncoder = func(cfg Config) (frameEncoder, error) {
	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate:     cfg.SampleRate,
		Channels:       cfg.Channels,
		Bitrate:        cfg.Bitrate,
		CBR:            cfg.CBR,
		ConstrainedVBR: cfg.ConstrainedVBR,
		// Complexity is passed through UNMAPPED. opus.EncoderConfig owns the
		// zero-value-means-default(10) rule, so the default lives in exactly one
		// place and the two packages cannot disagree about what a zero means.
		Complexity: cfg.Complexity,
		DTX:        cfg.DTX,
	})
	if err != nil {
		// Config.validate has already rejected a bad rate, channel count, bitrate
		// or complexity with ErrInvalidConfig, and every field oggopus exposes
		// (including DTX) is now implemented, so reaching here means an unexpected
		// codec error. It is wrapped rather than swallowed; errors.Is still finds
		// the underlying opus sentinel through the wrap.
		return nil, fmt.Errorf("oggopus: building the opus encoder: %w", err)
	}
	return &opusFrameEncoder{
		enc:        enc,
		sampleRate: cfg.SampleRate,
		channels:   cfg.Channels,
		preSkip48k: enc.Lookahead() * sampleRate48k / cfg.SampleRate,
		buf:        make([]byte, maxPacketBytes),
	}, nil
}

// opusFrameDecoder adapts the public opus.Decoder to the frameDecoder seam.
type opusFrameDecoder struct {
	dec      *opus.Decoder
	channels int
	buf      []int16
}

// decodeFrame decodes one packet. The buffer is sized for the longest Opus frame,
// so a short packet simply fills less of it; opus.Decoder reports how much.
func (d *opusFrameDecoder) decodeFrame(packet []byte) ([]int16, error) {
	n, err := d.dec.Decode(packet, d.buf)
	if err != nil {
		return nil, fmt.Errorf("oggopus: decoding a packet: %w", err)
	}
	return d.buf[:n*d.channels], nil
}

// close releases codec resources; see opusFrameEncoder.close.
func (d *opusFrameDecoder) close() {}

// opusMSFrameDecoder adapts the public opus.MultistreamDecoder to the frameDecoder
// seam for channel mapping family 1 (surround) and any other family that carries a
// mapping table. It mirrors opusFrameDecoder but routes each packet through the N
// sub-streams the multistream decoder schedules onto the API channels.
type opusMSFrameDecoder struct {
	dec      *opus.MultistreamDecoder
	channels int
	buf      []int16
}

// decodeFrame decodes one multistream packet into interleaved int16 PCM for all
// API channels. The buffer is sized for the longest Opus frame across every
// channel, so a short packet fills less of it; opus.MultistreamDecoder reports the
// per-channel sample count.
func (d *opusMSFrameDecoder) decodeFrame(packet []byte) ([]int16, error) {
	n, err := d.dec.Decode(packet, d.buf)
	if err != nil {
		return nil, fmt.Errorf("oggopus: decoding a multistream packet: %w", err)
	}
	return d.buf[:n*d.channels], nil
}

// close releases codec resources; see opusFrameEncoder.close.
func (d *opusMSFrameDecoder) close() {}

// newFrameDecoder builds the per-stream decoder from the parsed OpusHead. Channel
// mapping family 0 (mono/stereo) decodes through a single opus.Decoder; a non-zero
// family (family 1 surround, and any other family that carries a mapping table)
// decodes through opus.MultistreamDecoder, driven by the OpusHead stream count,
// coupled-stream count and channel mapping the container parsed.
//
// The decoder always runs at 48 kHz, whatever OpusHead's input-sample-rate field
// says: RFC 7845 section 5.1 makes that field informational ("the sample rate of
// the original input"), while granule positions, the pre-skip and the output are
// all defined at 48 kHz. Decoding at the original rate instead would make every
// granule in the stream mean something different from what the container says.
var newFrameDecoder = func(head opusHead) (frameDecoder, error) {
	channels := int(head.channels)
	if head.mappingFamily == mappingFamily0 {
		dec, err := opus.NewDecoder(sampleRate48k, channels)
		if err != nil {
			// parseOpusHead already holds family 0 to 1 or 2 channels, so a bad rate
			// or channel count here is not expected; wrap it rather than swallow it.
			return nil, fmt.Errorf("oggopus: building the opus decoder for %d channels: %w", channels, err)
		}
		return &opusFrameDecoder{
			dec:      dec,
			channels: channels,
			buf:      make([]int16, maxFrameSamples48k*channels),
		}, nil
	}
	// RFC 7845 section 5.1.1 defines channel mapping family 1 for 1 to 8 channels; a
	// family-1 header declaring more is malformed, so reject it rather than build a
	// decoder for a layout the format does not permit. Other families that carry a
	// mapping table are not bound by this limit.
	if head.mappingFamily == 1 && channels > 8 {
		return nil, fmt.Errorf("oggopus: channel mapping family 1 allows at most 8 channels, got %d (RFC 7845)", channels)
	}
	// A non-zero mapping family carries an OpusHead mapping table (stream count,
	// coupled count, and one mapping byte per channel); route the N sub-streams
	// through the multistream decoder. NewMultistreamDecoder validates the layout
	// and returns ErrBadArg for an inconsistent table, so a hostile header is
	// rejected with an error rather than a panic.
	dec, err := opus.NewMultistreamDecoder(sampleRate48k, channels,
		int(head.streamCount), int(head.coupledCount), head.channelMapping)
	if err != nil {
		return nil, fmt.Errorf("oggopus: building the multistream opus decoder for %d channels (mapping family %d, %d streams, %d coupled): %w",
			channels, head.mappingFamily, head.streamCount, head.coupledCount, err)
	}
	return &opusMSFrameDecoder{
		dec:      dec,
		channels: channels,
		buf:      make([]int16, maxFrameSamples48k*channels),
	}, nil
}
