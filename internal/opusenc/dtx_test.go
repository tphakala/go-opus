package opusenc

import "testing"

func TestCeltMaxabs16(t *testing.T) {
	tests := []struct {
		name string
		x    []int16
		n    int
		want int32
	}{
		{"mixed sign, negative peak", []int16{3, -5, 2}, 3, 5},
		{"all positive", []int16{1, 2, 3}, 3, 3},
		{"all negative", []int16{-10, -2}, 2, 10},
		{"positive and negative equal", []int16{4, -4}, 2, 4},
		{"n shorter than slice ignores tail", []int16{1, 2, 99}, 2, 2},
		{"n zero is empty window", []int16{7, 8, 9}, 0, 0},
		{"empty slice", []int16{}, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := celtMaxabs16(tc.x, tc.n); got != tc.want {
				t.Errorf("celtMaxabs16(%v, %d) = %d, want %d", tc.x, tc.n, got, tc.want)
			}
		})
	}
}

func TestCeltMaxabs16GuardPanics(t *testing.T) {
	tests := []struct {
		name string
		x    []int16
		n    int
	}{
		{"n exceeds len", []int16{1, 2}, 3},
		{"negative n", []int16{1, 2}, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("celtMaxabs16(%v, %d) did not panic", tc.x, tc.n)
				}
			}()
			celtMaxabs16(tc.x, tc.n)
		})
	}
}

func TestIsDigitalSilence(t *testing.T) {
	silent := make([]int16, 960)
	if !isDigitalSilence(silent, 480, 2) {
		t.Error("all-zero frame reported as non-silent")
	}

	active := make([]int16, 960)
	active[123] = 1
	if isDigitalSilence(active, 480, 2) {
		t.Error("frame with a nonzero sample reported as silent")
	}
}

func TestComputeFrameEnergy(t *testing.T) {
	// All-zero frame has zero energy.
	if got := computeFrameEnergy(make([]int16, 960), 480, 2); got != 0 {
		t.Errorf("computeFrameEnergy(zeros) = %d, want 0", got)
	}

	// Constant amplitude with shift == 0: energy reduces to c*c exactly. For c=100,
	// length=480: shift = IMAX(0, (ilog2(101)<<1)+ilog2(480)-28) = IMAX(0, 12+8-28) = 0,
	// so energy = sum(c*c)/length = c*c = 10000. Same length, mono or stereo.
	const c = int16(100)
	const wantEnergy = int32(10000)
	for _, cfg := range []struct {
		name             string
		frameSize, chans int
	}{
		{"mono 480", 480, 1},
		{"stereo 240", 240, 2},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			pcm := make([]int16, cfg.frameSize*cfg.chans)
			for i := range pcm {
				pcm[i] = c
			}
			if got := computeFrameEnergy(pcm, cfg.frameSize, cfg.chans); got != wantEnergy {
				t.Errorf("computeFrameEnergy(const %d) = %d, want %d", c, got, wantEnergy)
			}
		})
	}
}

func TestComputeFrameEnergyGuardPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("computeFrameEnergy with length exceeding len(pcm) did not panic")
		}
	}()
	// frameSize*channels = 240*2 = 480 > len(pcm) = 100.
	computeFrameEnergy(make([]int16, 100), 240, 2)
}
