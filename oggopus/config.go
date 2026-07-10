package oggopus

import "fmt"

// libVersion is the go-opus version reported in the default OpusTags vendor
// string. It is a placeholder until the codec phase unifies it with the
// opus.Version constant the raw package will export.
const libVersion = "0.1.0-dev"

// maxComplexity is the top of the Opus complexity range.
const maxComplexity = 10

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
	DTX            bool // zero value (false) is off

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
		return fmt.Errorf("%w: complexity %d (want 0..10)", ErrInvalidConfig, c.Complexity)
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
