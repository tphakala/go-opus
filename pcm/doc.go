// Package pcm is the uniform <module>/pcm PCM codec facade for go-opus, matching
// github.com/tphakala/go-flac/pcm and github.com/tphakala/go-aac/pcm so the three
// codec libraries present the same import path and the same API shape. A consumer
// that wraps all three behind one interface swaps only the import path.
//
// It is a thin facade over the oggopus package: interleaved little-endian int16
// PCM goes in, an RFC 7845 Ogg Opus stream comes out, and back. Every type here
// (Config, Encoder, Decoder, Info) is a type ALIAS of its oggopus counterpart, so
// the two packages interoperate with zero conversion: a *pcm.Encoder is an
// *oggopus.Encoder and a pcm.Config is an oggopus.Config. The container-level
// documentation (the Ogg pages and the OpusHead details) lives in oggopus; this
// package only re-presents the codec under the shared name.
//
// The package name deliberately collides with go-flac/pcm and go-aac/pcm. Import
// it under an alias when using more than one:
//
//	import opuspcm "github.com/tphakala/go-opus/pcm"
//
// # Encoding
//
// NewEncoder wraps any io.Writer; no seeking is ever required, because per-page
// granule positions carry the duration inline. Reset rebinds an encoder to a new
// sink for pooling, and EncodeInterleaved is the one-shot helper for a complete
// in-memory buffer.
//
// # Decoding
//
// NewDecoder reads an Ogg Opus stream and yields interleaved little-endian int16
// PCM through io.Reader and io.WriterTo. Decode output is always 48 kHz
// (OutputSampleRate); label the decoded PCM with Info().OutputSampleRate, never
// with Info().InputSampleRate, which is the informational original source rate
// recorded in OpusHead.
package pcm
