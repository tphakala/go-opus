package rangecoding

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// This file holds the pure-Go tests: they run under the normal `go test
// ./internal/rangecoding/...` with no cgo. The byte-exactness-vs-C gate lives in
// the refc differential test under internal/reftest/oracle; here we cover the
// Go/Go round trip, the tell() invariants, and the snapshot/restore/splice
// mechanics the two phase-4 RDO features rely on (docs/hard-parts.md 1).

// op is one coded symbol in a generated stream. Each op knows how to encode
// itself and how to decode-and-verify itself, so a single []op drives both an
// encoder and a matching decoder round trip.
type op struct {
	kind opKind
	// modeled ec_encode symbol: cumulative-frequency table cum (len n+1) and the
	// chosen symbol index.
	cum []uint32
	sym uint32
	// ec_enc_bit_logp
	val  int
	logp uint32
	// ec_enc_icdf
	icdf []byte
	ftb  uint32
	// ec_enc_uint / ec_enc_bits
	fl   uint32
	ft   uint32
	bits uint32
}

type opKind uint8

const (
	opEncode opKind = iota
	opBitLogp
	opIcdf
	opUint
	opBits
)

func (o op) encode(e *Encoder) {
	switch o.kind {
	case opEncode:
		e.Encode(o.cum[o.sym], o.cum[o.sym+1], o.cum[len(o.cum)-1])
	case opBitLogp:
		e.EncBitLogp(o.val, o.logp)
	case opIcdf:
		e.EncIcdf(int(o.sym), o.icdf, o.ftb)
	case opUint:
		e.EncUint(o.fl, o.ft)
	case opBits:
		e.EncBits(o.fl, o.bits)
	}
}

// decode decodes the op and returns whether the decoded value matched what was
// encoded.
func (o op) decode(d *Decoder) bool {
	switch o.kind {
	case opEncode:
		ft := o.cum[len(o.cum)-1]
		fs := d.Decode(ft)
		// Find the symbol whose [cum[s],cum[s+1]) interval contains fs: the
		// largest s with cum[s] <= fs, since cum is increasing from 0.
		s := 0
		for s+1 < len(o.cum) && fs >= o.cum[s+1] {
			s++
		}
		d.DecUpdate(o.cum[s], o.cum[s+1], ft)
		return uint32(s) == o.sym
	case opBitLogp:
		return d.DecBitLogp(o.logp) == o.val
	case opIcdf:
		return d.DecIcdf(o.icdf, o.ftb) == int(o.sym)
	case opUint:
		return d.DecUint(o.ft) == o.fl
	case opBits:
		return d.DecBits(o.bits) == o.fl
	}
	return false
}

// genOps builds n random but valid ops. It is shared by the round-trip test and
// the benchmarks so the workload mixes every primitive.
func genOps(r *rand.Rand, n int) []op {
	ops := make([]op, 0, n)
	for range n {
		switch r.IntN(5) {
		case 0: // ec_encode with a random cdf
			nsym := 2 + r.IntN(6)
			cum := make([]uint32, nsym+1)
			var acc uint32
			for i := range nsym {
				acc += 1 + uint32(r.IntN(64))
				cum[i+1] = acc
			}
			ops = append(ops, op{kind: opEncode, cum: cum, sym: uint32(r.IntN(nsym))})
		case 1: // ec_enc_bit_logp
			ops = append(ops, op{kind: opBitLogp, val: r.IntN(2), logp: uint32(1 + r.IntN(15))})
		case 2: // ec_enc_icdf
			ftb := uint32(3 + r.IntN(6)) // 3..8
			total := uint32(1) << ftb
			nsym := 2 + r.IntN(min(int(total), 8)-1)
			// random freqs >=1 summing to total
			freqs := make([]uint32, nsym)
			for i := range freqs {
				freqs[i] = 1
			}
			for left := total - uint32(nsym); left > 0; left-- {
				freqs[r.IntN(nsym)]++
			}
			icdf := make([]byte, nsym)
			var cum uint32
			for i := range nsym {
				cum += freqs[i]
				icdf[i] = byte(total - cum)
			}
			ops = append(ops, op{kind: opIcdf, icdf: icdf, ftb: ftb, sym: uint32(r.IntN(nsym))})
		case 3: // ec_enc_uint (mix small and large ranges to hit both branches)
			var ft uint32
			if r.IntN(2) == 0 {
				ft = uint32(2 + r.IntN(255)) // <= EC_UINT_BITS branch
			} else {
				ft = uint32(257 + r.IntN(1<<20)) // > EC_UINT_BITS branch
			}
			ops = append(ops, op{kind: opUint, fl: uint32(r.IntN(int(ft))), ft: ft})
		case 4: // ec_enc_bits
			bits := uint32(1 + r.IntN(24))
			ops = append(ops, op{kind: opBits, fl: uint32(r.Uint32()) & ((1 << bits) - 1), bits: bits})
		}
	}
	return ops
}

func encodeOps(ops []op, size int) *Encoder {
	buf := make([]byte, size)
	var e Encoder
	e.Init(buf)
	for _, o := range ops {
		o.encode(&e)
	}
	e.EncDone()
	return &e
}

// TestTellAtInit checks the documented "a newly initialized encoder or decoder
// claims to have used 1 bit" invariant, plus the exact tell_frac at init.
func TestTellAtInit(t *testing.T) {
	var e Encoder
	e.Init(make([]byte, 64))
	if got := e.Tell(); got != 1 {
		t.Errorf("encoder Tell() at init = %d, want 1", got)
	}
	if got := e.TellFrac(); got != 8 {
		t.Errorf("encoder TellFrac() at init = %d, want 8", got)
	}

	var d Decoder
	d.Init(make([]byte, 64))
	if got := d.Tell(); got != 1 {
		t.Errorf("decoder Tell() at init = %d, want 1", got)
	}
}

// TestEncodeDecodeRoundTrip encodes a random mixed stream and decodes it back,
// asserting every decoded symbol matches and the tell values agree end to end.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 0xC0FFEE))
		ops := genOps(r, 150)

		e := encodeOps(ops, 8192)
		if e.Error() != 0 {
			t.Fatalf("seed %d: encoder reported error %d", seed, e.Error())
		}

		var d Decoder
		d.Init(e.Buffer())
		for i, o := range ops {
			if !o.decode(&d) {
				t.Fatalf("seed %d: op %d (kind %d) decoded to the wrong value", seed, i, o.kind)
			}
		}
		if d.Error() != 0 {
			t.Fatalf("seed %d: decoder reported error %d", seed, d.Error())
		}
		// The encoder and decoder must agree on how many bits were used.
		if e.Tell() != d.Tell() {
			t.Errorf("seed %d: tell mismatch enc=%d dec=%d", seed, e.Tell(), d.Tell())
		}
	}
}

// TestSnapshotRestore proves that a value copy (saved := *enc) followed by a
// throwaway trial encode and a restore (*enc = saved) leaves the encoder able to
// reproduce byte-for-byte the stream it would have produced without the trial.
// This is the exact maneuver two-pass coarse energy and theta RDO perform.
func TestSnapshotRestore(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 42))
	prefix := genOps(r, 40)
	trial := genOps(r, 30)
	realOps := genOps(r, 40)

	// Reference: prefix then real, with no trial in between.
	ref := encodeOps(append(append([]op{}, prefix...), realOps...), 8192)
	wantBytes := append([]byte(nil), ref.Buffer()[:ref.RangeBytes()]...)

	// With a snapshot/restore around a trial encode.
	buf := make([]byte, 8192)
	var enc Encoder
	enc.Init(buf)
	for _, o := range prefix {
		o.encode(&enc)
	}
	saved := enc // struct copy; saved.buf aliases the same backing array
	for _, o := range trial {
		o.encode(&enc)
	}
	enc = saved // restore, exactly like `*enc = enc_start_state`
	for _, o := range realOps {
		o.encode(&enc)
	}
	enc.EncDone()

	if enc.Error() != 0 || ref.Error() != 0 {
		t.Fatalf("encoder error: trial=%d ref=%d", enc.Error(), ref.Error())
	}
	gotBytes := enc.Buffer()[:enc.RangeBytes()]
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("snapshot/restore bytes differ:\n got %x\nwant %x", gotBytes, wantBytes)
	}
	if enc.Rng() != ref.Rng() || enc.Val() != ref.Val() {
		t.Errorf("snapshot/restore state differs: rng %d/%d val %d/%d",
			enc.Rng(), ref.Rng(), enc.Val(), ref.Val())
	}
}

// TestSpliceRDO mirrors the two-pass coarse energy dance in full: snapshot the
// start state, encode candidate A, copy the emitted head bytes out, restore,
// encode candidate B, then (choosing A) restore A's saved state AND splice A's
// bytes back over the shared buffer with copy(). The result must equal a direct
// encode of the prefix followed by A.
func TestSpliceRDO(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 99))
	prefix := genOps(r, 25)
	candA := genOps(r, 35)
	candB := genOps(r, 20)

	// Ground truth: prefix + A encoded straight through.
	ref := encodeOps(append(append([]op{}, prefix...), candA...), 8192)
	wantBytes := append([]byte(nil), ref.Buffer()[:ref.RangeBytes()]...)

	buf := make([]byte, 8192)
	var enc Encoder
	enc.Init(buf)
	for _, o := range prefix {
		o.encode(&enc)
	}

	start := enc // save enc_start_state

	// Pass A.
	for _, o := range candA {
		o.encode(&enc)
	}
	// Copy the emitted range-coder bytes out of the shared buffer, exactly the
	// ec_get_buffer()+ec_range_bytes() window quant_bands.c saves.
	aBytes := append([]byte(nil), enc.Buffer()[:enc.RangeBytes()]...)
	aState := enc // enc_intra_state

	// Restore and encode pass B (the alternative we will discard).
	enc = start
	for _, o := range candB {
		o.encode(&enc)
	}

	// Choose A: restore A's struct and memcpy the saved bytes back over the buffer.
	enc = aState
	copy(enc.Buffer()[:len(aBytes)], aBytes)
	enc.EncDone()

	if enc.Error() != 0 {
		t.Fatalf("encoder error after splice: %d", enc.Error())
	}
	gotBytes := enc.Buffer()[:enc.RangeBytes()]
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("splice bytes differ:\n got %x\nwant %x", gotBytes, wantBytes)
	}
}

// TestNilBufferSentinel checks that a nil buffer (the Go spelling of C's
// ALLOC_NONE, save_bytes==0) is accepted and behaves: every write errors out but
// nothing panics, matching an encoder over a zero-length store.
func TestNilBufferSentinel(t *testing.T) {
	var e Encoder
	e.Init(nil)
	if e.storage != 0 {
		t.Fatalf("nil buffer storage = %d, want 0", e.storage)
	}
	e.Encode(0, 1, 4)
	e.EncBits(1, 3)
	e.EncDone()
	if e.Error() == 0 {
		t.Error("expected an error writing to a nil (ALLOC_NONE) buffer")
	}
	if e.Buffer() != nil {
		t.Error("Buffer() over a nil store should be nil")
	}
}

// goldenEncode applies a fixed hand-written sequence exercising every primitive
// and both fl==0 / fl>0 branches. The identical sequence is encoded by libopus in
// the refc differential test (TestRangeCoderGolden), which asserts the Go bytes
// equal C's; goldenBytes below is that confirmed C output. This gives CI a
// byte-exactness check that needs no cgo. Keep the two sequences in sync.
func goldenEncode(e *Encoder) {
	e.Encode(2, 5, 8)
	e.Encode(0, 3, 8)
	e.EncBitLogp(1, 3)
	e.EncBitLogp(0, 4)
	e.EncodeBin(1, 3, 4)
	e.EncUint(1234, 5000)
	e.EncUint(3, 4)
	e.EncIcdf(1, []byte{240, 128, 32, 0}, 8)
	e.EncIcdf(0, []byte{240, 128, 32, 0}, 8)
	e.EncBits(0xABCD, 16)
}

// goldenBytes is the libopus output for goldenEncode, captured from the refc
// TestRangeCoderGolden (finalRange 12623056). See goldenEncode.
var goldenBytes = []byte{0x5f, 0xe4, 0xd5, 0x80}

// TestGoldenVector confirms the Go encoder reproduces libopus's bytes for a fixed
// stream without needing the C oracle at run time.
func TestGoldenVector(t *testing.T) {
	var e Encoder
	e.Init(make([]byte, 256))
	goldenEncode(&e)
	e.EncDone()
	got := e.Buffer()[:e.RangeBytes()]
	if !bytes.Equal(got, goldenBytes) {
		t.Fatalf("golden bytes mismatch:\n got %#v\nwant %#v", got, goldenBytes)
	}
}

func BenchmarkEncode(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	ops := genOps(r, 1000)
	buf := make([]byte, 1<<16)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var e Encoder
		e.Init(buf)
		for _, o := range ops {
			o.encode(&e)
		}
		e.EncDone()
	}
}

func BenchmarkDecode(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	ops := genOps(r, 1000)
	e := encodeOps(ops, 1<<16)
	packet := e.Buffer()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var d Decoder
		d.Init(packet)
		for _, o := range ops {
			o.decode(&d)
		}
	}
}
