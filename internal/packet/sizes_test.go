package packet

import "testing"

// TestSizeRoundTrip checks that encodeSize and parseSize are inverses across the
// full 0..1275 frame-length range, and that the byte count (1 below 252, 2 at or
// above) matches libopus.
func TestSizeRoundTrip(t *testing.T) {
	buf := make([]byte, 2)
	for size := 0; size <= 1275; size++ {
		n := encodeSize(size, buf)
		wantBytes := 1
		if size >= 252 {
			wantBytes = 2
		}
		if n != wantBytes {
			t.Fatalf("encodeSize(%d) wrote %d bytes want %d", size, n, wantBytes)
		}
		gotBytes, gotSize := parseSize(buf[:n], n)
		if gotBytes != n || gotSize != size {
			t.Fatalf("parseSize round-trip for %d: got (%d,%d) want (%d,%d)", size, gotBytes, gotSize, n, size)
		}
	}
}

func TestParseSizeTruncation(t *testing.T) {
	// No bytes available.
	if n, s := parseSize(nil, 0); n != -1 || s != -1 {
		t.Errorf("parseSize(empty) = (%d,%d) want (-1,-1)", n, s)
	}
	// A two-byte size field with only one byte available.
	if n, s := parseSize([]byte{252}, 1); n != -1 || s != -1 {
		t.Errorf("parseSize(one byte of two) = (%d,%d) want (-1,-1)", n, s)
	}
}
