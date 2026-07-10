//go:build refc

package oracle

/*
#include "celtdec_shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/tphakala/go-opus/internal/celt"
)

// This file exposes the pinned libopus RAW CELT encoder/decoder (celt_encode_with_ec
// / celt_decode_with_ec over the frozen static mode48000_960) as plain Go types so
// celtdec_test.go can drive the pure-Go internal/celt decoder against the C oracle
// in lockstep. Package-level cgo CFLAGS live in oracle_cgo.go; this file only pulls
// in celtdec_shim.h. It never touches the shared oracle surface.

// cCELTNbEBands returns the mode's band count (21).
func cCELTNbEBands() int { return int(C.oracle_celtdec_nbebands()) }

// cCELTDecodeMemLen returns len(_decode_mem) = channels*(2048+overlap).
func cCELTDecodeMemLen(channels int) int {
	return int(C.oracle_celtdec_decodemem_len(C.int(channels)))
}

// cCELTLpcLen returns len(lpc) = channels*CELT_LPC_ORDER.
func cCELTLpcLen(channels int) int { return int(C.oracle_celtdec_lpc_len(C.int(channels))) }

// cCELTEncoder is a raw libopus CELT encoder (CBR, MODE_CELT_ONLY by
// construction) used only to PRODUCE valid CELT packets for the differential
// test.
type cCELTEncoder struct {
	enc      unsafe.Pointer
	channels int
}

// newCCELTEncoder creates a raw CELT encoder at 48 kHz with the given channel
// count and complexity (0..10). Close it when done.
func newCCELTEncoder(channels, complexity int) (*cCELTEncoder, error) {
	p := C.oracle_celtenc_create(C.int(channels), C.int(complexity))
	if p == nil {
		return nil, fmt.Errorf("oracle_celtenc_create(channels=%d) failed", channels)
	}
	return &cCELTEncoder{enc: p, channels: channels}, nil
}

// Encode CBR-encodes one interleaved int16 frame (frameSize samples/channel)
// into nbBytes bytes and returns the packet.
func (e *cCELTEncoder) Encode(pcm []int16, frameSize, nbBytes int) ([]byte, error) {
	if len(pcm) < frameSize*e.channels {
		return nil, fmt.Errorf("pcm has %d samples, need %d", len(pcm), frameSize*e.channels)
	}
	buf := make([]byte, nbBytes)
	n := C.oracle_celtenc_encode(e.enc,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(frameSize),
		C.int(nbBytes), (*C.uchar)(unsafe.Pointer(&buf[0])))
	if n < 0 {
		return nil, fmt.Errorf("celt_encode_with_ec returned %d", int(n))
	}
	return buf[:int(n)], nil
}

func (e *cCELTEncoder) Close() {
	if e.enc != nil {
		C.oracle_celtenc_destroy(e.enc)
		e.enc = nil
	}
}

// cCELTDecoder is a raw libopus CELT decoder driven in lockstep with the Go one.
type cCELTDecoder struct {
	dec      unsafe.Pointer
	channels int
}

// newCCELTDecoder creates a raw CELT decoder at 48 kHz with the given channel
// count. start=0, end=effEBands by default (matching the Go NewDecoder).
func newCCELTDecoder(channels int) (*cCELTDecoder, error) {
	p := C.oracle_celtdec_create(C.int(channels))
	if p == nil {
		return nil, fmt.Errorf("oracle_celtdec_create(channels=%d) failed", channels)
	}
	return &cCELTDecoder{dec: p, channels: channels}, nil
}

// Decode decodes one packet into interleaved int16 PCM and returns the decoded
// samples (samplesPerChannel*channels).
func (d *cCELTDecoder) Decode(packet []byte, frameSize int) ([]int16, error) {
	out := make([]int16, frameSize*d.channels)
	var dataPtr *C.uchar
	if len(packet) > 0 {
		dataPtr = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	n := C.oracle_celtdec_decode(d.dec, dataPtr, C.int(len(packet)),
		C.int(frameSize), (*C.int16_t)(unsafe.Pointer(&out[0])))
	if n < 0 {
		return nil, fmt.Errorf("celt_decode_with_ec returned %d", int(n))
	}
	return out[:int(n)*d.channels], nil
}

// State dumps the C decoder's persistent cross-frame state into a celt.State so
// the test can hash/compare it against the Go decoder's state with the SAME
// celt.State.Hash function (no cross-language serialization risk).
func (d *cCELTDecoder) State() celt.State {
	nb := cCELTNbEBands()
	dmLen := cCELTDecodeMemLen(d.channels)
	lpcLen := cCELTLpcLen(d.channels)

	scalars := make([]int32, 16)
	decodeMem := make([]int32, dmLen)
	oldE := make([]int32, 2*nb)
	oldLogE := make([]int32, 2*nb)
	oldLogE2 := make([]int32, 2*nb)
	background := make([]int32, 2*nb)
	lpc := make([]int16, lpcLen)

	C.oracle_celtdec_dump(d.dec, C.int(d.channels),
		(*C.int32_t)(unsafe.Pointer(&scalars[0])),
		(*C.int32_t)(unsafe.Pointer(&decodeMem[0])),
		(*C.int32_t)(unsafe.Pointer(&oldE[0])),
		(*C.int32_t)(unsafe.Pointer(&oldLogE[0])),
		(*C.int32_t)(unsafe.Pointer(&oldLogE2[0])),
		(*C.int32_t)(unsafe.Pointer(&background[0])),
		(*C.int16_t)(unsafe.Pointer(&lpc[0])))

	return celt.State{
		Rng:                 uint32(scalars[0]),
		PostfilterPeriod:    int(scalars[1]),
		PostfilterPeriodOld: int(scalars[2]),
		PostfilterGain:      int16(scalars[3]),
		PostfilterGainOld:   int16(scalars[4]),
		PostfilterTapset:    int(scalars[5]),
		PostfilterTapsetOld: int(scalars[6]),
		PrefilterAndFold:    int(scalars[7]),
		LossDuration:        int(scalars[8]),
		PlcDuration:         int(scalars[9]),
		SkipPlc:             int(scalars[10]),
		LastFrameType:       int(scalars[11]),
		LastPitchIndex:      int(scalars[12]),
		PreemphMemD:         [2]int32{scalars[13], scalars[14]},
		DecodeMem:           decodeMem,
		OldEBands:           oldE,
		OldLogE:             oldLogE,
		OldLogE2:            oldLogE2,
		BackgroundLogE:      background,
		Lpc:                 lpc,
	}
}

func (d *cCELTDecoder) Close() {
	if d.dec != nil {
		C.oracle_celtdec_destroy(d.dec)
		d.dec = nil
	}
}
