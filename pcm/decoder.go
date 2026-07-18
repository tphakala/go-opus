package pcm

import (
	"io"

	"github.com/tphakala/go-opus/oggopus"
)

// Decoder reads an Ogg Opus stream and yields interleaved little-endian int16 PCM
// at 48 kHz, implementing io.Reader and io.WriterTo. It is an alias of
// oggopus.Decoder; its methods (Read, WriteTo, Info) are documented there.
type Decoder = oggopus.Decoder

// Info reports the stream parameters parsed from OpusHead. It is an alias of
// oggopus.Info; see that type for the field documentation. OutputSampleRate is
// always 48000 (the decode output rate); InputSampleRate is the informational
// original source rate and may be 0.
type Info = oggopus.Info

// NewDecoder reads and validates the OpusHead and OpusTags headers from r and
// returns a Decoder positioned at the first audio packet. It forwards to
// oggopus.NewDecoder; a nil r returns an error.
func NewDecoder(r io.Reader) (*Decoder, error) {
	return oggopus.NewDecoder(r)
}
