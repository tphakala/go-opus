package oggopus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
)

// Encoder implements io.WriteCloser, byte-for-byte the go-flac pcm.Encoder shape.
var _ io.WriteCloser = (*Encoder)(nil)

// frameDurationMS is the fixed internal frame duration. 20 ms is the opusenc
// default and the best quality/overhead balance for a file encoder; it is an
// unexported constant with no Config field, exactly as go-flac fixes its
// 4096-sample block size internally (see docs/api-design.md resolved question 2).
const frameDurationMS = 20

// sampleRate48k is the fixed Opus coding/granule rate.
const sampleRate48k = 48000

// Encoder streams interleaved little-endian int16 PCM (as []byte) to an Ogg
// Opus stream on an io.Writer, implementing io.WriteCloser. It is shaped exactly
// like go-flac's pcm.Encoder: a flat Config, NewEncoder(w, cfg), Reset(w, cfg)
// for pooling, and a one-shot EncodeInterleaved.
//
// Duration is carried in per-page granule positions written inline as the
// stream progresses, so the Encoder is correct on a plain io.Writer with no
// seeking and no length known up front. This is the structural fix for the
// go-flac non-seekable zero-duration bug and requires no io.WriteSeeker (see
// docs/api-design.md "Non-seekable sinks are correct by construction").
//
// The container framing, header emission, and granule accounting are complete.
// The PCM<->packet conversion is the codec seam (codec.go): until the phase-4
// opus encoder is wired, Write, Close, and EncodeInterleaved return
// errCodecNotWired. An Encoder is not safe for concurrent use.
type Encoder struct {
	w   io.Writer
	cfg Config

	enc frameEncoder     // nil until the codec seam is wired
	cw  *containerWriter // created once the codec pre-skip is known

	frameLen   int     // input samples per channel per frame (SampleRate/50)
	frameBytes int     // bytes in one full frame (frameLen * channels * 2)
	frame      []int16 // reusable scratch for one deinterleaved frame
	carry      []byte  // buffered PCM bytes not yet a full frame (bounded to one frame)

	srcSamples int64 // real source samples per channel consumed (input rate)
	closed     bool
}

// NewEncoder validates cfg and returns an Encoder writing to w. It does not
// require an io.WriteSeeker. Until the codec is wired, the header pages are
// emitted by the codec-present path only; the PCM methods return
// errCodecNotWired. A config error (bad sample rate or channel count) is
// returned immediately.
func NewEncoder(w io.Writer, cfg Config) (*Encoder, error) { //nolint:gocritic // Config passed by value to match the go-flac-aligned public API (docs/api-design.md)
	e := &Encoder{}
	if err := e.reset(w, &cfg); err != nil {
		return nil, err
	}
	return e, nil
}

// Reset rebinds the Encoder to a new sink w and reconfigures it with cfg so one
// Encoder can encode many independent streams without re-allocating, the pooling
// path for the BirdNET-Go "many short clips" workload. It re-validates cfg,
// discards buffered input, resets per-stream state, and (once the codec is
// wired) re-emits the OpusHead and OpusTags header pages to w.
func (e *Encoder) Reset(w io.Writer, cfg Config) error { //nolint:gocritic // Config passed by value to match the go-flac-aligned public API (docs/api-design.md)
	return e.reset(w, &cfg)
}

// reset takes cfg by pointer so the flat, slice-carrying Config is not copied
// on every pooled Reset; the public entry points still take it by value to
// match the go-flac-aligned API.
func (e *Encoder) reset(w io.Writer, cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	e.w = w
	e.cfg = *cfg
	e.cw = nil
	e.frameLen = cfg.SampleRate / (1000 / frameDurationMS)
	e.frameBytes = e.frameLen * cfg.Channels * 2
	if cap(e.frame) >= e.frameLen*cfg.Channels {
		e.frame = e.frame[:0]
	} else {
		e.frame = make([]int16, 0, e.frameLen*cfg.Channels)
	}
	e.carry = e.carry[:0]
	e.srcSamples = 0
	e.closed = false

	enc, err := newFrameEncoder(e.cfg)
	if err != nil {
		// Container is complete; the PCM path is stubbed at the codec seam. Keep
		// the Encoder usable so config validation, Reset, and the returned
		// sentinel are all observable; the PCM methods surface errCodecNotWired.
		if errors.Is(err, errCodecNotWired) {
			e.enc = nil
			return nil
		}
		return err
	}
	e.enc = enc
	return e.initStream()
}

// initStream builds the container writer and emits the header pages. It runs
// only when the codec is wired, because OpusHead's pre-skip is the encoder
// lookahead.
func (e *Encoder) initStream() error {
	head := opusHead{
		version:         opusHeadVersion,
		channels:        byte(e.cfg.Channels),
		preSkip:         uint16(e.enc.lookahead()),
		inputSampleRate: uint32(e.cfg.SampleRate),
		outputGain:      0,
		mappingFamily:   mappingFamily0,
	}
	tags := opusTags{vendor: e.cfg.vendorString(), comments: e.cfg.Comments}
	cw, err := newContainerWriter(e.w, randomSerial(), head, tags)
	if err != nil {
		return err
	}
	e.cw = cw
	e.frame = e.frame[:e.frameLen*e.cfg.Channels]
	return nil
}

// Write consumes interleaved little-endian int16 PCM. Bytes that do not yet
// complete a frame are buffered until the next Write or Close, exactly like
// go-flac's block buffering; the carry stays bounded to a single frame. It
// returns the number of bytes consumed from p (io.Writer contract).
func (e *Encoder) Write(p []byte) (int, error) {
	if e.closed {
		return 0, ErrClosed
	}
	if e.enc == nil {
		return 0, errCodecNotWired
	}
	return e.write(p)
}

func (e *Encoder) write(p []byte) (int, error) {
	total := len(p)
	// Top off the carry to a full frame from the front of p.
	if len(e.carry) > 0 {
		need := e.frameBytes - len(e.carry)
		take := min(need, len(p))
		e.carry = append(e.carry, p[:take]...)
		p = p[take:]
		if len(e.carry) == e.frameBytes {
			if err := e.encodeFrameBytes(e.carry, e.frameLen); err != nil {
				return total - len(p), err
			}
			e.carry = e.carry[:0]
		}
	}
	// Encode whole frames straight from p without copying into the carry.
	for len(p) >= e.frameBytes {
		if err := e.encodeFrameBytes(p[:e.frameBytes], e.frameLen); err != nil {
			return total - len(p), err
		}
		p = p[e.frameBytes:]
	}
	// Hold the sub-frame remainder for next time.
	e.carry = append(e.carry, p...)
	return total, nil
}

// encodeFrameBytes deinterleaves one full frame of PCM bytes, encodes it through
// the codec seam, and queues the packet. realSamples is the number of genuine
// (non-padding) source samples per channel in the frame, used for end-trim.
func (e *Encoder) encodeFrameBytes(chunk []byte, realSamples int) error {
	n := e.frameLen * e.cfg.Channels
	for i := range n {
		e.frame[i] = int16(binary.LittleEndian.Uint16(chunk[2*i : 2*i+2]))
	}
	pkt, samples48k, err := e.enc.encodeFrame(e.frame[:n])
	if err != nil {
		return err
	}
	e.srcSamples += int64(realSamples)
	return e.cw.writePacket(pkt, samples48k)
}

// Close encodes the final partial frame (zero-padded), writes the last page with
// an end-trimmed granule position, and flushes. The stream duration is then
// exact. Close is idempotent. It errors if buffered trailing bytes are not a
// whole number of samples.
func (e *Encoder) Close() error {
	if e.closed {
		return nil
	}
	if e.enc == nil {
		return errCodecNotWired
	}
	if len(e.carry) > 0 {
		bytesPerSample := e.cfg.Channels * 2
		if len(e.carry)%bytesPerSample != 0 {
			return fmt.Errorf("oggopus: %d trailing bytes are not a whole number of samples", len(e.carry))
		}
		realSamples := len(e.carry) / bytesPerSample
		padded := make([]byte, e.frameBytes)
		copy(padded, e.carry)
		if err := e.encodeFrameBytes(padded, realSamples); err != nil {
			return err
		}
		e.carry = e.carry[:0]
	}
	e.closed = true
	e.enc.close()
	// Convert real source samples per channel from the input rate to 48 kHz for
	// the granule end-trim. Every valid rate divides 48000 exactly.
	src48k := e.srcSamples * sampleRate48k / int64(e.cfg.SampleRate)
	return e.cw.close(src48k)
}

// encoderPool backs EncodeInterleaved so repeated same-shape calls are
// allocation-light, mirroring go-flac pcm.EncodeInterleaved.
var encoderPool = sync.Pool{New: func() any { return &Encoder{} }}

// EncodeInterleaved encodes a complete interleaved little-endian int16 PCM
// buffer to an Ogg Opus stream on w in a single call, mirroring go-flac
// pcm.EncodeInterleaved. It draws an Encoder from an internal sync.Pool, so
// repeated same-shape calls are allocation-light, and it is safe for concurrent
// use. pcm must hold a whole number of samples for cfg.
func EncodeInterleaved(w io.Writer, cfg Config, pcm []byte) error { //nolint:gocritic // Config passed by value to match the go-flac-aligned public API (docs/api-design.md)
	if err := cfg.validate(); err != nil {
		return err
	}
	bytesPerSample := cfg.Channels * 2
	if len(pcm)%bytesPerSample != 0 {
		return fmt.Errorf("%w: pcm length %d is not a whole number of %d-channel samples", ErrInvalidConfig, len(pcm), cfg.Channels)
	}
	e, _ := encoderPool.Get().(*Encoder)
	defer encoderPool.Put(e)
	if err := e.reset(w, &cfg); err != nil {
		return err
	}
	if _, err := e.Write(pcm); err != nil {
		return err
	}
	return e.Close()
}

// randomSerial returns a random Ogg logical bitstream serial number. A fresh
// serial per stream keeps distinct streams distinguishable if concatenated.
func randomSerial() uint32 {
	return rand.Uint32()
}
