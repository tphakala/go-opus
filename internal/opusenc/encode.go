/* Copyright (c) 2010-2011 Xiph.Org Foundation, Skype Limited
   Written by Jean-Marc Valin and Koen Vos */
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

// encode.go is the DRIVER: opus_encode (opus_encoder.c:2662),
// opus_encode_native (:1182) and opus_encode_frame_native (:1855). It is the
// function that turns the pure-integer decision chain in rates.go and the PCM
// front end in pcm.go into an actual Opus packet: a TOC byte followed by a CELT
// payload.
//
// # The byte-budget chain, which is the whole game
//
// Three different byte counts are in play at once and they are NOT the same
// number. Confusing any two of them changes the packet length:
//
//	out_data_bytes      the caller's buffer size.
//	max_data_bytes      :1221  IMIN(1276*6, out_data_bytes), then in CBR
//	                    :1333  IMAX(1, cbr_bytes) -- the CBR target.
//	orig_max_data_bytes the value opus_encode_native PASSES to
//	                    opus_encode_frame_native (:1840), i.e. the post-CBR
//	                    max_data_bytes.
//	max_data_bytes      :1893  again, now IMIN(orig_max_data_bytes, 1276), the
//	  (frame-local)     1275-byte ceiling on a single CELT payload.
//
// and the range coder straddles them:
//
//	:1964  ec_enc_init(&enc, data, orig_max_data_bytes-1)   <- the ORIGINAL
//	:2392  nb_compr_bytes = (max_data_bytes-1) - redundancy_bytes
//	:2413  ec_enc_shrink(&enc, nb_compr_bytes)
//
// In CBR, CELT's OWN bitrate is left at OPUS_BITRATE_MAX (set at :2286; the
// OPUS_SET_BITRATE(st->bitrate_bps) at :2462 lives inside `if (st->use_vbr)`).
// celt_encoder.c:1917's CBR clamp requires `bitrate != OPUS_BITRATE_MAX`, so it
// is SKIPPED, and ec_enc_done therefore fills the entire (shrunk) storage window
// with the range coder's own padding. THAT is what makes a CBR packet come out at
// exactly the requested size -- opus_packet_pad is not doing it.
//
// TWO HONEST FOOTNOTES, both established by deliberately breaking the port and
// watching the differential test NOT fail. They do not change the code (which
// follows the C), but they say exactly how much the oracle can prove:
//
//  1. Setting CELT's bitrate in the CBR path anyway is, at 48 kHz, OUTPUT-NEUTRAL.
//     :1331 requantizes st->bitrate_bps to bits_to_bitrate(cbr_bytes*8), and
//     celt_encoder.c:1918's clamp then recomputes
//     (bitrate*frame_size + 4*Fs)/(8*Fs) - 1, which is identically cbr_bytes-1 ==
//     nb_compr_bytes. The clamp is an identity and its ec_enc_shrink a no-op.
//     The port still does NOT set it, because that is the C, and because the
//     identity is an artefact of the requantization that SILK/hybrid would break.
//  2. ec_enc_init's size (orig_max_data_bytes-1 vs the clamped max_data_bytes-1)
//     is likewise unobservable HERE, because :2413 ec_enc_shrink runs before a
//     single symbol is coded, and shrinking an empty coder just overwrites
//     storage. It stops being unobservable the moment SILK writes into the coder
//     between :1964 and :2413. The port uses orig_max_data_bytes-1, as the C does.
//
// The gate DOES bite on the things it must: reordering the delay-ring update past
// the fades, or skipping opus_packet_pad, both fail immediately.
//
// opus_packet_pad (:2646) does run on every CBR frame, but when
// cbr_bytes <= 1276 it short-circuits (repacketizer.c:343: len == new_len) and
// writes nothing. Real code-3 padding only happens when cbr_bytes > 1276, i.e.
// when the :1893 clamp cut orig_max_data_bytes down to 1276.
//
// # The delay-compensation ring
//
// The per-frame order is load-bearing:
//
//  1. :1967  the newest total_buffer samples of delay_buffer (its TAIL) are
//     copied to the HEAD of pcm_buf;
//  2. :2008  dc_reject writes the new frame AFTER that history;
//  3. :2304  delay_buffer is refreshed FROM pcm_buf -- BEFORE the fades, so the
//     stored history is PRE-FADE (the C says why at :2313-2314);
//  4. :2344  stereo_fade mutates pcm_buf;
//  5. :2493  CELT reads pcm_buf FROM INDEX 0, i.e. starting at the history, so
//     its input is deliberately time-shifted back by total_buffer samples.
//
// Any reordering of 3 and 4 diverges on the frame after next.
//
// # Deliberately not ported
//
// MULTIFRAME (:1698-1838). It only fires for frame_size > Fs/50 (> 20 ms) in
// CELT-only, and phase 4 tops out at 20 ms. encodeNative REJECTS that case with
// ErrUnimplemented at exactly the point the C branches into the repacketizer,
// rather than silently coding a wrong-length frame. Note the degenerate branch
// at :1340 comes FIRST and does handle > 20 ms frames, so a starved 60 ms frame
// still produces the same TOC-only packet the C does.
//
// DTX (:2564-2576), redundancy / celt_to_silk / to_celt / prefill (they need a
// mode transition, which a forced-CELT-only encoder can never make), the whole
// SILK block (:2043-2261), gain_fade (:2315, HB_gain is pinned to Q15ONE) and
// analysis (DISABLE_FLOAT_API). Each is marked at its line below.

import (
	"errors"

	"github.com/tphakala/go-opus/internal/fixedmath"
	"github.com/tphakala/go-opus/internal/packet"
	"github.com/tphakala/go-opus/internal/rangecoding"
)

// opusUnimplemented is OPUS_UNIMPLEMENTED (include/opus_defines.h:59). This port
// returns it for the deferred multiframe path (> 20 ms frames) and for the
// unported OPUS_AUTO mode decision, so a configuration outside the phase-4 scope
// fails loudly instead of emitting a wrong packet.
const opusUnimplemented = -5

// packetPayloadCap is the 1276-byte ceiling opus_encode_frame_native puts on a
// single coded frame (:1893). It is the same number as PacketSizeCap, but it is
// a DIFFERENT clamp at a different point in the chain, so it gets its own name.
const packetPayloadCap = 1276

// Witness records which branches the last EncodeRaw call took. It exists for the
// differential test's NON-VACUITY GUARDS: a sweep that never enters the CBR
// padding path, or never takes the delay ring's MOVE branch, is passing
// vacuously. Nothing in the encoder reads it.
type Witness struct {
	// Degenerate reports that :1340 fired: a TOC-only PLC packet.
	Degenerate bool
	// CBRBytes is cbr_bytes (:1330), or -1 under VBR.
	CBRBytes int
	// CBRBufferLimited reports that the CALLER'S BUFFER, not the requested bitrate,
	// set the packet size: user_bitrate_to_bitrate's own IMIN (:745) clamped the
	// rate down to bits_to_bitrate(max_data_bytes*8).
	CBRBufferLimited bool
	// CBRFloor1 reports that IMAX(1, cbr_bytes) at :1333 raised a zero budget. It
	// needs under ~4 bits per frame, so at 48 kHz only the short frame sizes can
	// reach it (2.5 ms at the 500 bps floor gives bits == 1 -> cbr_bytes == 0).
	CBRFloor1 bool
	// CBRIMinFired reports that the IMIN against max_data_bytes at :1330 actually
	// LOWERED cbr_bytes.
	//
	// IT IS ASSERTED DEAD, and the differential test fails if it ever fires.
	// user_bitrate_to_bitrate (:745) has ALREADY clamped the rate to
	// bits_to_bitrate(max_data_bytes*8, Fs, frame_size), and at 48 kHz the two
	// conversions round-trip exactly (6*Fs/frame_size is a whole number for every
	// legal frame size), so bitrate_to_bits of that is exactly max_data_bytes*8 and
	// (max_data_bytes*8 + 4)/8 is exactly max_data_bytes. The :1330 IMIN therefore
	// has nothing left to clamp and is defensive code. Recording it (rather than
	// claiming to have covered it) is the honest way to report a branch the frozen
	// configuration cannot reach.
	CBRIMinFired bool
	// OrigMaxDataBytes is what opus_encode_native handed to
	// opus_encode_frame_native (:1840), and FrameMaxDataBytes is the :1893 clamp
	// of it. They differ exactly when real padding runs.
	OrigMaxDataBytes  int
	FrameMaxDataBytes int
	// PaddingRan reports that opus_packet_pad (:2648) did real work, i.e.
	// cbr_bytes > 1276 and the packet was code-3 padded up to it.
	PaddingRan bool
	// StreamChannels is st->stream_channels (:1428-1456) and Downmixed reports a
	// STEREO encoder that was rate-downmixed to a MONO stream (which flips the
	// TOC's stereo bit and CELT's coded channel count).
	StreamChannels int
	Downmixed      bool
	// CurrBandwidth is curr_bandwidth (:1686) and EndBand the CELT end band it
	// maps to (:2266-2283).
	CurrBandwidth int
	EndBand       int
	// DelayMoveBranch / DelayCopyBranch are the two arms of :2304. Both must fire
	// across a sweep: MOVE needs frame_size < encoder_buffer-total_buffer (288 at
	// 48 kHz, so LM0/LM1), COPY needs frame_size >= that (LM2/LM3).
	DelayMoveBranch bool
	DelayCopyBranch bool
	// StereoFadeRan reports that :2344 stereo_fade fired (stereo, and the width
	// dropped below unity because equiv_rate <= 32000).
	StereoFadeRan bool
	// StereoWidthQ14 is silk_mode.stereoWidth_Q14 (:2320-2327).
	StereoWidthQ14 int32
	// CeltEncodeRan reports that the :2488 budget check passed and CELT actually
	// coded the frame. It must ALWAYS be true on a non-degenerate frame.
	CeltEncodeRan bool
	// BustCheckFired is the :2580 "the SILK encoder busted its target" check. It
	// is provably unreachable in CELT-only (on entry ec_tell() == 1 and
	// nb_compr_bytes >= 2), and the differential test ASSERTS IT NEVER FIRES.
	BustCheckFired bool
	// CeltBytes is celt_encode_with_ec's return (:2493), before the TOC byte is
	// counted at :2601.
	CeltBytes int
}

// Witness returns the branch record of the last EncodeRaw call. Differential-test
// seam; see the type doc.
func (st *Encoder) Witness() Witness { return st.wit }

// EncodeRaw is opus_encode (opus_encoder.c:2662). It returns the C return value
// VERBATIM: a positive packet length in bytes, or a negative OPUS_* error code.
//
// data is the caller's output buffer and maxDataBytes is out_data_bytes, the
// number of bytes of it the encoder may use; it must not exceed len(data).
// analysisFrameSize is the caller's sample count per channel, which
// frame_size_select (:827) turns into the frame size actually coded (or -1, which
// :1224 rejects as OPUS_BAD_ARG).
//
// The lsb_depth argument is 16, hard-coded by the opus_int16 entry point (:2667);
// float_api is 0 and the downmix/analysis arguments are dead under
// DISABLE_FLOAT_API.
func (st *Encoder) EncodeRaw(pcm []int16, analysisFrameSize int, data []byte, maxDataBytes int) int {
	frameSize := FrameSizeSelect(st.application, int32(analysisFrameSize), st.variableDuration, st.Fs)
	return st.encodeNative(pcm, int(frameSize), data, maxDataBytes, 16)
}

// Encode is EncodeRaw with the negative OPUS_* codes mapped onto this package's
// sentinel errors. On success it returns the packet length, and data[:n] is the
// packet.
func (st *Encoder) Encode(pcm []int16, analysisFrameSize int, data []byte, maxDataBytes int) (int, error) {
	ret := st.EncodeRaw(pcm, analysisFrameSize, data, maxDataBytes)
	if ret < 0 {
		return 0, codeErr(ret)
	}
	return ret, nil
}

// encodeNative is opus_encode_native (opus_encoder.c:1182) for the frozen config.
//
// Stage map, with the C line and LIVE/DEAD in this configuration:
//
//	:1221      max_data_bytes cap                                     LIVE
//	:1223      st->rangeFinal = 0                                     LIVE
//	:1224-1235 the rejection rules                                    LIVE
//	:1242      lsb_depth = IMIN(lsb_depth, st->lsb_depth)             LIVE
//	:1246      is_digital_silence                                     DEAD (feeds only voice_ratio, which :1307 re-pins, and DTX)
//	:1247-1305 run_analysis / detected_bandwidth                      DEAD (DISABLE_FLOAT_API)
//	:1310-1320 peak_signal_energy                                     DEAD-OUTPUT (DTX only; omitted, see State doc)
//	:1321-1324 compute_stereo_width                                   DEAD-OUTPUT (OPUS_AUTO mode decision only; omitted)
//	:1325-1685 the decision chain                                     LIVE  -> DecideRates
//	:1340-1406 the degenerate TOC-only packet                         LIVE  -> EncodeDegenerate
//	:1541-1581 redundancy / prefill / SILK stereo->mono delay         DEAD (needs a mode transition)
//	:1675      decide_fec                                             DEAD (returns 0 in CELT-only)
//	:1678      CELT_SET_LSB_DEPTH                                     LIVE
//	:1698-1838 multiframe                                             DEFERRED -> opusUnimplemented
//	:1840      opus_encode_frame_native                               LIVE
func (st *Encoder) encodeNative(pcm []int16, frameSize int, data []byte, outDataBytes, lsbDepth int) int {
	st.wit = Witness{CBRBytes: -1}

	// :1223. Before the rejection rules, so a rejected frame still zeroes it.
	st.rangeFinal = 0

	// :1221, :1224-1235.
	maxDataBytes, err := st.CheckEncodeArgs(frameSize, outDataBytes)
	if err != nil {
		return errCode(err)
	}

	// :1242. opus_encode passes 16; st->lsb_depth defaults to 24 and is settable
	// down to 8, so this is a real IMIN, not a formality.
	lsbDepth = fixedmath.IMIN(lsbDepth, st.lsbDepth)

	// The CBR witness values have to be derived HERE, from the pre-:1331 bitrate:
	// once DecideRates has requantized st.bitrateBps to the rounded byte budget,
	// every one of these predicates reads back as an identity and proves nothing.
	// Pure reads; no state is touched.
	if st.useVbr == 0 {
		br := UserBitrateToBitrate(st.Fs, st.channels, st.userBitrateBps, frameSize, maxDataBytes)
		maxBitrate := bitsToBitrate(int32(maxDataBytes)*8, st.Fs, int32(frameSize))
		st.wit.CBRBufferLimited = br == maxBitrate
		st.wit.CBRIMinFired = int((bitrateToBits(br, st.Fs, int32(frameSize))+4)/8) > maxDataBytes
	}

	// :1325-1685. DecideRates re-pins voice_ratio (:1307), resolves the bitrate,
	// applies the CBR byte-budget derivation, makes the stereo/mono and bandwidth
	// decisions, and reports the degenerate case. It MUTATES st.bitrateBps,
	// st.streamChannels, st.mode, st.bandwidth and st.autoBandwidth.
	d, ok := st.DecideRates(frameSize, maxDataBytes)
	if !ok {
		// The OPUS_AUTO mode decision (:1473-1527) is not ported: it interpolates
		// mode_thresholds on stereo_width, whose only consumer it is. Callers must
		// OPUS_SET_FORCE_MODE.
		return opusUnimplemented
	}

	st.wit.CBRBytes = d.CBRBytes
	st.wit.StreamChannels = st.streamChannels
	st.wit.Downmixed = st.channels == 2 && st.streamChannels == 1
	st.wit.CurrBandwidth = d.CurrBandwidth
	if st.useVbr == 0 {
		st.wit.CBRFloor1 = d.CBRBytes < 1
	}

	// :1340-1406. NOT an error: a 1- or 2-byte TOC-only packet, returned with a
	// POSITIVE length. It runs BEFORE the multiframe split, so it is also the C's
	// answer for a starved > 20 ms frame.
	if d.Degenerate {
		st.wit.Degenerate = true
		return st.EncodeDegenerate(data, outDataBytes, d.MaxDataBytes, d.FrameRate)
	}

	// :1678.
	st.celt.SetLsbDepth(lsbDepth)

	// :1698. The multiframe path. In CELT-only the condition reduces to
	// frame_size > Fs/50, i.e. anything above 20 ms; a 40/60/80/100/120 ms frame
	// would be split into 20 ms sub-frames, encoded one at a time through
	// opus_encode_frame_native and re-assembled by the repacketizer into a code-2
	// or code-3 packet. That is DEFERRED, and rejected here rather than
	// mishandled.
	if (frameSize > int(st.Fs)/50 && st.mode != ModeSilkOnly) || frameSize > 3*int(st.Fs)/50 {
		return opusUnimplemented
	}

	// :1840. NOTE what is passed as orig_max_data_bytes: the POST-CBR
	// max_data_bytes, which is what ec_enc_init is sized from and what
	// opus_packet_pad pads up to.
	return st.encodeFrameNative(pcm, frameSize, data, d.MaxDataBytes, d.EquivRate)
}

// encodeFrameNative is opus_encode_frame_native (opus_encoder.c:1855, static) for
// the frozen config: forced MODE_CELT_ONLY, no redundancy, no DTX, no DRED, no
// analysis.
//
// Stage map, with the C line and LIVE/DEAD:
//
//	:1893      max_data_bytes = IMIN(orig_max_data_bytes, 1276)       LIVE
//	:1894      st->rangeFinal = 0                                     LIVE
//	:1902-1907 curr_bandwidth, delay_compensation, total_buffer       LIVE
//	:1911-1930 the activity / VAD decision                            DEAD (DTX only)
//	:1933-1940 silk_bw_switch                                         DEAD (SILK only)
//	:1944-1957 redundancy                                             DEAD (CELT-only forces it off at :1945)
//	:1960      bits_target                                            DEAD (read only at :2051, inside the SILK block)
//	:1962      data += 1                                              LIVE
//	:1964      ec_enc_init over orig_max_data_bytes-1                 LIVE
//	:1966-1967 pcm_buf + the delay-buffer history                     LIVE  -> PCMFront
//	:1969-1978 variable_HP_smth2_Q15                                  LIVE  -> PCMFront
//	:1980-2001 hp_cutoff (VOIP)                                       DEAD
//	:2008      dc_reject                                              LIVE  -> PCMFront
//	:2010-2025 the float NaN guard                                    DEAD (FIXED_POINT)
//	:2043-2261 the whole SILK block                                   DEAD
//	:2044      HB_gain = Q15ONE                                       LIVE (constant)
//	:2263-2295 the CELT ctls                                          LIVE
//	:2297-2302 tmp_prefill                                            DEAD (needs a mode transition)
//	:2304-2312 the delay-buffer update                                LIVE  -> UpdateDelayBuffer
//	:2315-2318 gain_fade                                              DEAD (HB_gain == prev_HB_gain == Q15ONE)
//	:2319-2349 prev_HB_gain, stereoWidth_Q14, stereo_fade             LIVE  -> PCMFades
//	:2351-2377 the redundancy signalling                              DEAD (mode != CELT_ONLY guard)
//	:2379-2384 !redundancy cleanup, start_band                        LIVE (both no-ops)
//	:2386-2414 nb_compr_bytes + ec_enc_shrink                         LIVE
//	:2416-2442 CELT_SET_ANALYSIS / SILK info / CELT->SILK redundancy  DEAD
//	:2444-2447 CELT_SET_START_BAND, data[-1] = 0                      LIVE
//	:2448-2486 the VBR/CBR CELT ctls and the prefill                  LIVE / DEAD (prefill)
//	:2488-2509 celt_encode_with_ec + OPUS_GET_FINAL_RANGE             LIVE
//	:2514-2545 the SILK->CELT redundancy frame                        DEAD
//	:2549-2553 the TOC byte                                           LIVE
//	:2555-2562 the epilogue (prev_mode, prev_channels, first, ...)    LIVE
//	:2564-2576 the DTX decision                                       DEAD (use_dtx == 0 -> the else at :2574)
//	:2578-2589 the ec_tell bust check                                 LIVE but PROVABLY UNREACHABLE (asserted)
//	:2590-2599 the SILK-only trailing-zero strip                      DEAD
//	:2601-2654 ret, apply_padding, opus_packet_pad                    LIVE
//
//nolint:gocyclo // Verbatim transliteration of a single C function; splitting it would break the 1:1 mapping the differential oracle depends on.
func (st *Encoder) encodeFrameNative(pcm []int16, frameSize int, data []byte, origMaxDataBytes int, equivRate int32) int {
	// :1893. A single coded frame can never exceed 1275 payload bytes, whatever
	// the CBR target says. When origMaxDataBytes is bigger, the difference is made
	// up by opus_packet_pad at :2648, and ONLY then does it do real work.
	maxDataBytes := fixedmath.IMIN(origMaxDataBytes, packetPayloadCap)
	st.wit.OrigMaxDataBytes = origMaxDataBytes
	st.wit.FrameMaxDataBytes = maxDataBytes

	// :1894.
	st.rangeFinal = 0

	// :1902. Identical to opus_encode_native's curr_bandwidth (:1686): nothing
	// between the two writes st->bandwidth on this path.
	currBandwidth := st.bandwidth

	// :1903-1907. RESTRICTED_LOWDELAY (and the RESTRICTED_* apps generally) zero
	// the delay compensation, which collapses the ring; AUDIO and VOIP keep it.
	delayCompensation := st.delayCompensation
	if st.application == ApplicationRestrictedLowdelay {
		delayCompensation = 0
	}
	totalBuffer := delayCompensation

	// :1909 frame_rate: its only consumer is compute_redundancy_bytes (:1949),
	// which redundancy == 0 makes unreachable. Not computed.
	//
	// :1911-1930 activity: consumed only by decide_dtx_mode (:2567), which
	// `st->use_dtx` gates off. Not computed. See the State doc: this is why
	// peak_signal_energy is excluded from the state comparison, and why enabling
	// DTX must revisit that.
	//
	// :1877, :1944-1945. MODE_CELT_ONLY forces redundancy off no matter what
	// opus_encode_native decided, so redundancy_bytes is 0 for the whole function
	// and every `+ redundancy_bytes` / `- redundancy_bytes` below is an identity.
	//
	// The C declares it here (:1877) AND re-zeroes it at :2382, and both are kept:
	// the linter is right that the first assignment is dead, but collapsing them
	// would erase the :2382 statement from the transliteration, and :2382 is the
	// line that goes LIVE the moment redundancy can be non-zero (i.e. as soon as a
	// mode transition is reachable).
	//
	//nolint:wastedassign // Verbatim C: :1877 declares it, :2382 re-zeroes it. See above.
	redundancyBytes := 0

	// :1960 bits_target: read only at :2051, inside `if (st->mode != MODE_CELT_ONLY)`.
	// Not computed.

	// :1962. `data += 1` reserves byte 0 for the TOC, which is not written until
	// :2447/:2551. payload is the C's advanced `data`; data[-1] is our data[0].
	payload := data[1:]

	// :1964. THE ORIGINAL byte count, not the :1893-clamped one. In CBR with
	// cbr_bytes > 1276 the coder's storage is therefore LARGER than the window it
	// is about to be shrunk to at :2413; that is deliberate and harmless.
	var enc rangecoding.Encoder
	enc.Init(payload[:origMaxDataBytes-1])

	// :1966-1967, :1969-1978, :2008.
	pcmBuf := st.PCMFront(pcm, frameSize, totalBuffer)

	// :2044. HB_gain is only moved off Q15ONE inside the SILK block (:2061), so on
	// this path it is a constant. The ASSIGNMENT st->prev_HB_gain = HB_gain (:2319)
	// is still live cross-frame state, and PCMFades performs it.
	hbGain := int16(q15One)

	// :2266-2283. curr_bandwidth -> CELT's end band.
	endband := 21
	switch currBandwidth {
	case BandwidthNarrowband:
		endband = 13
	case BandwidthMediumband, BandwidthWideband:
		endband = 17
	case BandwidthSuperwideband:
		endband = 19
	case BandwidthFullband:
		endband = 21
	}
	st.wit.EndBand = endband

	// :2284-2286.
	st.celt.SetEndBand(endband)
	st.celt.SetStreamChannels(st.streamChannels)
	// THE CBR PACKET LENGTH DEPENDS ON THIS. Leaving CELT at OPUS_BITRATE_MAX is
	// what makes celt_encoder.c:1917's `!vbr && bitrate != OPUS_BITRATE_MAX` clamp
	// false, so CELT does not shrink the coder and ec_enc_done fills the whole
	// storage window. Under VBR :2462 below raises it to st->bitrate_bps; under CBR
	// it MUST stay here.
	st.celt.SetBitrate(OpusBitrateMax)

	// :2288-2295. CELT_SET_PREDICTION(celt_pred), expanded to the two fields its
	// ctl writes (celt_encoder.c:2972-2979: disable_pf = value<=1,
	// force_intra = value==0), because internal/celt exposes them separately.
	celtPred := 2
	if st.silkMode.reducedDependency != 0 {
		celtPred = 0
	}
	st.celt.SetDisablePf(b2i(celtPred <= 1))
	st.celt.SetForceIntra(b2i(celtPred == 0))

	// :2297-2302 tmp_prefill: `st->mode != st->prev_mode && st->prev_mode > 0` can
	// never hold (prev_mode is 0 on frame 1 and MODE_CELT_ONLY forever after).

	// :2304-2312. BEFORE the fades: the delay buffer must hold the PRE-FADE
	// samples.
	if st.channels*(st.encoderBuffer-(frameSize+totalBuffer)) > 0 {
		st.wit.DelayMoveBranch = true
	} else {
		st.wit.DelayCopyBranch = true
	}
	st.UpdateDelayBuffer(pcmBuf, frameSize, totalBuffer)

	// :2313-2349. gain_fade (dead), prev_HB_gain, stereoWidth_Q14 and stereo_fade.
	g1Before := st.hybridStereoWidthQ14
	st.PCMFades(pcmBuf, frameSize, equivRate, hbGain)
	st.wit.StereoWidthQ14 = st.silkMode.stereoWidthQ14
	st.wit.StereoFadeRan = st.energyMasking == nil && st.channels == 2 &&
		(int32(g1Before) < (1<<14) || st.silkMode.stereoWidthQ14 < (1<<14))

	// :2351-2377. The guard is `st->mode != MODE_CELT_ONLY && ...`, so the else at
	// :2375 is taken: redundancy = 0 (it already is).
	//
	// :2379-2383. !redundancy: both of these are already true, but they are the C.
	st.silkBwSwitch = 0
	redundancyBytes = 0

	// :2384. start_band is 17 only when mode != MODE_CELT_ONLY.
	startBand := 0

	// :2386-2414. The mode == MODE_SILK_ONLY arm is dead; this is the else.
	nbComprBytes := (maxDataBytes - 1) - redundancyBytes
	enc.EncShrink(uint32(nbComprBytes))

	// :2416-2425 CELT_SET_ANALYSIS / CELT_SET_SILK_INFO: DISABLE_FLOAT_API and
	// hybrid-only. :2428-2442 the CELT->SILK redundancy frame: redundancy == 0.

	// :2444-2445.
	st.celt.SetStartBand(startBand)

	// :2447. `data[-1] = 0`. The TOC byte is ORed in at :2551, so it has to start
	// from a known zero: the caller's buffer is not required to be clean.
	data[0] = 0

	// :2448-2464. The mode == MODE_HYBRID arm is dead; this is the else.
	st.celt.SetVBR(st.useVbr)
	if st.useVbr != 0 {
		st.celt.SetVBR(1)
		st.celt.SetConstrainedVBR(st.vbrConstraint)
		st.celt.SetBitrate(st.bitrateBps)
	}

	// :2478-2486 the mode-transition prefill: unreachable, see :2297.

	// :2487-2508. "If false, we already busted the budget and we'll end up with a
	// PLC frame". It cannot be false here: a freshly Init'ed coder has
	// ec_tell() == 1, and nb_compr_bytes == max_data_bytes-1 >= 2 because the
	// degenerate check at :1340 already rejected max_data_bytes < 3. The
	// differential test asserts CeltEncodeRan on every non-degenerate frame.
	ret := 0
	if int(enc.Tell()) <= 8*nbComprBytes {
		st.wit.CeltEncodeRan = true
		// :2493. compressed == NULL, enc != NULL: CELT writes into the coder the
		// Opus layer already opened over the packet buffer and shrank to
		// nb_compr_bytes.
		ret = st.celt.EncodeWithEC(pcmBuf, frameSize, &enc, nbComprBytes)
		if ret < 0 {
			return opusInternalError
		}
		st.wit.CeltBytes = ret
	}
	// :2509. OPUS_GET_FINAL_RANGE runs even if CELT did not.
	st.rangeFinal = st.celt.Rng()

	// :2514-2545 the SILK->CELT redundancy frame: redundancy == 0.

	// :2549-2551. `data--` then `data[0] |= gen_toc(...)`. gen_toc takes
	// st->stream_channels, NOT st->channels: a stereo encoder downmixed to mono by
	// :1448 emits a MONO TOC.
	data[0] |= GenTOC(st.mode, int(st.Fs)/frameSize, currBandwidth, st.streamChannels)

	// :2553. redundant_rng is 0, so the XOR is an identity.

	// :2555-2562. to_celt is always 0 (it needs a SILK->CELT transition).
	st.prevMode = st.mode
	st.prevChannels = st.streamChannels
	st.prevFramesize = frameSize
	st.first = 0

	// :2564-2576. st->use_dtx is 0, so the else at :2574 runs:
	// st->nb_no_activity_ms_Q1 = 0 (a field this port does not carry, because it
	// is never made non-zero). DTX is deferred.

	// :2578-2589. Ported, and asserted never to fire: ec_tell() cannot exceed
	// 8*(max_data_bytes-1) == 8*nb_compr_bytes, because CELT was handed exactly
	// that budget and its own ec_enc_done respects it.
	if int(enc.Tell()) > (maxDataBytes-1)*8 {
		st.wit.BustCheckFired = true
		if maxDataBytes < 2 {
			return opusBufferTooSmall
		}
		data[1] = 0
		ret = 1
		st.rangeFinal = 0
	}
	// :2590-2599 the trailing-zero strip: MODE_SILK_ONLY only.

	// :2601. Count the ToC (and the redundancy, which is 0).
	ret += 1 + redundancyBytes

	// :2602, :2646-2654. In CBR opus_packet_pad ALWAYS runs, but it short-circuits
	// when ret == orig_max_data_bytes (repacketizer.c:343), which is the normal
	// case: CELT's own ec_enc_done already filled the storage window. It only does
	// real code-3 padding when :1893 clamped orig_max_data_bytes down to 1276.
	if st.useVbr == 0 {
		st.wit.PaddingRan = ret != origMaxDataBytes
		if err := packet.Pad(data, ret, origMaxDataBytes); err != nil {
			return opusInternalError
		}
		ret = origMaxDataBytes
	}
	return ret
}

// b2i is C's implicit bool-to-int in `st->disable_pf = value<=1;`.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// errCode maps a sentinel error back to the OPUS_* integer the C returns.
// CheckEncodeArgs reports its verdict as an error; opus_encode_native returns an
// int, and the differential test compares those ints exactly.
func errCode(err error) int {
	switch {
	case err == nil:
		return opusOK
	case errors.Is(err, ErrBadArg):
		return opusBadArg
	case errors.Is(err, ErrBufferTooSmall):
		return opusBufferTooSmall
	case errors.Is(err, ErrUnimplemented):
		return opusUnimplemented
	default:
		return opusInternalError
	}
}
