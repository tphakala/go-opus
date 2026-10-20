package oggopus

import "errors"

// Sentinel errors returned by the oggopus package, testable with errors.Is.
var (
	// ErrInvalidConfig reports that a Config is not usable: an unsupported sample
	// rate or channel count, or an out-of-range bitrate or complexity.
	ErrInvalidConfig = errors.New("oggopus: invalid config")
	// ErrClosed reports an operation on an Encoder or Decoder after Close.
	ErrClosed = errors.New("oggopus: use of closed stream")
	// ErrNotOpus reports that the first packet of the first logical stream is not
	// an RFC 7845 OpusHead identification header, for example because the file is
	// an Ogg Vorbis, Ogg FLAC, Speex or Theora stream. NewDecoder, Decoder.Reset,
	// DecodeInterleaved and DecodeInterleavedLimit return it wrapped. It is not
	// returned for input that is not Ogg at all, for an OpusHead that is present
	// but corrupt, or for OpusTags errors.
	ErrNotOpus = errors.New("oggopus: not an Ogg Opus stream")
	// ErrSeekUnsupported reports that Decoder.Length or Decoder.SeekToSample cannot
	// work on this stream: the source given to NewDecoder or Reset is not a working
	// io.Seeker, it cannot seek relative to its end, or the stream has a layout the
	// granule arithmetic cannot describe (a nonzero initial granule position, audio
	// data on the OpusTags page, or a final granule position below the pre-skip).
	// The Decoder stays usable for sequential decoding.
	ErrSeekUnsupported = errors.New("oggopus: seek unsupported")
	// ErrInvalidSeek reports a SeekToSample call with a negative sample index. The
	// Decoder position is unchanged.
	ErrInvalidSeek = errors.New("oggopus: invalid seek (negative sample index)")
	// ErrPacketTooLarge reports that an audio packet reassembled from Ogg pages is
	// larger than the decoder accepts. The limit is 122,880 bytes per Opus stream,
	// times the stream count in OpusHead (one for mapping family 0). A packet
	// without padding is at most 61,298*N - 2 bytes for N streams (RFC 7845
	// section 6), so only code-3 padding of roughly 61 KB or more per stream goes
	// past the limit. That is legal Opus, but section 6 says a demuxer should treat
	// such a packet as invalid, so the stream is not decoded.
	//
	// It is a resource limit that keeps a stream whose pages never end a packet from
	// growing the decoder's memory without bound. Like ErrDecodeLimit it is not a
	// verdict that the stream is corrupt, and it is distinct from both
	// ErrDecodeLimit and the corruption errors. Read, WriteTo, DecodeInterleaved,
	// DecodeInterleavedLimit, Length and SeekToSample return it wrapped. NewDecoder
	// and Reset do not: OpusHead and OpusTags keep at most 61,440 bytes each and the
	// rest of a larger one is skipped. After Read or WriteTo return it, they keep
	// returning it until a successful SeekToSample or Reset.
	ErrPacketTooLarge = errors.New("oggopus: packet too large")
)

// errUninitialized guards the zero value. NewEncoder, Reset and NewDecoder each
// either return a fully constructed stream or an error, so an Encoder or Decoder
// reached through the documented API always has its codec; this is what a
// zero-value oggopus.Encoder{} or a pooled Encoder whose Reset failed reports
// instead of panicking or, worse, appending to the previous stream's container.
//
// It replaces the construction-phase errCodecNotWired sentinel, which the PCM
// entry points returned until the codec was wired to the seam in codec.go. It is
// unexported because it reports a programming error rather than a condition a
// caller can act on. In-package tests assert against it directly.
var errUninitialized = errors.New("oggopus: encoder or decoder is not initialized")

// errNilWriter and errNilReader report a nil sink or source passed to the public
// constructors. They are plain errors, not exported sentinels: a nil io.Writer or
// io.Reader is a programming error, not a condition a caller branches on (matching
// go-flac's and go-aac's pcm packages). They replace the panic that the first
// header-page write (nil io.Writer) or the first container read (nil io.Reader)
// would otherwise raise, so a caller that passes nil gets a clean error instead.
var (
	errNilWriter = errors.New("oggopus: nil writer")
	errNilReader = errors.New("oggopus: nil reader")
)
