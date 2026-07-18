package pcm_test

import (
	"bytes"
	"fmt"

	"github.com/tphakala/go-opus/pcm"
)

// ExampleEncodeInterleaved encodes a complete in-memory PCM buffer to an Ogg Opus
// stream in one call.
func ExampleEncodeInterleaved() {
	// 20 ms of silence at 48 kHz mono (interleaved little-endian int16).
	samples := make([]byte, 48000/50*2)

	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, pcm.Config{SampleRate: 48000, Channels: 1}, samples); err != nil {
		fmt.Println("encode:", err)
		return
	}
	fmt.Println("encoded a non-empty Ogg Opus stream:", buf.Len() > 0)
	// Output: encoded a non-empty Ogg Opus stream: true
}

// ExampleNewDecoder shows the rate a caller must label decoded PCM with. The
// stream was encoded from 16 kHz source audio, but decode output is always 48 kHz:
// read Info().OutputSampleRate, never Info().InputSampleRate.
func ExampleNewDecoder() {
	// Encode 100 ms of 16 kHz mono, then decode it back.
	source := make([]byte, 16000/10*2)
	var enc bytes.Buffer
	if err := pcm.EncodeInterleaved(&enc, pcm.Config{SampleRate: 16000, Channels: 1}, source); err != nil {
		fmt.Println("encode:", err)
		return
	}

	d, err := pcm.NewDecoder(bytes.NewReader(enc.Bytes()))
	if err != nil {
		fmt.Println("decode:", err)
		return
	}
	info := d.Info()
	fmt.Println("input rate (informational):", info.InputSampleRate)
	fmt.Println("output rate (label PCM with this):", info.OutputSampleRate)
	// Output:
	// input rate (informational): 16000
	// output rate (label PCM with this): 48000
}
