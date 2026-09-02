//go:build dispatchcount

package celt

import "sync/atomic"

// Dispatch observation, counting build (-tags dispatchcount). The twin of
// dispatch_count_off.go: each observe* call increments a package counter instead
// of doing nothing, so dispatch_count_test.go can assert that a shape which must
// take the vector path actually reached the library kernel, and that a shape which
// must take the in-tree scalar fallback did not. See dispatch_count_off.go for why
// the differential suites cannot observe this on their own (issue #72).
//
// The counters are atomic because the pitch differential tests run t.Parallel();
// the dispatch tests read them serially (before the parallel batch resumes) and
// use a before/after delta, but atomics keep the increments race-free under -race
// regardless of which tests run alongside.
var (
	combDispatches      atomic.Uint64
	haar1Dispatches     atomic.Uint64
	bandGainDispatches  atomic.Uint64
	innerProdDispatches atomic.Uint64
	xcorrDispatches     atomic.Uint64
)

func observeCombDispatch()      { combDispatches.Add(1) }
func observeHaar1Dispatch()     { haar1Dispatches.Add(1) }
func observeBandGainDispatch()  { bandGainDispatches.Add(1) }
func observeInnerProdDispatch() { innerProdDispatches.Add(1) }
func observeXcorrDispatch()     { xcorrDispatches.Add(1) }
