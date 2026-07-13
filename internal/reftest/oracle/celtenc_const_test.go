//go:build refc

package oracle

// Differential test for the float-literal-sensitive constants.
//
// C's QCONST32(x,bits) evaluates (x)*(1<<bits) in the precision of the literal x. A
// literal with an "f" suffix is a float, so it is rounded to float32 BEFORE the shift;
// an unsuffixed literal is a double. The results differ by a few ULPs, for example
// QCONST32(.99f,29) is 531502208 while the float64 evaluation gives 531502203.
//
// That gap is not cosmetic. These constants are compared directly against toneishness
// and lpc[0], so an input landing inside the gap takes a different branch in Go than in
// C, and every byte of the packet after that point diverges. The window is only a few
// values wide out of 2^29, so a randomized differential test can run for a long time
// without ever hitting it: two such divergences were found by adversarial review, not by
// the sweeps.
//
// This test removes the guesswork by asserting each Go constant against the constant the
// C compiler actually produces from the literal at that call site.

import (
	"testing"

	"github.com/tphakala/go-opus/internal/celt"
)

func TestFloatLiteralConstsMatchC(t *testing.T) {
	want := map[string]int32{
		"QCONST32(.98f,29)":       cConstQ29_098f(),
		"QCONST32(.99f,29)":       cConstQ29_099f(),
		"QCONST32(1.999999f,29)":  cConstQ29_1_999999f(),
		"QCONST32(3.999999,29)":   cConstQ29_3_999999d(), // DOUBLE literal, no "f"
		"QCONST32(.70710678f,31)": cConstQ31Sqrt2Inv(),
		"GCONST(31.9f)":           cConstGconst31_9(),
		"GCONST(.0062f)":          cConstGconst0_0062(),
	}

	got := celt.FloatLiteralConsts()
	if len(got) != len(want) {
		t.Fatalf("constant count: go has %d, oracle pins %d", len(got), len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Fatalf("%s: missing from celt.FloatLiteralConsts", name)
		}
		if g != w {
			t.Errorf("%s: go=%d c=%d (delta %d). The Go side used the wrong helper: an "+
				"\"f\"-suffixed C literal needs qconst32Float (float32 rounding), an "+
				"unsuffixed one needs fixedmath.QCONST32 (float64).", name, g, w, int64(g)-int64(w))
		}
	}

	// Guard the premise: float32 and float64 evaluation must genuinely differ for at
	// least one of these, otherwise the test is vacuous and would not catch a regression.
	// (Computed through variables so Go evaluates it at run time rather than folding it
	// into an untyped constant.)
	x, shift := 0.99, int64(1)<<29
	if f64 := int32(0.5 + x*float64(shift)); cConstQ29_099f() == f64 {
		t.Fatalf("vacuous: QCONST32(.99f,29) equals its float64 evaluation (%d), so this "+
			"test cannot detect the wrong-helper bug it exists to catch", f64)
	}
}
