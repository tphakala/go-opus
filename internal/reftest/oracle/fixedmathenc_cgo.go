//go:build refc

package oracle

/*
#include "fixedmathenc_shim.h"
*/
import "C"

// Encoder-only additions to the CELT fixed-point math oracle surface (the
// decoder subset lives in fixedmath_cgo.go). Exposes the pinned libopus
// celt_sqrt32 / celt_rcp_norm32 as plain Go functions so fixedmathenc_test.go
// can compare the internal/fixedmath port without importing "C" itself. Names
// mirror the C function they wrap, prefixed with a lowercase c.

func cCeltSqrt32(x int32) int32    { return int32(C.oracle_fme_celt_sqrt32(C.int32_t(x))) }
func cCeltRcpNorm32(x int32) int32 { return int32(C.oracle_fme_celt_rcp_norm32(C.int32_t(x))) }
