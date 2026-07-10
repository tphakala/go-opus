// Copyright (c) 2011-2012 Xiph.Org Foundation, Mozilla Corporation
// Written by Jean-Marc Valin and Timothy B. Terriberry
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
//
//   - Redistributions of source code must retain the above copyright
//     notice, this list of conditions and the following disclaimer.
//
//   - Redistributions in binary form must reproduce the above copyright
//     notice, this list of conditions and the following disclaimer in the
//     documentation and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// ``AS IS'' AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER
// OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
// EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
// PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
// PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
// LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
// NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
//
// This file is a faithful Go port of libopus src/opus_compare.c @ v1.6.1
// (https://gitlab.xiph.org/xiph/opus, tag v1.6.1). The port preserves the
// original algorithm structure, constants, band table, weighting filters,
// self-contained naive O(N^2) DFT (band_energy), NMR-style error accumulation,
// and the final quality metric / PASS-FAIL threshold exactly, so it stays
// diffable against the C and can gate RFC 6716 conformance with no C toolchain.
// float32 (C float) versus float64 (C double) usage is mirrored throughout so
// the metric tracks the C reference to within a small float epsilon.

package opuscompare

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Sentinel errors mirror the fatal conditions opus_compare.c reports on stderr
// before returning EXIT_FAILURE.
var (
	// ErrSampleCountMismatch corresponds to "Sample counts do not match".
	ErrSampleCountMismatch = errors.New("sample counts do not match")
	// ErrInsufficientData corresponds to "Insufficient sample data".
	ErrInsufficientData = errors.New("insufficient sample data")
	// ErrInvalidRate corresponds to the rejected -r rate argument.
	ErrInvalidRate = errors.New("sampling rate must be 8000, 12000, 16000, 24000, or 48000")
	// ErrInvalidChannels is returned for a Channels value other than 0, 1, or 2.
	ErrInvalidChannels = errors.New("channels must be 1 or 2")
)

// opusPI is the float32 constant OPUS_PI from opus_compare.c. It is deliberately
// float32 (not math.Pi) so the window/twiddle angles are computed exactly as the
// C does before being promoted to double for cos/sin.
const opusPI float32 = 3.14159265

const (
	nBands      = 21  // NBANDS
	nFreqs      = 240 // NFREQS
	testWinSize = 480 // TEST_WIN_SIZE
	testWinStep = 120 // TEST_WIN_STEP
)

// bands are the Bark-derived CELT band edges (BANDS[] in opus_compare.c) on which
// the pseudo-NMR is computed.
var bands = [nBands + 1]int{
	0, 2, 4, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 40, 48, 56, 68, 80, 96, 120, 156, 200,
}

// Config selects the comparison mode, mirroring opus_compare.c's -s and -r flags.
type Config struct {
	// Channels is the comparison channel count: 1 (mono) or 2 (stereo). Zero
	// defaults to 1, matching opus_compare's default (no -s flag). The RFC 6716
	// conformance gate decodes to stereo and uses Channels: 2.
	//
	// Note: as in the C, the reference PCM is always interpreted as interleaved
	// stereo (2 channels). When Channels == 1 the reference is downmixed to mono
	// on the fly (0.5*(L+R)) before comparison; the test PCM is read with
	// Channels channels.
	Channels int
	// Rate is the test PCM sample rate in Hz: one of 8000, 12000, 16000, 24000,
	// or 48000. Zero defaults to 48000. Rates below 48000 mirror opus_compare's
	// -r flag: the test spectrum is compared against the reference decimated by
	// 48000/Rate, over a reduced number of bands.
	Rate int
}

// Result holds the outcome of a comparison, matching what opus_compare.c prints.
type Result struct {
	// Quality is the Opus quality metric Q in percent (100 == perfect). It is
	// computed as a C float and widened here.
	Quality float64
	// Passed reports whether the test PCM passes, i.e. Q >= 0, the exact
	// threshold opus_compare.c uses (Q < 0 => "Test vector FAILS").
	Passed bool
	// WeightedError is the internal weighted error (err) reported alongside Q.
	WeightedError float64
	// Frames is the number of analysis frames (nframes).
	Frames int
}

// Compare reads interleaved little-endian 16-bit PCM from ref and test and
// compares them, returning the Opus quality metric and PASS/FAIL. ref is
// interpreted as interleaved stereo regardless of cfg.Channels (see Config).
func Compare(ref, test io.Reader, cfg Config) (Result, error) {
	refBytes, err := io.ReadAll(ref)
	if err != nil {
		return Result{}, fmt.Errorf("opuscompare: reading reference: %w", err)
	}
	testBytes, err := io.ReadAll(test)
	if err != nil {
		return Result{}, fmt.Errorf("opuscompare: reading test: %w", err)
	}
	return CompareInt16(bytesToInt16LE(refBytes), bytesToInt16LE(testBytes), cfg)
}

// CompareInt16 compares reference and test PCM supplied as interleaved 16-bit
// samples. ref is interpreted as interleaved stereo regardless of cfg.Channels
// (see Config); test is interleaved cfg.Channels-channel PCM.
func CompareInt16(ref, test []int16, cfg Config) (Result, error) {
	nchannels := cfg.Channels
	if nchannels == 0 {
		nchannels = 1
	}
	if nchannels != 1 && nchannels != 2 {
		return Result{}, ErrInvalidChannels
	}
	rate := cfg.Rate
	if rate == 0 {
		rate = 48000
	}
	downsample, ybands, yfreqs, err := resolveRate(rate)
	if err != nil {
		return Result{}, err
	}

	// Read in the reference (always stereo) and, for a mono comparison, downmix.
	xlength := len(ref) / 2
	x := make([]float32, xlength*2)
	for i := 0; i < xlength*2; i++ {
		x[i] = float32(ref[i])
	}
	if nchannels == 1 {
		for xi := 0; xi < xlength; xi++ {
			x[xi] = float32(.5 * float64(x[2*xi]+x[2*xi+1]))
		}
		x = x[:xlength]
	}

	// Read the test PCM with nchannels channels.
	ylength := len(test) / nchannels
	y := make([]float32, ylength*nchannels)
	for i := 0; i < ylength*nchannels; i++ {
		y[i] = float32(test[i])
	}

	if xlength != ylength*downsample {
		return Result{}, fmt.Errorf("%w (%d != %d)", ErrSampleCountMismatch, xlength, ylength*downsample)
	}
	if xlength < testWinSize {
		return Result{}, fmt.Errorf("%w (%d < %d)", ErrInsufficientData, xlength, testWinSize)
	}

	nframes := (xlength - testWinSize + testWinStep) / testWinStep
	xb := make([]float32, nframes*nBands*nchannels)
	xps := make([]float32, nframes*nFreqs*nchannels)
	yps := make([]float32, nframes*yfreqs*nchannels)
	// Compute the per-band spectral energy of the original signal and the error.
	bandEnergy(xb, xps, bands[:], nBands, x, nchannels, nframes,
		testWinSize, testWinStep, 1)
	bandEnergy(nil, yps, bands[:], ybands, y, nchannels, nframes,
		testWinSize/downsample, testWinStep/downsample, downsample)

	applyWeighting(xb, xps, yps, nchannels, nframes, ybands, yfreqs)
	averageFrames(xps, yps, nchannels, nframes, ybands, yfreqs)

	// If working at a lower sampling rate, don't take into account the last
	// 300 Hz to allow for different transition bands. For 12 kHz, we don't skip
	// anything, because the last band already skips 400 Hz.
	var maxCompare int
	switch rate {
	case 48000:
		maxCompare = bands[nBands]
	case 12000:
		maxCompare = bands[ybands]
	default:
		maxCompare = bands[ybands] - 3
	}

	e := 0.0
	for xi := 0; xi < nframes; xi++ {
		var ef float64
		for bi := 0; bi < ybands; bi++ {
			var eb float64
			for xj := bands[bi]; xj < bands[bi+1] && xj < maxCompare; xj++ {
				for ci := 0; ci < nchannels; ci++ {
					re := yps[(xi*yfreqs+xj)*nchannels+ci] / xps[(xi*nFreqs+xj)*nchannels+ci]
					im := float32(float64(re) - math.Log(float64(re)) - 1)
					// Make comparison less sensitive around the SILK/CELT
					// cross-over to allow for mode freedom in the filters.
					if xj >= 79 && xj <= 81 {
						im *= 0.1
					}
					if xj == 80 {
						im *= 0.1
					}
					eb += float64(im)
				}
			}
			eb /= float64((bands[bi+1] - bands[bi]) * nchannels)
			ef += eb * eb
		}
		// Using a fixed normalization value means we're willing to accept
		// slightly lower quality for lower sampling rates.
		ef /= nBands
		ef *= ef
		e += ef * ef
	}

	e = math.Pow(e/float64(nframes), 1.0/16)
	q := float32(100 * (1 - 0.5*math.Log(1+e)/math.Log(1.13)))
	return Result{
		Quality:       float64(q),
		Passed:        q >= 0,
		WeightedError: e,
		Frames:        nframes,
	}, nil
}

// resolveRate maps a sample rate to the decimation factor and reduced band /
// frequency counts, mirroring the -r handling in main().
func resolveRate(rate int) (downsample, ybands, yfreqs int, err error) {
	switch rate {
	case 8000, 12000, 16000, 24000, 48000:
	default:
		return 0, 0, 0, ErrInvalidRate
	}
	downsample = 48000 / rate
	ybands = nBands
	switch rate {
	case 8000:
		ybands = 13
	case 12000:
		ybands = 15
	case 16000:
		ybands = 17
	case 24000:
		ybands = 19
	}
	yfreqs = nFreqs / downsample
	return downsample, ybands, yfreqs, nil
}

// bandEnergy is a direct port of band_energy(): it windows each frame, computes a
// naive DFT via precomputed cosine/sine tables, and accumulates a floored power
// spectrum into ps (and, when out != nil, per-band average energy into out).
func bandEnergy(out, ps []float32, bandTab []int, nbands int, in []float32,
	nchannels, nframes, windowSz, step, downsample int) {
	window := make([]float32, windowSz)
	c := make([]float32, windowSz)
	s := make([]float32, windowSz)
	xbuf := make([]float32, nchannels*windowSz)
	psSz := windowSz / 2
	for xj := 0; xj < windowSz; xj++ {
		window[xj] = 0.5 - 0.5*float32(math.Cos(float64((2*opusPI/float32(windowSz-1))*float32(xj))))
	}
	for xj := 0; xj < windowSz; xj++ {
		c[xj] = float32(math.Cos(float64((2 * opusPI / float32(windowSz)) * float32(xj))))
	}
	for xj := 0; xj < windowSz; xj++ {
		s[xj] = float32(math.Sin(float64((2 * opusPI / float32(windowSz)) * float32(xj))))
	}
	for xi := 0; xi < nframes; xi++ {
		for ci := 0; ci < nchannels; ci++ {
			for xk := 0; xk < windowSz; xk++ {
				xbuf[ci*windowSz+xk] = window[xk] * in[(xi*step+xk)*nchannels+ci]
			}
		}
		xj := 0
		for bi := 0; bi < nbands; bi++ {
			var p [2]float32
			for ; xj < bandTab[bi+1]; xj++ {
				for ci := 0; ci < nchannels; ci++ {
					ti := 0
					var re, im float32
					for xk := 0; xk < windowSz; xk++ {
						re += c[ti] * xbuf[ci*windowSz+xk]
						im -= s[ti] * xbuf[ci*windowSz+xk]
						ti += xj
						if ti >= windowSz {
							ti -= windowSz
						}
					}
					re *= float32(downsample)
					im *= float32(downsample)
					ps[(xi*psSz+xj)*nchannels+ci] = re*re + im*im + 100000
					p[ci] += ps[(xi*psSz+xj)*nchannels+ci]
				}
			}
			if out != nil {
				out[(xi*nbands+bi)*nchannels] = p[0] / float32(bandTab[bi+1]-bandTab[bi])
				if nchannels == 2 {
					out[(xi*nbands+bi)*nchannels+1] = p[1] / float32(bandTab[bi+1]-bandTab[bi])
				}
			}
		}
	}
}

// applyWeighting ports the per-frame masking filters (frequency masking low->high
// and high->low, temporal masking, stereo cross-talk) and folds the resulting
// band energies back into the reference and test power spectra.
func applyWeighting(xb, xps, yps []float32, nchannels, nframes, ybands, yfreqs int) {
	for xi := 0; xi < nframes; xi++ {
		// Frequency masking (low to high): 10 dB/Bark slope.
		for bi := 1; bi < nBands; bi++ {
			for ci := 0; ci < nchannels; ci++ {
				xb[(xi*nBands+bi)*nchannels+ci] += 0.1 * xb[(xi*nBands+bi-1)*nchannels+ci]
			}
		}
		// Frequency masking (high to low): 15 dB/Bark slope.
		for bi := nBands - 2; bi >= 0; bi-- {
			for ci := 0; ci < nchannels; ci++ {
				xb[(xi*nBands+bi)*nchannels+ci] += 0.03 * xb[(xi*nBands+bi+1)*nchannels+ci]
			}
		}
		if xi > 0 {
			// Temporal masking: -3 dB/2.5ms slope.
			for bi := 0; bi < nBands; bi++ {
				for ci := 0; ci < nchannels; ci++ {
					xb[(xi*nBands+bi)*nchannels+ci] += 0.5 * xb[((xi-1)*nBands+bi)*nchannels+ci]
				}
			}
		}
		// Allowing some cross-talk.
		if nchannels == 2 {
			for bi := 0; bi < nBands; bi++ {
				l := xb[(xi*nBands+bi)*nchannels+0]
				r := xb[(xi*nBands+bi)*nchannels+1]
				xb[(xi*nBands+bi)*nchannels+0] += 0.01 * r
				xb[(xi*nBands+bi)*nchannels+1] += 0.01 * l
			}
		}

		// Apply masking.
		for bi := 0; bi < ybands; bi++ {
			for xj := bands[bi]; xj < bands[bi+1]; xj++ {
				for ci := 0; ci < nchannels; ci++ {
					xps[(xi*nFreqs+xj)*nchannels+ci] += 0.1 * xb[(xi*nBands+bi)*nchannels+ci]
					yps[(xi*yfreqs+xj)*nchannels+ci] += 0.1 * xb[(xi*nBands+bi)*nchannels+ci]
				}
			}
		}
	}
}

// averageFrames ports the "average of consecutive frames" pass that makes the
// comparison slightly less sensitive by summing each bin with its predecessor.
func averageFrames(xps, yps []float32, nchannels, nframes, ybands, yfreqs int) {
	for bi := 0; bi < ybands; bi++ {
		for xj := bands[bi]; xj < bands[bi+1]; xj++ {
			for ci := 0; ci < nchannels; ci++ {
				xtmp := xps[xj*nchannels+ci]
				ytmp := yps[xj*nchannels+ci]
				for xi := 1; xi < nframes; xi++ {
					xtmp2 := xps[(xi*nFreqs+xj)*nchannels+ci]
					ytmp2 := yps[(xi*yfreqs+xj)*nchannels+ci]
					xps[(xi*nFreqs+xj)*nchannels+ci] += xtmp
					yps[(xi*yfreqs+xj)*nchannels+ci] += ytmp
					xtmp = xtmp2
					ytmp = ytmp2
				}
			}
		}
	}
}

// bytesToInt16LE decodes interleaved little-endian 16-bit samples, dropping any
// trailing partial sample, mirroring fread's whole-element behavior.
func bytesToInt16LE(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return out
}
