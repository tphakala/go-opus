package oggopus

import (
	"bytes"
	"errors"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

// toneBase encodes n samples per channel of a sine whose frequency is hz on
// channel 0 and 1.5*hz on channel 1. Steady tones are where the codec state
// rebuilt by an approximate seek is slowest to match a sequential decode, so
// SeekToSample output differs from it on these fixtures.
func toneBase(t *testing.T, ch, frameMS, bitrate, n int, hz float64) []byte {
	t.Helper()
	pcm := make([]int16, n*ch)
	for i := range n {
		for c := range ch {
			pcm[i*ch+c] = int16(16384 * math.Sin(2*math.Pi*hz*(1+0.5*float64(c))*float64(i)/48000))
		}
	}
	return encodeOgg(t, Config{SampleRate: 48000, Channels: ch, Bitrate: bitrate, FrameDurationMS: frameMS}, pcm)
}

// toneStream is toneBase repaged onto pages of at most 3 lacing values, so the
// seek has fine-grained landings.
func toneStream(t *testing.T, ch, frameMS, bitrate, n int, hz float64) []byte {
	t.Helper()
	return repage(t, toneBase(t, ch, frameMS, bitrate, n, hz), &repageOpts{maxSegs: 3})
}

// TestSeekToSampleExactMatchesSequential pins that SeekToSampleExact yields the
// bytes of a sequential decode from the target, after any history of reads, exact
// and approximate seeks: targets inside the pre-skip and first packet, backward and
// forward moves, partially read samples, the end of the stream, and a forward move
// after SeekToSample (which must restart, not continue from its approximate state).
//
// The discriminating targets are ones where SeekToSample from a fresh decoder
// differs from sequential decoding on these fixtures. The control below fails if
// that stops being true, so the test cannot pass by comparing two equal things.
func TestSeekToSampleExactMatchesSequential(t *testing.T) {
	fixtures := []struct {
		name                 string
		ch, frameMS, bitrate int
		hz                   float64
		disc                 [3]int64 // ascending, all above 100000
	}{
		{"stereo20ms", 2, 20, 128000, 1100, [3]int64{70449, 90469, 130509}},
		{"mono60ms", 1, 60, 96000, 1100, [3]int64{73879, 95203, 123635}},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			stream := toneStream(t, fx.ch, fx.frameMS, fx.bitrate, 4*48000, fx.hz)
			want := streamingDecode(t, stream)
			bps := int64(2 * fx.ch)
			length := int64(len(want)) / bps

			// Control: approximate seeking is inexact at every discriminating target.
			for _, target := range fx.disc {
				d := seekDecoder(t, stream)
				if _, err := d.SeekToSample(target); err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(readAll(t, d), want[target*bps:]) {
					t.Fatalf("SeekToSample(%d) equals sequential decoding: fixture no longer distinguishes exact from approximate seeking; re-measure targets", target)
				}
			}

			d := seekDecoder(t, stream)
			// exact seeks to target, reads nread bytes (all of the rest when negative)
			// and compares them with the sequential decode.
			exact := func(target int64, nread int) {
				t.Helper()
				at := min(target, length)
				got, err := d.SeekToSampleExact(target)
				if err != nil || got != at {
					t.Fatalf("SeekToSampleExact(%d) = %d, %v; want %d, nil", target, got, err, at)
				}
				tail := want[at*bps:]
				if nread < 0 || nread > len(tail) {
					nread = len(tail)
				}
				if nread == 0 {
					if _, err := d.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
						t.Fatalf("SeekToSampleExact(%d): Read at the end = %v, want io.EOF", target, err)
					}
					return
				}
				buf := make([]byte, nread)
				if _, err := io.ReadFull(d, buf); err != nil {
					t.Fatalf("SeekToSampleExact(%d): ReadFull(%d): %v", target, nread, err)
				}
				if !bytes.Equal(buf, tail[:nread]) {
					t.Fatalf("SeekToSampleExact(%d): %d bytes differ from sequential decoding, first at byte %d", target, nread, firstDiffByte(buf, tail[:nread]))
				}
			}

			// Inside the pre-skip and the first packet; odd byte counts leave part of a
			// sample behind. Then one sample backward.
			exact(0, 7)
			exact(1, 3)
			exact(311, 1001)
			exact(312, 4)
			exact(960, 5001)
			exact(959, 11)
			// Forward across the approximate seek's landing bound; the same index again
			// after a read that left part of a sample; then the next sample after a
			// whole-sample read (served from the buffered samples).
			exact(70000, 6001)
			exact(70000, 13)
			exact(70001, 12)
			next := int64(70001) + 12/bps
			exact(next, 5)
			exact(next+5/bps+1, 9)
			// Forward steps over discriminating targets without reading the rest.
			exact(fx.disc[0], 20001)
			exact(fx.disc[1], 20001)
			// A forward move after an approximate seek must restart.
			if _, err := d.SeekToSample(100000); err != nil {
				t.Fatal(err)
			}
			exact(fx.disc[2], -1)
			// The end of the stream, past it, and back.
			exact(length-1, -1)
			exact(length, -1)
			exact(length+3, -1)
			exact(5, 21)
		})
	}
}

// TestSeekToSampleExactRestartsOnlyWhenNeeded pins when SeekToSampleExact decodes
// forward and when it restarts from the first audio page. The restart is the only
// path that seeks the source to the stream start, so the recorded Seek calls show
// which one ran, which a content comparison cannot (both are bit-identical).
func TestSeekToSampleExactRestartsOnlyWhenNeeded(t *testing.T) {
	stream := toneStream(t, 2, 20, 128000, 4*48000, 1100)
	want := streamingDecode(t, stream)
	const bps = 4

	newDec := func(t *testing.T, stream []byte) (*Decoder, *flakySource) {
		t.Helper()
		src := newFlaky(stream)
		d, err := NewDecoder(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Length(); err != nil { // build the index before recording
			t.Fatal(err)
		}
		return d, src
	}
	check := func(t *testing.T, d *Decoder, target int64) {
		t.Helper()
		got, err := d.SeekToSampleExact(target)
		if err != nil || got != target {
			t.Fatalf("SeekToSampleExact(%d) = %d, %v", target, got, err)
		}
		buf := make([]byte, 20001)
		if _, err := io.ReadFull(d, buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, want[target*bps:target*bps+int64(len(buf))]) {
			t.Fatalf("SeekToSampleExact(%d): output differs from sequential decoding", target)
		}
	}

	t.Run("forward after a read decodes forward, backward restarts", func(t *testing.T) {
		d, src := newDec(t, stream)
		if _, err := io.ReadFull(d, make([]byte, 10001)); err != nil {
			t.Fatal(err)
		}
		src.startSeek = nil
		check(t, d, 100000)
		if len(src.startSeek) != 0 {
			t.Fatalf("forward seek restarted: source seeks to %v", src.startSeek)
		}
		check(t, d, 50000)
		if !slices.Equal(src.startSeek, []int64{0}) {
			t.Fatalf("backward seek: source seeks to %v, want [0]", src.startSeek)
		}
	})
	t.Run("after SeekToSample restarts", func(t *testing.T) {
		d, src := newDec(t, stream)
		if _, err := d.SeekToSample(120000); err != nil {
			t.Fatal(err)
		}
		src.startSeek = nil
		check(t, d, 140000)
		if !slices.Contains(src.startSeek, 0) {
			t.Fatalf("seek after SeekToSample did not restart: source seeks to %v", src.startSeek)
		}
	})
	t.Run("after a failed seek restarts", func(t *testing.T) {
		d, src := newDec(t, stream)
		src.failAll = true
		if _, err := d.SeekToSample(150000); !errors.Is(err, errInjected) {
			t.Fatalf("SeekToSample = %v, want the injected failure", err)
		}
		if _, err := d.Read(make([]byte, 4)); !errors.Is(err, errInjected) {
			t.Fatalf("Read after the failed seek = %v, want the latched failure", err)
		}
		src.failAll = false
		src.startSeek = nil
		check(t, d, 100000)
		if !slices.Contains(src.startSeek, 0) {
			t.Fatalf("seek after a failed seek did not restart: source seeks to %v", src.startSeek)
		}
	})
	t.Run("a failed restart is latched and the next seek recovers", func(t *testing.T) {
		d, src := newDec(t, stream)
		if _, err := io.ReadFull(d, make([]byte, 10001)); err != nil {
			t.Fatal(err)
		}
		src.failAll = true
		if _, err := d.SeekToSampleExact(100); !errors.Is(err, errInjected) {
			t.Fatalf("SeekToSampleExact = %v, want the injected failure", err)
		}
		if _, err := d.Read(make([]byte, 4)); !errors.Is(err, errInjected) {
			t.Fatalf("Read after the failed seek = %v, want the latched failure", err)
		}
		src.failAll = false
		check(t, d, 60000)
	})
	t.Run("after a read error restarts and meets the same error", func(t *testing.T) {
		mono := toneStream(t, 1, 20, 64000, 2*48000, 1000)
		cr, pkts := readContainer(t, bytes.NewReader(mono))
		const badAt = 60
		// A code 3 packet that announces one frame and padding longer than the packet
		// has a duration (it comes from the TOC and the frame count alone) but fails
		// to parse when decoded.
		pkts[badAt] = []byte{pkts[badAt][0] | 3, 0x41, 0xFF}
		var buf bytes.Buffer
		cw, err := newContainerWriter(&buf, 1, cr.head, cr.tags)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkts {
			if err := cw.writePacket(p, 960); err != nil {
				t.Fatal(err)
			}
		}
		if err := cw.close(cr.finalGranule - int64(cr.head.preSkip)); err != nil {
			t.Fatal(err)
		}
		// A page with a bad CRC fails in the container reader instead of the codec.
		offs, _, _ := pageMetaOf(t, mono)
		damaged := bytes.Clone(mono)
		damaged[offs[len(offs)/2+1]-2] ^= 0xff

		for name, tc := range map[string]struct {
			stream  []byte
			wantErr error // nil accepts any error
		}{
			"undecodable packet": {buf.Bytes(), opus.ErrInvalidPacket},
			"damaged page":       {damaged, nil},
		} {
			t.Run(name, func(t *testing.T) {
				_, seqErr := io.ReadAll(seekDecoder(t, tc.stream))
				d := seekDecoder(t, tc.stream)
				_, err := io.ReadAll(d)
				if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
					t.Fatalf("Read = %v, want an error (wrapping %v when not nil)", err, tc.wantErr)
				}
				if seqErr == nil || seqErr.Error() != err.Error() {
					t.Fatalf("sequential decode = %v, Read = %v", seqErr, err)
				}
				// Past the failure. Continuing from the state the failed packet left would
				// succeed here; a decoder built fresh fails at the same place.
				_, err = d.SeekToSampleExact(int64(badAt+10) * 960)
				if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
					t.Fatalf("SeekToSampleExact past the failure = %v, want an error (wrapping %v when not nil)", err, tc.wantErr)
				}
			})
		}
	})
}
