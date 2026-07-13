//go:build refc

package oracle

/*
#include "opusenc_shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// This file exposes the pinned libopus TOP-LEVEL Opus encoder (src/opus_encoder.c)
// over plain Go-typed values so the CP9 differential test can drive the pure-Go
// internal/opusenc port against the C oracle without importing "C" itself: the
// file statics (gen_toc, dc_reject, stereo_fade, user_bitrate_to_bitrate,
// compute_equiv_rate, frame_size_select, bits_to_bitrate / bitrate_to_bits) and a
// create / encode-frame / dump-state / destroy handle triple that returns, per
// frame, the opus_encode return value, the packet bytes, st->rangeFinal and a
// field-level state dump in the canonical order.
//
// The package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls in
// opusenc_shim.h, which is the SOLE translation unit that compiles
// src/opus_encoder.c (see the header and gen_wrappers.sh). It does not edit any
// shared oracle file.

// Opus ctl sentinel values (include/opus_defines.h, src/opus_private.h) the CP9
// driver needs. Mirrored here so the tests never hard-code raw magic numbers.
const (
	oOpusAuto       = -1000 // OPUS_AUTO
	oOpusBitrateMax = -1    // OPUS_BITRATE_MAX

	oAppVoip              = 2048 // OPUS_APPLICATION_VOIP
	oAppAudio             = 2049 // OPUS_APPLICATION_AUDIO
	oAppRestrictedLowDlay = 2051 // OPUS_APPLICATION_RESTRICTED_LOWDELAY

	// The other OPUS_BANDWIDTH_* constants already live in opusdec_cgo.go.
	oBandwidthMediumband = 1102 // OPUS_BANDWIDTH_MEDIUMBAND

	oFramesizeArg = 5000 // OPUS_FRAMESIZE_ARG
)

// cOpusencCfg is the full ctl set the oracle encoder handle is created with. It
// maps 1:1 onto oracle_topenc_cfg in opusenc_shim.h. Build it with
// defaultOpusencCfg and override only what a case varies, so the ctls the case
// does not care about keep the opus_encoder_init defaults.
type cOpusencCfg struct {
	Fs                  int
	Channels            int
	Application         int
	ForceMode           int
	Bitrate             int
	Complexity          int
	VBR                 int
	VBRConstraint       int
	ForceChannels       int
	MaxBandwidth        int
	Bandwidth           int
	SignalType          int
	LsbDepth            int
	DTX                 int
	InbandFEC           int
	PacketLossPerc      int
	PredictionDisabled  int
	LFE                 int
	ExpertFrameDuration int
}

// defaultOpusencCfg returns the frozen CP9 configuration: 48 kHz,
// OPUS_APPLICATION_AUDIO (the only application that keeps encoder_buffer = Fs/100
// and delay_compensation = Fs/250, so the delay-compensation ring exists), forced
// MODE_CELT_ONLY, and the opus_encoder_init defaults (opus_encoder.c:293-317) for
// every other ctl.
func defaultOpusencCfg(channels int) cOpusencCfg {
	return cOpusencCfg{
		Fs:                  48000,
		Channels:            channels,
		Application:         oAppAudio,
		ForceMode:           oModeCeltOnly,
		Bitrate:             oOpusAuto,
		Complexity:          9, // opus_encoder_init: silk_mode.complexity = 9
		VBR:                 1,
		VBRConstraint:       1,
		ForceChannels:       oOpusAuto,
		MaxBandwidth:        oBandwidthFullband,
		Bandwidth:           oOpusAuto,
		SignalType:          oOpusAuto,
		LsbDepth:            24,
		DTX:                 0,
		InbandFEC:           0,
		PacketLossPerc:      0,
		PredictionDisabled:  0,
		LFE:                 0,
		ExpertFrameDuration: oFramesizeArg,
	}
}

func (c cOpusencCfg) toC() C.oracle_topenc_cfg {
	return C.oracle_topenc_cfg{
		Fs:                    C.int32_t(c.Fs),
		channels:              C.int32_t(c.Channels),
		application:           C.int32_t(c.Application),
		force_mode:            C.int32_t(c.ForceMode),
		bitrate:               C.int32_t(c.Bitrate),
		complexity:            C.int32_t(c.Complexity),
		vbr:                   C.int32_t(c.VBR),
		vbr_constraint:        C.int32_t(c.VBRConstraint),
		force_channels:        C.int32_t(c.ForceChannels),
		max_bandwidth:         C.int32_t(c.MaxBandwidth),
		bandwidth:             C.int32_t(c.Bandwidth),
		signal_type:           C.int32_t(c.SignalType),
		lsb_depth:             C.int32_t(c.LsbDepth),
		dtx:                   C.int32_t(c.DTX),
		inband_fec:            C.int32_t(c.InbandFEC),
		packet_loss_perc:      C.int32_t(c.PacketLossPerc),
		prediction_disabled:   C.int32_t(c.PredictionDisabled),
		lfe:                   C.int32_t(c.LFE),
		expert_frame_duration: C.int32_t(c.ExpertFrameDuration),
	}
}

// cOpusencState is the field-level cross-frame OpusEncoder state dump, in the
// CANONICAL ORDER shared with opusenc_shim.h's oracle_topenc_state and the Go
// internal/opusenc State(). width_mem and peak_signal_energy are deliberately
// excluded (executed but output-dead in the frozen forced-CELT-only config); see
// the header for the full reasoning.
type cOpusencState struct {
	StreamChannels       int32
	HybridStereoWidthQ14 int32
	VariableHPSmth2Q15   int32
	PrevHBGain           int32
	HPMem                [4]int32
	Mode                 int32
	PrevMode             int32
	PrevChannels         int32
	PrevFramesize        int32
	Bandwidth            int32
	AutoBandwidth        int32
	First                int32
	BitrateBps           int32
	RangeFinal           uint32
	DelayBuffer          []int16
}

func stateFromC(s *C.oracle_topenc_state) cOpusencState {
	out := cOpusencState{
		StreamChannels:       int32(s.stream_channels),
		HybridStereoWidthQ14: int32(s.hybrid_stereo_width_Q14),
		VariableHPSmth2Q15:   int32(s.variable_HP_smth2_Q15),
		PrevHBGain:           int32(s.prev_HB_gain),
		Mode:                 int32(s.mode),
		PrevMode:             int32(s.prev_mode),
		PrevChannels:         int32(s.prev_channels),
		PrevFramesize:        int32(s.prev_framesize),
		Bandwidth:            int32(s.bandwidth),
		AutoBandwidth:        int32(s.auto_bandwidth),
		First:                int32(s.first),
		BitrateBps:           int32(s.bitrate_bps),
		RangeFinal:           uint32(s.rangeFinal),
	}
	for i := range out.HPMem {
		out.HPMem[i] = int32(s.hp_mem[i])
	}
	n := int(s.delay_len)
	out.DelayBuffer = make([]int16, n)
	for i := 0; i < n; i++ {
		out.DelayBuffer[i] = int16(s.delay_buffer[i])
	}
	return out
}

// ---------------------------------------------------------------------------
// Flat wrappers over the opus_encoder.c file statics.
// ---------------------------------------------------------------------------

// cOpusencGenTOC is gen_toc (opus_encoder.c:330). channels is
// st->stream_channels at the :2551 call site, NOT st->channels.
func cOpusencGenTOC(mode, framerate, bandwidth, channels int) byte {
	return byte(C.oracle_topenc_gen_toc(C.int(mode), C.int(framerate),
		C.int(bandwidth), C.int(channels)))
}

// cOpusencDCReject is dc_reject (opus_encoder.c:479, FIXED_POINT). in and out are
// interleaved int16 of len*channels; hpMem (4 int32) is threaded across frames.
// Returns the filtered output and the updated hpMem.
func cOpusencDCReject(in []int16, cutoffHz int32, hpMem [4]int32, length, channels int, fs int32) ([]int16, [4]int32) {
	out := make([]int16, length*channels)
	mem := hpMem
	var inPtr *C.int16_t
	if len(in) > 0 {
		inPtr = (*C.int16_t)(unsafe.Pointer(&in[0]))
	}
	C.oracle_topenc_dc_reject(inPtr, C.int32_t(cutoffHz),
		(*C.int16_t)(unsafe.Pointer(&out[0])),
		(*C.int32_t)(unsafe.Pointer(&mem[0])),
		C.int(length), C.int(channels), C.int32_t(fs))
	return out, mem
}

// cOpusencStereoFade is stereo_fade (opus_encoder.c:548) with the 48 kHz / 960
// mode's window and overlap (120), matching the :2344 call site. It reads in and
// writes a fresh out (the C call is in-place; the caller can pass the same slice
// contents twice to reproduce that).
func cOpusencStereoFade(in []int16, g1, g2 int16, frameSize, channels int, fs int32) []int16 {
	out := make([]int16, len(in))
	copy(out, in)
	C.oracle_topenc_stereo_fade(
		(*C.int16_t)(unsafe.Pointer(&in[0])),
		(*C.int16_t)(unsafe.Pointer(&out[0])),
		C.int16_t(g1), C.int16_t(g2),
		C.int(frameSize), C.int(channels), C.int32_t(fs))
	return out
}

// cOpusencUserBitrateToBitrate is user_bitrate_to_bitrate (opus_encoder.c:733).
func cOpusencUserBitrateToBitrate(fs int32, channels int, userBitrateBps int32, frameSize, maxDataBytes int) int32 {
	return int32(C.oracle_topenc_user_bitrate_to_bitrate(C.int32_t(fs), C.int(channels),
		C.int32_t(userBitrateBps), C.int(frameSize), C.int(maxDataBytes)))
}

// cOpusencComputeEquivRate is compute_equiv_rate (opus_encoder.c:1027).
func cOpusencComputeEquivRate(bitrate int32, channels, frameRate, vbr, mode, complexity, loss int) int32 {
	return int32(C.oracle_topenc_compute_equiv_rate(C.int32_t(bitrate), C.int(channels),
		C.int(frameRate), C.int(vbr), C.int(mode), C.int(complexity), C.int(loss)))
}

// cOpusencBitsToBitrate is bits_to_bitrate (celt/celt.h:147).
func cOpusencBitsToBitrate(bits, fs, frameSize int32) int32 {
	return int32(C.oracle_topenc_bits_to_bitrate(C.int32_t(bits), C.int32_t(fs), C.int32_t(frameSize)))
}

// cOpusencBitrateToBits is bitrate_to_bits (celt/celt.h:151).
func cOpusencBitrateToBits(bitrate, fs, frameSize int32) int32 {
	return int32(C.oracle_topenc_bitrate_to_bits(C.int32_t(bitrate), C.int32_t(fs), C.int32_t(frameSize)))
}

// cOpusencFrameSizeSelect is frame_size_select (opus_encoder.c:827); -1 means the
// frame size is rejected, which opus_encode_native turns into OPUS_BAD_ARG.
func cOpusencFrameSizeSelect(application int, frameSize int32, variableDuration int, fs int32) int32 {
	return int32(C.oracle_topenc_frame_size_select(C.int(application), C.int32_t(frameSize),
		C.int(variableDuration), C.int32_t(fs)))
}

// cOpusencPacketPad is opus_packet_pad (src/repacketizer.c:359). It pads a copy
// of data[:length] up to newLen bytes in place and returns the padded packet plus
// the OPUS_* return code (0 == OPUS_OK). The input slice is not modified.
func cOpusencPacketPad(data []byte, length, newLen int) ([]byte, int) {
	buf := make([]byte, newLen+1) // +1 keeps &buf[0] addressable when newLen == 0
	copy(buf, data[:length])
	ret := int(C.oracle_topenc_packet_pad((*C.uchar)(unsafe.Pointer(&buf[0])),
		C.int(length), C.int(newLen)))
	if ret != 0 {
		return nil, ret
	}
	return buf[:newLen], ret
}

// ---------------------------------------------------------------------------
// Handle triple.
// ---------------------------------------------------------------------------

// cOpusencHandle is a live C OpusEncoder plus its config.
type cOpusencHandle struct {
	h   *C.oracle_topenc_h
	cfg cOpusencCfg
}

// cOpusencCreate creates the C encoder and applies the whole ctl set.
func cOpusencCreate(cfg cOpusencCfg) (*cOpusencHandle, error) {
	var cerr C.int
	ccfg := cfg.toC()
	h := C.oracle_topenc_create(&ccfg, &cerr)
	if h == nil {
		return nil, fmt.Errorf("oracle_topenc_create: %w", errString(cerr))
	}
	return &cOpusencHandle{h: h, cfg: cfg}, nil
}

// Encode encodes one frame of interleaved int16 PCM into a maxDataBytes buffer.
// It returns opus_encode's return value VERBATIM (a positive packet length, or a
// negative OPUS_* error code), the packet bytes (empty on error), and the
// field-level state dump taken after the call. Errors are returned as codes, not
// Go errors, because matching the rejection rules exactly is part of the gate.
func (e *cOpusencHandle) Encode(pcm []int16, frameSize, maxDataBytes int) (int, []byte, cOpusencState) {
	buf := make([]byte, maxDataBytes+1) // +1 so a 0-byte request still has an addressable base
	var st C.oracle_topenc_state
	var pcmPtr *C.int16_t
	if len(pcm) > 0 {
		pcmPtr = (*C.int16_t)(unsafe.Pointer(&pcm[0]))
	}
	ret := int(C.oracle_topenc_encode(e.h, pcmPtr, C.int(frameSize),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(maxDataBytes), &st))
	state := stateFromC(&st)
	if ret < 0 {
		return ret, nil, state
	}
	pkt := make([]byte, ret)
	copy(pkt, buf[:ret])
	return ret, pkt, state
}

// State returns the field-level state dump without encoding.
func (e *cOpusencHandle) State() cOpusencState {
	var st C.oracle_topenc_state
	C.oracle_topenc_get_state(e.h, &st)
	return stateFromC(&st)
}

// Reset applies OPUS_RESET_STATE.
func (e *cOpusencHandle) Reset() error {
	if code := C.oracle_topenc_reset(e.h); code != C.OPUS_OK {
		return fmt.Errorf("OPUS_RESET_STATE: %w", errString(code))
	}
	return nil
}

// Close frees the encoder. Safe to call once; the handle is nilled.
func (e *cOpusencHandle) Close() {
	if e.h != nil {
		C.oracle_topenc_destroy(e.h)
		e.h = nil
	}
}
