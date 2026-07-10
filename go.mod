module github.com/tphakala/go-opus

go 1.26

// Ruleguard DSL backs the custom gocritic ruleguard matchers in rules/*.go.
// The rule files carry the `ruleguard` build tag, so the normal toolchain
// ignores them; tools/tools.go anchors the dependency for `go mod tidy`.
require github.com/quasilyte/go-ruleguard/dsl v0.3.23
