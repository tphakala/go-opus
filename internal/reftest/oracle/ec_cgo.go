//go:build refc

package oracle

/*
#include "shim.h"
*/
import "C"

import "unsafe"

// refEnc wraps a libopus ec_enc via the shim, exposing one Go method per range
// coder primitive so a test can drive it in lockstep with the pure-Go encoder.
type refEnc struct{ h *C.oracle_ec }

// refDec wraps a libopus ec_dec via the shim.
type refDec struct{ h *C.oracle_ec }

// ecState is the Go mirror of oracle_ec_state (mutable ec_ctx state minus buf),
// used to snapshot/restore the C coder around an RDO trial.
type ecState struct{ c C.oracle_ec_state }

func newRefEnc(size int) *refEnc { return &refEnc{h: C.oracle_ec_enc_create(C.int(size))} }

func (e *refEnc) encode(fl, fh, ft uint32) {
	C.oracle_ec_enc_encode(e.h, C.unsigned(fl), C.unsigned(fh), C.unsigned(ft))
}
func (e *refEnc) encodeBin(fl, fh, bits uint32) {
	C.oracle_ec_enc_encode_bin(e.h, C.unsigned(fl), C.unsigned(fh), C.unsigned(bits))
}
func (e *refEnc) bitLogp(val int, logp uint32) {
	C.oracle_ec_enc_bit_logp(e.h, C.int(val), C.unsigned(logp))
}
func (e *refEnc) icdf(s int, tbl []byte, ftb uint32) {
	C.oracle_ec_enc_icdf(e.h, C.int(s), (*C.uchar)(unsafe.Pointer(&tbl[0])), C.unsigned(ftb))
}
func (e *refEnc) encUint(fl, ft uint32) {
	C.oracle_ec_enc_uint(e.h, C.opus_uint32(fl), C.opus_uint32(ft))
}
func (e *refEnc) encBits(fl, bits uint32) {
	C.oracle_ec_enc_bits(e.h, C.opus_uint32(fl), C.unsigned(bits))
}
func (e *refEnc) patchInitialBits(val, nbits uint32) {
	C.oracle_ec_enc_patch_initial_bits(e.h, C.unsigned(val), C.unsigned(nbits))
}
func (e *refEnc) shrink(size uint32) { C.oracle_ec_enc_shrink(e.h, C.opus_uint32(size)) }
func (e *refEnc) done()              { C.oracle_ec_enc_done(e.h) }
func (e *refEnc) rangeBytes() uint32 { return uint32(C.oracle_ec_enc_range_bytes(e.h)) }
func (e *refEnc) tell() int          { return int(C.oracle_ec_tell(e.h)) }
func (e *refEnc) tellFrac() uint32   { return uint32(C.oracle_ec_tell_frac(e.h)) }
func (e *refEnc) rng() uint32        { return uint32(C.oracle_ec_get_rng(e.h)) }
func (e *refEnc) val() uint32        { return uint32(C.oracle_ec_get_val(e.h)) }
func (e *refEnc) errCode() int       { return int(C.oracle_ec_get_error(e.h)) }
func (e *refEnc) close()             { C.oracle_ec_destroy(e.h); e.h = nil }

// bytesN returns the first n bytes of the handle's storage.
func (e *refEnc) bytesN(n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	got := int(C.oracle_ec_copy_out(e.h, (*C.uchar)(unsafe.Pointer(&out[0])), C.int(n)))
	return out[:got]
}

// writeIn splices src over buf[0:len(src)] (the head-window copy).
func (e *refEnc) writeIn(src []byte) {
	if len(src) == 0 {
		return
	}
	C.oracle_ec_write_in(e.h, (*C.uchar)(unsafe.Pointer(&src[0])), C.int(len(src)))
}

// copyRegion returns a copy of buf[start:start+n]; writeRegion splices src over
// buf[start:start+len(src)]. These model the arbitrary RDO byte window.
func (e *refEnc) copyRegion(start, n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	C.oracle_ec_copy_region(e.h, (*C.uchar)(unsafe.Pointer(&out[0])), C.int(start), C.int(n))
	return out
}
func (e *refEnc) writeRegion(start int, src []byte) {
	if len(src) == 0 {
		return
	}
	C.oracle_ec_write_region(e.h, (*C.uchar)(unsafe.Pointer(&src[0])), C.int(start), C.int(len(src)))
}

func (e *refEnc) getState() ecState {
	var s ecState
	C.oracle_ec_get_state(e.h, &s.c)
	return s
}
func (e *refEnc) setState(s ecState) { C.oracle_ec_set_state(e.h, &s.c) }

// offs / endOffs expose the two range/raw-bit offsets as plain uint32 so tests
// (which cannot import "C") can size an ec_enc_shrink without touching C types.
func (e *refEnc) offs() uint32 {
	var s C.oracle_ec_state
	C.oracle_ec_get_state(e.h, &s)
	return uint32(s.offs)
}
func (e *refEnc) endOffs() uint32 {
	var s C.oracle_ec_state
	C.oracle_ec_get_state(e.h, &s)
	return uint32(s.end_offs)
}

func newRefDec(buf []byte) *refDec {
	var p *C.uchar
	if len(buf) > 0 {
		p = (*C.uchar)(unsafe.Pointer(&buf[0]))
	}
	return &refDec{h: C.oracle_ec_dec_create(p, C.int(len(buf)))}
}

func (d *refDec) decode(ft uint32) uint32 {
	return uint32(C.oracle_ec_dec_decode(d.h, C.unsigned(ft)))
}
func (d *refDec) decodeBin(bits uint32) uint32 {
	return uint32(C.oracle_ec_dec_decode_bin(d.h, C.unsigned(bits)))
}
func (d *refDec) update(fl, fh, ft uint32) {
	C.oracle_ec_dec_update(d.h, C.unsigned(fl), C.unsigned(fh), C.unsigned(ft))
}
func (d *refDec) bitLogp(logp uint32) int {
	return int(C.oracle_ec_dec_bit_logp(d.h, C.unsigned(logp)))
}
func (d *refDec) icdf(tbl []byte, ftb uint32) int {
	return int(C.oracle_ec_dec_icdf(d.h, (*C.uchar)(unsafe.Pointer(&tbl[0])), C.unsigned(ftb)))
}
func (d *refDec) decUint(ft uint32) uint32 {
	return uint32(C.oracle_ec_dec_uint(d.h, C.opus_uint32(ft)))
}
func (d *refDec) decBits(bits uint32) uint32 {
	return uint32(C.oracle_ec_dec_bits(d.h, C.unsigned(bits)))
}
func (d *refDec) tell() int        { return int(C.oracle_ec_tell(d.h)) }
func (d *refDec) tellFrac() uint32 { return uint32(C.oracle_ec_tell_frac(d.h)) }
func (d *refDec) rng() uint32      { return uint32(C.oracle_ec_get_rng(d.h)) }
func (d *refDec) errCode() int     { return int(C.oracle_ec_get_error(d.h)) }
func (d *refDec) close()           { C.oracle_ec_destroy(d.h); d.h = nil }
