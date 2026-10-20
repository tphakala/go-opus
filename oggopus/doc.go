// Package oggopus implements the RFC 7845 Ogg Opus container: an io.Writer-based
// encoder and an io.Reader-based decoder wrapping the raw opus codec. This is
// the BirdNET-Go deliverable. See docs/api-design.md.
//
// Decode output is always 48 kHz interleaved little-endian int16 PCM
// (OutputSampleRate), independent of the encoded stream's original input rate;
// Info.InputSampleRate is informational only.
//
// # Limits
//
// The decoder bounds the memory a packet that spans Ogg pages can take. An audio
// packet over the limit described for ErrPacketTooLarge fails with that error.
// OpusHead and OpusTags are cut instead: each keeps its first 61,440 bytes and
// the rest is read and dropped, so a stream with large tags still decodes (the
// comments of a cut OpusTags are not kept). The ceiling on one-shot decoded
// output, ErrDecodeLimit, is separate.
package oggopus
