package opusenc

import (
	"encoding/binary"
	"hash/fnv"
)

// State is the field-level dump of the cross-frame OpusEncoder state, in the
// CANONICAL ORDER shared with the C oracle (internal/reftest/oracle/opusenc_shim.h,
// oracle_topenc_state) and with Hash below. Everything here lives at or after
// OPUS_ENCODER_RESET_START (opus_encoder.c:111), i.e. it is exactly the region
// OPUS_RESET_STATE clears.
//
// THE CANONICAL ORDER (do not reorder; the CP9 driver and the shim both depend on
// it, and Hash mixes the fields in exactly this sequence):
//
//	StreamChannels, HybridStereoWidthQ14, VariableHPSmth2Q15, PrevHBGain,
//	HPMem[4], Mode, PrevMode, PrevChannels, PrevFramesize, Bandwidth,
//	AutoBandwidth, First, BitrateBps, RangeFinal, DelayBuffer[encoderBuffer*channels]
//
// # Two fields are DELIBERATELY EXCLUDED
//
// st->width_mem (the StereoWidthState that compute_stereo_width mutates at
// opus_encoder.c:1322) and st->peak_signal_energy (mutated at :1317) are EXECUTED
// but OUTPUT-DEAD in the frozen forced-CELT-only configuration:
//
//   - stereo_width, the return value that width_mem accumulates into, is consumed
//     only inside the user_forced_mode == OPUS_AUTO mode-decision block. With
//     OPUS_SET_FORCE_MODE(MODE_CELT_ONLY) that block is never entered, so no
//     packet byte can depend on width_mem.
//   - peak_signal_energy is consumed only by DTX, which is deferred.
//
// This port therefore does not compute either one (stereo_width is pinned to 0),
// and comparing them against the oracle would compare a value the port provably
// does not need. That is a deliberate, bounded decision, not an oversight.
//
// IT MUST BE REVISITED WHEN DTX LANDS: enabling DTX makes peak_signal_energy LIVE
// (it gates the digital-silence / no-activity decision), at which point the field
// must be computed, added here, and added to the shim's dump in the same position.
//
// Also absent, and correctly so, because they are not cross-frame state on this
// path: silk_mode.stereoWidth_Q14 (recomputed from equiv_rate every frame at
// :2320-2327 before any read), nonfinal_frame (written only inside the deferred
// multiframe path), and silk_bw_switch / nb_no_activity_ms_Q1 (SILK/DTX only,
// never written in CELT-only).
type State struct {
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
	// DelayBuffer is the VALID prefix of st->delay_buffer: encoder_buffer*channels
	// samples. The C declares opus_res[MAX_ENCODER_BUFFER*2] but shortens the
	// allocation by MAX_ENCODER_BUFFER*sizeof(opus_res) for mono (:235), so only
	// this prefix exists.
	DelayBuffer []int16
}

// State returns the cross-frame encoder state in the canonical order. The
// DelayBuffer slice is a copy, so the caller can hold it across frames.
func (st *Encoder) State() State {
	s := State{
		StreamChannels:       int32(st.streamChannels),
		HybridStereoWidthQ14: int32(st.hybridStereoWidthQ14),
		VariableHPSmth2Q15:   st.variableHPSmth2Q15,
		PrevHBGain:           int32(st.prevHBGain),
		HPMem:                st.hpMem,
		Mode:                 int32(st.mode),
		PrevMode:             int32(st.prevMode),
		PrevChannels:         int32(st.prevChannels),
		PrevFramesize:        int32(st.prevFramesize),
		Bandwidth:            int32(st.bandwidth),
		AutoBandwidth:        int32(st.autoBandwidth),
		First:                int32(st.first),
		BitrateBps:           st.bitrateBps,
		RangeFinal:           st.rangeFinal,
	}
	s.DelayBuffer = make([]int16, len(st.delayBuffer))
	copy(s.DelayBuffer, st.delayBuffer)
	return s
}

// Hash is an FNV-1a digest of State's fields mixed in the canonical order, with
// every scalar written little-endian. It is a convenience for spotting the FRAME
// on which a sequence diverges; it is NOT a substitute for the field-level
// comparison, which is what tells you WHICH field went wrong. The CP9 gate
// compares fields, not just this.
func (s State) Hash() uint64 {
	h := fnv.New64a()
	var b [4]byte
	put := func(v uint32) {
		binary.LittleEndian.PutUint32(b[:], v)
		_, _ = h.Write(b[:])
	}

	put(uint32(s.StreamChannels))
	put(uint32(s.HybridStereoWidthQ14))
	put(uint32(s.VariableHPSmth2Q15))
	put(uint32(s.PrevHBGain))
	for _, v := range s.HPMem {
		put(uint32(v))
	}
	put(uint32(s.Mode))
	put(uint32(s.PrevMode))
	put(uint32(s.PrevChannels))
	put(uint32(s.PrevFramesize))
	put(uint32(s.Bandwidth))
	put(uint32(s.AutoBandwidth))
	put(uint32(s.First))
	put(uint32(s.BitrateBps))
	put(s.RangeFinal)
	for _, v := range s.DelayBuffer {
		put(uint32(int32(v)))
	}
	return h.Sum64()
}

// Hash returns State().Hash().
func (st *Encoder) Hash() uint64 { return st.State().Hash() }
