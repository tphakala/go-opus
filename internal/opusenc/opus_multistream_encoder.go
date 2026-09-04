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

// This file transliterates the libopus multistream ENCODER for the frozen
// FIXED_POINT + DISABLE_FLOAT_API build, restricted to channel mapping
// MAPPING_TYPE_NONE (RFC 7845 families 0 and 255): opus_multistream_encoder.c's
// plain path. It is the symmetric counterpart of internal/opusdec's multistream
// decoder: a channel layout plus one single-stream Encoder per stream (the first
// nb_coupled_streams are stereo, the rest mono). Each frame is demuxed from the
// interleaved input, encoded per stream, and the per-stream packets are
// concatenated with self-delimited framing (every stream but the last).
//
// The per-stream range coding is done entirely by Encoder, so the only new
// bit-exactness surface here is the cross-stream rate allocation, the channel
// demux, and the self-delimited packet framing plus its byte budget; all three
// mirror the C byte for byte.
//
// DELIBERATELY OUT OF SCOPE (MAPPING_TYPE_SURROUND / family 1, and
// MAPPING_TYPE_AMBISONICS / families 2 and 3): surround_analysis, the per-stream
// energy mask, the LFE stream, and the surround bandwidth/force-mode/force-channels
// forcing. The LFE terms are carried through surroundRateAllocation but multiply by
// nb_lfe == 0 here; a future family-1 follow-up re-enables them by setting lfeStream,
// on top of the separate surround_analysis and energy-mask work listed above.

import (
	"github.com/tphakala/go-opus/internal/celt"
	"github.com/tphakala/go-opus/internal/packet"
)

// msFrameTmp is MS_FRAME_TMP (opus_multistream_encoder.c:838, non-QEXT): the
// per-stream sub-packet scratch, sized for the encoder returning six 20 ms frames
// (6*1275 CELT bytes + 12 framing).
const msFrameTmp = 6*1275 + 12

// imin/imax are IMIN/IMAX on Go int (used for the byte-budget arithmetic, which C
// keeps in int and whose values never approach the int32 range).
func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func imax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// imin32/imax32 are IMIN/IMAX on int32. rate_allocation is done in int32 because C
// computes it in 32-bit int/opus_int32, which wraps where Go's 64-bit int would
// not; using int32 reproduces the C truncation and any wraparound bit-for-bit.
func imin32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func imax32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// OpusMSEncoder mirrors OpusMSEncoder (src/opus_multistream_encoder.c) for
// MAPPING_TYPE_NONE: a channel layout plus one Encoder per stream (the first
// nb_coupled_streams are stereo, the rest mono). It is stateful, since every
// sub-encoder carries cross-packet memory, and is not safe for concurrent use;
// use one per goroutine.
type OpusMSEncoder struct {
	layout      channelLayout
	Fs          int32
	application int

	// lfeStream is st.lfe_stream. For MAPPING_TYPE_NONE it is always -1, so
	// nb_lfe is 0 throughout rate allocation. It is a field, not an inlined
	// constant, so surroundRateAllocation reads as a literal transliteration of
	// the C; a future surround (family 1) follow-up sets it to re-enable the LFE
	// rate terms (which is one part of that larger follow-up, not the whole of it).
	lfeStream int

	// bitrateBps is st.bitrate_bps, the multistream-level target; OpusAuto by
	// default (opus_multistream_encoder_init_impl:480, ctl:1170).
	bitrateBps int32

	// variableDur is st.variable_duration; OPUS_FRAMESIZE_ARG by default
	// (init_impl:483). The MS encoder resolves frame_size once from this and
	// passes it straight to every sub-encoder.
	variableDur int

	encoders []*Encoder

	// buf is the interleaved per-stream demux scratch (2*frame_size int16 for a
	// coupled stream's stereo pair), grown on demand. C uses a stack
	// ALLOC(buf, 2*frame_size) (:905). NOT state: every Encode rewrites it before
	// reading. Not safe for concurrent use.
	buf []int16

	// tmpData is the per-stream sub-packet scratch the sub-encoder writes into
	// before the repacketizer reframes it (C: unsigned char tmp_data[MS_FRAME_TMP],
	// :863). NOT state.
	tmpData []byte

	// rp is the repacketizer reused to add self-delimiting lengths (:864/:976).
	rp packet.Repacketizer

	// bitrates receives rate_allocation's per-stream bitrate (C: opus_int32
	// bitrates[256], :867). A field rather than a per-call allocation so a
	// steady-state Encode allocates nothing beyond what the sub-encoders do.
	bitrates [256]int32
}

// NewMSEncoder is opus_multistream_encoder_init_impl (opus_multistream_encoder.c:436)
// plus opus_multistream_encoder_create (:611) for MAPPING_TYPE_NONE: validate the
// layout, then create the per-stream encoders. Fs is one of
// 8000/12000/16000/24000/48000; channels is 1..255; streams 1..255; coupledStreams
// 0..streams with streams+coupledStreams <= 255 AND streams+coupledStreams <=
// channels; mapping has one entry per channel, each naming a stream channel (<
// streams+coupledStreams) or 255 for a muted channel; and every stream must be
// referenced by at least one channel. application is VOIP / AUDIO /
// RESTRICTED_LOWDELAY. Returns a nil encoder and ErrBadArg on any invalid argument.
func NewMSEncoder(Fs int32, channels, streams, coupledStreams, application int, mapping []byte) (*OpusMSEncoder, error) {
	// init_impl:452-454. The encoder adds streams+coupled > channels, which the
	// decoder does not have (a decoder may reference fewer streams than it holds).
	if channels > 255 || channels < 1 || coupledStreams > streams ||
		streams < 1 || coupledStreams < 0 || streams > 255-coupledStreams ||
		streams+coupledStreams > channels {
		return nil, ErrBadArg
	}
	if len(mapping) < channels {
		return nil, ErrBadArg
	}

	st := &OpusMSEncoder{
		Fs:          Fs,
		application: application,
		lfeStream:   -1, // MAPPING_TYPE_NONE (init_impl:478-479)
		bitrateBps:  OpusAuto,
		variableDur: FramesizeArg,
	}
	st.layout.nbChannels = channels
	st.layout.nbStreams = streams
	st.layout.nbCoupledStreams = coupledStreams
	for i := 0; i < channels; i++ {
		st.layout.mapping[i] = mapping[i]
	}
	// init_impl:486-489: both layout validations must pass.
	if !validateLayout(&st.layout) {
		return nil, ErrBadArg
	}
	if !validateEncoderLayout(&st.layout) {
		return nil, ErrBadArg
	}

	// init_impl:494-509: the first nb_coupled_streams sub-encoders are stereo, the
	// rest mono. NewEncoder is opus_encoder_init; a nil return is its OPUS_BAD_ARG
	// (bad Fs or application), which the C surfaces from the coupled_size/mono_size
	// query at :457-462.
	st.encoders = make([]*Encoder, streams)
	for i := 0; i < streams; i++ {
		ch := 1
		if i < coupledStreams {
			ch = 2
		}
		enc := NewEncoder(Fs, ch, application)
		if enc == nil {
			return nil, ErrBadArg
		}
		st.encoders[i] = enc
	}

	st.tmpData = make([]byte, msFrameTmp)
	return st, nil
}

// Streams returns the number of Opus streams the encoder emits per packet.
func (st *OpusMSEncoder) Streams() int { return st.layout.nbStreams }

// CoupledStreams returns the number of stereo (coupled) streams; the remaining
// Streams()-CoupledStreams() streams are mono.
func (st *OpusMSEncoder) CoupledStreams() int { return st.layout.nbCoupledStreams }

// Mapping returns a copy of the channel mapping table: one entry per input
// channel naming the stream channel it feeds (or 255 for a muted channel).
func (st *OpusMSEncoder) Mapping() []byte {
	m := make([]byte, st.layout.nbChannels)
	copy(m, st.layout.mapping[:st.layout.nbChannels])
	return m
}

// FinalRange is OPUS_GET_FINAL_RANGE (ctl:1221): the XOR of every sub-encoder's
// post-encode range-coder state, the strong per-packet bit-exactness check.
func (st *OpusMSEncoder) FinalRange() uint32 {
	var v uint32
	for _, enc := range st.encoders {
		v ^= enc.FinalRange()
	}
	return v
}

// SetBitrate is OPUS_SET_BITRATE (ctl:1161). The multistream-level clamp is per
// whole stream (750000/500 times the total channel count), distinct from the
// single encoder's per-channel clamp. OpusAuto and OpusBitrateMax pass through.
func (st *OpusMSEncoder) SetBitrate(v int32) error {
	if v != OpusAuto && v != OpusBitrateMax {
		if v <= 0 {
			return ErrBadArg
		}
		nbCh := int32(st.layout.nbChannels)
		v = imin32(750000*nbCh, imax32(500*nbCh, v))
	}
	st.bitrateBps = v
	return nil
}

// SetComplexity forwards OPUS_SET_COMPLEXITY to every sub-encoder (ctl:1246).
func (st *OpusMSEncoder) SetComplexity(v int) error {
	for _, enc := range st.encoders {
		if err := enc.SetComplexity(v); err != nil {
			return err
		}
	}
	return nil
}

// SetVBR forwards OPUS_SET_VBR to every sub-encoder (ctl:1247). v is 0 for CBR.
func (st *OpusMSEncoder) SetVBR(v int) error {
	for _, enc := range st.encoders {
		if err := enc.SetVBR(v); err != nil {
			return err
		}
	}
	return nil
}

// SetVBRConstraint forwards OPUS_SET_VBR_CONSTRAINT to every sub-encoder (ctl:1248).
func (st *OpusMSEncoder) SetVBRConstraint(v int) error {
	for _, enc := range st.encoders {
		if err := enc.SetVBRConstraint(v); err != nil {
			return err
		}
	}
	return nil
}

// SetDTX forwards OPUS_SET_DTX to every sub-encoder (ctl:1255).
func (st *OpusMSEncoder) SetDTX(v int) error {
	for _, enc := range st.encoders {
		if err := enc.SetDTX(v); err != nil {
			return err
		}
	}
	return nil
}

// SetForceMode forwards OPUS_SET_FORCE_MODE to every sub-encoder (ctl:1256). The
// frozen build pins this to MODE_CELT_ONLY.
func (st *OpusMSEncoder) SetForceMode(v int) error {
	for _, enc := range st.encoders {
		if err := enc.SetForceMode(v); err != nil {
			return err
		}
	}
	return nil
}

// ResetState is OPUS_RESET_STATE (ctl:1319): reset every sub-encoder. The surround
// preemph/window clear (:1322) is skipped; MAPPING_TYPE_NONE carries no such state.
func (st *OpusMSEncoder) ResetState() {
	for _, enc := range st.encoders {
		enc.Reset()
	}
}

// rateAllocation is rate_allocation (opus_multistream_encoder.c:805) for
// MAPPING_TYPE_NONE: fill rate[0:nb_streams] with each stream's target bitrate,
// floor each at 500, and return their sum. The ambisonics branch (:819) is out of
// scope; the surround branch is the one that runs for families 0/255.
func (st *OpusMSEncoder) rateAllocation(rate []int32, frameSize int) int32 {
	st.surroundRateAllocation(rate, frameSize, st.Fs)
	var rateSum int32
	for i := 0; i < st.layout.nbStreams; i++ {
		rate[i] = imax32(rate[i], 500)
		rateSum += rate[i]
	}
	return rateSum
}

// surroundRateAllocation is surround_rate_allocation (opus_multistream_encoder.c:702).
// For MAPPING_TYPE_NONE lfeStream is -1, so nb_lfe is 0 and the LFE terms
// (lfe_offset, lfe_ratio) drop out of every expression; they are kept verbatim so
// the function stays a literal transliteration for a future family-1 follow-up.
//
// The arithmetic is int32 because the C computes it in 32-bit int/opus_int32; the
// one place C widens to opus_int64 (the channel_rate numerator, :758, whose 256x
// scaling overflows 32 bits) is widened here too, then narrowed back to int32.
func (st *OpusMSEncoder) surroundRateAllocation(rate []int32, frameSize int, Fs int32) {
	var nbLfe int32
	if st.lfeStream != -1 {
		nbLfe = 1
	}
	nbCoupled := int32(st.layout.nbCoupledStreams)
	nbUncoupled := int32(st.layout.nbStreams) - nbCoupled - nbLfe
	nbNormal := 2*nbCoupled + nbUncoupled

	fsFrame := Fs / int32(frameSize)

	// Give each non-LFE channel enough bits per channel for coding band energy.
	channelOffset := 40 * imax32(50, fsFrame)

	var bitrate int32
	switch st.bitrateBps {
	case OpusAuto:
		bitrate = nbNormal*(channelOffset+Fs+10000) + 8000*nbLfe
	case OpusBitrateMax:
		bitrate = nbNormal*750000 + nbLfe*128000
	default:
		bitrate = st.bitrateBps
	}

	// Give LFE some basic stream_channel allocation but never exceed 1/20 of the
	// total rate for the non-energy part to avoid problems at really low rate.
	lfeOffset := imin32(bitrate/20, 3000) + 15*imax32(50, fsFrame)

	// Give each stream (coupled or uncoupled) a starting bitrate. This models the
	// main saving of coupled channels over uncoupled.
	streamOffset := (bitrate - channelOffset*nbNormal - lfeOffset*nbLfe) / nbNormal / 2
	streamOffset = imax32(0, imin32(20000, streamOffset))

	// Coupled streams get twice the mono rate after the offset is allocated.
	coupledRatio := int32(512) // Q8
	// LFE gets 1/8 the bits of mono.
	lfeRatio := int32(32) // Q8

	total := (nbUncoupled << 8) + // mono
		coupledRatio*nbCoupled + // stereo
		nbLfe*lfeRatio
	inner := bitrate - lfeOffset*nbLfe - streamOffset*(nbCoupled+nbUncoupled) - channelOffset*nbNormal
	channelRate := int32(256 * int64(inner) / int64(total))

	for i := 0; i < st.layout.nbStreams; i++ {
		switch {
		case i < st.layout.nbCoupledStreams:
			rate[i] = 2*channelOffset + imax32(0, streamOffset+(channelRate*coupledRatio>>8))
		case i != st.lfeStream:
			rate[i] = channelOffset + imax32(0, streamOffset+channelRate)
		default:
			rate[i] = imax32(0, lfeOffset+(channelRate*lfeRatio>>8))
		}
	}
}

// Encode is opus_multistream_encode (opus_multistream_encoder.c:1111): encode one
// frame of interleaved int16 pcm (nbChannels samples per frame period, in channel
// order) into data and return the packet length. analysisFrameSize is the caller's
// per-channel frame length in samples; data must have room for maxDataBytes. On
// success data[:n] is the multistream packet. Errors map to the opusenc sentinels
// (ErrBadArg / ErrBufferTooSmall / ErrInternal).
func (st *OpusMSEncoder) Encode(pcm []int16, analysisFrameSize int, data []byte, maxDataBytes int) (int, error) {
	ret := st.encodeNative(pcm, analysisFrameSize, data, maxDataBytes)
	if ret < 0 {
		return 0, codeErr(ret)
	}
	return ret, nil
}

// encodeNative is opus_multistream_encode_native (opus_multistream_encoder.c:841)
// for MAPPING_TYPE_NONE with the int16 copy_channel_in, lsb_depth 16 and
// float_api 0 the opus_multistream_encode entry point fixes. It returns the packet
// length or a negative Opus error code.
//
// The MAPPING_TYPE_SURROUND / MAPPING_TYPE_AMBISONICS branches are omitted: the
// preemph/window fetch (:876), the celt_mode query (:886, used only by
// surround_analysis), the bandSMR allocation and surround_analysis (:909), the
// per-stream bandwidth/force-mode/force-channels forcing (:939), the per-stream
// bandLogE population (:989/:1005) and OPUS_SET_ENERGY_MASK (:1013) all belong to
// those mappings. Everything else runs for NONE.
func (st *OpusMSEncoder) encodeNative(pcm []int16, analysisFrameSize int, data []byte, maxDataBytes int) int {
	Fs := st.Fs
	// vbr is read from the first sub-encoder (:884), not a multistream-level field:
	// every sub-encoder shares the VBR setting the forwarding ctl applied.
	vbr := st.encoders[0].useVbr

	frameSize := int(FrameSizeSelect(st.application, int32(analysisFrameSize), st.variableDur, Fs))
	if frameSize <= 0 {
		return opusBadArg
	}

	// Smallest packet the encoder can produce (:896-899): one ToC byte per stream
	// plus the self-delimiting length bytes; 100 ms needs one extra ToC byte per
	// stream.
	smallestPacket := st.layout.nbStreams*2 - 1
	if int(Fs)/frameSize == 10 {
		smallestPacket += st.layout.nbStreams
	}
	if maxDataBytes < smallestPacket {
		return opusBufferTooSmall
	}

	// Demux scratch, 2*frame_size int16 for a coupled stream's stereo pair (:905).
	if cap(st.buf) < 2*frameSize {
		st.buf = make([]int16, 2*frameSize)
	}
	buf := st.buf[:2*frameSize]

	// Cross-stream rate allocation (:916).
	rateSum := st.rateAllocation(st.bitrates[:], frameSize)

	// CBR shrinks the overall byte budget before the per-stream loop (:918-927), so
	// every stream's curr_max below is computed against the reduced max_data_bytes.
	if vbr == 0 {
		if st.bitrateBps == OpusAuto {
			maxDataBytes = imin(maxDataBytes, (int(celt.BitrateToBits(rateSum, Fs, int32(frameSize)))+4)/8)
		} else if st.bitrateBps != OpusBitrateMax {
			maxDataBytes = imin(maxDataBytes, imax(smallestPacket,
				(int(celt.BitrateToBits(st.bitrateBps, Fs, int32(frameSize)))+4)/8))
		}
	}

	// First per-stream loop: set each stream's target bitrate (:930-963). rate[s]
	// was floored at 500 by rateAllocation, so SetBitrate never rejects it.
	for s := 0; s < st.layout.nbStreams; s++ {
		_ = st.encoders[s].SetBitrate(st.bitrates[s])
	}

	// Second per-stream loop: demux, byte budget, encode, self-delimited reframe
	// (:965-1049). data offset and tot_size advance together, so tot_size is the
	// running write cursor into data.
	totSize := 0
	for s := 0; s < st.layout.nbStreams; s++ {
		st.rp.Init()
		enc := st.encoders[s]

		// Demux this stream's channel(s) from the interleaved input. copy_channel_in
		// is opus_copy_channel_in_short (:1075): INT16TORES is the identity in the
		// frozen fixed build, so this is a plain strided int16 copy.
		if s < st.layout.nbCoupledStreams {
			left := getLeftChannel(&st.layout, s, -1)
			right := getRightChannel(&st.layout, s, -1)
			for i := 0; i < frameSize; i++ {
				buf[2*i] = pcm[i*st.layout.nbChannels+left]
				buf[2*i+1] = pcm[i*st.layout.nbChannels+right]
			}
		} else {
			chn := getMonoChannel(&st.layout, s, -1)
			for i := 0; i < frameSize; i++ {
				buf[i] = pcm[i*st.layout.nbChannels+chn]
			}
		}

		// Byte budget for this stream (:1015-1024): reserve one byte for the last
		// stream and two for the others (the self-delimiting length), an extra ToC
		// byte per remaining stream at 100 ms, cap at the sub-packet scratch, and
		// drop the one/two bytes the repacketizer will add for a self-delimited frame.
		currMax := maxDataBytes - totSize
		currMax -= imax(0, 2*(st.layout.nbStreams-s-1)-1)
		if int(Fs)/frameSize == 10 {
			currMax -= st.layout.nbStreams - s - 1
		}
		currMax = imin(currMax, msFrameTmp)
		if s != st.layout.nbStreams-1 {
			if currMax > 253 {
				currMax -= 2
			} else {
				currMax -= 1
			}
		}
		// Under CBR the last stream's bitrate is re-derived from the bytes actually
		// left, so the packet fills the budget exactly (:1025-1026).
		if vbr == 0 && s == st.layout.nbStreams-1 {
			_ = enc.SetBitrate(celt.BitsToBitrate(int32(currMax*8), Fs, int32(frameSize)))
		}

		n := enc.encodeNative(buf, frameSize, st.tmpData, currMax, 16)
		if n < 0 {
			return n
		}
		// Reframe through the repacketizer so a multi-frame sub-packet (e.g. 60 ms
		// CELT-only) keeps its internal boundaries (:1037-1046).
		if err := st.rp.Cat(st.tmpData[:n]); err != nil {
			return opusInternalError
		}
		var wlen int
		var werr error
		if s != st.layout.nbStreams-1 {
			// Self-delimited: maxlen is max_data_bytes-tot_size, no padding.
			wlen, werr = st.rp.OutSelfDelimited(data[totSize:maxDataBytes])
		} else {
			// Last stream: plain framing, padded to the remaining budget under CBR.
			wlen, werr = st.rp.OutRangePadded(0, st.rp.GetNbFrames(), data[totSize:], maxDataBytes-totSize, vbr == 0)
		}
		if werr != nil {
			return opusInternalError
		}
		totSize += wlen
	}
	return totSize
}

// RateAllocationSeam exposes rate_allocation (opus_multistream_encoder.c:805) for
// the refc differential oracle in internal/reftest/oracle: it builds a
// MAPPING_TYPE_NONE encoder for the layout, applies bitrateBps through the
// multistream SetBitrate clamp, runs the per-stream rate allocation for frameSize,
// and returns the per-stream bitrates plus their sum. It exists only so the harness
// can pin rate_allocation against libopus in isolation, before the end-to-end
// packet gate; it has no production caller. Errors map to the opusenc sentinels.
func RateAllocationSeam(Fs int32, channels, streams, coupledStreams, application int, mapping []byte, bitrateBps int32, frameSize int) ([]int32, int32, error) {
	st, err := NewMSEncoder(Fs, channels, streams, coupledStreams, application, mapping)
	if err != nil {
		return nil, 0, err
	}
	if err := st.SetBitrate(bitrateBps); err != nil {
		return nil, 0, err
	}
	sum := st.rateAllocation(st.bitrates[:], frameSize)
	rates := make([]int32, streams)
	copy(rates, st.bitrates[:streams])
	return rates, sum, nil
}
