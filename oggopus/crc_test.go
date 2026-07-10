package oggopus

import (
	"math/rand/v2"
	"testing"
)

// crcBitSerial is an independent, deliberately naive reference implementation of
// the Ogg CRC-32 (bit-by-bit, no table). The production crc32 is table-driven;
// asserting the two agree on random inputs cross-checks the table construction
// against a second implementation that shares no code.
func crcBitSerial(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ crcPoly
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func TestCRC32BitSerialParity(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		n := r.IntN(300)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		if got, want := crc32(0, b), crcBitSerial(b); got != want {
			t.Fatalf("crc mismatch on %d bytes: table %#08x, bit-serial %#08x", n, got, want)
		}
	}
}

// TestCRC32KnownPageVector checks the table-driven CRC against a full Ogg page
// whose checksum was computed by an independent Python implementation of the Ogg
// CRC (poly 0x04c11db7, init 0, no reflection). The page is a real OpusHead BOS
// page: capture pattern, version, flags, granule 0, serial 0x12345678, sequence
// 0, one packet of 19 bytes.
func TestCRC32KnownPageVector(t *testing.T) {
	// goldenHeadPage is the byte-exact page with its CRC field already filled.
	page := goldenHeadPage()
	const wantCRC = 0xbbc0113d

	// Recompute over the page with the CRC field zeroed, exactly as readPage does.
	zeroed := make([]byte, len(page))
	copy(zeroed, page)
	for i := crcFieldOffset; i < crcFieldOffset+4; i++ {
		zeroed[i] = 0
	}
	if got := crc32(0, zeroed); got != wantCRC {
		t.Fatalf("crc over known page = %#08x, want %#08x", got, wantCRC)
	}
}
