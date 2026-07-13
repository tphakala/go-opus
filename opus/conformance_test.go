package opus_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/go-opus/internal/opuscompare"
	"github.com/tphakala/go-opus/opus"
)

// vectorsDir is the extracted RFC 8251 bitstream set, relative to this package.
// Fetch it with scripts/fetch-vectors.sh; the conformance test skips cleanly
// when it is absent.
const vectorsDir = "../testdata/vectors/opus_newvectors"

// allVectors are all 12 RFC 6716 test vectors, the phase 3 gate set. Vectors
// 01/07/11 are pure CELT, 02/03/04 pure SILK, 05/06 pure hybrid, and
// 08/09/10/12 exercise the mode-transition machine (CELT<->SILK<->hybrid with
// the redundancy crossfades). Each stream is decoded to stereo (.dec reference)
// and mono (.m.dec reference) at 48 kHz through the public opus.Decoder.
var allVectors = []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12"}

// bitRecord is one opus_demo record: the encoder final range and the packet.
type bitRecord struct {
	finalRange uint32
	packet     []byte
}

// readBitRecords parses the opus_demo record framing of a .bit file: each record
// is a 4-byte big-endian payload length, a 4-byte big-endian encoder final
// range, then that many packet bytes. A zero-length record is an opus_demo
// lost-frame marker; it carries no packet and is returned with a nil packet.
func readBitRecords(t *testing.T, data []byte) []bitRecord {
	t.Helper()
	var recs []bitRecord
	off := 0
	for off+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[off:]))
		rng := binary.BigEndian.Uint32(data[off+4:])
		off += 8
		if length == 0 {
			recs = append(recs, bitRecord{finalRange: rng})
			continue
		}
		if off+length > len(data) {
			t.Fatalf("truncated record: need %d bytes at offset %d, have %d", length, off, len(data)-off)
		}
		recs = append(recs, bitRecord{finalRange: rng, packet: data[off : off+length : off+length]})
		off += length
	}
	return recs
}

// readRefInt16 reads a little-endian 16-bit PCM reference (.dec / .m.dec) file.
func readRefInt16(t *testing.T, path string) []int16 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference %s: %v", path, err)
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out
}

// decodeVector decodes every packet of recs through a fresh channels-channel
// decoder at 48 kHz and returns the concatenated interleaved PCM plus, for the
// stereo decode, the per-packet final-range match count. The final range is a
// property of the entropy decode and is independent of the output channel count,
// so it is only asserted on the stereo pass.
func decodeVector(t *testing.T, recs []bitRecord, channels int) (pcm []int16, packets, rangeMatches int, firstMismatch string) {
	t.Helper()
	dec, err := opus.NewDecoder(48000, channels)
	if err != nil {
		t.Fatalf("NewDecoder(48000,%d): %v", channels, err)
	}
	buf := make([]int16, 5760*channels) // 120 ms at 48 kHz: max packet
	for i, rec := range recs {
		if rec.packet == nil { // opus_demo lost-frame marker (none in the RFC vectors)
			continue
		}
		packets++
		n, err := dec.Decode(rec.packet, buf)
		if err != nil {
			t.Fatalf("packet %d (%d bytes): decode failed: %v", i, len(rec.packet), err)
		}
		pcm = append(pcm, buf[:n*channels]...)
		if got := dec.FinalRange(); got == rec.finalRange {
			rangeMatches++
		} else if firstMismatch == "" {
			firstMismatch = fmt.Sprintf("packet=%d want=%d got=%d", i, rec.finalRange, got)
		}
	}
	return pcm, packets, rangeMatches, firstMismatch
}

// TestConformance is the phase 3 gate: it decodes all 12 RFC 6716 vectors end to
// end through the public opus.Decoder and asserts, per vector, that (a) every
// packet's decoder final range exactly matches the range recorded in the
// bitstream (bit-exact entropy-decode conformance, which must hold across the
// CELT-only, SILK-only, hybrid and mode-transition vectors) and (b) the full
// decoded PCM passes the ported opus_compare against the RFC 8251 stereo (.dec)
// and mono (.m.dec) references at the spec threshold (Quality >= 0, PASS). It
// reports the per-vector final-range match and opus_compare quality.
func TestConformance(t *testing.T) {
	if _, err := os.Stat(vectorsDir); err != nil {
		t.Skipf("test vectors not present (%v); run scripts/fetch-vectors.sh", err)
	}

	for _, name := range allVectors {
		t.Run("vector"+name, func(t *testing.T) {
			// The 12 vectors are wholly independent: each subtest reads its own files
			// and builds its own Decoder, and nothing is shared but the read-only
			// vector list. Running them in parallel is what makes this gate AFFORDABLE
			// IN CI now that CI actually fetches the vectors and runs it: serially under
			// -race the 12 vectors take ~14 minutes, which would not fit the job.
			t.Parallel()

			bitPath := filepath.Join(vectorsDir, "testvector"+name+".bit")
			bitData, err := os.ReadFile(bitPath)
			if err != nil {
				t.Skipf("cannot read %s: %v", bitPath, err)
			}
			recs := readBitRecords(t, bitData)

			// Stereo decode: final-range check + opus_compare vs the stereo .dec.
			stereoPCM, packets, rangeMatches, firstMismatch := decodeVector(t, recs, 2)
			if rangeMatches != packets {
				t.Errorf("final-range match %d/%d packets (first mismatch %s)", rangeMatches, packets, firstMismatch)
			} else {
				t.Logf("final-range: %d/%d packets match exactly", rangeMatches, packets)
			}

			stereoRef := readRefInt16(t, filepath.Join(vectorsDir, "testvector"+name+".dec"))
			stereoRes, err := opuscompare.CompareInt16(stereoRef, stereoPCM, opuscompare.Config{Channels: 2})
			if err != nil {
				t.Fatalf("opus_compare stereo: %v (decoded %d samples, reference %d)", err, len(stereoPCM), len(stereoRef))
			}
			t.Logf("stereo opus_compare: Quality=%.4f Passed=%v Frames=%d", stereoRes.Quality, stereoRes.Passed, stereoRes.Frames)
			if !stereoRes.Passed {
				t.Errorf("vector %s FAILS stereo opus_compare: Quality=%.4f (want >= 0)", name, stereoRes.Quality)
			}

			// Mono decode: opus_compare vs the mono .m.dec reference.
			monoPCM, _, _, _ := decodeVector(t, recs, 1)
			monoRef := readRefInt16(t, filepath.Join(vectorsDir, "testvector"+name+"m.dec"))
			monoRes, err := opuscompare.CompareInt16(monoRef, monoPCM, opuscompare.Config{Channels: 1})
			if err != nil {
				t.Fatalf("opus_compare mono: %v (decoded %d samples, reference %d)", err, len(monoPCM), len(monoRef))
			}
			t.Logf("mono opus_compare: Quality=%.4f Passed=%v Frames=%d", monoRes.Quality, monoRes.Passed, monoRes.Frames)
			if !monoRes.Passed {
				t.Errorf("vector %s FAILS mono opus_compare: Quality=%.4f (want >= 0)", name, monoRes.Quality)
			}
		})
	}
}

// TestDecoderRejectsBadConfig confirms NewDecoder validates its arguments.
func TestDecoderRejectsBadConfig(t *testing.T) {
	if _, err := opus.NewDecoder(44100, 2); !errors.Is(err, opus.ErrBadArg) {
		t.Errorf("NewDecoder(44100,2) err = %v, want ErrBadArg", err)
	}
	if _, err := opus.NewDecoder(48000, 3); !errors.Is(err, opus.ErrBadArg) {
		t.Errorf("NewDecoder(48000,3) err = %v, want ErrBadArg", err)
	}
	if _, err := opus.NewDecoder(48000, 0); !errors.Is(err, opus.ErrBadArg) {
		t.Errorf("NewDecoder(48000,0) err = %v, want ErrBadArg", err)
	}
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		if _, err := opus.NewDecoder(rate, 1); err != nil {
			t.Errorf("NewDecoder(%d,1) err = %v, want nil", rate, err)
		}
	}
}

// FuzzDecode asserts the decoder never panics on arbitrary input. It feeds fuzz
// bytes as an Opus packet to a fresh decoder (and, on nil, exercises the PLC
// entry), for both mono and stereo, requiring only that Decode returns rather
// than panics or writes out of bounds.
func FuzzDecode(f *testing.F) {
	// Seed with a few real packets from a vector when available, plus edge cases.
	if bitData, err := os.ReadFile(filepath.Join(vectorsDir, "testvector08.bit")); err == nil {
		off, seeded := 0, 0
		for off+8 <= len(bitData) && seeded < 16 {
			length := int(binary.BigEndian.Uint32(bitData[off:]))
			off += 8
			if length == 0 || off+length > len(bitData) {
				continue
			}
			f.Add(bitData[off : off+length])
			off += length
			seeded++
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF})
	f.Add([]byte{0xFF, 0xFF})
	f.Add([]byte{0x0C, 0x00, 0x00}) // hybrid TOC, code 0
	f.Add([]byte{0x60, 0x03})       // SILK code-3, truncated

	f.Fuzz(func(t *testing.T, pkt []byte) {
		for _, channels := range []int{1, 2} {
			dec, err := opus.NewDecoder(48000, channels)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			pcm := make([]int16, 5760*channels)
			// Must not panic. Any (n, err) result is acceptable; a successful
			// decode must not claim more samples than the buffer can hold.
			n, derr := dec.Decode(pkt, pcm)
			if derr == nil && n*channels > len(pcm) {
				t.Fatalf("Decode reported %d samples/ch (%d total) into a %d buffer", n, n*channels, len(pcm))
			}
		}
	})
}
