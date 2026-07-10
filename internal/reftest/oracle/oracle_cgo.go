//go:build refc

// Package oracle wraps the pinned libopus (../libopus, tag v1.6.1) built as the
// FIXED_POINT + DISABLE_FLOAT_API differential-test oracle and exposes a small,
// pure-Go-typed surface: a CELT-only forced-mode encoder, a decoder, the final
// range on both (the primary differential check), a build-config probe, and the
// per-frame persistent-state hash tap.
//
// Everything here is behind the `refc` build tag; the normal `go build ./...`
// never compiles cgo or touches libopus. The C sources are compiled directly by
// cgo via the per-source wrapper files (w_*.c, see gen_wrappers.sh); no autotools
// or prebuilt library is required, only a C compiler with CGO_ENABLED=1.
package oracle

/*
#cgo CFLAGS: -O2 -DFIXED_POINT -DDISABLE_FLOAT_API -DOPUS_BUILD -DHAVE_STDINT_H -DVAR_ARRAYS
#cgo CFLAGS: -I${SRCDIR}/.. -I${SRCDIR}/../libopus -I${SRCDIR}/../libopus/include
#cgo CFLAGS: -I${SRCDIR}/../libopus/celt -I${SRCDIR}/../libopus/silk
#cgo CFLAGS: -I${SRCDIR}/../libopus/silk/fixed -I${SRCDIR}/../libopus/src
#cgo LDFLAGS: -lm

#include "shim.h"
#include "opus.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// BuildConfig reports the compile-time configuration of the oracle libopus.
type BuildConfig struct {
	FixedPoint      bool // FIXED_POINT
	DisableFloatAPI bool // DISABLE_FLOAT_API
	FastInt64       bool // celt/arch.h OPUS_FAST_INT64
	CustomModes     bool // CUSTOM_MODES (must be false)
	EnableQEXT      bool // ENABLE_QEXT (must be false)
	ArchInt64       bool // sizeof(opus_int64) == 8
}

// GetBuildConfig returns the oracle's frozen build configuration. shim.c also
// asserts FIXED_POINT/DISABLE_FLOAT_API/OPUS_FAST_INT64 at compile time, so a
// mis-flagged oracle fails to build rather than reaching this call.
func GetBuildConfig() BuildConfig {
	var c C.oracle_build_config
	C.oracle_get_build_config(&c)
	return BuildConfig{
		FixedPoint:      c.fixed_point != 0,
		DisableFloatAPI: c.disable_float_api != 0,
		FastInt64:       c.fast_int64 != 0,
		CustomModes:     c.custom_modes != 0,
		EnableQEXT:      c.enable_qext != 0,
		ArchInt64:       c.arch_int64 != 0,
	}
}

// VersionString returns libopus's own version string (e.g. "libopus 1.6.1").
func VersionString() string {
	return C.GoString(C.oracle_version_string())
}

// errString turns an opus error code into a Go error, or nil for OPUS_OK.
func errString(code C.int) error {
	if code == C.OPUS_OK {
		return nil
	}
	return fmt.Errorf("opus error %d: %s", int(code), C.GoString(C.opus_strerror(code)))
}

// Encoder is a CELT-only forced-mode, OPUS_APPLICATION_AUDIO oracle encoder.
type Encoder struct {
	enc      *C.OpusEncoder
	channels int
}

// NewCELTEncoder creates an encoder forced to MODE_CELT_ONLY at the given sample
// rate (48000 for the phase-4 oracle), channel count, bitrate (bits/s) and
// complexity (0-10). Close it when done.
func NewCELTEncoder(sampleRate, channels, bitrate, complexity int) (*Encoder, error) {
	var cerr C.int
	enc := C.oracle_encoder_create_celt(C.opus_int32(sampleRate), C.int(channels),
		C.opus_int32(bitrate), C.int(complexity), &cerr)
	if enc == nil {
		return nil, fmt.Errorf("create CELT encoder: %w", errString(cerr))
	}
	return &Encoder{enc: enc, channels: channels}, nil
}

// Encode encodes one frame of interleaved int16 PCM (frameSize samples per
// channel) and returns the packet bytes. frameSize must be a valid Opus frame
// size for the sample rate (e.g. 960 for a 20 ms frame at 48 kHz).
func (e *Encoder) Encode(pcm []int16, frameSize int) ([]byte, error) {
	if len(pcm) < frameSize*e.channels {
		return nil, fmt.Errorf("pcm has %d samples, need %d (frameSize %d * %d channels)",
			len(pcm), frameSize*e.channels, frameSize, e.channels)
	}
	// Opus caps a single frame at 1275 bytes; give the encoder generous room.
	const maxDataBytes = 4000
	buf := make([]byte, maxDataBytes)
	n := C.oracle_encode(e.enc,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(frameSize),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.int(maxDataBytes))
	if n < 0 {
		return nil, fmt.Errorf("encode: %w", errString(C.int(n)))
	}
	return buf[:int(n)], nil
}

// FinalRange returns the encoder's range coder state after the last frame
// (OPUS_GET_FINAL_RANGE). Bit-exact agreement of this value is the primary
// encoder differential check.
func (e *Encoder) FinalRange() uint32 {
	return uint32(C.oracle_encoder_final_range(e.enc))
}

// StateHash returns the per-frame persistent-state hash of the encoder (see the
// oracle_encoder_state_hash doc in shim.h). Phase-0 stub: FNV-1a over the whole
// encoder allocation; stable frame-to-frame within one run, not across runs.
func (e *Encoder) StateHash() uint64 {
	return uint64(C.oracle_encoder_state_hash(e.enc, C.int(e.channels)))
}

// Close frees the encoder. Safe to call once; the pointer is nilled.
func (e *Encoder) Close() {
	if e.enc != nil {
		C.oracle_encoder_destroy(e.enc)
		e.enc = nil
	}
}

// Decoder is an oracle Opus decoder.
type Decoder struct {
	dec      *C.OpusDecoder
	channels int
}

// NewDecoder creates a decoder at the given sample rate and channel count.
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	var cerr C.int
	dec := C.oracle_decoder_create(C.opus_int32(sampleRate), C.int(channels), &cerr)
	if dec == nil {
		return nil, fmt.Errorf("create decoder: %w", errString(cerr))
	}
	return &Decoder{dec: dec, channels: channels}, nil
}

// Decode decodes one packet into interleaved int16 PCM. frameSize is the maximum
// number of samples per channel the output buffer can hold. Returns the decoded
// samples (per channel). Pass an empty packet with decodeFEC=false to trigger PLC.
func (d *Decoder) Decode(packet []byte, frameSize int, decodeFEC bool) ([]int16, error) {
	out := make([]int16, frameSize*d.channels)
	var dataPtr *C.uchar
	if len(packet) > 0 {
		dataPtr = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	fec := C.int(0)
	if decodeFEC {
		fec = 1
	}
	n := C.oracle_decode(d.dec, dataPtr, C.int(len(packet)),
		(*C.opus_int16)(unsafe.Pointer(&out[0])), C.int(frameSize), fec)
	if n < 0 {
		return nil, fmt.Errorf("decode: %w", errString(C.int(n)))
	}
	return out[:int(n)*d.channels], nil
}

// FinalRange returns the decoder's range coder state after the last packet
// (OPUS_GET_FINAL_RANGE). It must match the encoder's FinalRange for a matching
// packet, which is the cross-check that pins encoder and decoder together.
func (d *Decoder) FinalRange() uint32 {
	return uint32(C.oracle_decoder_final_range(d.dec))
}

// Close frees the decoder. Safe to call once; the pointer is nilled.
func (d *Decoder) Close() {
	if d.dec != nil {
		C.oracle_decoder_destroy(d.dec)
		d.dec = nil
	}
}
