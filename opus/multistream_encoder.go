package opus

import (
	"errors"
	"fmt"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// MultistreamEncoderConfig configures a MultistreamEncoder. Like EncoderConfig it
// is a flat struct whose every field's zero value is documented, so a literal with
// only the layout fields set is a complete, valid configuration.
//
// Like the single-stream Encoder, every sub-encoder is CELT-only
// (OPUS_APPLICATION_AUDIO, OPUS_SET_FORCE_MODE(MODE_CELT_ONLY)); the tuning fields
// below are forwarded to all of them. This is the RFC 7845 channel mapping family
// 0 and 255 encoder: N independent streams, the first CoupledStreams stereo and
// the rest mono, concatenated with self-delimited framing. Surround (family 1,
// which adds per-stream energy masking) is not yet supported.
type MultistreamEncoderConfig struct {
	// SampleRate is the input sample rate in Hz: 8000, 12000, 16000, 24000, or
	// 48000. Required; there is no zero default.
	SampleRate int
	// Channels is the number of interleaved input channels, 1..255. Required.
	Channels int
	// Streams is the number of Opus streams the encoder emits per packet, 1..255.
	// Required.
	Streams int
	// CoupledStreams is how many of the streams are stereo (coupled); the rest are
	// mono. 0..Streams, with Streams+CoupledStreams <= 255 and <= Channels.
	CoupledStreams int
	// Mapping has exactly Channels entries; entry i names the encoded stream channel
	// fed by input channel i, where the coupled streams occupy indices
	// 0..2*CoupledStreams-1 (left/right interleaved) and the mono streams the indices
	// above them, or 255 to drop the channel. Every stream must be referenced by at
	// least one channel. Required.
	//
	// For the standard family-1 surround layouts, use the same (Streams,
	// CoupledStreams, Mapping) a decoder reads from the stream's OpusHead.
	Mapping []byte

	// Bitrate is the target rate in bits per second for the WHOLE stream. The zero
	// value selects automatic (OPUS_AUTO). A positive value is clamped into
	// [500*Channels, 750000*Channels], as the multistream OPUS_SET_BITRATE does; a
	// negative value is rejected.
	Bitrate int
	// CBR forces constant bitrate (OPUS_SET_VBR(0)) on every sub-encoder. The zero
	// value (false) is variable bitrate.
	CBR bool
	// ConstrainedVBR selects constrained VBR (OPUS_SET_VBR_CONSTRAINT(1)); meaningful
	// only when CBR is false. The zero value (false) is unconstrained VBR. NOTE that
	// libopus' own default is constrained; this field's zero value deliberately
	// differs, matching EncoderConfig.
	ConstrainedVBR bool
	// Complexity is 1..10 (OPUS_SET_COMPLEXITY) on every sub-encoder. The zero value
	// selects the default, 10. Values outside 0..10 are rejected.
	Complexity int
	// DTX requests discontinuous transmission (OPUS_SET_DTX) on every sub-encoder.
	// The zero value (false) leaves it off.
	DTX bool
}

// MultistreamEncoder encodes interleaved 16-bit PCM to multistream Opus packets
// (RFC 7845 channel mapping families 0 and 255). It is the surround-capable
// counterpart of Encoder and wraps internal/opusenc's transliteration of libopus
// opus_multistream_encode.
//
// Like Encoder it is stateful (every sub-encoder carries cross-frame memory) and
// not safe for concurrent use; feed one stream's frames to one encoder in order,
// and use one MultistreamEncoder per goroutine.
type MultistreamEncoder struct {
	sampleRate int
	channels   int
	enc        *opusenc.OpusMSEncoder
}

// NewMultistreamEncoder returns a MultistreamEncoder for cfg, mirroring
// opus_multistream_encoder_create followed by the OPUS_SET_* ctls cfg names. An
// invalid field returns ErrBadArg.
//
// Every sub-encoder is created with OPUS_APPLICATION_AUDIO and
// OPUS_SET_FORCE_MODE(MODE_CELT_ONLY), the only mode go-opus currently supports.
func NewMultistreamEncoder(cfg MultistreamEncoderConfig) (*MultistreamEncoder, error) { //nolint:gocritic // Config passed by value to match the public API (as opus.NewEncoder)
	switch cfg.SampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return nil, fmt.Errorf("%w: sample rate %d (want 8000, 12000, 16000, 24000, or 48000)",
			ErrBadArg, cfg.SampleRate)
	}
	if cfg.Channels < 1 || cfg.Channels > 255 {
		return nil, fmt.Errorf("%w: channels %d (want 1..255)", ErrBadArg, cfg.Channels)
	}
	if cfg.Streams < 1 || cfg.Streams > 255 {
		return nil, fmt.Errorf("%w: streams %d (want 1..255)", ErrBadArg, cfg.Streams)
	}
	if cfg.CoupledStreams < 0 || cfg.CoupledStreams > cfg.Streams || cfg.Streams > 255-cfg.CoupledStreams {
		return nil, fmt.Errorf("%w: coupled streams %d (want 0..%d with streams+coupled<=255)",
			ErrBadArg, cfg.CoupledStreams, cfg.Streams)
	}
	if cfg.Streams+cfg.CoupledStreams > cfg.Channels {
		return nil, fmt.Errorf("%w: streams+coupled %d exceeds channels %d",
			ErrBadArg, cfg.Streams+cfg.CoupledStreams, cfg.Channels)
	}
	if len(cfg.Mapping) != cfg.Channels {
		return nil, fmt.Errorf("%w: mapping has %d entries (want channels=%d)",
			ErrBadArg, len(cfg.Mapping), cfg.Channels)
	}
	maxChan := cfg.Streams + cfg.CoupledStreams
	for i, m := range cfg.Mapping {
		if int(m) >= maxChan && m != 255 {
			return nil, fmt.Errorf("%w: mapping[%d]=%d out of range (want <%d or 255)",
				ErrBadArg, i, m, maxChan)
		}
	}
	if cfg.Bitrate < 0 {
		return nil, fmt.Errorf("%w: bitrate %d (want a positive bits-per-second, or 0 for automatic)",
			ErrBadArg, cfg.Bitrate)
	}
	if cfg.Complexity < 0 || cfg.Complexity > maxComplexity {
		return nil, fmt.Errorf("%w: complexity %d (want 1..%d, or 0 for the default %d)",
			ErrBadArg, cfg.Complexity, maxComplexity, defaultComplexity)
	}

	enc, err := opusenc.NewMSEncoder(int32(cfg.SampleRate), cfg.Channels, cfg.Streams,
		cfg.CoupledStreams, opusenc.ApplicationAudio, cfg.Mapping)
	if err != nil {
		// The public range checks above cover every argument the internal validator
		// rejects except the encoder-layout rule (every stream referenced by a
		// channel), which surfaces here as ErrBadArg.
		return nil, mapEncErr(err)
	}

	bitrate := int32(opusenc.OpusAuto)
	if cfg.Bitrate > 0 {
		// Clamp in Go BEFORE narrowing to int32, so a value above MaxInt32 cannot wrap
		// into OPUS_AUTO or another sentinel. Values within int32 are left to the
		// multistream SetBitrate, which applies the same [500*ch, 750000*ch] clamp.
		if maxBitrate := 750000 * cfg.Channels; cfg.Bitrate > maxBitrate {
			bitrate = int32(maxBitrate)
		} else {
			bitrate = int32(cfg.Bitrate)
		}
	}
	complexity := cfg.Complexity
	if complexity == 0 {
		complexity = defaultComplexity
	}
	vbr, vbrConstraint := 1, 0
	if cfg.CBR {
		vbr = 0
	}
	if cfg.ConstrainedVBR {
		vbrConstraint = 1
	}
	dtx := 0
	if cfg.DTX {
		dtx = 1
	}

	if err := errors.Join(
		enc.SetForceMode(opusenc.ModeCeltOnly),
		enc.SetBitrate(bitrate),
		enc.SetComplexity(complexity),
		enc.SetVBR(vbr),
		enc.SetVBRConstraint(vbrConstraint),
		enc.SetDTX(dtx),
	); err != nil {
		return nil, mapEncErr(err)
	}

	return &MultistreamEncoder{
		sampleRate: cfg.SampleRate,
		channels:   cfg.Channels,
		enc:        enc,
	}, nil
}

// Encode encodes exactly one frame of interleaved PCM into buf and returns the
// packet length in bytes; buf[:n] is the multistream packet. It mirrors
// opus_multistream_encode.
//
// len(pcm) must be samplesPerChannel*Channels, and samplesPerChannel must be one of
// the Opus frame durations (2.5, 5, 10, 20, 40, 60, 80, 100 or 120 ms). The 40 ms
// and longer durations are coded as multiframe sub-packets. Any other length
// returns ErrBadArg. buf must hold at least the smallest packet, which is 2*Streams-1
// bytes for most frame durations and 3*Streams-1 for a 100 ms frame (Opus reserves an
// extra table-of-contents byte per stream there); too small returns ErrBufferTooSmall.
// buf is caller-provided, so a steady-state encode allocates nothing.
//
// After a rejected Encode (the three checks below), FinalRange keeps the previous
// packet's value rather than resetting to 0. This differs from the single-stream
// Encoder, and it matches libopus: a multistream encoder has no top-level range
// state, so a rejection that never reaches a sub-encoder leaves the sub-encoders'
// ranges (and thus their XOR) untouched.
func (e *MultistreamEncoder) Encode(pcm []int16, buf []byte) (int, error) {
	if len(pcm) == 0 || len(pcm)%e.channels != 0 {
		return 0, fmt.Errorf("%w: len(pcm) is %d, want a positive multiple of %d (the channel count)",
			ErrBadArg, len(pcm), e.channels)
	}
	frameSize := len(pcm) / e.channels
	if err := e.checkFrameSize(frameSize); err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, fmt.Errorf("%w: the output buffer is empty", ErrBufferTooSmall)
	}

	n, err := e.enc.Encode(pcm, frameSize, buf, len(buf))
	if err != nil {
		return 0, mapEncErr(err)
	}
	return n, nil
}

// checkFrameSize applies the Opus frame-duration domain to a per-channel sample
// count, matching Encoder.checkFrameSize.
func (e *MultistreamEncoder) checkFrameSize(frameSize int) error {
	fs := e.sampleRate
	switch frameSize {
	case fs / 400, fs / 200, fs / 100, fs / 50, // 2.5, 5, 10, 20 ms
		fs / 25, 3 * fs / 50, 4 * fs / 50, 5 * fs / 50, 6 * fs / 50: // 40, 60, 80, 100, 120 ms
		return nil
	default:
		return fmt.Errorf("%w: a %d-sample frame is not an Opus frame duration; want 2.5, 5, "+
			"10, 20, 40, 60, 80, 100 or 120 ms (%d to %d samples per channel at %d Hz)",
			ErrBadArg, frameSize, fs/400, 6*fs/50, fs)
	}
}

// Channels returns the number of interleaved input channels the encoder expects.
func (e *MultistreamEncoder) Channels() int { return e.channels }

// Streams returns the number of Opus streams each packet carries.
func (e *MultistreamEncoder) Streams() int { return e.enc.Streams() }

// CoupledStreams returns the number of stereo (coupled) streams; the remaining
// Streams()-CoupledStreams() streams are mono.
func (e *MultistreamEncoder) CoupledStreams() int { return e.enc.CoupledStreams() }

// Mapping returns a copy of the channel mapping table passed to
// NewMultistreamEncoder.
func (e *MultistreamEncoder) Mapping() []byte { return e.enc.Mapping() }

// FinalRange returns the XOR of every stream's range-coder state after the last
// packet (OPUS_GET_FINAL_RANGE), the strong per-packet bit-exactness check.
func (e *MultistreamEncoder) FinalRange() uint32 { return e.enc.FinalRange() }

// Reset clears cross-frame state in every sub-encoder while keeping the configured
// layout and tuning, mirroring opus_multistream_encoder_ctl(OPUS_RESET_STATE).
func (e *MultistreamEncoder) Reset() { e.enc.ResetState() }
