//go:build refc

package oracle

/*
#include "multistream_shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// This file exposes the pinned libopus multistream codec (surround encoder to
// synthesize packet sequences, plus a stateful decoder handle) over plain
// Go-typed values so multistream_test.go can drive the pure-Go
// opusdec.OpusMSDecoder against the C oracle in lockstep, asserting bit-identical
// int16 PCM and equal per-packet final range. The libopus multistream sources are
// compiled by the wm_src_opus_multistream*.c wrappers; this file only pulls in
// multistream_shim.h and never edits the shared oracle files. Build flags and
// include paths come from oracle_cgo.go.

// oAppAudioMS mirrors OPUS_APPLICATION_AUDIO (the value also lives in
// opusenc_cgo.go's constant block, kept here so this file stands alone).
const oAppAudioMS = 2049 // OPUS_APPLICATION_AUDIO

// maxMSPacketBytes bounds one encoded multistream packet. A multistream packet is
// up to nb_streams sub-packets; at 8 channels / <=5 streams and normal bitrates
// this is generous.
const maxMSPacketBytes = 8000

// MSLayout is the channel layout a surround encoder derived for a channel count.
type MSLayout struct {
	Streams int
	Coupled int
	Mapping []byte // one entry per API channel
}

// MSSurroundEncodeSeq builds a fresh mapping_family surround encoder at
// Fs/channels/bitrate and encodes numFrames frames (frameSize samples per channel)
// of interleaved int16 PCM, returning the derived layout and one packet per frame.
// pcm must hold channels*frameSize*numFrames samples.
func MSSurroundEncodeSeq(family, channels, Fs, bitrate, frameSize int, pcm []int16, numFrames int) (MSLayout, [][]byte, error) {
	if numFrames <= 0 {
		return MSLayout{}, nil, fmt.Errorf("numFrames must be positive, got %d", numFrames)
	}
	if len(pcm) < channels*frameSize*numFrames {
		return MSLayout{}, nil, fmt.Errorf("pcm has %d samples, need %d", len(pcm), channels*frameSize*numFrames)
	}

	mapping := make([]byte, channels)
	packets := make([]byte, numFrames*maxMSPacketBytes)
	lens := make([]int32, numFrames)
	var streams, coupled C.int

	r := C.oracle_ms_surround_encode_seq(
		C.int(family), C.int(channels), C.int(Fs), C.int(oAppAudioMS), C.int(bitrate),
		C.int(frameSize), (*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(numFrames),
		&streams, &coupled, (*C.uchar)(unsafe.Pointer(&mapping[0])),
		(*C.uchar)(unsafe.Pointer(&packets[0])), (*C.int32_t)(unsafe.Pointer(&lens[0])),
		C.int(maxMSPacketBytes))
	if r != 0 {
		return MSLayout{}, nil, fmt.Errorf("oracle_ms_surround_encode_seq: error %d", int(r))
	}

	out := make([][]byte, numFrames)
	for i := 0; i < numFrames; i++ {
		n := int(lens[i])
		p := make([]byte, n)
		copy(p, packets[i*maxMSPacketBytes:i*maxMSPacketBytes+n])
		out[i] = p
	}
	return MSLayout{Streams: int(streams), Coupled: int(coupled), Mapping: mapping}, out, nil
}

// CMSDecoder is a stateful C multistream decoder for replaying a packet sequence
// with an explicit layout. Destroy it when done.
type CMSDecoder struct {
	st       unsafe.Pointer
	channels int
}

// NewCMSDecoder creates a libopus multistream decoder at Fs/channels with the
// given (streams, coupled, mapping) layout.
func NewCMSDecoder(Fs, channels, streams, coupled int, mapping []byte) (*CMSDecoder, error) {
	if len(mapping) < channels {
		return nil, fmt.Errorf("mapping has %d entries, need %d", len(mapping), channels)
	}
	var cerr C.int
	st := C.oracle_ms_dec_create(C.int(Fs), C.int(channels), C.int(streams),
		C.int(coupled), (*C.uchar)(unsafe.Pointer(&mapping[0])), &cerr)
	if st == nil || cerr != 0 {
		return nil, fmt.Errorf("oracle_ms_dec_create: error %d", int(cerr))
	}
	return &CMSDecoder{st: st, channels: channels}, nil
}

// Decode decodes one packet (nil for PLC) into interleaved int16 pcm and returns
// the per-channel sample count.
func (d *CMSDecoder) Decode(pkt []byte, pcm []int16, frameSize int) (int, error) {
	var dp *C.uchar
	var dl C.int32_t
	if len(pkt) > 0 {
		dp = (*C.uchar)(unsafe.Pointer(&pkt[0]))
		dl = C.int32_t(len(pkt))
	}
	n := C.oracle_ms_dec_decode(d.st, dp, dl, (*C.int16_t)(unsafe.Pointer(&pcm[0])),
		C.int(frameSize), 0)
	if int(n) < 0 {
		return 0, fmt.Errorf("opus_multistream_decode: error %d", int(n))
	}
	return int(n), nil
}

// FinalRange returns the decoder's OPUS_GET_FINAL_RANGE (XOR across streams).
func (d *CMSDecoder) FinalRange() uint32 {
	return uint32(C.oracle_ms_dec_final_range(d.st))
}

// Destroy frees the C decoder.
func (d *CMSDecoder) Destroy() {
	C.oracle_ms_dec_destroy(d.st)
	d.st = nil
}
