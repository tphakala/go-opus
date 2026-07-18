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
)
