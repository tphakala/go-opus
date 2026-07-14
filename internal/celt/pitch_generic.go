//go:build !arm64 && !amd64

// Portable fallback: on architectures with no hand-written kernel, celtInnerProd
// and xcorrKernel *are* the scalar reference in pitch_ref.go. arm64 and amd64
// override them with assembly (pitch_arm64.go, pitch_amd64.go).

package celt

// celtInnerProd is celt_inner_prod_c (pitch.h:159): sum_i x[xOff+i]*y[yOff+i].
func celtInnerProd(x []int16, xOff int, y []int16, yOff, N int) int32 {
	return celtInnerProdGeneric(x, xOff, y, yOff, N)
}

// xcorrKernel is xcorr_kernel_c (pitch.h:65): sum[k] += sum_j x[j]*y[k+j] for the
// four lags k in [0,4). Preconditions: len(x) >= len_, len(y) >= len_+3.
func xcorrKernel(x, y []int16, sum *[4]int32, len_ int) {
	xcorrKernelGeneric(x, y, sum, len_)
}
