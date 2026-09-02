package celt

import (
	"os"
	"testing"

	"github.com/tphakala/simd/cpu"
)

// Forced-fallback guard for the simd Go path (#38).
//
// The pitch (i16), haar1/band-gain/comb (i32), and FFT butterfly (cint) kernels
// are backed by github.com/tphakala/simd, which selects NEON/SSE2/AVX2 assembly at
// compile time on arm64/amd64 and a pure-Go fallback (dotGo/xcorrGo, scaleQ31Go,
// mulGo, ...) on every other GOARCH. go-opus's own differential suites
// (pitch_simd_test.go, haar1_simd_test.go, bandgain_simd_test.go,
// comb_simd_test.go, fft_cint_oracle_test.go) run on the amd64/arm64 CI hosts, so
// they normally exercise the assembly, never that Go fallback. On riscv64/ppc64le/s390x/
// loong64/wasm the fallback's bit-exactness would then rest only on the simd
// library's own tests, not go-opus's gate.
//
// The `forcego` CI leg closes that gap: it sets SIMD_DISABLE=all in the
// environment, which the simd cpu package honours at init() by clearing every
// detected CPU feature (cpu.applyDisable's "all" token does *f = Features{}).
// With no feature bits set, every kernel dispatch falls to its default arm, the
// exact same Go functions the exotic arches run, so the existing differential
// tests now cover the fallback on a native host.
//
// This guard exists so that leg cannot silently pass without actually forcing
// the fallback: a green run only proves coverage if the feature flags really are
// cleared. SIMD_DISABLE is read once in cpu.init(), before any package-level
// feature-var initializer, so it must be present in the environment when the
// test binary starts; os.Setenv here would be too late. The leg therefore also
// sets GOOPUS_ASSERT_SIMD_DISABLED so this assertion only fires when the fallback
// is meant to be forced, and stays a no-op in the normal (SIMD-enabled) test
// runs on developer machines and the main CI matrix.
func TestSIMDFallbackForced(t *testing.T) {
	if os.Getenv("GOOPUS_ASSERT_SIMD_DISABLED") == "" {
		t.Skip("only asserted in the forced-Go-fallback CI leg (SIMD_DISABLE=all)")
	}
	var zero cpu.Features
	if cpu.X86 != zero {
		t.Fatalf("SIMD_DISABLE=all did not clear x86 CPU features (%+v); the "+
			"forced-Go-fallback leg is exercising SIMD, not the pure-Go path", cpu.X86)
	}
	if cpu.ARM64 != zero {
		t.Fatalf("SIMD_DISABLE=all did not clear arm64 CPU features (%+v); the "+
			"forced-Go-fallback leg is exercising SIMD, not the pure-Go path", cpu.ARM64)
	}
}
