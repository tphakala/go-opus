//go:build refc

package oracle

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/tphakala/go-opus/internal/rangecoding"
)

// This is the phase-1 gate for internal/rangecoding: the pure-Go range coder is
// driven in lockstep with the real libopus ec_enc/ec_dec (compiled into the
// oracle) and must produce byte-identical output with matching ec_tell /
// ec_tell_frac at every step, round-trip cross-language, and reproduce the
// snapshot/restore/splice maneuver the two phase-4 RDO features perform
// (docs/hard-parts.md 1, plan phase-1 gate).

const ecBufSize = 8192

type ecOpKind uint8

const (
	ecEncode ecOpKind = iota
	ecBitLogp
	ecIcdf
	ecUint
	ecBits
)

// ecOp is one coded symbol, applicable to both the Go and C encoders/decoders.
type ecOp struct {
	kind ecOpKind
	cum  []uint32 // ecEncode: cumulative-frequency table, len nsym+1
	sym  uint32   // ecEncode/ecIcdf: chosen symbol index
	val  int      // ecBitLogp
	logp uint32   // ecBitLogp
	icdf []byte   // ecIcdf
	ftb  uint32   // ecIcdf
	fl   uint32   // ecUint/ecBits
	ft   uint32   // ecUint
	bits uint32   // ecBits
}

func (o ecOp) ft0() uint32 { return o.cum[len(o.cum)-1] }

func (o ecOp) encGo(e *rangecoding.Encoder) {
	switch o.kind {
	case ecEncode:
		e.Encode(o.cum[o.sym], o.cum[o.sym+1], o.ft0())
	case ecBitLogp:
		e.EncBitLogp(o.val, o.logp)
	case ecIcdf:
		e.EncIcdf(int(o.sym), o.icdf, o.ftb)
	case ecUint:
		e.EncUint(o.fl, o.ft)
	case ecBits:
		e.EncBits(o.fl, o.bits)
	}
}

func (o ecOp) encC(e *refEnc) {
	switch o.kind {
	case ecEncode:
		e.encode(o.cum[o.sym], o.cum[o.sym+1], o.ft0())
	case ecBitLogp:
		e.bitLogp(o.val, o.logp)
	case ecIcdf:
		e.icdf(int(o.sym), o.icdf, o.ftb)
	case ecUint:
		e.encUint(o.fl, o.ft)
	case ecBits:
		e.encBits(o.fl, o.bits)
	}
}

// decodeSym decodes an ecEncode op's symbol index from a cumulative frequency:
// the largest s with cum[s] <= fs, since cum is increasing from 0.
func (o ecOp) decodeSym(fs uint32) (fl, fh uint32, sym int) {
	s := 0
	for s+1 < len(o.cum) && fs >= o.cum[s+1] {
		s++
	}
	return o.cum[s], o.cum[s+1], s
}

// decGo decodes the op with the Go decoder and reports whether the decoded value
// matched what was encoded.
func (o ecOp) decGo(d *rangecoding.Decoder) bool {
	switch o.kind {
	case ecEncode:
		fs := d.Decode(o.ft0())
		fl, fh, s := o.decodeSym(fs)
		d.DecUpdate(fl, fh, o.ft0())
		return uint32(s) == o.sym
	case ecBitLogp:
		return d.DecBitLogp(o.logp) == o.val
	case ecIcdf:
		return d.DecIcdf(o.icdf, o.ftb) == int(o.sym)
	case ecUint:
		return d.DecUint(o.ft) == o.fl
	case ecBits:
		return d.DecBits(o.bits) == o.fl
	}
	return false
}

// decC decodes the op with the C decoder and reports whether it matched.
func (o ecOp) decC(d *refDec) bool {
	switch o.kind {
	case ecEncode:
		fs := d.decode(o.ft0())
		fl, fh, s := o.decodeSym(fs)
		d.update(fl, fh, o.ft0())
		return uint32(s) == o.sym
	case ecBitLogp:
		return d.bitLogp(o.logp) == o.val
	case ecIcdf:
		return d.icdf(o.icdf, o.ftb) == int(o.sym)
	case ecUint:
		return d.decUint(o.ft) == o.fl
	case ecBits:
		return d.decBits(o.bits) == o.fl
	}
	return false
}

func genECOps(r *rand.Rand, n int) []ecOp {
	ops := make([]ecOp, 0, n)
	for range n {
		switch r.IntN(5) {
		case 0:
			nsym := 2 + r.IntN(6)
			cum := make([]uint32, nsym+1)
			var acc uint32
			for i := range nsym {
				acc += 1 + uint32(r.IntN(64))
				cum[i+1] = acc
			}
			ops = append(ops, ecOp{kind: ecEncode, cum: cum, sym: uint32(r.IntN(nsym))})
		case 1:
			ops = append(ops, ecOp{kind: ecBitLogp, val: r.IntN(2), logp: uint32(1 + r.IntN(15))})
		case 2:
			ftb := uint32(3 + r.IntN(6))
			total := uint32(1) << ftb
			nsym := 2 + r.IntN(6)
			if nsym > int(total) {
				nsym = int(total)
			}
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
			ops = append(ops, ecOp{kind: ecIcdf, icdf: icdf, ftb: ftb, sym: uint32(r.IntN(nsym))})
		case 3:
			var ft uint32
			if r.IntN(2) == 0 {
				ft = uint32(2 + r.IntN(255))
			} else {
				ft = uint32(257 + r.IntN(1<<20))
			}
			ops = append(ops, ecOp{kind: ecUint, fl: uint32(r.IntN(int(ft))), ft: ft})
		case 4:
			bits := uint32(1 + r.IntN(24))
			ops = append(ops, ecOp{kind: ecBits, fl: r.Uint32() & ((1 << bits) - 1), bits: bits})
		}
	}
	return ops
}

// TestRangeCoderEncodeDifferential drives the Go and C encoders in lockstep over
// random op streams, asserting tell/tell_frac/rng/val agree after every symbol
// and the final byte buffers are identical. This is the byte-exactness gate.
func TestRangeCoderEncodeDifferential(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x9E3779B9))
		ops := genECOps(r, 200)

		var g rangecoding.Encoder
		g.Init(make([]byte, ecBufSize))
		c := newRefEnc(ecBufSize)

		for i, o := range ops {
			o.encGo(&g)
			o.encC(c)
			if g.Tell() != c.tell() || g.TellFrac() != c.tellFrac() ||
				g.Rng() != c.rng() || g.Val() != c.val() {
				t.Fatalf("seed %d op %d (kind %d): state mismatch\n"+
					"  go: tell=%d tellFrac=%d rng=%d val=%d\n"+
					"   c: tell=%d tellFrac=%d rng=%d val=%d",
					seed, i, o.kind,
					g.Tell(), g.TellFrac(), g.Rng(), g.Val(),
					c.tell(), c.tellFrac(), c.rng(), c.val())
			}
		}

		g.EncDone()
		c.done()

		if g.RangeBytes() != c.rangeBytes() {
			t.Fatalf("seed %d: range bytes differ go=%d c=%d", seed, g.RangeBytes(), c.rangeBytes())
		}
		if g.Rng() != c.rng() {
			t.Errorf("seed %d: final rng differ go=%d c=%d", seed, g.Rng(), c.rng())
		}
		if (g.Error() != 0) != (c.errCode() != 0) {
			t.Errorf("seed %d: error flag differ go=%d c=%d", seed, g.Error(), c.errCode())
		}
		gb := g.Buffer()
		cb := c.bytesN(ecBufSize)
		if !bytes.Equal(gb, cb) {
			// Localize the first differing byte.
			for k := range gb {
				if gb[k] != cb[k] {
					t.Fatalf("seed %d: buffer differs at byte %d: go=%#02x c=%#02x",
						seed, k, gb[k], cb[k])
				}
			}
			t.Fatalf("seed %d: buffers differ in length", seed)
		}
		c.close()
	}
}

// TestRangeCoderCrossDecode encodes with one implementation and decodes the raw
// bytes with the other, in both directions, verifying every symbol and the
// enc/dec final-range agreement that pins the two together.
func TestRangeCoderCrossDecode(t *testing.T) {
	for seed := uint64(1); seed <= 100; seed++ {
		r := rand.New(rand.NewPCG(seed, 0xA5A5A5A5))
		ops := genECOps(r, 150)

		// Encode with Go.
		var g rangecoding.Encoder
		g.Init(make([]byte, ecBufSize))
		for _, o := range ops {
			o.encGo(&g)
		}
		g.EncDone()
		goBytes := append([]byte(nil), g.Buffer()...)

		// Decode Go's bytes with the C decoder.
		cd := newRefDec(goBytes)
		for i, o := range ops {
			if !o.decC(cd) {
				t.Fatalf("seed %d: C decode of Go bytes wrong at op %d (kind %d)", seed, i, o.kind)
			}
		}
		if cd.errCode() != 0 {
			t.Errorf("seed %d: C decoder error %d", seed, cd.errCode())
		}
		if cd.rng() != g.Rng() {
			t.Errorf("seed %d: C dec final rng %d != Go enc final rng %d", seed, cd.rng(), g.Rng())
		}
		cd.close()

		// Encode the same stream with C, decode with the Go decoder.
		ce := newRefEnc(ecBufSize)
		for _, o := range ops {
			o.encC(ce)
		}
		ce.done()
		cBytes := ce.bytesN(ecBufSize)
		var gd rangecoding.Decoder
		gd.Init(cBytes)
		for i, o := range ops {
			if !o.decGo(&gd) {
				t.Fatalf("seed %d: Go decode of C bytes wrong at op %d (kind %d)", seed, i, o.kind)
			}
		}
		if gd.Error() != 0 {
			t.Errorf("seed %d: Go decoder error %d", seed, gd.Error())
		}
		if gd.Rng() != ce.rng() {
			t.Errorf("seed %d: Go dec final rng %d != C enc final rng %d", seed, gd.Rng(), ce.rng())
		}
		ce.close()
	}
}

// genECOpsNoRaw generates only range-coder ops (ec_encode / ec_enc_bit_logp /
// ec_enc_icdf), never raw bits. Coarse-energy coding is Laplace/icdf only, so it
// never touches the raw-bit tail and can splice a head-only window.
func genECOpsNoRaw(r *rand.Rand, n int) []ecOp {
	ops := make([]ecOp, 0, n)
	for len(ops) < n {
		o := genECOps(r, 1)[0]
		if o.kind == ecUint || o.kind == ecBits {
			continue
		}
		ops = append(ops, o)
	}
	return ops
}

// runSpliceDance performs the save/restore/splice RDO maneuver identically on the
// pure-Go and libopus encoders and asserts their final buffers agree with each
// other AND with a straight encode of prefix+candA. The saved byte window runs
// from the START offset to windowEnd: the coarse-energy dance (quant_bands.c)
// uses windowEnd = candA's offs (head only), the theta dance (bands.c) uses
// windowEnd = storage (head + raw-bit tail). This is the phase-4 RDO gate
// (docs/hard-parts.md 1).
func runSpliceDance(t *testing.T, seed uint64, prefix, candA, candB []ecOp, fullWindow bool) {
	t.Helper()

	// Reference: straight prefix+candA.
	var ref rangecoding.Encoder
	ref.Init(make([]byte, ecBufSize))
	for _, o := range prefix {
		o.encGo(&ref)
	}
	for _, o := range candA {
		o.encGo(&ref)
	}
	ref.EncDone()
	want := ref.Buffer()

	// --- Go dance ---
	var g rangecoding.Encoder
	g.Init(make([]byte, ecBufSize))
	for _, o := range prefix {
		o.encGo(&g)
	}
	startOffs := int(g.RangeBytes())
	gStart := g // enc_start_state = *enc
	for _, o := range candA {
		o.encGo(&g)
	}
	windowEnd := int(g.RangeBytes()) // coarse: nintra_bytes
	if fullWindow {
		windowEnd = len(g.Buffer()) // theta: storage
	}
	// bytes_save = OPUS_COPY of buf[start_offs : windowEnd]. When empty this is
	// the ALLOC_NONE (save_bytes==0) sentinel, spelled as a nil slice.
	gSave := append([]byte(nil), g.Buffer()[startOffs:windowEnd]...)
	gAState := g // enc_intra_state
	g = gStart   // *enc = enc_start_state
	for _, o := range candB {
		o.encGo(&g)
	}
	g = gAState // *enc = enc_intra_state
	copy(g.Buffer()[startOffs:windowEnd], gSave)
	g.EncDone()

	// --- C dance (identical) ---
	c := newRefEnc(ecBufSize)
	for _, o := range prefix {
		o.encC(c)
	}
	cStartOffs := int(c.rangeBytes())
	cStart := c.getState()
	for _, o := range candA {
		o.encC(c)
	}
	cWindowEnd := int(c.rangeBytes())
	if fullWindow {
		cWindowEnd = ecBufSize
	}
	cSave := c.copyRegion(cStartOffs, cWindowEnd-cStartOffs)
	cAState := c.getState()
	c.setState(cStart)
	for _, o := range candB {
		o.encC(c)
	}
	c.setState(cAState)
	c.writeRegion(cStartOffs, cSave)
	c.done()
	defer c.close()

	if startOffs != cStartOffs || windowEnd != cWindowEnd {
		t.Fatalf("seed %d: window mismatch go=[%d:%d] c=[%d:%d]",
			seed, startOffs, windowEnd, cStartOffs, cWindowEnd)
	}
	gb := g.Buffer()
	cb := c.bytesN(ecBufSize)
	if !bytes.Equal(gb, cb) {
		t.Fatalf("seed %d: Go dance and C dance buffers differ", seed)
	}
	if !bytes.Equal(gb, want) {
		t.Fatalf("seed %d: spliced buffer != straight prefix+candA reference", seed)
	}
	if g.Rng() != c.rng() || g.RangeBytes() != c.rangeBytes() {
		t.Errorf("seed %d: post-splice state differs rng %d/%d bytes %d/%d",
			seed, g.Rng(), c.rng(), g.RangeBytes(), c.rangeBytes())
	}
}

func TestRangeCoderSnapshotSplice(t *testing.T) {
	// Coarse-energy style: Laplace/icdf candidates, head-only window.
	t.Run("coarse_head_only", func(t *testing.T) {
		for seed := uint64(1); seed <= 100; seed++ {
			r := rand.New(rand.NewPCG(seed, 0x1234))
			runSpliceDance(t, seed,
				genECOpsNoRaw(r, 30), genECOpsNoRaw(r, 40), genECOpsNoRaw(r, 25), false)
		}
	})
	// Theta style: arbitrary candidates (incl. raw bits), full head+tail window.
	t.Run("theta_full_window", func(t *testing.T) {
		for seed := uint64(1); seed <= 100; seed++ {
			r := rand.New(rand.NewPCG(seed, 0x5678))
			runSpliceDance(t, seed,
				genECOps(r, 30), genECOps(r, 40), genECOps(r, 25), true)
		}
	})
	// ALLOC_NONE sentinel: candA emits nothing, so the saved window is empty
	// (nil), and the restore relies purely on the value-copy state snapshot.
	t.Run("alloc_none", func(t *testing.T) {
		r := rand.New(rand.NewPCG(9, 9))
		runSpliceDance(t, 0, nil, nil, genECOps(r, 25), false)
	})
}

// TestRangeCoderPatchInitialBits checks ec_enc_patch_initial_bits matches C:
// several power-of-two symbols are encoded, the first nbits are patched, then the
// rest of the stream is encoded and the byte buffers are compared.
func TestRangeCoderPatchInitialBits(t *testing.T) {
	for seed := uint64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewPCG(seed, 0xBEEF))
		nbits := uint32(1 + r.IntN(8))
		patch := uint32(r.IntN(1 << nbits))

		var g rangecoding.Encoder
		g.Init(make([]byte, ecBufSize))
		c := newRefEnc(ecBufSize)

		// Encode a few 8-bit power-of-two symbols so >= nbits bits exist up front.
		for range 3 {
			v := uint32(r.IntN(256))
			g.EncodeBin(v, v+1, 8)
			c.encodeBin(v, v+1, 8)
		}
		g.EncPatchInitialBits(patch, nbits)
		c.patchInitialBits(patch, nbits)

		// Some more ops after the patch.
		ops := genECOps(r, 40)
		for _, o := range ops {
			o.encGo(&g)
			o.encC(c)
		}
		g.EncDone()
		c.done()

		if !bytes.Equal(g.Buffer(), c.bytesN(ecBufSize)) {
			t.Fatalf("seed %d: patch_initial_bits buffers differ (nbits=%d patch=%d)", seed, nbits, patch)
		}
		c.close()
	}
}

// TestRangeCoderShrink checks ec_enc_shrink matches C: range symbols plus a raw
// bit tail are encoded, the buffer is shrunk to a tight size, then finished; the
// resulting bytes must be byte-identical.
func TestRangeCoderShrink(t *testing.T) {
	const size = 512
	for seed := uint64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x5417))

		var g rangecoding.Encoder
		g.Init(make([]byte, size))
		c := newRefEnc(size)

		// A modest stream: some range symbols and a raw-bit tail.
		ops := genECOps(r, 30)
		for _, o := range ops {
			o.encGo(&g)
			o.encC(c)
		}

		// Shrink to a size that still holds offs+end_offs, read from C's state.
		newSize := c.offs() + c.endOffs() + uint32(1+r.IntN(8))
		if newSize > size {
			newSize = size
		}
		g.EncShrink(newSize)
		c.shrink(newSize)
		g.EncDone()
		c.done()

		ns := int(newSize)
		if !bytes.Equal(g.Buffer()[:ns], c.bytesN(ns)) {
			t.Fatalf("seed %d: shrink buffers differ (newSize=%d)", seed, ns)
		}
		c.close()
	}
}

// goldenStream applies a fixed, hand-written op sequence exercising every
// primitive and both fl==0 / fl>0 branches. The pure-Go golden test in
// internal/rangecoding runs the identical sequence; this test confirms the Go
// bytes equal C's and prints them so the embedded golden can be refreshed.
func goldenEncodeC(e *refEnc) {
	e.encode(2, 5, 8)
	e.encode(0, 3, 8)
	e.bitLogp(1, 3)
	e.bitLogp(0, 4)
	e.encodeBin(1, 3, 4)
	e.encUint(1234, 5000)
	e.encUint(3, 4)
	e.icdf(1, []byte{240, 128, 32, 0}, 8)
	e.icdf(0, []byte{240, 128, 32, 0}, 8)
	e.encBits(0xABCD, 16)
}

func goldenEncodeGo(e *rangecoding.Encoder) {
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

func TestRangeCoderGolden(t *testing.T) {
	c := newRefEnc(ecBufSize)
	goldenEncodeC(c)
	c.done()
	cBytes := c.bytesN(int(c.rangeBytes()))
	cFinalRange := c.rng()
	c.close()

	var g rangecoding.Encoder
	g.Init(make([]byte, ecBufSize))
	goldenEncodeGo(&g)
	g.EncDone()
	goBytes := g.Buffer()[:g.RangeBytes()]

	if !bytes.Equal(goBytes, cBytes) {
		t.Fatalf("golden: Go bytes %x != C bytes %x", goBytes, cBytes)
	}
	if g.Rng() != cFinalRange {
		t.Errorf("golden: final range go=%d c=%d", g.Rng(), cFinalRange)
	}
	t.Logf("golden stream: %d bytes, finalRange=%d", len(cBytes), cFinalRange)
	t.Logf("golden bytes = %#v", cBytes)
}
