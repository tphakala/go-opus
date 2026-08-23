package pcm

import (
	"io"

	"github.com/tphakala/go-opus/oggopus"
)

// DefaultMaxDecodedBytes is the default ceiling DecodeInterleaved enforces on its
// output. It is an alias of oggopus.DefaultMaxDecodedBytes; see that constant for
// why the limit exists.
const DefaultMaxDecodedBytes = oggopus.DefaultMaxDecodedBytes

// ErrDecodeLimit reports that a one-shot decode was stopped because its output
// would exceed the byte ceiling in effect. It is the oggopus.ErrDecodeLimit
// sentinel; test for it with errors.Is.
var ErrDecodeLimit = oggopus.ErrDecodeLimit

// DecodeInterleaved reads a complete Ogg Opus stream from r and returns the
// decoded interleaved little-endian int16 PCM at 48 kHz together with the stream
// info, the one-shot mirror of EncodeInterleaved for callers that want the whole
// decode in a buffer. It forwards to oggopus.DecodeInterleaved and stops at
// DefaultMaxDecodedBytes with a wrapped ErrDecodeLimit; use DecodeInterleavedLimit
// for a custom ceiling, or NewDecoder to stream a stream of unbounded length.
func DecodeInterleaved(r io.Reader) (samples []byte, info Info, err error) {
	return oggopus.DecodeInterleaved(r)
}

// DecodeInterleavedLimit is DecodeInterleaved with a caller-chosen ceiling; a
// maxBytes of zero or less removes the limit. It forwards to
// oggopus.DecodeInterleavedLimit.
func DecodeInterleavedLimit(r io.Reader, maxBytes int) (samples []byte, info Info, err error) {
	return oggopus.DecodeInterleavedLimit(r, maxBytes)
}
