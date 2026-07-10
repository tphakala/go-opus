package packet

import (
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// census is the aggregate TOC/frame profile of one test vector, mirroring the
// output of tools/vector-census.py. The expected values below are that tool's
// output, so this test turns docs/test-vectors.md into an executable oracle.
type census struct {
	packets     int
	transitions int
	stereo      int
	mono        int
	frames      int
	samples48k  int
	modes       map[Mode]int
	bandwidths  map[Bandwidth]int
	durations   map[FrameDuration]int
	codes       map[uint8]int
}

// vectorCensus is the per-vector expected profile, generated from the official
// RFC 6716 / RFC 8251 bitstreams by tools/vector-census.py (and cross-checked
// against docs/test-vectors.md). See docs/test-vectors.md for provenance.
var vectorCensus = map[string]census{
	"01": {packets: 2147, transitions: 0, stereo: 2147, mono: 0, frames: 5524, samples48k: 1415040,
		modes: map[Mode]int{ModeCELTOnly: 2147}, bandwidths: map[Bandwidth]int{BandwidthFullband: 2147},
		durations: map[FrameDuration]int{FrameDuration10ms: 296, FrameDuration20ms: 148, FrameDuration2500us: 1154, FrameDuration5ms: 549}, codes: map[uint8]int{1: 106, 2: 358, 3: 1683}},
	"02": {packets: 1185, transitions: 0, stereo: 583, mono: 602, frames: 1185, samples48k: 1201440,
		modes: map[Mode]int{ModeSILKOnly: 1185}, bandwidths: map[Bandwidth]int{BandwidthNarrowband: 1185},
		durations: map[FrameDuration]int{FrameDuration10ms: 607, FrameDuration20ms: 314, FrameDuration40ms: 158, FrameDuration60ms: 106}, codes: map[uint8]int{0: 1185}},
	"03": {packets: 998, transitions: 0, stereo: 488, mono: 510, frames: 998, samples48k: 1015680,
		modes: map[Mode]int{ModeSILKOnly: 998}, bandwidths: map[Bandwidth]int{BandwidthMediumband: 998},
		durations: map[FrameDuration]int{FrameDuration10ms: 508, FrameDuration20ms: 266, FrameDuration40ms: 134, FrameDuration60ms: 90}, codes: map[uint8]int{0: 998}},
	"04": {packets: 1265, transitions: 0, stereo: 625, mono: 640, frames: 1265, samples48k: 1278240,
		modes: map[Mode]int{ModeSILKOnly: 1265}, bandwidths: map[Bandwidth]int{BandwidthWideband: 1265},
		durations: map[FrameDuration]int{FrameDuration10ms: 651, FrameDuration20ms: 334, FrameDuration40ms: 168, FrameDuration60ms: 112}, codes: map[uint8]int{0: 1265}},
	"05": {packets: 2037, transitions: 0, stereo: 1017, mono: 1020, frames: 2037, samples48k: 1304160,
		modes: map[Mode]int{ModeHybrid: 2037}, bandwidths: map[Bandwidth]int{BandwidthSuperwideband: 2037},
		durations: map[FrameDuration]int{FrameDuration10ms: 1357, FrameDuration20ms: 680}, codes: map[uint8]int{0: 2037}},
	"06": {packets: 1876, transitions: 0, stereo: 937, mono: 939, frames: 1876, samples48k: 1200960,
		modes: map[Mode]int{ModeHybrid: 1876}, bandwidths: map[Bandwidth]int{BandwidthFullband: 1876},
		durations: map[FrameDuration]int{FrameDuration10ms: 1250, FrameDuration20ms: 626}, codes: map[uint8]int{0: 1876}},
	"07": {packets: 4186, transitions: 0, stereo: 2058, mono: 2128, frames: 4186, samples48k: 1085040,
		modes: map[Mode]int{ModeCELTOnly: 4186}, bandwidths: map[Bandwidth]int{BandwidthFullband: 1064, BandwidthNarrowband: 994, BandwidthSuperwideband: 1064, BandwidthWideband: 1064},
		durations: map[FrameDuration]int{FrameDuration10ms: 568, FrameDuration20ms: 288, FrameDuration2500us: 2194, FrameDuration5ms: 1136}, codes: map[uint8]int{0: 4186}},
	"08": {packets: 1247, transitions: 1, stereo: 886, mono: 361, frames: 1839, samples48k: 1310160,
		modes: map[Mode]int{ModeCELTOnly: 1242, ModeSILKOnly: 5}, bandwidths: map[Bandwidth]int{BandwidthFullband: 456, BandwidthNarrowband: 152, BandwidthSuperwideband: 339, BandwidthWideband: 300},
		durations: map[FrameDuration]int{FrameDuration10ms: 173, FrameDuration20ms: 586, FrameDuration2500us: 174, FrameDuration5ms: 314}, codes: map[uint8]int{0: 846, 1: 16, 2: 194, 3: 191}},
	"09": {packets: 1337, transitions: 1, stereo: 703, mono: 634, frames: 1896, samples48k: 1323600,
		modes: map[Mode]int{ModeCELTOnly: 1332, ModeSILKOnly: 5}, bandwidths: map[Bandwidth]int{BandwidthFullband: 1266, BandwidthNarrowband: 20, BandwidthSuperwideband: 35, BandwidthWideband: 16},
		durations: map[FrameDuration]int{FrameDuration10ms: 192, FrameDuration20ms: 629, FrameDuration2500us: 274, FrameDuration5ms: 242}, codes: map[uint8]int{0: 971, 1: 130, 2: 43, 3: 193}},
	"10": {packets: 1912, transitions: 16, stereo: 987, mono: 925, frames: 4606, samples48k: 1536480,
		modes: map[Mode]int{ModeCELTOnly: 1598, ModeHybrid: 314}, bandwidths: map[Bandwidth]int{BandwidthFullband: 1912},
		durations: map[FrameDuration]int{FrameDuration10ms: 412, FrameDuration20ms: 340, FrameDuration2500us: 778, FrameDuration5ms: 382}, codes: map[uint8]int{0: 756, 1: 47, 2: 325, 3: 784}},
	"11": {packets: 553, transitions: 0, stereo: 553, mono: 0, frames: 1501, samples48k: 1440960,
		modes: map[Mode]int{ModeCELTOnly: 553}, bandwidths: map[Bandwidth]int{BandwidthFullband: 553},
		durations: map[FrameDuration]int{FrameDuration20ms: 553}, codes: map[uint8]int{0: 204, 1: 21, 2: 79, 3: 249}},
	"12": {packets: 1332, transitions: 3, stereo: 0, mono: 1332, frames: 1332, samples48k: 1278720,
		modes: map[Mode]int{ModeHybrid: 264, ModeSILKOnly: 1068}, bandwidths: map[Bandwidth]int{BandwidthMediumband: 222, BandwidthNarrowband: 352, BandwidthSuperwideband: 264, BandwidthWideband: 494},
		durations: map[FrameDuration]int{FrameDuration20ms: 1332}, codes: map[uint8]int{0: 1332}},
}

// vectorsDir is the extracted RFC 8251 bitstream set, relative to this package.
const vectorsDir = "../../testdata/vectors/opus_newvectors"

// TestVectorCensus parses every packet of every official test vector and asserts
// the aggregate TOC/frame census matches the recorded oracle. It is the phase 1
// packet gate ("parses every RFC vector bitstream"). The test skips cleanly when
// the vectors have not been fetched (scripts/fetch-vectors.sh).
func TestVectorCensus(t *testing.T) {
	if _, err := os.Stat(vectorsDir); err != nil {
		t.Skipf("test vectors not present (%v); run scripts/fetch-vectors.sh", err)
	}

	names := slices.Sorted(maps.Keys(vectorCensus))
	for _, name := range names {
		want := vectorCensus[name]
		t.Run("vector"+name, func(t *testing.T) {
			path := filepath.Join(vectorsDir, "testvector"+name+".bit")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Skipf("cannot read %s: %v", path, err)
			}
			got := censusOfVector(t, data)
			compareCensus(t, &got, &want)
		})
	}
}

// censusOfVector walks the opus_demo record framing (4-byte big-endian length,
// 4-byte big-endian final range, then the packet bytes), parses every packet,
// and accumulates its census. It asserts each packet parses without error and
// that the parser consumes exactly the record's byte count.
func censusOfVector(t *testing.T, data []byte) census {
	t.Helper()
	got := census{
		modes:      map[Mode]int{},
		bandwidths: map[Bandwidth]int{},
		durations:  map[FrameDuration]int{},
		codes:      map[uint8]int{},
	}
	var prevMode Mode
	havePrev := false

	off := 0
	for off+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[off:]))
		off += 8
		// Skip lost-frame markers (length 0) and any truncated trailing record,
		// exactly as tools/vector-census.py does.
		if length == 0 || off+length > len(data) {
			off += length
			continue
		}
		pkt := data[off : off+length]
		off += length

		p, err := Parse(pkt)
		if err != nil {
			t.Fatalf("Parse failed on a vector packet (len %d, toc %#02x): %v", length, pkt[0], err)
		}
		if p.Consumed != length {
			t.Fatalf("Consumed = %d but record length = %d (toc %#02x)", p.Consumed, length, pkt[0])
		}
		samples, err := Samples(pkt, 48000)
		if err != nil {
			t.Fatalf("Samples failed on a vector packet: %v", err)
		}
		if samples != p.Duration(48000) {
			t.Fatalf("Samples=%d disagrees with Duration=%d", samples, p.Duration(48000))
		}

		toc := p.TOC
		m := toc.Mode()
		got.packets++
		got.modes[m]++
		got.bandwidths[toc.Bandwidth()]++
		got.durations[toc.FrameDuration()]++
		got.codes[toc.FrameCountCode()]++
		if toc.Stereo() {
			got.stereo++
		} else {
			got.mono++
		}
		if havePrev && prevMode != m {
			got.transitions++
		}
		prevMode = m
		havePrev = true
		got.frames += len(p.Frames)
		got.samples48k += samples
	}
	return got
}

func compareCensus(t *testing.T, got, want *census) {
	t.Helper()
	if got.packets != want.packets {
		t.Errorf("packets = %d want %d", got.packets, want.packets)
	}
	if got.transitions != want.transitions {
		t.Errorf("mode transitions = %d want %d", got.transitions, want.transitions)
	}
	if got.stereo != want.stereo {
		t.Errorf("stereo packets = %d want %d", got.stereo, want.stereo)
	}
	if got.mono != want.mono {
		t.Errorf("mono packets = %d want %d", got.mono, want.mono)
	}
	if got.frames != want.frames {
		t.Errorf("total frames = %d want %d", got.frames, want.frames)
	}
	if got.samples48k != want.samples48k {
		t.Errorf("total samples@48k = %d want %d", got.samples48k, want.samples48k)
	}
	if !maps.Equal(got.modes, want.modes) {
		t.Errorf("mode distribution = %v want %v", got.modes, want.modes)
	}
	if !maps.Equal(got.bandwidths, want.bandwidths) {
		t.Errorf("bandwidth distribution = %v want %v", got.bandwidths, want.bandwidths)
	}
	if !maps.Equal(got.durations, want.durations) {
		t.Errorf("frame-size distribution = %v want %v", got.durations, want.durations)
	}
	if !maps.Equal(got.codes, want.codes) {
		t.Errorf("frame-code distribution = %v want %v", got.codes, want.codes)
	}
}

func BenchmarkParse(b *testing.B) {
	// A code 3 VBR packet with three frames, a representative multi-frame case.
	pkt := []byte{0x03, 0x83, 0x01, 0x02, 10, 20, 21, 30, 31, 32}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(pkt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseTOC(b *testing.B) {
	for b.Loop() {
		toc := ParseTOC(0xFC)
		_ = toc.Mode()
		_ = toc.Bandwidth()
		_ = toc.FrameDuration()
	}
}
