//go:build !dispatchcount

package celt

// Dispatch observation, default build. The observe* calls the SIMD dispatchers
// make (comb_simd.go, haar1_simd.go, bandgain_simd.go, pitch_simd.go) compile to
// these empty functions, which the compiler inlines to nothing, so a normal build
// carries no counter and no cost (the comb zero-alloc guarantee and the tuned
// hot-path codegen are unaffected). The counting twins in dispatch_count.go take
// over under -tags dispatchcount, where dispatch_count_test.go asserts each
// dispatcher actually reached its vector kernel.
//
// The gap this closes: every SIMD differential suite diffs a dispatcher against a
// scalar reference that is ALSO the dispatcher's fallback, so a green suite cannot
// tell "the vector kernel produced the right answer" from "the vector kernel never
// ran" (issue #72). For the dispatchers with a real fallback BRANCH (comb, haar1)
// the counter sits on the vector side of it, so a threshold retune that silently
// demoted every call into the scalar fallback drops the count to zero and turns
// the -tags dispatchcount test red. For the branch-free wrappers (band-gain,
// pitch) the counter proves only that the dispatcher is exercised; see the header
// of dispatch_count_test.go for exactly what each kind of counter does and does
// not guarantee.
func observeCombDispatch()      {}
func observeHaar1Dispatch()     {}
func observeBandGainDispatch()  {}
func observeInnerProdDispatch() {}
func observeXcorrDispatch()     {}
