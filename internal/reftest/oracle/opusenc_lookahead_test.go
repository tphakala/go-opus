//go:build refc

package oracle

// OPUS_GET_LOOKAHEAD is invisible to the packet-level differential gate: it never
// reaches a packet byte, so a wrong value passes every bit-exact test. It is also the
// value the Ogg Opus pre-skip field is derived from, so getting it wrong misaligns
// every decoded stream. It was in fact wrong (the Fs/400 CELT overlap term was
// missing, giving 192 instead of 312 at 48 kHz), and only adversarial review caught
// it. Pin it against the C ctl.

import (
	"testing"

	"github.com/tphakala/go-opus/internal/opusenc"
)

func TestOpusencLookaheadMatchesC(t *testing.T) {
	apps := []struct {
		name string
		v    int
	}{
		{"VOIP", opusenc.ApplicationVOIP},
		{"AUDIO", opusenc.ApplicationAudio},
		{"RESTRICTED_LOWDELAY", opusenc.ApplicationRestrictedLowdelay},
	}
	for _, fs := range []int32{8000, 12000, 16000, 24000, 48000} {
		for _, ch := range []int{1, 2} {
			for _, app := range apps {
				want := cTopencLookahead(fs, ch, app.v)
				if want < 0 {
					t.Fatalf("oracle refused fs=%d ch=%d app=%s", fs, ch, app.name)
				}
				enc := opusenc.NewEncoder(fs, ch, app.v)
				if enc == nil {
					t.Fatalf("NewEncoder(%d,%d,%s) = nil", fs, ch, app.name)
				}
				if got := int32(enc.Lookahead()); got != want {
					t.Errorf("Lookahead fs=%d ch=%d app=%s: go=%d c=%d", fs, ch, app.name, got, want)
				}
			}
		}
	}

	// Vacuity guard: the two terms must actually differ, or this test could not have
	// caught the missing Fs/400 term it exists for.
	audio := cTopencLookahead(48000, 1, opusenc.ApplicationAudio)
	lowdelay := cTopencLookahead(48000, 1, opusenc.ApplicationRestrictedLowdelay)
	if audio == lowdelay {
		t.Fatalf("vacuous: AUDIO and RESTRICTED_LOWDELAY lookahead both %d, so the "+
			"delay_compensation term is not being exercised", audio)
	}
}
