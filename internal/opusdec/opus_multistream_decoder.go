/* Copyright (c) 2011 Xiph.Org Foundation
   Written by Jean-Marc Valin */
/*
   Redistribution and use in source and binary forms, with or without
   modification, are permitted provided that the following conditions
   are met:

   - Redistributions of source code must retain the above copyright
   notice, this list of conditions and the following disclaimer.

   - Redistributions in binary form must reproduce the above copyright
   notice, this list of conditions and the following disclaimer in the
   documentation and/or other materials provided with the distribution.

   THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
   ``AS IS'' AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
   LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
   A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER
   OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
   EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
   PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
   PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
   LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
   NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
   SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package opusdec

// This file transliterates the libopus multistream decoder for the frozen
// FIXED_POINT + DISABLE_FLOAT_API build: opus_multistream_decoder.c (the decode
// coordinator) and the ChannelLayout helpers from opus_multistream.c. The
// multistream decoder is a thin scheduler around N single-stream OpusDecoders:
// the first nb_coupled_streams decode as stereo, the rest as mono, and each
// decoded stream is scattered to the API channels its mapping table names. The
// per-stream range coding is done entirely by OpusDecoder, so the only new
// bit-exactness surface here is the packet framing (self-delimited for every
// stream but the last) and the channel scatter; both mirror the C byte for byte.

import "github.com/tphakala/go-opus/internal/packet"

// maxChannels (255) is the ceiling opus_multistream_decoder_init enforces on
// nb_channels; the mapping array below is maxChannels+1 = 256 entries to match
// libopus's ChannelLayout.mapping[256] (src/opus_private.h).
const maxChannels = 255

// channelLayout mirrors ChannelLayout (src/opus_private.h): the API channel
// count, the stream and coupled-stream counts, and the per-API-channel mapping
// that names which decoded stream channel feeds it (255 means "muted").
type channelLayout struct {
	nbChannels       int
	nbStreams        int
	nbCoupledStreams int
	mapping          [maxChannels + 1]byte
}

// validateLayout is validate_layout (opus_multistream.c:41): every non-muted
// mapping entry must name a decoded stream channel that exists. The largest
// valid index is nb_streams+nb_coupled_streams-1 (get_mono_channel maps mono
// stream s to index s+nb_coupled_streams, so the mono streams occupy the range
// above the 2*nb_coupled_streams coupled channels), so max_channel is the
// exclusive bound nb_streams+nb_coupled_streams.
func validateLayout(layout *channelLayout) bool {
	maxChannel := layout.nbStreams + layout.nbCoupledStreams
	if maxChannel > 255 {
		return false
	}
	for i := 0; i < layout.nbChannels; i++ {
		if int(layout.mapping[i]) >= maxChannel && layout.mapping[i] != 255 {
			return false
		}
	}
	return true
}

// getLeftChannel is get_left_channel (opus_multistream.c:57): the next API
// channel at or after prev that draws the left channel of coupled stream
// streamID (mapping value streamID*2), or -1 when there is none. prev is -1 on
// the first call so a single decoded channel can feed several API channels.
func getLeftChannel(layout *channelLayout, streamID, prev int) int {
	i := 0
	if prev >= 0 {
		i = prev + 1
	}
	for ; i < layout.nbChannels; i++ {
		if int(layout.mapping[i]) == streamID*2 {
			return i
		}
	}
	return -1
}

// getRightChannel is get_right_channel (opus_multistream.c:69): as getLeftChannel
// for the right channel of coupled stream streamID (mapping value streamID*2+1).
func getRightChannel(layout *channelLayout, streamID, prev int) int {
	i := 0
	if prev >= 0 {
		i = prev + 1
	}
	for ; i < layout.nbChannels; i++ {
		if int(layout.mapping[i]) == streamID*2+1 {
			return i
		}
	}
	return -1
}

// getMonoChannel is get_mono_channel (opus_multistream.c:81): the next API
// channel fed by mono stream streamID (mapping value streamID+nb_coupled_streams,
// the index space above the coupled channels), or -1.
func getMonoChannel(layout *channelLayout, streamID, prev int) int {
	i := 0
	if prev >= 0 {
		i = prev + 1
	}
	for ; i < layout.nbChannels; i++ {
		if int(layout.mapping[i]) == streamID+layout.nbCoupledStreams {
			return i
		}
	}
	return -1
}

// OpusMSDecoder mirrors OpusMSDecoder (src/opus_multistream_decoder.c): a channel
// layout plus one OpusDecoder per stream (the first nb_coupled_streams are stereo,
// the rest mono). It is stateful, since every sub-decoder carries cross-packet
// memory, and is not safe for concurrent use; use one per goroutine.
type OpusMSDecoder struct {
	layout   channelLayout
	Fs       int32
	decoders []*OpusDecoder

	// buf is the reusable per-stream decode scratch (2*max frame_size int16 for a
	// coupled stream's interleaved output), so a steady-state Decode allocates
	// nothing beyond what the sub-decoders themselves do. C uses a stack ALLOC of
	// 2*frame_size (opus_multistream_decoder.c:208).
	buf []int16

	// valPkt / valFrames back the pre-decode packet validation parse
	// (opus_multistream_packet_validate), which walks the streams once before any
	// decode; they carry nothing across calls.
	valPkt    packet.Packet
	valFrames [packet.MaxFrames][]byte
}

// NewMSDecoder is opus_multistream_decoder_init (opus_multistream_decoder.c:66)
// plus opus_multistream_decoder_create (:113): validate the layout, then create
// the per-stream decoders. Fs is one of 8000/12000/16000/24000/48000; channels is
// 1..255; streams 1..255; coupledStreams 0..streams with streams+coupledStreams
// <= 255; and mapping has one entry per channel, each naming a decoded stream
// channel (< streams+coupledStreams) or 255 for a muted channel. Returns a nil
// decoder and an error (ErrBadArg / ErrInternal) on any invalid argument.
func NewMSDecoder(Fs int32, channels, streams, coupledStreams int, mapping []byte) (*OpusMSDecoder, error) {
	st, code := newMSDecoder(Fs, channels, streams, coupledStreams, mapping)
	if code != opusOK {
		return nil, codeErr(code)
	}
	return st, nil
}

// newMSDecoder is the int-code opus_multistream_decoder_init body; NewMSDecoder
// wraps it.
func newMSDecoder(Fs int32, channels, streams, coupledStreams int, mapping []byte) (*OpusMSDecoder, int) {
	if channels > 255 || channels < 1 || coupledStreams > streams ||
		streams < 1 || coupledStreams < 0 || streams > 255-coupledStreams {
		return nil, opusBadArg
	}
	if len(mapping) < channels {
		return nil, opusBadArg
	}

	st := &OpusMSDecoder{Fs: Fs}
	st.layout.nbChannels = channels
	st.layout.nbStreams = streams
	st.layout.nbCoupledStreams = coupledStreams
	for i := 0; i < channels; i++ {
		st.layout.mapping[i] = mapping[i]
	}
	if !validateLayout(&st.layout) {
		return nil, opusBadArg
	}

	st.decoders = make([]*OpusDecoder, streams)
	for i := 0; i < streams; i++ {
		ch := 1
		if i < coupledStreams {
			ch = 2
		}
		dec, code := newDecoder(Fs, ch)
		if code != opusOK {
			return nil, code
		}
		st.decoders[i] = dec
	}

	// Fs/25*3 is the 120 ms clamp opus_multistream_decode_native applies to
	// frame_size (line 207); a coupled stream writes 2 samples per frame.
	maxFrame := int(Fs) / 25 * 3
	st.buf = make([]int16, 2*maxFrame)
	return st, opusOK
}

// msPacketValidate is opus_multistream_packet_validate (opus_multistream_decoder.c:149):
// walk every stream's sub-packet (self-delimited for all but the last) to confirm
// the framing is well formed and that all streams carry the same per-channel
// sample count. Returns that sample count, or a negative Opus error code.
func (st *OpusMSDecoder) msPacketValidate(data []byte, Fs int) int {
	samples := 0
	// The parses below slice each stream's frames out of the caller's data buffer
	// into st.valFrames; drop those references on return so this long-lived decoder
	// does not pin the last validated packet's backing buffer until the next Decode,
	// the same guard opusDecodeNativeImpl applies to its own parse scratch.
	defer func() {
		clear(st.valFrames[:])
		st.valPkt.Frames = nil
		st.valPkt.Padding = nil
	}()
	for s := 0; s < st.layout.nbStreams; s++ {
		if len(data) <= 0 {
			return opusInvalidPacket
		}
		var perr error
		if s != st.layout.nbStreams-1 {
			perr = packet.ParseSelfDelimitedInto(data, &st.valPkt, &st.valFrames)
		} else {
			perr = packet.ParseInto(data, &st.valPkt, &st.valFrames)
		}
		if perr != nil {
			return opusInvalidPacket
		}
		packetOffset := st.valPkt.Consumed
		tmpSamples, serr := packet.Samples(data, Fs)
		if serr != nil {
			return opusInvalidPacket
		}
		if s != 0 && samples != tmpSamples {
			return opusInvalidPacket
		}
		samples = tmpSamples
		data = data[packetOffset:]
	}
	return samples
}

// decodeNative is opus_multistream_decode_native (opus_multistream_decoder.c:178)
// for the FIXED_POINT int16 output path (copy_channel_out_short, OPTIONAL_CLIP=0).
// data is the whole multistream packet (nil/empty for PLC), pcm the interleaved
// output for all API channels, frameSize the caller's per-channel capacity,
// decodeFec the FEC request. Returns the per-channel sample count or a negative
// Opus error code.
func (st *OpusMSDecoder) decodeNative(data []byte, pcm []int16, frameSize, decodeFec int) int {
	Fs := int(st.Fs)
	if frameSize <= 0 {
		return opusBadArg
	}
	/* Limit frame_size to avoid excessive stack allocations. */
	frameSize = imin(frameSize, Fs/25*3)

	doPlc := len(data) == 0
	if !doPlc && len(data) < 2*st.layout.nbStreams-1 {
		return opusInvalidPacket
	}
	if !doPlc {
		ret := st.msPacketValidate(data, Fs)
		if ret < 0 {
			return ret
		} else if ret > frameSize {
			return opusBufferTooSmall
		}
	}

	nbChannels := st.layout.nbChannels
	for s := 0; s < st.layout.nbStreams; s++ {
		dec := st.decoders[s]

		if !doPlc && len(data) <= 0 {
			return opusInternalError
		}
		selfDelimited := s != st.layout.nbStreams-1
		ret, packetOffset := dec.decodeStream(data, st.buf, frameSize, decodeFec, selfDelimited)
		if !doPlc {
			data = data[packetOffset:]
		}
		if ret <= 0 {
			return ret
		}
		frameSize = ret
		if s < st.layout.nbCoupledStreams {
			/* Copy "left" audio to the channel(s) where it belongs (src stride 2,
			   offset 0), then "right" (offset 1). */
			prev := -1
			for {
				chn := getLeftChannel(&st.layout, s, prev)
				if chn == -1 {
					break
				}
				copyChannelOutShort(pcm, nbChannels, chn, st.buf, 2, 0, frameSize)
				prev = chn
			}
			prev = -1
			for {
				chn := getRightChannel(&st.layout, s, prev)
				if chn == -1 {
					break
				}
				copyChannelOutShort(pcm, nbChannels, chn, st.buf, 2, 1, frameSize)
				prev = chn
			}
		} else {
			/* Copy mono audio to the channel(s) where it belongs (src stride 1). */
			prev := -1
			for {
				chn := getMonoChannel(&st.layout, s, prev)
				if chn == -1 {
					break
				}
				copyChannelOutShort(pcm, nbChannels, chn, st.buf, 1, 0, frameSize)
				prev = chn
			}
		}
	}
	/* Handle muted channels */
	for c := 0; c < nbChannels; c++ {
		if st.layout.mapping[c] == 255 {
			muteChannelOutShort(pcm, nbChannels, c, frameSize)
		}
	}
	return frameSize
}

// copyChannelOutShort is opus_copy_channel_out_short (opus_multistream_decoder.c:337)
// for a present source: scatter frameSize samples from src (starting at srcOff,
// advancing srcStride per sample: stride 2 for a coupled stream's L/R interleave,
// 1 for mono) into the dstChannel lane of the interleaved dst with dstStride equal
// to the API channel count. RES2INT16 is the identity on the frozen fixed-point
// non-RES24 build, so the sub-decoder's int16 output is copied verbatim.
func copyChannelOutShort(dst []int16, dstStride, dstChannel int, src []int16, srcStride, srcOff, frameSize int) {
	for i := 0; i < frameSize; i++ {
		dst[i*dstStride+dstChannel] = src[i*srcStride+srcOff]
	}
}

// muteChannelOutShort is opus_copy_channel_out_short with src==NULL: zero the
// dstChannel lane (a mapping value of 255 marks an unused/muted API channel).
func muteChannelOutShort(dst []int16, dstStride, dstChannel, frameSize int) {
	for i := 0; i < frameSize; i++ {
		dst[i*dstStride+dstChannel] = 0
	}
}

// Decode decodes one multistream packet into interleaved int16 pcm and returns
// the per-channel sample count, mirroring opus_multistream_decode. A nil/empty
// packet requests PLC. frameSize is the caller's per-channel buffer capacity.
func (st *OpusMSDecoder) Decode(data []byte, pcm []int16, frameSize, decodeFec int) (int, error) {
	if frameSize <= 0 {
		return 0, codeErr(opusBadArg)
	}
	ret := st.decodeNative(data, pcm, frameSize, decodeFec)
	if ret < 0 {
		return 0, codeErr(ret)
	}
	return ret, nil
}

// Channels returns the API channel count the decoder produces.
func (st *OpusMSDecoder) Channels() int { return st.layout.nbChannels }

// Streams returns the number of Opus streams the packet carries.
func (st *OpusMSDecoder) Streams() int { return st.layout.nbStreams }

// FinalRange is opus_multistream_decoder_ctl(OPUS_GET_FINAL_RANGE): the XOR of
// every stream's final range-coder state (opus_multistream_decoder.c:456), the
// per-packet bit-exactness check against the encoder.
func (st *OpusMSDecoder) FinalRange() uint32 {
	var v uint32
	for _, dec := range st.decoders {
		v ^= dec.FinalRange()
	}
	return v
}

// LastPacketDuration returns the per-channel sample count of the most recent
// successful Decode (the first stream's, which packet validation forced all
// streams to share).
func (st *OpusMSDecoder) LastPacketDuration() int {
	return st.decoders[0].LastPacketDuration()
}

// ResetState is opus_multistream_decoder_ctl(OPUS_RESET_STATE): reset every
// stream's cross-packet decoder state (opus_multistream_decoder.c:480).
func (st *OpusMSDecoder) ResetState() {
	for _, dec := range st.decoders {
		dec.ResetState()
	}
}
