module github.com/tphakala/go-opus

go 1.26

// Ruleguard DSL backs the custom gocritic ruleguard matchers in rules/*.go.
// The rule files carry the `ruleguard` build tag, so the normal toolchain
// ignores them; tools/tools.go anchors the dependency for `go mod tidy`.
require github.com/quasilyte/go-ruleguard/dsl v0.3.23

require (
	github.com/tphakala/simd v1.5.0
	golang.org/x/sys v0.45.0 // indirect
)
