//go:build amd64

#include "textflag.h"

// SSE2 implementations of celt_inner_prod and xcorr_kernel, transliterated from
// libopus's own SIMD for these exact kernels: celt_inner_prod_sse2
// (celt/x86/pitch_sse2.c) and xcorr_kernel_sse4_1 (celt/x86/pitch_sse4_1.c),
// whose main loop is pure SSE2 and is reused here unchanged.
//
// PMADDWD is SSE2, and Go's GOAMD64=v1 baseline already requires SSE2, so every
// amd64 Go target has it: no CPU feature detection and no runtime dispatch.
// Go's assembler spells PMADDWD as PMADDWL; it assembles natively, so unlike the
// arm64 side this file needs no raw WORD encodings.
//
// ---------------------------------------------------------------------------
// Why the vectorization is bit-exact, including the one degenerate case
// ---------------------------------------------------------------------------
// PMADDWL multiplies eight signed int16 pairs into eight exact int32 products
// (|product| <= 2^30) and adds each *adjacent* pair into one int32 lane. The sum
// of two such products is at most 2^31, so the only input that can overflow the
// lane is the all-INT16_MIN corner: 0x8000*0x8000 + 0x8000*0x8000 = 2^30 + 2^30
// = 2^31, which PMADDWD delivers as 0x80000000 -- exactly what wrapping int32
// addition produces. PADDD likewise wraps rather than saturates. So the vector
// path reproduces MAC16_16's wrapping arithmetic in every case, and because
// wrapping int32 addition is associative and commutative the lane grouping and
// the horizontal-reduction order do not matter. See pitch_ref.go.
//
// NOTE: this file is assembled and vetted in CI on amd64, and its numerical
// equivalence to the scalar reference is what pitch_simd_test.go checks when it
// runs there. It was additionally executed under Rosetta 2 during development.

// func innerProdSSE2(x, y *int16, n int) int32
//
// Returns sum_i x[i]*y[i] over n samples, accumulated in a wrapping int32.
TEXT ·innerProdSSE2(SB), NOSPLIT|NOFRAME, $0-28
	MOVQ x+0(FP), SI
	MOVQ y+8(FP), DI
	MOVQ n+16(FP), CX

	PXOR X0, X0 // accumulator 1
	PXOR X1, X1 // accumulator 2

	// 16 samples per iteration, two independent accumulator chains.
loop16:
	CMPQ    CX, $16
	JL      loop8
	MOVOU   (SI), X2
	MOVOU   16(SI), X3
	MOVOU   (DI), X4
	MOVOU   16(DI), X5
	PMADDWL X4, X2
	PMADDWL X5, X3
	PADDL   X2, X0
	PADDL   X3, X1
	ADDQ    $32, SI
	ADDQ    $32, DI
	SUBQ    $16, CX
	JMP     loop16

loop8:
	PADDL X1, X0

	// One 8-sample block; runs at most once.
	CMPQ    CX, $8
	JL      reduce
	MOVOU   (SI), X2
	MOVOU   (DI), X4
	PMADDWL X4, X2
	PADDL   X2, X0
	ADDQ    $16, SI
	ADDQ    $16, DI
	SUBQ    $8, CX

	// Horizontal sum of the four int32 lanes. Every add wraps, so the order is
	// irrelevant. PSHUFD is SSE2.
reduce:
	PSHUFD $0x0E, X0, X1 // lanes [2,3] down to [0,1]
	PADDL  X1, X0
	PSHUFD $0x01, X0, X1 // lane 1 down to lane 0
	PADDL  X1, X0
	MOVL   X0, AX

	// Scalar epilogue for the remaining 0..7 samples. MOVWLSX sign-extends the
	// int16 loads and IMULL/ADDL wrap in 32 bits, so this is exactly MAC16_16.
tail:
	TESTQ   CX, CX
	JZ      done
	MOVWLSX (SI), BX
	MOVWLSX (DI), DX
	IMULL   DX, BX
	ADDL    BX, AX
	ADDQ    $2, SI
	ADDQ    $2, DI
	SUBQ    $1, CX
	JMP     tail

done:
	MOVL AX, ret+24(FP)
	RET

// func xcorrKernelSSE2(x, y *int16, sum *[4]int32, n int)
//
// sum[k] += sum_j x[j]*y[j+k] for the four lags k in [0,4). This is the main loop
// of xcorr_kernel_sse4_1: eight x samples loaded once, multiply-accumulated
// against four overlapping unaligned y windows (y+0..y+3), one accumulator per
// lag. The four lag accumulators never mix.
//
// Requires len(x) >= n and len(y) >= n+3: the lag-3 window reaches y[n+2].
TEXT ·xcorrKernelSSE2(SB), NOSPLIT|NOFRAME, $0-32
	MOVQ x+0(FP), SI
	MOVQ y+8(FP), DI
	MOVQ sum+16(FP), DX
	MOVQ n+24(FP), CX

	PXOR X4, X4 // lag 0
	PXOR X5, X5 // lag 1
	PXOR X6, X6 // lag 2
	PXOR X7, X7 // lag 3

	// PMADDWL is symmetric in its operands, so multiplying the y window into
	// itself (dst = madd(dst, src)) keeps the shared x vector X0 intact.
loop8:
	CMPQ    CX, $8
	JL      reduce
	MOVOU   (SI), X0 // x[j..j+7]
	MOVOU   (DI), X1 // y[j+0..j+7]
	PMADDWL X0, X1
	PADDL   X1, X4
	MOVOU   2(DI), X1 // y[j+1..j+8]
	PMADDWL X0, X1
	PADDL   X1, X5
	MOVOU   4(DI), X1 // y[j+2..j+9]
	PMADDWL X0, X1
	PADDL   X1, X6
	MOVOU   6(DI), X1 // y[j+3..j+10]
	PMADDWL X0, X1
	PADDL   X1, X7
	ADDQ    $16, SI
	ADDQ    $16, DI
	SUBQ    $8, CX
	JMP     loop8

	// Per-lag horizontal reduction into AX, BX, R8, R9. Lags never mix.
reduce:
	PSHUFD $0x0E, X4, X0
	PADDL  X0, X4
	PSHUFD $0x01, X4, X0
	PADDL  X0, X4
	MOVL   X4, AX

	PSHUFD $0x0E, X5, X0
	PADDL  X0, X5
	PSHUFD $0x01, X5, X0
	PADDL  X0, X5
	MOVL   X5, BX

	PSHUFD $0x0E, X6, X0
	PADDL  X0, X6
	PSHUFD $0x01, X6, X0
	PADDL  X0, X6
	MOVL   X6, R8

	PSHUFD $0x0E, X7, X0
	PADDL  X0, X7
	PSHUFD $0x01, X7, X0
	PADDL  X0, X7
	MOVL   X7, R9

	// Scalar epilogue for the remaining 0..7 samples. DI is the lag-0 y cursor,
	// so y[j+k] is at offset 2*k from it.
tail:
	TESTQ   CX, CX
	JZ      store
	MOVWLSX (SI), R10 // x[j]
	MOVWLSX (DI), R11
	IMULL   R10, R11
	ADDL    R11, AX
	MOVWLSX 2(DI), R11
	IMULL   R10, R11
	ADDL    R11, BX
	MOVWLSX 4(DI), R11
	IMULL   R10, R11
	ADDL    R11, R8
	MOVWLSX 6(DI), R11
	IMULL   R10, R11
	ADDL    R11, R9
	ADDQ    $2, SI
	ADDQ    $2, DI
	SUBQ    $1, CX
	JMP     tail

	// sum[k] += acc[k]  (the kernel accumulates into sum, it does not overwrite).
store:
	ADDL AX, (DX)
	ADDL BX, 4(DX)
	ADDL R8, 8(DX)
	ADDL R9, 12(DX)
	RET
