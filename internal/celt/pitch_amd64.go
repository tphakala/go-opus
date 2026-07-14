//go:build amd64

// amd64 dispatch for the two hot pitch kernels. PMADDWD is SSE2 and Go's
// GOAMD64=v1 baseline already requires SSE2, so the assembly in pitch_amd64.s is
// unconditionally available on every amd64 Go target: no CPU feature detection
// and no runtime dispatch.
//
// The scalar reference these are proven equal to lives in pitch_ref.go.

package celt

// innerProdSSE2 returns sum_i x[i]*y[i] over n samples (wrapping int32).
// n may be 0. Reads exactly n int16 from each of x and y.
//
//go:noescape
func innerProdSSE2(x, y *int16, n int) int32

// xcorrKernelSSE2 adds sum_j x[j]*y[j+k] into sum[k] for the four lags k in
// [0,4). Reads n int16 from x and n+3 from y.
//
//go:noescape
func xcorrKernelSSE2(x, y *int16, sum *[4]int32, n int)

// celtInnerProd is celt_inner_prod_c (pitch.h:159): sum_i x[xOff+i]*y[yOff+i].
func celtInnerProd(x []int16, xOff int, y []int16, yOff, N int) int32 {
	if N <= 0 {
		return 0
	}
	// Bounds-check the full extent the assembly will read, so a caller that got
	// the lengths wrong panics here rather than reading past the slice.
	_ = x[xOff+N-1]
	_ = y[yOff+N-1]
	return innerProdSSE2(&x[xOff], &y[yOff], N)
}

// xcorrKernel is xcorr_kernel_c (pitch.h:65): sum[k] += sum_j x[j]*y[k+j] for the
// four lags k in [0,4). Preconditions: len(x) >= len_, len(y) >= len_+3.
func xcorrKernel(x, y []int16, sum *[4]int32, len_ int) {
	if len_ <= 0 {
		// Matches the scalar reference, which accumulates nothing for len_ <= 0.
		return
	}
	// The lag-3 window reaches y[len_+2]; bounds-check it before the assembly runs.
	_ = x[len_-1]
	_ = y[len_+2]
	xcorrKernelSSE2(&x[0], &y[0], sum, len_)
}
