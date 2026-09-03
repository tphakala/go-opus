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

// maxMSPacketBytes bounds one encoded multistream packet. A multistream packet is
// up to nb_streams sub-packets; at 8 channels / <=5 streams and normal bitrates
// this is generous.
const maxMSPacketBytes = 8000

// oSignalVoice mirrors OPUS_SIGNAL_VOICE; passed to OPUS_SET_SIGNAL to bias the
// sub-encoders toward SILK / hybrid so the FEC leg emits real LBRR.
const oSignalVoice = 3001 // OPUS_SIGNAL_VOICE

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
		C.int(family), C.int(channels), C.int(Fs), C.int(oAppAudio), C.int(bitrate),
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

// MSEncodeOpts carries the extra surround-encoder controls the differential legs
// need (in-band FEC / LBRR, DTX, a forced bandwidth and signal type). The zero
// value means auto bandwidth and signal, no FEC and no DTX; Bitrate, Complexity and
// VBR are always applied.
type MSEncodeOpts struct {
	Bitrate       int
	Complexity    int
	VBR           int // 0 or 1
	InbandFEC     bool
	PacketLossPct int
	DTX           bool
	Bandwidth     int // 0 = auto, else an OPUS_BANDWIDTH_* value
	SignalType    int // 0 = auto, else an OPUS_SIGNAL_* value
}

// MSSurroundEncodeSeqOpts is MSSurroundEncodeSeq with the extra encoder controls in
// opts, so a test can force FEC / LBRR, DTX, bandwidth and signal type. The layout
// and per-frame packets are returned the same way.
func MSSurroundEncodeSeqOpts(family, channels, Fs, frameSize int, pcm []int16, numFrames int, opts MSEncodeOpts) (MSLayout, [][]byte, error) {
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

	fec, dtx := 0, 0
	if opts.InbandFEC {
		fec = 1
	}
	if opts.DTX {
		dtx = 1
	}

	r := C.oracle_ms_surround_encode_seq_opts(
		C.int(family), C.int(channels), C.int(Fs), C.int(oAppAudio), C.int(opts.Bitrate),
		C.int(opts.Complexity), C.int(opts.VBR), C.int(fec), C.int(opts.PacketLossPct), C.int(dtx),
		C.int(opts.Bandwidth), C.int(opts.SignalType),
		C.int(frameSize), (*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(numFrames),
		&streams, &coupled, (*C.uchar)(unsafe.Pointer(&mapping[0])),
		(*C.uchar)(unsafe.Pointer(&packets[0])), (*C.int32_t)(unsafe.Pointer(&lens[0])),
		C.int(maxMSPacketBytes))
	if r != 0 {
		return MSLayout{}, nil, fmt.Errorf("oracle_ms_surround_encode_seq_opts: error %d", int(r))
	}

	out := make([][]byte, numFrames)
	for i := 0; i < numFrames; i++ {
		n := int(lens[i])
		p := make([]byte, n)
		if n > 0 {
			copy(p, packets[i*maxMSPacketBytes:i*maxMSPacketBytes+n])
		}
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
	return d.decode(pkt, pcm, frameSize, 0)
}

// DecodeFEC reconstructs a lost frame from the in-band FEC carried by pkt (the next
// received packet), mirroring opus_multistream_decode with decode_fec=1. Streams
// without FEC for the lost duration fall back to concealment.
func (d *CMSDecoder) DecodeFEC(pkt []byte, pcm []int16, frameSize int) (int, error) {
	return d.decode(pkt, pcm, frameSize, 1)
}

// decode is the shared body of Decode and DecodeFEC.
func (d *CMSDecoder) decode(pkt []byte, pcm []int16, frameSize, decodeFec int) (int, error) {
	if d.st == nil {
		return 0, fmt.Errorf("CMSDecoder: decoder is destroyed")
	}
	// opus_multistream_decode writes up to frameSize*channels interleaved samples
	// into pcm; guard the capacity here so a mis-sized caller gets an error instead
	// of a Go panic on &pcm[0] (empty pcm) or the C decoder writing past the Go
	// allocation.
	if frameSize <= 0 || len(pcm) < frameSize*d.channels {
		return 0, fmt.Errorf("CMSDecoder: pcm holds %d samples, need %d (frameSize=%d, channels=%d)",
			len(pcm), frameSize*d.channels, frameSize, d.channels)
	}
	var dp *C.uchar
	var dl C.int32_t
	if len(pkt) > 0 {
		dp = (*C.uchar)(unsafe.Pointer(&pkt[0]))
		dl = C.int32_t(len(pkt))
	}
	n := C.oracle_ms_dec_decode(d.st, dp, dl, (*C.int16_t)(unsafe.Pointer(&pcm[0])),
		C.int(frameSize), C.int(decodeFec))
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
