//go:build ruleguard

// Package gorules defines custom linter rules for Go modernization.
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// WaitGroupGo detects the old sync.WaitGroup pattern and suggests using Go 1.25's wg.Go().
//
// The old pattern:
//
//	wg.Add(1)
//	go func() {
//	    defer wg.Done()
//	    doSomething()
//	}()
//
// Can be simplified to:
//
//	wg.Go(func() {
//	    doSomething()
//	})
//
// Benefits:
//   - Cleaner, less error-prone (no Add/Done mismatch)
//   - Single function call
//   - Automatic panic handling
//
// See: https://pkg.go.dev/sync#WaitGroup.Go
func WaitGroupGo(m dsl.Matcher) {
	// Pattern 1: wg.Add(1) followed by go func() with defer wg.Done()
	// This matches when the defer is the first statement
	m.Match(
		`$wg.Add(1); go func() { defer $wg.Done(); $*body }()`,
	).
		Where(m["wg"].Type.Is("*sync.WaitGroup") || m["wg"].Type.Is("sync.WaitGroup")).
		// No Suggest: the replacement embeds the goroutine body ($*body), and the
		// go-ruleguard engine bundled in golangci-lint (v0.4.5) panics rendering a
		// node slice in a message template (*gogrep.NodeSlice). The Report keeps a
		// literal "{ ... }" for the same reason.
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)")

	// Pattern 2: Same but with pointer receiver explicitly
	m.Match(
		`$wg.Add(1); go func() { defer $wg.Done(); $*body }()`,
	).
		Where(m["wg"].Type.Underlying().Is("sync.WaitGroup")).
		// No Suggest: the replacement embeds the goroutine body ($*body), and the
		// go-ruleguard engine bundled in golangci-lint (v0.4.5) panics rendering a
		// node slice in a message template (*gogrep.NodeSlice). The Report keeps a
		// literal "{ ... }" for the same reason.
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)")

	// Pattern 3: When wg is passed by reference to the closure
	m.Match(
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }($wg)`,
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }(&$wg)`,
	).
		Where(m["wg"].Type.Is("*sync.WaitGroup") || m["wg"].Type.Is("sync.WaitGroup")).
		// A literal "{ ... }" here rather than $body, for the same node-slice
		// render reason noted on patterns 1 and 2 above.
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)")
}
