package pcm

import (
	"io"

	"github.com/tphakala/go-opus/oggopus"
)

// EncodeInterleaved encodes a complete interleaved little-endian int16 PCM buffer
// to an Ogg Opus stream on w in a single call, the one-shot helper for callers
// that already hold the whole buffer. It forwards to oggopus.EncodeInterleaved,
// which draws an Encoder from an internal sync.Pool, so repeated same-shape calls
// are allocation-light and it is safe for concurrent use. pcm must hold a whole
// number of samples for cfg, or ErrInvalidConfig is returned before any sink write.
func EncodeInterleaved(w io.Writer, cfg Config, pcm []byte) error { //nolint:gocritic // Config by value to match the go-flac-aligned public API (as oggopus.EncodeInterleaved)
	return oggopus.EncodeInterleaved(w, cfg, pcm)
}
