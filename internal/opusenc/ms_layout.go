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

package opusenc

// This file holds the ChannelLayout helpers the multistream encoder needs:
// validate_layout and the get_{left,right,mono}_channel selectors from
// opus_multistream.c, plus validate_encoder_layout from
// opus_multistream_encoder.c (the encoder-only check that every stream is
// referenced by at least one API channel). All are a byte-for-byte transliteration
// of the C. validate_layout and the three get_*_channel selectors are a deliberate
// private copy of the identical helpers in internal/opusdec (the two internal
// packages keep their own copies rather than sharing, matching the decoder
// precedent and keeping package boundaries clean); validate_encoder_layout is
// encoder-only and has no opusdec counterpart.

// maxChannels (255) is the ceiling opus_multistream_encoder_init enforces on
// nb_channels; the mapping array below is maxChannels+1 = 256 entries to match
// libopus's ChannelLayout.mapping[256] (src/opus_private.h).
const maxChannels = 255

// channelLayout mirrors ChannelLayout (src/opus_private.h): the API channel
// count, the stream and coupled-stream counts, and the per-API-channel mapping
// that names which stream channel it draws from (255 means "muted", never
// produced on the encode side but permitted by the mapping validation).
type channelLayout struct {
	nbChannels       int
	nbStreams        int
	nbCoupledStreams int
	mapping          [maxChannels + 1]byte
}

// validateLayout is validate_layout (opus_multistream.c:41): every non-muted
// mapping entry must name a stream channel that exists. The largest valid index
// is nb_streams+nb_coupled_streams-1 (get_mono_channel maps mono stream s to
// index s+nb_coupled_streams, so the mono streams occupy the range above the
// 2*nb_coupled_streams coupled channels), so max_channel is the exclusive bound
// nb_streams+nb_coupled_streams.
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

// validateEncoderLayout is validate_encoder_layout
// (opus_multistream_encoder.c:133): every stream must be consumed by at least
// one API channel, so no sub-encoder produces audio that the mapping drops. A
// coupled stream needs both a left and a right API channel; a mono stream needs
// one. The decoder has no equivalent check (a decoder may legitimately mute a
// stream's output), so this lives only on the encode side.
func validateEncoderLayout(layout *channelLayout) bool {
	for s := 0; s < layout.nbStreams; s++ {
		if s < layout.nbCoupledStreams {
			if getLeftChannel(layout, s, -1) == -1 {
				return false
			}
			if getRightChannel(layout, s, -1) == -1 {
				return false
			}
		} else {
			if getMonoChannel(layout, s, -1) == -1 {
				return false
			}
		}
	}
	return true
}

// getLeftChannel is get_left_channel (opus_multistream.c:57): the next API
// channel at or after prev that draws the left channel of coupled stream
// streamID (mapping value streamID*2), or -1 when there is none. prev is -1 on
// the first call so a single stream channel can feed several API channels.
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
