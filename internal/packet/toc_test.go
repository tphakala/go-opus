package packet

import "testing"

// tocCase is one row of the RFC 6716 Table 2 configuration layout.
type tocCase struct {
	config uint8
	mode   Mode
	bw     Bandwidth
	dur    FrameDuration
}

// rfcConfigTable is RFC 6716 §3.1 Table 2: the mapping from the 5-bit config
// number to (mode, bandwidth, frame duration). It is written out in full rather
// than computed so the test is an independent oracle for toc.go.
var rfcConfigTable = []tocCase{
	{0, ModeSILKOnly, BandwidthNarrowband, FrameDuration10ms},
	{1, ModeSILKOnly, BandwidthNarrowband, FrameDuration20ms},
	{2, ModeSILKOnly, BandwidthNarrowband, FrameDuration40ms},
	{3, ModeSILKOnly, BandwidthNarrowband, FrameDuration60ms},
	{4, ModeSILKOnly, BandwidthMediumband, FrameDuration10ms},
	{5, ModeSILKOnly, BandwidthMediumband, FrameDuration20ms},
	{6, ModeSILKOnly, BandwidthMediumband, FrameDuration40ms},
	{7, ModeSILKOnly, BandwidthMediumband, FrameDuration60ms},
	{8, ModeSILKOnly, BandwidthWideband, FrameDuration10ms},
	{9, ModeSILKOnly, BandwidthWideband, FrameDuration20ms},
	{10, ModeSILKOnly, BandwidthWideband, FrameDuration40ms},
	{11, ModeSILKOnly, BandwidthWideband, FrameDuration60ms},
	{12, ModeHybrid, BandwidthSuperwideband, FrameDuration10ms},
	{13, ModeHybrid, BandwidthSuperwideband, FrameDuration20ms},
	{14, ModeHybrid, BandwidthFullband, FrameDuration10ms},
	{15, ModeHybrid, BandwidthFullband, FrameDuration20ms},
	{16, ModeCELTOnly, BandwidthNarrowband, FrameDuration2500us},
	{17, ModeCELTOnly, BandwidthNarrowband, FrameDuration5ms},
	{18, ModeCELTOnly, BandwidthNarrowband, FrameDuration10ms},
	{19, ModeCELTOnly, BandwidthNarrowband, FrameDuration20ms},
	{20, ModeCELTOnly, BandwidthWideband, FrameDuration2500us},
	{21, ModeCELTOnly, BandwidthWideband, FrameDuration5ms},
	{22, ModeCELTOnly, BandwidthWideband, FrameDuration10ms},
	{23, ModeCELTOnly, BandwidthWideband, FrameDuration20ms},
	{24, ModeCELTOnly, BandwidthSuperwideband, FrameDuration2500us},
	{25, ModeCELTOnly, BandwidthSuperwideband, FrameDuration5ms},
	{26, ModeCELTOnly, BandwidthSuperwideband, FrameDuration10ms},
	{27, ModeCELTOnly, BandwidthSuperwideband, FrameDuration20ms},
	{28, ModeCELTOnly, BandwidthFullband, FrameDuration2500us},
	{29, ModeCELTOnly, BandwidthFullband, FrameDuration5ms},
	{30, ModeCELTOnly, BandwidthFullband, FrameDuration10ms},
	{31, ModeCELTOnly, BandwidthFullband, FrameDuration20ms},
}

func TestParseTOCConfigTable(t *testing.T) {
	for _, tc := range rfcConfigTable {
		// Sweep the stereo flag and all four frame-count codes for each config
		// to prove those low bits do not perturb the config decoding.
		for _, stereo := range []bool{false, true} {
			for code := uint8(0); code < 4; code++ {
				b := tc.config<<3 | code
				if stereo {
					b |= 0x4
				}
				toc := ParseTOC(b)
				if got := toc.Config(); got != tc.config {
					t.Errorf("config byte %#02x: Config()=%d want %d", b, got, tc.config)
				}
				if got := toc.Mode(); got != tc.mode {
					t.Errorf("config %d: Mode()=%v want %v", tc.config, got, tc.mode)
				}
				if got := toc.Bandwidth(); got != tc.bw {
					t.Errorf("config %d: Bandwidth()=%v want %v", tc.config, got, tc.bw)
				}
				if got := toc.FrameDuration(); got != tc.dur {
					t.Errorf("config %d: FrameDuration()=%v want %v", tc.config, got, tc.dur)
				}
				if got := toc.Stereo(); got != stereo {
					t.Errorf("byte %#02x: Stereo()=%v want %v", b, got, stereo)
				}
				wantCh := 1
				if stereo {
					wantCh = 2
				}
				if got := toc.Channels(); got != wantCh {
					t.Errorf("byte %#02x: Channels()=%d want %d", b, got, wantCh)
				}
				if got := toc.FrameCountCode(); got != code {
					t.Errorf("byte %#02x: FrameCountCode()=%d want %d", b, got, code)
				}
				if got := toc.Byte(); got != b {
					t.Errorf("Byte()=%#02x want %#02x", got, b)
				}
			}
		}
	}
}

// TestSamplesPerFrame checks the samples-per-frame result against the frame
// duration for every config at each valid Opus sample rate.
func TestSamplesPerFrame(t *testing.T) {
	rates := []int{8000, 12000, 16000, 24000, 48000}
	for _, tc := range rfcConfigTable {
		toc := ParseTOC(tc.config << 3)
		for _, fs := range rates {
			// Expected samples = fs * duration_seconds. Using microseconds keeps
			// this exact for all standard rates.
			want := fs * tc.dur.Microseconds() / 1_000_000
			if got := toc.SamplesPerFrame(fs); got != want {
				t.Errorf("config %d fs=%d: SamplesPerFrame=%d want %d", tc.config, fs, got, want)
			}
		}
	}
}

func TestFrameDurationString(t *testing.T) {
	cases := map[FrameDuration]string{
		FrameDuration2500us: "2.5ms",
		FrameDuration5ms:    "5ms",
		FrameDuration10ms:   "10ms",
		FrameDuration20ms:   "20ms",
		FrameDuration40ms:   "40ms",
		FrameDuration60ms:   "60ms",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("FrameDuration(%d).String()=%q want %q", int(d), got, want)
		}
	}
}

func TestModeAndBandwidthString(t *testing.T) {
	if ModeSILKOnly.String() != "SILK" || ModeHybrid.String() != "Hybrid" || ModeCELTOnly.String() != "CELT" {
		t.Errorf("mode strings: %q %q %q", ModeSILKOnly, ModeHybrid, ModeCELTOnly)
	}
	bws := []struct {
		b    Bandwidth
		want string
	}{
		{BandwidthNarrowband, "NB"}, {BandwidthMediumband, "MB"}, {BandwidthWideband, "WB"},
		{BandwidthSuperwideband, "SWB"}, {BandwidthFullband, "FB"},
	}
	for _, c := range bws {
		if got := c.b.String(); got != c.want {
			t.Errorf("Bandwidth.String()=%q want %q", got, c.want)
		}
	}
}
