//go:build arm64

#include "textflag.h"

// NEON implementations of celt_inner_prod and xcorr_kernel, following libopus's
// own hand-written SIMD for these exact kernels (celt/arm/pitch_neon_intr.c and
// celt/x86/pitch_sse4_1.c's xcorr_kernel_sse4_1, which is the same dataflow).
//
// NEON is mandatory in the ARMv8-A baseline, so every arm64 Go target has these
// instructions and no runtime feature detection is needed.
//
// ---------------------------------------------------------------------------
// Raw WORD encodings: Go's arm64 assembler has NO integer vector multiply.
// ---------------------------------------------------------------------------
// The only vector multiplies it knows are VFMLA (floating point) and VPMULL
// (polynomial); SMLAL, SMULL and even plain VMUL are rejected with
// "unrecognized instruction". The widening signed multiply-accumulate this code
// is built on therefore has to be emitted as raw words.
//
// SMLAL/SMLAL2 (vector, signed multiply-accumulate long) encode as:
//
//	 31 30 29 28..24 23 22 21 20..16 15..12 11 10  9..5  4..0
//	  0  Q  0  01110  size   1   Rm    1000   0  0   Rn    Rd
//
// with size=01 for 16-bit source elements (4H/8H -> 4S). Q=0 selects SMLAL,
// which widens the *low* four halfwords; Q=1 selects SMLAL2, which widens the
// *high* four. That gives the two base words
//
//	SMLAL  Vd.4S, Vn.4H, Vm.4H  =  0x0E608000 | Rm<<16 | Rn<<5 | Rd
//	SMLAL2 Vd.4S, Vn.8H, Vm.8H  =  0x4E608000 | Rm<<16 | Rn<<5 | Rd
//
// Every WORD below was cross-checked by assembling the mnemonic with clang
// (clang -c -target arm64-apple-macos) and diffing the bytes, and the whole
// kernel is then checked numerically against the scalar reference for every
// length 0..600 and for adversarial INT16_MIN / wraparound inputs by
// pitch_simd_test.go. Do not hand-edit a WORD without redoing both.
//
// ---------------------------------------------------------------------------
// Why the vectorization is bit-exact
// ---------------------------------------------------------------------------
// SMLAL widens int16 x int16 into an exact int32 product (|product| <= 2^30, so
// the multiply cannot overflow, not even for the -32768 * -32768 = 2^30 corner)
// and accumulates it into the int32 lane with plain wrapping (non-saturating)
// addition -- the same arithmetic as Go's MAC16_16. VADD and VADDV likewise wrap
// rather than saturate. Wrapping int32 addition is associative and commutative,
// so splitting the sum across lanes and reducing them in any order reproduces
// the scalar result bit for bit, including when it overflows. See pitch_ref.go.

// func innerProdNEON(x, y *int16, n int) int32
//
// Returns sum_i x[i]*y[i] over n samples, accumulated in a wrapping int32.
// Four independent int32x4 accumulators (V16..V19) keep four SMLAL dependency
// chains in flight; they are summed at the end, which is exact (see above).
TEXT ·innerProdNEON(SB), NOSPLIT|NOFRAME, $0-28
	MOVD x+0(FP), R0
	MOVD y+8(FP), R1
	MOVD n+16(FP), R2

	// Zero the four accumulators. VMOVI cannot take an .S4 arrangement in Go's
	// assembler, so XOR them with themselves instead.
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	VEOR V18.B16, V18.B16, V18.B16
	VEOR V19.B16, V19.B16, V19.B16

	// 16 samples per iteration: two 8x int16 vectors from each of x and y.
loop16:
	CMP    $16, R2
	BLT    loop8
	VLD1.P 32(R0), [V0.H8, V1.H8]
	VLD1.P 32(R1), [V2.H8, V3.H8]
	WORD   $0x0E628010 // SMLAL  V16.4S, V0.4H, V2.4H
	WORD   $0x4E628011 // SMLAL2 V17.4S, V0.8H, V2.8H
	WORD   $0x0E638032 // SMLAL  V18.4S, V1.4H, V3.4H
	WORD   $0x4E638033 // SMLAL2 V19.4S, V1.8H, V3.8H
	SUB    $16, R2
	B      loop16

	// One 8-sample block, then one 4-sample block; each runs at most once.
loop8:
	CMP    $8, R2
	BLT    loop4
	VLD1.P 16(R0), [V0.H8]
	VLD1.P 16(R1), [V2.H8]
	WORD   $0x0E628010 // SMLAL  V16.4S, V0.4H, V2.4H
	WORD   $0x4E628011 // SMLAL2 V17.4S, V0.8H, V2.8H
	SUB    $8, R2

loop4:
	CMP    $4, R2
	BLT    reduce
	VLD1.P 8(R0), [V0.H4]
	VLD1.P 8(R1), [V2.H4]
	WORD   $0x0E628010 // SMLAL  V16.4S, V0.4H, V2.4H
	SUB    $4, R2

	// Fold the four accumulators together, then sum the four lanes across.
	// Every add here wraps in int32, so the grouping is irrelevant.
reduce:
	VADD  V17.S4, V16.S4, V16.S4
	VADD  V19.S4, V18.S4, V18.S4
	VADD  V18.S4, V16.S4, V16.S4
	VADDV V16.S4, V6
	VMOV  V6.S[0], R7

	// Scalar epilogue for the remaining 0..3 samples. MOVH sign-extends the
	// int16 loads, and MADDW does a 32-bit wrapping multiply-accumulate, so this
	// is exactly MAC16_16.
tail:
	CBZ    R2, done
	MOVH.P 2(R0), R4
	MOVH.P 2(R1), R5
	MADDW  R5, R7, R4, R7
	SUB    $1, R2
	B      tail

done:
	MOVW R7, ret+24(FP)
	RET

// func xcorrKernelNEON(x, y *int16, sum *[4]int32, n int)
//
// sum[k] += sum_j x[j]*y[j+k] for the four lags k in [0,4).
//
// This is the dataflow of xcorr_kernel_sse4_1: load eight x samples once, and
// multiply-accumulate them against four *overlapping* unaligned windows of y
// (y+0, y+1, y+2, y+3), one per lag. The four lag accumulators are completely
// independent -- nothing ever crosses between them -- so unlike a plain inner
// product this kernel does not even need cross-lane reassociation to be exact,
// only the final per-lag horizontal reduction.
//
// Requires len(x) >= n and len(y) >= n+3: the widest read is the lag-3 window,
// which reaches y[n+2], exactly as in the C pointer walk.
TEXT ·xcorrKernelNEON(SB), NOSPLIT|NOFRAME, $0-32
	MOVD x+0(FP), R0
	MOVD y+8(FP), R1
	MOVD sum+16(FP), R2
	MOVD n+24(FP), R3

	// Four y cursors, one per lag, each two bytes (one int16) apart.
	ADD $2, R1, R4  // y+1
	ADD $4, R1, R5  // y+2
	ADD $6, R1, R6  // y+3

	// Eight accumulators: V16..V19 take the low halves of each lag, V20..V23 the
	// high halves. Two chains per lag keeps the SMLAL latency covered.
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	VEOR V18.B16, V18.B16, V18.B16
	VEOR V19.B16, V19.B16, V19.B16
	VEOR V20.B16, V20.B16, V20.B16
	VEOR V21.B16, V21.B16, V21.B16
	VEOR V22.B16, V22.B16, V22.B16
	VEOR V23.B16, V23.B16, V23.B16

	// Eight x samples against four y windows: 32 multiply-accumulates in eight
	// instructions.
loop8:
	CMP    $8, R3
	BLT    reduce
	VLD1.P 16(R0), [V0.H8] // x[j..j+7]
	VLD1.P 16(R1), [V1.H8] // y[j+0..j+7]
	VLD1.P 16(R4), [V2.H8] // y[j+1..j+8]
	VLD1.P 16(R5), [V3.H8] // y[j+2..j+9]
	VLD1.P 16(R6), [V4.H8] // y[j+3..j+10]
	WORD   $0x0E618010     // SMLAL  V16.4S, V0.4H, V1.4H   lag 0, low
	WORD   $0x4E618014     // SMLAL2 V20.4S, V0.8H, V1.8H   lag 0, high
	WORD   $0x0E628011     // SMLAL  V17.4S, V0.4H, V2.4H   lag 1, low
	WORD   $0x4E628015     // SMLAL2 V21.4S, V0.8H, V2.8H   lag 1, high
	WORD   $0x0E638012     // SMLAL  V18.4S, V0.4H, V3.4H   lag 2, low
	WORD   $0x4E638016     // SMLAL2 V22.4S, V0.8H, V3.8H   lag 2, high
	WORD   $0x0E648013     // SMLAL  V19.4S, V0.4H, V4.4H   lag 3, low
	WORD   $0x4E648017     // SMLAL2 V23.4S, V0.8H, V4.8H   lag 3, high
	SUB    $8, R3
	B      loop8

	// Per-lag horizontal reduction into R7..R10. Lags never mix.
reduce:
	VADD  V20.S4, V16.S4, V16.S4
	VADD  V21.S4, V17.S4, V17.S4
	VADD  V22.S4, V18.S4, V18.S4
	VADD  V23.S4, V19.S4, V19.S4
	VADDV V16.S4, V0
	VADDV V17.S4, V1
	VADDV V18.S4, V2
	VADDV V19.S4, V3
	VMOV  V0.S[0], R7
	VMOV  V1.S[0], R8
	VMOV  V2.S[0], R9
	VMOV  V3.S[0], R10

	// Scalar epilogue for the remaining 0..7 samples. R1 is the lag-0 y cursor,
	// so y[j+k] is at offset 2*k from it.
tail:
	CBZ    R3, store
	MOVH.P 2(R0), R11 // x[j]
	MOVH   (R1), R12
	MADDW  R12, R7, R11, R7
	MOVH   2(R1), R12
	MADDW  R12, R8, R11, R8
	MOVH   4(R1), R12
	MADDW  R12, R9, R11, R9
	MOVH   6(R1), R12
	MADDW  R12, R10, R11, R10
	ADD    $2, R1
	SUB    $1, R3
	B      tail

	// sum[k] += acc[k]  (the kernel accumulates into sum, it does not overwrite).
store:
	MOVW (R2), R11
	ADDW R7, R11
	MOVW R11, (R2)
	MOVW 4(R2), R11
	ADDW R8, R11
	MOVW R11, 4(R2)
	MOVW 8(R2), R11
	ADDW R9, R11
	MOVW R11, 8(R2)
	MOVW 12(R2), R11
	ADDW R10, R11
	MOVW R11, 12(R2)
	RET
