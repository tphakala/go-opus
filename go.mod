module github.com/tphakala/go-opus

go 1.27

require (
	// Ruleguard DSL backs the custom gocritic ruleguard matchers in rules/*.go.
	// The rule files carry the `ruleguard` build tag, so the normal toolchain
	// ignores them; tools/tools.go anchors the dependency for `go mod tidy`.
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	github.com/tphakala/simd v1.9.0
)

require golang.org/x/sys v0.47.0 // indirect
