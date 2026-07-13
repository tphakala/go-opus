package oggopus

import (
	"fmt"

	"github.com/tphakala/go-opus/opus"
)

// libVersion is the go-opus version reported in the default OpusTags vendor
// string. It DERIVES from opus.Version rather than restating it: the two used to be
// independent "0.1.0-dev" literals, which is a version string that can go stale in
// exactly one of the two places a consumer reads it, and nothing in a packet or a
// container byte would say so (the vendor string is free-form text, so no gate can
// tell a stale one from a correct one). Now the container cannot claim a version the
// codec does not. TestVendorStringDerivesFromOpusVersion is the guard on the
// FORMAT ("go-opus <version>"); the equality itself is enforced by the compiler.
const libVersion = opus.Version

// maxComplexity is the top of the Opus complexity range, and defaultComplexity is
// what Config.Complexity's zero value selects. The zero value MUST mean the
// default rather than an explicit complexity 0, so the useful range is 1..10 and
// 0 is the "unset" marker; the mapping itself is opus.EncoderConfig's, which owns
// it for both packages. defaultComplexity is duplicated here only to name the
// number in Config's documentation and in the validation error, and
// TestComplexityZeroMeansDefault pins the two against each other by comparing the
// packets they produce, so they cannot drift apart silently.
const (
	maxComplexity     = 10
	defaultComplexity = 10
)

// validSampleRates enumerates the sample rates Opus accepts. Anything else is
// rejected rather than clamped (unlike go-flac, which accepts a wide range).
var validSampleRates = [...]int{8000, 12000, 16000, 24000, 48000}

// Config configures an oggopus Encoder. It is a flat struct mirroring go-flac's
// pcm.Config convention: no embedding, and every field's zero value is
// documented so a literal reads cleanly. See docs/api-design.md.
type Config struct {
	SampleRate int // 8000, 12000, 16000, 24000, or 48000; required (no zero default)
	Channels   int // 1 or 2; required (no zero default)

	Bitrate        int  // bits per second; zero selects automatic
	CBR            bool // zero value (false) means VBR
	ConstrainedVBR bool // meaningful only when CBR is false
	Complexity     int  // 1..10; zero selects the library default (10)

	// DTX requests discontinuous transmission. It is NOT IMPLEMENTED: setting it
	// makes NewEncoder return opus.ErrUnsupported rather than quietly encoding
	// without it. The zero value (false) is off.
	DTX bool

	// Vendor overrides the OpusTags vendor string; zero value uses
	// "go-opus <version>".
	Vendor string
	// Comments are OpusTags user comments in "TAG=value" order; nil emits tags
	// with only the vendor string.
	Comments []string
}

// validate checks the fields that have a hard domain. It leaves the tuning
// fields (bitrate/complexity zero-defaulting, CBR/VBR, DTX) for the codec, which
// owns their semantics. The receiver is a pointer to avoid copying the flat
// Config; the public entry points still take it by value (go-flac-aligned API).
func (c *Config) validate() error {
	if !validSampleRate(c.SampleRate) {
		return fmt.Errorf("%w: sample rate %d (want 8000, 12000, 16000, 24000, or 48000)", ErrInvalidConfig, c.SampleRate)
	}
	if c.Channels != 1 && c.Channels != 2 {
		return fmt.Errorf("%w: channels %d (want 1 or 2)", ErrInvalidConfig, c.Channels)
	}
	if c.Bitrate < 0 {
		return fmt.Errorf("%w: negative bitrate %d", ErrInvalidConfig, c.Bitrate)
	}
	if c.Complexity < 0 || c.Complexity > maxComplexity {
		return fmt.Errorf("%w: complexity %d (want 1..%d, or 0 for the default %d)",
			ErrInvalidConfig, c.Complexity, maxComplexity, defaultComplexity)
	}
	return nil
}

// vendorString returns the vendor string to write into OpusTags.
func (c *Config) vendorString() string {
	if c.Vendor != "" {
		return c.Vendor
	}
	return "go-opus " + libVersion
}

func validSampleRate(r int) bool {
	for _, v := range validSampleRates {
		if r == v {
			return true
		}
	}
	return false
}
