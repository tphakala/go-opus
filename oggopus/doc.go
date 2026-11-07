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
// comments of a cut OpusTags are not kept). A header packet that spans more than
// 32 MiB fails NewDecoder and Reset with ErrPacketTooLarge, so a header that
// never ends is not read forever. The ceiling on one-shot decoded output,
// ErrDecodeLimit, is separate.
//
// # Channel mapping families
//
// The decoder reads family 0 (mono and stereo), family 1 (surround, up to 8
// channels), family 2 (ambisonics, whose channels are returned as stored, not
// rendered to speakers; the channel count is not validated against the family)
// and family 255 (unidentified channels, returned as stored). Family 3
// (ambisonics with a demixing matrix, which needs a projection decoder) and the
// reserved families 4 to 254 fail NewDecoder and Reset with an error wrapping
// opus.ErrUnsupported. The encoder writes family 0 only.
package oggopus
