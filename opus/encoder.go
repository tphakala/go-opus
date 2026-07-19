package opus

import (
	"errors"
	"fmt"

	"github.com/tphakala/go-opus/internal/opusenc"
)

// ErrUnsupported reports a configuration that the Opus format allows but that
// this release does not implement, as opposed to one that is simply invalid.
// libopus reports these as OPUS_UNIMPLEMENTED, and it is a separate sentinel for
// the same reason libopus keeps a separate code: "the format does not permit
// this" and "this build cannot do it yet" are different facts, and only the
// second one can change without a format change.
//
// It is returned for exactly two things in v1: EncoderConfig.DTX (discontinuous
// transmission is deferred), and a frame longer than 20 ms handed to
// Encoder.Encode (libopus splits those into 20 ms sub-frames and reassembles them
// with the repacketizer at opus_encoder.c:1698; that path is deferred). Neither is
// silently accepted.
var ErrUnsupported = errors.New("opus: unsupported configuration")

const (
	// defaultComplexity is the complexity EncoderConfig.Complexity's zero value
	// selects. It is 10, the top of the range, which is the go-opus default and
	// NOT the libopus one: opus_encoder_init seeds silk_mode.complexity to 9
	// (opus_encoder.c:270). NewEncoder therefore always issues an explicit
	// OPUS_SET_COMPLEXITY and never relies on the C default.
	defaultComplexity = 10
	// maxComplexity is the top of the OPUS_SET_COMPLEXITY range
	// (opus_encoder.c:2936).
	maxComplexity = 10
)

// EncoderConfig configures an Encoder. It is a flat struct: every field's zero
// value is documented, so a literal with only SampleRate and Channels set is a
// complete, valid configuration.
//
// v1 fixes the internals this struct does not expose: OPUS_APPLICATION_AUDIO,
// OPUS_SET_FORCE_MODE(MODE_CELT_ONLY), automatic (Nyquist-limited) bandwidth, and
// int16 PCM. They become options only if a later phase adds SILK.
type EncoderConfig struct {
	// SampleRate is the input sample rate in Hz: 8000, 12000, 16000, 24000, or
	// 48000. Required; there is no zero default. Every one of the five is coded
	// bit-exactly against libopus, and anything else is rejected rather than
	// resampled.
	SampleRate int
	// Channels is 1 (mono) or 2 (interleaved stereo). Required; there is no zero
	// default. A stereo encoder may still emit mono-coded packets when the
	// bitrate is too low to carry two channels; that is libopus' own decision
	// (opus_encoder.c:1428) and it shows up in the packet's TOC byte.
	Channels int

	// Bitrate is the target rate in bits per second. The zero value selects
	// automatic (OPUS_AUTO), which libopus derives from the sample rate and the
	// channel count. A positive value is clamped into [500, 750000*Channels], as
	// OPUS_SET_BITRATE does (opus_encoder.c:2817); a negative value is rejected.
	Bitrate int
	// CBR forces constant bitrate (OPUS_SET_VBR(0)). The zero value (false) is
	// variable bitrate.
	CBR bool
	// ConstrainedVBR selects constrained VBR (OPUS_SET_VBR_CONSTRAINT(1)), which
	// keeps the rate within the limits of a hypothetical decoder buffer. It is
	// meaningful only when CBR is false: with CBR set, CELT's rate controller
	// never reads the constraint. The zero value (false) is unconstrained VBR.
	//
	// NOTE that libopus' own default is CONSTRAINED VBR (opus_encoder.c:299 sets
	// vbr_constraint = 1). This field's zero value deliberately differs, because a
	// Go config field whose zero value is "on" reads as a trap. Set it explicitly
	// to reproduce the libopus default.
	ConstrainedVBR bool

	// Complexity is 1..10 (OPUS_SET_COMPLEXITY). The zero value selects the
	// library default, which is 10. Values outside 0..10 are rejected.
	Complexity int

	// DTX requests discontinuous transmission (OPUS_SET_DTX).
	//
	// IT IS NOT IMPLEMENTED. Setting it returns ErrUnsupported from NewEncoder
	// rather than being quietly ignored, because a caller who asked for DTX and
	// silently did not get it would ship a stream that is merely larger than they
	// budgeted for, with nothing to show them why.
	DTX bool
}

// Encoder encodes interleaved 16-bit PCM to Opus packets. It is the thin,
// idiomatic public wrapper over internal/opusenc, which holds the verbatim
// transliteration of libopus opus_encode / opus_encode_native (the rate,
// stereo/mono and bandwidth decision chain, the delay-compensation ring, and the
// CELT core). One Encode call produces exactly one packet, mirroring opus_encode;
// the Ogg container layer lives in the sibling oggopus package.
//
// An Encoder is stateful: the delay-compensation ring, the DC-blocking filter,
// the CELT overlap-add, energy prediction, prefilter and VBR reservoir all carry
// across frames, so the frames of one stream must be fed to one Encoder in order.
// It is not safe for concurrent use; use one Encoder per goroutine.
type Encoder struct {
	sampleRate int
	channels   int
	enc        *opusenc.Encoder
}

// NewEncoder returns an Encoder for cfg, mirroring opus_encoder_create followed by
// the OPUS_SET_* ctls cfg names. An invalid field returns ErrBadArg; a valid but
// unimplemented one (DTX) returns ErrUnsupported.
//
// The encoder is created with OPUS_APPLICATION_AUDIO and
// OPUS_SET_FORCE_MODE(MODE_CELT_ONLY), which is the v1 scope and is not
// configurable. Every other ctl is left at its opus_encoder_init default except
// the four cfg sets: bitrate, complexity, VBR and the VBR constraint.
func NewEncoder(cfg EncoderConfig) (*Encoder, error) {
	switch cfg.SampleRate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return nil, fmt.Errorf("%w: sample rate %d (want 8000, 12000, 16000, 24000, or 48000)",
			ErrBadArg, cfg.SampleRate)
	}
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return nil, fmt.Errorf("%w: channels %d (want 1 or 2)", ErrBadArg, cfg.Channels)
	}
	if cfg.Bitrate < 0 {
		return nil, fmt.Errorf("%w: bitrate %d (want a positive bits-per-second, or 0 for automatic)",
			ErrBadArg, cfg.Bitrate)
	}
	if cfg.Complexity < 0 || cfg.Complexity > maxComplexity {
		return nil, fmt.Errorf("%w: complexity %d (want 1..%d, or 0 for the default %d)",
			ErrBadArg, cfg.Complexity, maxComplexity, defaultComplexity)
	}
	if cfg.DTX {
		return nil, fmt.Errorf("%w: DTX is not implemented in this release", ErrUnsupported)
	}

	enc := opusenc.NewEncoder(int32(cfg.SampleRate), cfg.Channels, opusenc.ApplicationAudio)
	if enc == nil {
		// Unreachable: the sample rate and channel count were just validated
		// against the same domain opus_encoder_init enforces. Reported rather than
		// dereferenced, so a future divergence between the two domains surfaces as
		// an error and not as a nil-pointer panic.
		return nil, fmt.Errorf("%w: opus_encoder_init rejected sample rate %d, channels %d",
			ErrBadArg, cfg.SampleRate, cfg.Channels)
	}

	bitrate := int32(opusenc.OpusAuto)
	if cfg.Bitrate > 0 {
		// Clamp in Go BEFORE narrowing to int32. EncoderConfig.Bitrate is a 64-bit
		// int, so a value above MaxInt32 wraps on conversion and silently becomes
		// something else entirely: 4294966296 wraps to -1000, which is OPUS_AUTO, so
		// the encoder ignores the caller's request without any error. The C ctl takes
		// an opus_int32 and cannot express these inputs at all, so there is no C
		// behaviour to match here; clamping honours the documented
		// [500, 750000*Channels] contract rather than wrapping into it. Values that
		// do fit int32 are left to SetBitrate, which applies the same clamp the C does
		// (opus_encoder.c:2817).
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

	// None of these five can fail on a config that passed the checks above, but an
	// unchecked ctl is how a silently misconfigured encoder happens, so they are
	// checked. errors.Join keeps errors.Is working through mapEncErr.
	if err := errors.Join(
		enc.SetForceMode(opusenc.ModeCeltOnly),
		enc.SetBitrate(bitrate),
		enc.SetComplexity(complexity),
		enc.SetVBR(vbr),
		enc.SetVBRConstraint(vbrConstraint),
	); err != nil {
		return nil, mapEncErr(err)
	}

	return &Encoder{
		sampleRate: cfg.SampleRate,
		channels:   cfg.Channels,
		enc:        enc,
	}, nil
}

// Encode encodes exactly one frame of interleaved PCM into buf and returns the
// packet length in bytes; buf[:n] is the packet. It mirrors opus_encode.
//
// len(pcm) must be samplesPerChannel*Channels, and samplesPerChannel must be one
// of the four durations this release codes: 2.5, 5, 10 or 20 ms, i.e. SampleRate
// divided by 400, 200, 100 or 50. Any other length returns ErrBadArg, except the
// longer durations Opus itself defines (40 ms and up), which return ErrUnsupported
// because libopus codes them by splitting into 20 ms sub-frames and repacketizing,
// and that path is deferred. The frame duration may change from call to call.
//
// buf is caller-provided, so a steady-state encode allocates nothing. Unlike a
// decoder, the encoder ADAPTS to the buffer it is given: if buf cannot hold the
// packet the bitrate asks for, libopus lowers the rate to fit rather than failing
// (opus_encoder.c:745), so a short buffer costs quality and does not return an
// error. The one fatal case is a buffer with no room at all, which returns
// ErrBufferTooSmall. 1276 bytes holds any VBR packet this encoder produces; hard
// CBR at a very high bitrate can ask for more, and then it pads up to whatever buf
// allows.
func (e *Encoder) Encode(pcm []int16, buf []byte) (int, error) {
	// opus_encode_native zeroes st->rangeFinal BEFORE its rejection rules
	// (opus_encoder.c:1223), so in libopus a rejected call leaves OPUS_GET_FINAL_RANGE
	// reading 0 rather than the previous packet's range. The internal layer mirrors
	// that (internal/opusenc/encode.go:272), but these checks reject before reaching
	// it, so clear it here too. Without this, FinalRange after a failed Encode still
	// reports the last successful packet, which no packet-level test can see.
	if len(pcm) == 0 || len(pcm)%e.channels != 0 {
		e.enc.ClearFinalRange()
		return 0, fmt.Errorf("%w: len(pcm) is %d, want a positive multiple of %d (the channel count)",
			ErrBadArg, len(pcm), e.channels)
	}
	frameSize := len(pcm) / e.channels
	if err := e.checkFrameSize(frameSize); err != nil {
		e.enc.ClearFinalRange()
		return 0, err
	}
	if len(buf) == 0 {
		e.enc.ClearFinalRange()
		return 0, fmt.Errorf("%w: the output buffer is empty", ErrBufferTooSmall)
	}

	n, err := e.enc.Encode(pcm, frameSize, buf, len(buf))
	if err != nil {
		return 0, mapEncErr(err)
	}
	return n, nil
}

// checkFrameSize applies the v1 frame-duration domain to a per-channel sample
// count, splitting the two rejections libopus itself distinguishes: a length that
// is not an Opus frame duration at all (frame_size_select returns -1 at
// opus_encoder.c:827, which opus_encode_native turns into OPUS_BAD_ARG) versus a
// legal duration this release does not code (OPUS_UNIMPLEMENTED).
func (e *Encoder) checkFrameSize(frameSize int) error {
	fs := e.sampleRate
	switch frameSize {
	case fs / 400, fs / 200, fs / 100, fs / 50: // 2.5, 5, 10, 20 ms
		return nil
	case fs / 25, 3 * fs / 50, 4 * fs / 50, 5 * fs / 50, 6 * fs / 50: // 40, 60, 80, 100, 120 ms
		return fmt.Errorf("%w: a %d-sample frame is longer than 20 ms; this release codes only "+
			"2.5, 5, 10 and 20 ms frames (%d, %d, %d or %d samples per channel at %d Hz)",
			ErrUnsupported, frameSize, fs/400, fs/200, fs/100, fs/50, fs)
	default:
		return fmt.Errorf("%w: a %d-sample frame is not an Opus frame duration; want %d, %d, %d "+
			"or %d samples per channel (2.5, 5, 10 or 20 ms at %d Hz)",
			ErrBadArg, frameSize, fs/400, fs/200, fs/100, fs/50, fs)
	}
}

// Lookahead returns the encoder's algorithmic delay in samples AT THE CONFIGURED
// SAMPLE RATE, mirroring OPUS_GET_LOOKAHEAD (opus_encoder.c:3082). It is
// SampleRate/400 (the CELT MDCT overlap) plus SampleRate/250 (the
// delay-compensation ring), so 312 at 48 kHz.
//
// This is the source of the Ogg Opus pre-skip, but it is NOT the pre-skip: RFC
// 7845 defines that field at 48 kHz regardless of the coding rate, so a container
// must scale it, preSkip48k = Lookahead() * 48000 / SampleRate. Both terms are
// proportional to the sample rate and divide exactly, so the scaled value is 312
// at every rate this encoder accepts. Returning the unscaled figure is what
// OPUS_GET_LOOKAHEAD does, and conflating the two misaligns a decoded stream by
// the difference.
func (e *Encoder) Lookahead() int { return e.enc.Lookahead() }

// PreSkip returns the encoder pre-skip in samples at 48 kHz: the value a
// container writes into the Ogg OpusHead or the MP4 dOps box. RFC 7845 defines
// the pre-skip at 48 kHz regardless of the coding rate, so this scales Lookahead
// accordingly, PreSkip = Lookahead() * 48000 / SampleRate. Both terms are
// proportional to the sample rate and divide exactly, so the result is 312 at
// every rate this encoder accepts. Use this, not Lookahead, to fill a container
// header; Lookahead returns the unscaled coding-rate delay (OPUS_GET_LOOKAHEAD),
// and conflating the two misaligns a decoded stream by the difference. This is
// the encode-side counterpart of oggopus.Info.PreSkip on the decode side.
func (e *Encoder) PreSkip() int { return e.Lookahead() * 48000 / e.sampleRate }

// FinalRange returns the range-coder state after the last encoded packet
// (OPUS_GET_FINAL_RANGE). It is the strong per-packet bit-exactness check: a
// conformant decoder that consumes the packet ends on the same value, and the
// differential gate compares it against libopus for every frame it encodes.
func (e *Encoder) FinalRange() uint32 { return e.enc.FinalRange() }

// Reset clears cross-frame encoder state (the delay-compensation ring, the
// DC-blocking filter memory, the CELT overlap-add, energy prediction, prefilter
// and VBR reservoir) while keeping the configured sample rate, channel count and
// tuning, mirroring opus_encoder_ctl(OPUS_RESET_STATE). Feeding the same PCM to a
// freshly reset Encoder reproduces the same packets byte for byte.
func (e *Encoder) Reset() {
	e.enc.Reset()
}

// mapEncErr translates an internal opusenc error into the public sentinel set. It
// double-wraps, exactly as mapDecErr does, so errors.Is finds both the public
// sentinel and the internal one.
func mapEncErr(err error) error {
	switch {
	case errors.Is(err, opusenc.ErrBadArg):
		return fmt.Errorf("%w: %w", ErrBadArg, err)
	case errors.Is(err, opusenc.ErrBufferTooSmall):
		return fmt.Errorf("%w: %w", ErrBufferTooSmall, err)
	case errors.Is(err, opusenc.ErrUnimplemented):
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	default:
		return fmt.Errorf("%w: %w", ErrInternal, err)
	}
}
