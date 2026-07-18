package pcm

import (
	"io"

	"github.com/tphakala/go-opus/oggopus"
)

// Encoder streams interleaved little-endian int16 PCM (as []byte) to an Ogg Opus
// stream on an io.Writer, implementing io.WriteCloser. It is an alias of
// oggopus.Encoder, so its full method set (Write, Close, Reset) and behavior are
// documented there; see oggopus.Encoder.
type Encoder = oggopus.Encoder

// NewEncoder validates cfg, builds the codec, and returns an Encoder writing to w,
// having already emitted the OpusHead and OpusTags header pages. It forwards to
// oggopus.NewEncoder: an invalid cfg returns ErrInvalidConfig and a nil w returns
// an error, both before any byte is written. It does not require an io.WriteSeeker.
func NewEncoder(w io.Writer, cfg Config) (*Encoder, error) { //nolint:gocritic // Config by value to match the go-flac-aligned public API (as oggopus.NewEncoder)
	return oggopus.NewEncoder(w, cfg)
}
