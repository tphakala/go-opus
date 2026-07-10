package oggopus

// This file is the codec seam: the single, clearly marked boundary where the
// not-yet-built opus codec attaches to the finished container layer. Everything
// else in oggopus (the page layer, the RFC 7845 headers, the packet-level
// writer and reader) is complete and tested against real Opus packets without a
// codec. Only the two constructor variables below are stubbed; when the phase-4
// encoder and the phase-2/3 decoder exist, their bodies build an *opus.Encoder /
// *opus.Decoder, wrap it in an adapter satisfying the interfaces here, and
// return it. No other oggopus code changes.

// frameEncoder turns one 20 ms frame of interleaved int16 PCM into one opus
// packet. It is the minimal surface the container writer needs from the codec.
// The interface is unexported, so the adapter that satisfies it lives inside
// oggopus and wraps the public opus.Encoder; the container never depends on the
// opus package's concrete types.
type frameEncoder interface {
	// encodeFrame encodes one frame of interleaved int16 PCM (channels
	// interleaved; len(pcm) == samplesPerChannel*channels) and returns the opus
	// packet bytes and the frame's duration in samples per channel at 48 kHz.
	encodeFrame(pcm []int16) (packet []byte, samples48k int, err error)
	// lookahead reports the encoder pre-skip in 48 kHz samples, written into
	// OpusHead.
	lookahead() int
	// close releases codec resources.
	close()
}

// frameDecoder turns one opus packet into interleaved int16 PCM at 48 kHz. It is
// the minimal surface the container reader needs from the codec.
type frameDecoder interface {
	// decodeFrame decodes one opus packet into interleaved int16 PCM at 48 kHz
	// (channels interleaved).
	decodeFrame(packet []byte) (pcm []int16, err error)
	// close releases codec resources.
	close()
}

// newFrameEncoder builds the per-stream encoder. It is a package variable (not a
// plain function) so the phase-4 codec can be wired in at one site, and so the
// container's codec-present paths stay live code rather than being folded away
// by the compiler while the stub is the only implementation.
//
// SEAM: replace the body with a real *opus.Encoder construction when the codec
// lands. Until then it reports errCodecNotWired.
var newFrameEncoder = func(cfg Config) (frameEncoder, error) {
	return nil, errCodecNotWired
}

// newFrameDecoder builds the per-stream decoder from the parsed OpusHead.
//
// SEAM: replace the body with a real *opus.Decoder construction when the codec
// lands. Until then it reports errCodecNotWired.
var newFrameDecoder = func(head opusHead) (frameDecoder, error) {
	return nil, errCodecNotWired
}
