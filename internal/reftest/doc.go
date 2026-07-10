// Package reftest is the cgo differential-test harness that builds the pinned
// libopus (v1.6.1, FIXED_POINT + DISABLE_FLOAT_API oracle plus a float variant)
// and compares go-opus against it: encoder packets bit-exact, decoder output
// within threshold, and per-frame persistent-state hashes for sequence tests.
// The cgo code carries the `refc` build tag and is never imported by library
// code. See docs/plan.md test strategy.
package reftest
