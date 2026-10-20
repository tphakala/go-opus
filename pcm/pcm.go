package pcm

import "github.com/tphakala/go-opus/oggopus"

// OutputSampleRate is the fixed 48 kHz rate of all decoded PCM, in Hz. It
// re-exports oggopus.OutputSampleRate; see that constant and Info.OutputSampleRate.
const OutputSampleRate = oggopus.OutputSampleRate

// Config configures an Encoder. It is an alias of oggopus.Config: the same flat,
// go-flac-shaped struct with the same documented zero-value semantics, so a value
// built for either package is accepted by the other with no conversion. The field
// documentation lives on oggopus.Config.
type Config = oggopus.Config

// Re-exported error sentinels. They are the SAME values as oggopus's, so
// errors.Is matches an error from either package.
var (
	// ErrInvalidConfig reports a Config that is not usable (an unsupported sample
	// rate or channel count, or an out-of-range bitrate or complexity). Same value
	// as oggopus.ErrInvalidConfig.
	ErrInvalidConfig = oggopus.ErrInvalidConfig
	// ErrClosed reports an operation on an Encoder or Decoder after Close. Same
	// value as oggopus.ErrClosed.
	ErrClosed = oggopus.ErrClosed
	// ErrNotOpus reports that the first packet of the first logical Ogg stream is
	// not an OpusHead identification header. Same value as oggopus.ErrNotOpus.
	ErrNotOpus = oggopus.ErrNotOpus
	// ErrSeekUnsupported reports that Decoder.Length, Decoder.SeekToSample or
	// Decoder.SeekToSampleExact cannot work on this source or stream. Same value as oggopus.ErrSeekUnsupported.
	ErrSeekUnsupported = oggopus.ErrSeekUnsupported
	// ErrInvalidSeek reports a SeekToSample or SeekToSampleExact call with a negative
	// sample index. Same value as oggopus.ErrInvalidSeek.
	ErrInvalidSeek = oggopus.ErrInvalidSeek
	// ErrPacketTooLarge reports an audio packet larger than the decoder accepts
	// (a resource limit, not a corruption verdict). Same value as
	// oggopus.ErrPacketTooLarge.
	ErrPacketTooLarge = oggopus.ErrPacketTooLarge
)
