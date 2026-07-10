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

// celtVectors are the pure-CELT RFC 6716 test vectors, the phase 2 gate set
// (docs/test-vectors.md: "Pure-CELT vectors for the phase 2 gate: 01, 07, 11").
// Every packet in these three streams is CELT-only, so a CELT-only decoder can
// decode them end to end. Each stream decodes to stereo at 48 kHz; the .dec
// reference is the stereo opus_demo decode output.
var celtVectors = []string{"01", "07", "11"}

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

// readRefInt16 reads a little-endian 16-bit PCM reference (.dec) file.
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

// TestCELTConformance is the phase 2 gate: it decodes the pure-CELT RFC vectors
// end to end through the public opus.Decoder and asserts, per vector, that (a)
// every packet's decoder final range exactly matches the range recorded in the
// bitstream (bit-exact entropy-decode conformance) and (b) the full decoded PCM
// passes the ported opus_compare against the RFC 8251 stereo reference at the
// spec threshold (Quality >= 0, PASS). It reports the opus_compare quality per
// vector.
func TestCELTConformance(t *testing.T) {
	if _, err := os.Stat(vectorsDir); err != nil {
		t.Skipf("test vectors not present (%v); run scripts/fetch-vectors.sh", err)
	}

	for _, name := range celtVectors {
		t.Run("vector"+name, func(t *testing.T) {
			bitPath := filepath.Join(vectorsDir, "testvector"+name+".bit")
			bitData, err := os.ReadFile(bitPath)
			if err != nil {
				t.Skipf("cannot read %s: %v", bitPath, err)
			}
			recs := readBitRecords(t, bitData)

			// Decode the whole stream to stereo at 48 kHz, the reference config.
			dec, err := opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			buf := make([]int16, 5760*2) // 120 ms at 48 kHz, stereo: max packet
			var pcm []int16

			var packets, rangeMatches, nonCELT, lost int
			var firstMismatch string
			for i, rec := range recs {
				if rec.packet == nil { // opus_demo lost-frame marker
					lost++
					continue
				}
				mode, _, _, _ := opus.ParseTOC(rec.packet[0])
				if mode != opus.ModeCELTOnly {
					// The census says 01/07/11 are pure CELT; note and skip if not.
					nonCELT++
					t.Logf("packet %d is %s, not CELT (unexpected for a pure-CELT vector); skipping", i, mode)
					continue
				}
				packets++

				n, err := dec.Decode(rec.packet, buf)
				if err != nil {
					t.Fatalf("packet %d (%d bytes): decode failed: %v", i, len(rec.packet), err)
				}
				pcm = append(pcm, buf[:n*2]...)

				if got := dec.FinalRange(); got == rec.finalRange {
					rangeMatches++
				} else if firstMismatch == "" {
					firstMismatch = fmt.Sprintf("packet=%d want=%d got=%d", i, rec.finalRange, got)
				}
			}

			if rangeMatches != packets {
				t.Errorf("final-range match %d/%d packets (first mismatch %s)", rangeMatches, packets, firstMismatch)
			} else {
				t.Logf("final-range: %d/%d packets match exactly", rangeMatches, packets)
			}
			if lost > 0 {
				t.Logf("skipped %d opus_demo lost-frame markers", lost)
			}
			if nonCELT > 0 {
				t.Logf("skipped %d non-CELT packets (deferred to phase 3)", nonCELT)
			}

			// opus_compare against the stereo reference (.dec).
			refPath := filepath.Join(vectorsDir, "testvector"+name+".dec")
			ref := readRefInt16(t, refPath)
			res, err := opuscompare.CompareInt16(ref, pcm, opuscompare.Config{Channels: 2})
			if err != nil {
				t.Fatalf("opus_compare: %v (decoded %d samples, reference %d)", err, len(pcm), len(ref))
			}
			t.Logf("opus_compare: Quality=%.4f Passed=%v Frames=%d", res.Quality, res.Passed, res.Frames)
			if !res.Passed {
				t.Errorf("vector %s FAILS opus_compare: Quality=%.4f (want >= 0)", name, res.Quality)
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
