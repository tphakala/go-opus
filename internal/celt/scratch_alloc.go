//go:build !poison

package celt

// alloc is the Go stand-in for ALLOC(x, n, T): it hands out an n-element window of
// a pooled buffer, growing the backing array only when a call asks for more than
// its current capacity. A Decoder's fields are presized to their bounds
// (presizeDecode in scratch.go, and SetDownsample for decDeemph), so decoding
// never reallocates; an Encoder's grow to the largest request seen.
//
// The window is NOT zeroed: see the zeroing contract in scratch.go's file comment.
// A build-tagged twin in scratch_poison.go deliberately fills it with 0x5A instead,
// so the -tags poison test build surfaces any read-before-write dependency.
func alloc[T any](p *[]T, n int) []T {
	if cap(*p) < n {
		*p = make([]T, n)
	}
	*p = (*p)[:n]
	return *p
}
