//go:build poison

package celt

import "testing"

// TestPoisonAllocActive proves the poison build of alloc actually stamps its
// windows with 0x5A. Without this, a broken build-tag expression would silently
// select the production alloc and every "poison" run would be an ordinary green
// run that audits nothing.
func TestPoisonAllocActive(t *testing.T) {
	var p32 []int32
	for i, v := range alloc(&p32, 8) {
		if uint32(v) != 0x5A5A5A5A {
			t.Fatalf("alloc[int32][%d] = %#x, want 0x5A5A5A5A: poison fill not active", i, uint32(v))
		}
	}
	var pb []byte
	for i, v := range alloc(&pb, 8) {
		if v != 0x5A {
			t.Fatalf("alloc[byte][%d] = %#x, want 0x5A: poison fill not active", i, v)
		}
	}
	// Reuse must re-poison: a stale window from a previous call is exactly what
	// the harness exists to simulate.
	w := alloc(&p32, 4)
	for i := range w {
		w[i] = 7
	}
	for i, v := range alloc(&p32, 4) {
		if uint32(v) != 0x5A5A5A5A {
			t.Fatalf("reused alloc[int32][%d] = %#x, want re-poisoned 0x5A5A5A5A", i, uint32(v))
		}
	}
}
