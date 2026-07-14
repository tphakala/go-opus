// Encoder scratch pool. libopus takes every per-frame working buffer off the C
// stack with VARDECL(x,n) / ALLOC(x,n,T) (alloca): free to obtain, reclaimed on
// frame pop. The transliteration mapped each ALLOC to a Go make(), which is a heap
// allocation — 501 objects and 145 KB per stereo 20 ms frame. This file holds them
// on the Encoder instead, so a steady-state Encode allocates nothing.
//
// THE ZEROING CONTRACT — the one thing that can silently break bit-exactness here.
// Three behaviours must not be confused:
//
//	C's stack ALLOC   is UNINITIALISED: it carries whatever the last stack frame left.
//	Go's make()       ZEROES.
//	A pooled buffer   carries THE PREVIOUS FRAME'S VALUES.
//
// Every site that draws from this scratch was audited to write its entire read
// window before reading it, which collapses those three behaviours into one. That
// is not luck: libopus is valgrind-clean, so a plain ALLOC it reads before writing
// would be reading uninitialised stack, and the codec would not be deterministic.
// The two sites where C does NOT rely on that — because C itself issues an
// OPUS_CLEAR — keep an explicit clear at the point of use: surround_dynalloc
// (celt_encoder.c:2107) and importance (whose read window in tf_analysis exceeds
// dynalloc_analysis' write window when start>0). Both are commented there.
//
// NOT SAFE FOR CONCURRENT USE, exactly like the Encoder that owns it (one per
// goroutine, as documented on EncodeWithEC). That is why the buffers need neither a
// mutex nor a sync.Pool: C gets the same isolation for free from having one stack
// per thread, and locking here would be pure overhead.

package celt

// alloc is the Go stand-in for ALLOC(x, n, T): it hands out an n-element window of
// a pooled buffer, growing the backing array only when a call asks for more than
// any previous call did. Sizes are a function of (mode, channels, frameSize) alone,
// so after the first largest-frame Encode no call reallocates.
//
// The window is NOT zeroed — see the zeroing contract in the file comment.
func alloc[T any](p *[]T, n int) []T {
	if cap(*p) < n {
		*p = make([]T, n)
	}
	*p = (*p)[:n]
	return *p
}

// scratch pools one Encoder's VARDECL buffers. Fields are named after the C
// variable they stand in for and grouped by the function that declares them; two
// sites share a field only where their lifetimes provably cannot overlap.
type scratch struct {
	// celt_encode_with_ec (celt_encoder.c:1726). All live at once across the frame.
	in               []int32 // :1968  CC*(N+overlap)
	freq             []int32 // :2059  CC*N
	bandE            []int32 // :2060  nbEBands*CC
	bandLogE         []int32 // :2061  nbEBands*CC
	bandLogE2        []int32 // :2074  C*nbEBands
	surroundDynalloc []int32 // :2106  C*nbEBands   (explicitly cleared; see below)
	X                []int32 // :2238  C*N
	offsets          []int   // :2245  nbEBands
	importance       []int   // :2246  nbEBands     (explicitly cleared; see below)
	spreadWeight     []int   // :2247  nbEBands
	tfRes            []int   // :2255  nbEBands
	energyErr        []int32 // :2290  C*nbEBands   (C: error[])
	caps             []int   // :2356  nbEBands
	fineQuant        []int   // :2591  nbEBands
	pulses           []int   // :2592  nbEBands
	finePriority     []int   // :2593  nbEBands
	collapseMasks    []byte  // :2646  C*nbEBands

	// transient_analysis (:267) / tone_detect (:1362). Sequential, but kept apart:
	// sharing would silently couple two independent analyses if one ever nested.
	transientTmp []int16 // :283   N+overlap
	toneX        []int16 // :1368  N

	// run_prefilter (:1404) and the pitch analysis it drives.
	pre      []int32    // :1415  CC*(N+maxPeriod) — the largest single buffer
	pitchBuf []int16    // :1440  (maxPeriod+N)>>1
	preCh    [2][]int32 // :1441  the 2-element pointer array; a value field, never allocated

	// pitch_search (pitch.c:307) / remove_doubling (pitch.c:454).
	xLp4       []int16 // x_lp4
	yLp4       []int16 // y_lp4
	pitchXcorr []int32 // xcorr, maxPitch>>1
	yyLookup   []int32 // yy_lookup, maxperiod+1

	// _celt_autocorr (celt_lpc.c:284).
	autocorrXX []int16 // xx, n

	// clt_mdct_forward (mdct.c:107). Called up to CC*B times per frame, so a large
	// share of the object count even though each buffer is small.
	mdctF  []int32      // f,  N2
	mdctF2 []kissFFTCpx // f2, N4

	// dynalloc_analysis (celt_encoder.c:1049).
	follower   []int32 // C*nbEBands
	noiseFloor []int32 // C*nbEBands
	bandLogE3  []int32 // nbEBands
	mask       []int32 // nbEBands
	sig        []int32 // nbEBands

	// tf_analysis (celt_encoder.c:663).
	tfMetric []int   // metric, len
	tfTmp    []int32 // tmp,  widest band << LM
	tfTmp1   []int32 // tmp_1
	tfPath0  []int   // path0, len
	tfPath1  []int   // path1, len

	// quant_coarse_energy (quant_bands.c:260).
	oldEBandsIntra []int32 // C*nbEBands
	errorIntra     []int32 // C*nbEBands
	intraBits      []byte  // intra_buf window, bounded by the 1275-byte packet cap

	// clt_compute_allocation (rate.c:377).
	bits1      []int // len
	bits2      []int // len
	thresh     []int // len
	trimOffset []int // trim_offset

	// quant_all_bands (bands.c:1589).
	norm           []int32 // _norm, C*normLen
	lowbandScratch []int32 // only allocated on the encode+resynth path
	XSave          []int32 // the five theta-RDO save buffers, each resynthAlloc
	YSave          []int32
	XSave2         []int32
	YSave2         []int32
	normSave2      []int32
	bytesSave      []byte // 1275, theta-RDO only

	// deinterleave_hadamard (bands.c:574) / interleave_hadamard (bands.c:600). One
	// field for both: each call copies tmp back into X before returning, and
	// quant_band is not recursive (only quant_partition is, and it never reorders).
	hadamardTmp []int32

	// op_pvq_search (vq.c:205) / alg_quant (vq.c:550). alg_quant is a leaf — it
	// consumes iy entirely before returning — so quant_partition's recursion cannot
	// hold two of these live at once. N is bounded by the widest band times M (176).
	pvqY     []int32 // y
	pvqSignx []int   // signx
	pvqIy    []int32 // iy
}
