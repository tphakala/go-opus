package oggopus

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestResetMatchesFreshDecoder pins that Reset onto a new stream yields exactly what
// a new Decoder yields, whether the codec is reused (same layout) or rebuilt, and
// whatever the previous stream left in it.
func TestResetMatchesFreshDecoder(t *testing.T) {
	mono := seekFixture(t, 1, 20, 48000, 64000)
	stereo := seekFixture(t, 2, 20, 48000, 64000)
	ms2 := func(streams, coupled int, mapping ...byte) []byte {
		return msStream(t, 1, 2, streams, coupled, mapping, 40)
	}
	rows := []struct {
		name    string
		a, b    []byte
		partial int // bytes read from a before Reset; 0 decodes it fully
	}{
		{"mono->mono", mono, mono, 0},
		{"stereo->stereo partial", stereo, stereo, 5000},
		{"family1->family1", family1Stream(t, 40), family1Stream(t, 30), 5000},
		{"mono->stereo", mono, stereo, 0},
		{"stereo->family1 2ch", stereo, ms2(2, 0, 0, 1), 5000},
		{"family1 2 streams->1 coupled stream", ms2(2, 0, 0, 1), ms2(1, 1, 0, 1), 5000},
		{"family1 mapping 0,1->1,0", ms2(2, 0, 0, 1), ms2(2, 0, 1, 0), 5000},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d := seekDecoder(t, row.a)
			if row.partial == 0 {
				drainDecoder(t, d, nil)
			} else if _, err := io.ReadFull(d, make([]byte, row.partial)); err != nil {
				t.Fatalf("partial read: %v", err)
			}
			if err := d.Reset(bytes.NewReader(row.b)); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			var got bytes.Buffer
			if _, err := d.WriteTo(&got); err != nil {
				t.Fatalf("WriteTo: %v", err)
			}
			if want := streamingDecode(t, row.b); !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("Reset output differs from a new Decoder's: %d bytes, want %d", got.Len(), len(want))
			}
		})
	}
}

// TestResetDoesNotReuseCodecAcrossFamilyValidation pins that codec reuse never
// accepts a header the constructor would reject: a family-255 9-channel codec must
// not be reused for a family-1 9-channel header, which RFC 7845 caps at 8 channels.
func TestResetDoesNotReuseCodecAcrossFamilyValidation(t *testing.T) {
	mapping := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8}
	a := msStream(t, 255, 9, 9, 0, mapping, 10)
	b := msStream(t, 1, 9, 9, 0, mapping, 10)
	d := seekDecoder(t, a)
	if err := d.Reset(bytes.NewReader(b)); err == nil {
		t.Fatal("Reset accepted a 9-channel family-1 header")
	}
	if _, err := d.Read(make([]byte, 16)); !errors.Is(err, errUninitialized) {
		t.Fatalf("Read after a failed Reset: err = %v, want errUninitialized", err)
	}
}
