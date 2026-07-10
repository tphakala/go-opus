package silk

import (
	"reflect"
	"testing"
)

// These tests assert structural facts and spot values of the generated SILK
// tables against constants read directly from the pinned libopus v1.6.1 source
// (silk/tables_*.c, silk/table_LSF_cos.c, silk/pitch_est_tables.c). They need no
// C toolchain, so a silent regeneration drift (a bad submodule bump or a manual
// edit of tables_gen.go) is caught here even where `cmd/gentables -check` cannot
// run.

// TestNLSFCodebooksNBMB checks the NB/MB NLSF codebook scalars, the array
// lengths implied by nVectors/order, and a few spot values.
func TestNLSFCodebooksNBMB(t *testing.T) {
	cb := silkNLSFCBNBMB
	if cb.nVectors != 32 || cb.order != 10 {
		t.Fatalf("NB/MB nVectors/order = %d/%d, want 32/10", cb.nVectors, cb.order)
	}
	// SILK_FIX_CONST(0.18, 16) and SILK_FIX_CONST(1.0/0.18, 6).
	if cb.quantStepSizeQ16 != 11796 {
		t.Errorf("NB/MB quantStepSizeQ16 = %d, want 11796", cb.quantStepSizeQ16)
	}
	if cb.invQuantStepSizeQ6 != 356 {
		t.Errorf("NB/MB invQuantStepSizeQ6 = %d, want 356", cb.invQuantStepSizeQ6)
	}

	n, o := int(cb.nVectors), int(cb.order)
	checkLen(t, "NB/MB cb1NLSFQ8", len(cb.cb1NLSFQ8), n*o)     // 320
	checkLen(t, "NB/MB cb1WghtQ9", len(cb.cb1WghtQ9), n*o)     // 320
	checkLen(t, "NB/MB cb1ICDF", len(cb.cb1ICDF), 2*n)         // 64
	checkLen(t, "NB/MB predQ8", len(cb.predQ8), 2*(o-1))       // 18
	checkLen(t, "NB/MB ecSel", len(cb.ecSel), 160)             // order-dependent literal
	checkLen(t, "NB/MB ecICDF", len(cb.ecICDF), 72)            // (2*NLSF_QUANT_MAX_AMPLITUDE+1)*8
	checkLen(t, "NB/MB ecRatesQ5", len(cb.ecRatesQ5), 72)      //
	checkLen(t, "NB/MB deltaMinQ15", len(cb.deltaMinQ15), o+1) // 11

	// First codebook vector and deltaMin endpoints (tables_NLSF_CB_NB_MB.c).
	wantFirstRow := []byte{12, 35, 60, 83, 108, 132, 157, 180, 206, 228}
	if !reflect.DeepEqual(cb.cb1NLSFQ8[:10], wantFirstRow) {
		t.Errorf("NB/MB cb1NLSFQ8[0:10] = %v, want %v", cb.cb1NLSFQ8[:10], wantFirstRow)
	}
	if cb.deltaMinQ15[0] != 250 || cb.deltaMinQ15[10] != 461 {
		t.Errorf("NB/MB deltaMinQ15 ends = %d..%d, want 250..461", cb.deltaMinQ15[0], cb.deltaMinQ15[10])
	}
}

// TestNLSFCodebooksWB checks the WB NLSF codebook scalars and array lengths.
func TestNLSFCodebooksWB(t *testing.T) {
	cb := silkNLSFCBWB
	if cb.nVectors != 32 || cb.order != 16 {
		t.Fatalf("WB nVectors/order = %d/%d, want 32/16", cb.nVectors, cb.order)
	}
	// SILK_FIX_CONST(0.15, 16) and SILK_FIX_CONST(1.0/0.15, 6).
	if cb.quantStepSizeQ16 != 9830 {
		t.Errorf("WB quantStepSizeQ16 = %d, want 9830", cb.quantStepSizeQ16)
	}
	if cb.invQuantStepSizeQ6 != 427 {
		t.Errorf("WB invQuantStepSizeQ6 = %d, want 427", cb.invQuantStepSizeQ6)
	}

	n, o := int(cb.nVectors), int(cb.order)
	checkLen(t, "WB cb1NLSFQ8", len(cb.cb1NLSFQ8), n*o)     // 512
	checkLen(t, "WB cb1WghtQ9", len(cb.cb1WghtQ9), n*o)     // 512
	checkLen(t, "WB cb1ICDF", len(cb.cb1ICDF), 2*n)         // 64
	checkLen(t, "WB predQ8", len(cb.predQ8), 2*(o-1))       // 30
	checkLen(t, "WB ecSel", len(cb.ecSel), 256)             //
	checkLen(t, "WB ecICDF", len(cb.ecICDF), 72)            //
	checkLen(t, "WB ecRatesQ5", len(cb.ecRatesQ5), 72)      //
	checkLen(t, "WB deltaMinQ15", len(cb.deltaMinQ15), o+1) // 17
}

// TestLSFCosTab checks the LSF conversion cosine table (table_LSF_cos.c):
// LSF_COS_TAB_SZ_FIX+1 = 129 entries, symmetric about a zero midpoint.
func TestLSFCosTab(t *testing.T) {
	checkLen(t, "silkLSFCosTabFIXQ12", len(silkLSFCosTabFIXQ12), 129)
	if silkLSFCosTabFIXQ12[0] != 8192 {
		t.Errorf("silkLSFCosTabFIXQ12[0] = %d, want 8192", silkLSFCosTabFIXQ12[0])
	}
	if silkLSFCosTabFIXQ12[64] != 0 {
		t.Errorf("silkLSFCosTabFIXQ12[64] = %d, want 0", silkLSFCosTabFIXQ12[64])
	}
	if silkLSFCosTabFIXQ12[128] != -8192 {
		t.Errorf("silkLSFCosTabFIXQ12[128] = %d, want -8192", silkLSFCosTabFIXQ12[128])
	}
}

// TestLTPCodebooks checks the three LTP gain codebooks and their pointer tables
// (tables_LTP.c): sizes {8,16,32}, LTP_ORDER=5 taps per vector.
func TestLTPCodebooks(t *testing.T) {
	wantSizes := []int8{8, 16, 32}
	if !reflect.DeepEqual(silkLTPVQSizes, wantSizes) {
		t.Errorf("silkLTPVQSizes = %v, want %v", silkLTPVQSizes, wantSizes)
	}
	checkLen(t, "silkLTPVQPtrsQ7", len(silkLTPVQPtrsQ7), 3)
	checkLen(t, "silkLTPGainICDFPtrs", len(silkLTPGainICDFPtrs), 3)
	checkLen(t, "silkLTPGainBITSQ5Ptrs", len(silkLTPGainBITSQ5Ptrs), 3)
	checkLen(t, "silkLTPVQGainPtrsQ7", len(silkLTPVQGainPtrsQ7), 3)

	for i, sz := range wantSizes {
		checkLen(t, "silkLTPVQPtrsQ7 codebook", len(silkLTPVQPtrsQ7[i]), int(sz)*5)
		checkLen(t, "silkLTPGainICDFPtrs entry", len(silkLTPGainICDFPtrs[i]), int(sz))
		checkLen(t, "silkLTPGainBITSQ5Ptrs entry", len(silkLTPGainBITSQ5Ptrs[i]), int(sz))
		checkLen(t, "silkLTPVQGainPtrsQ7 entry", len(silkLTPVQGainPtrsQ7[i]), int(sz))
	}

	// First vector of the first codebook (silk_LTP_gain_vq_0[0]).
	wantFirst := []int8{4, 6, 24, 7, 5}
	if !reflect.DeepEqual(silkLTPGainVQ0[:5], wantFirst) {
		t.Errorf("silkLTPGainVQ0[0:5] = %v, want %v", silkLTPGainVQ0[:5], wantFirst)
	}
	if !reflect.DeepEqual(silkLTPPerIndexICDF, []byte{179, 99, 0}) {
		t.Errorf("silkLTPPerIndexICDF = %v, want [179 99 0]", silkLTPPerIndexICDF)
	}
}

// TestGainAndPulseTables checks shapes and spot values for the gain, pulse and
// shell tables (tables_gain.c, tables_pulses_per_block.c).
func TestGainAndPulseTables(t *testing.T) {
	if len(silkGainICDF) != 3 || len(silkGainICDF[0]) != 8 {
		t.Fatalf("silkGainICDF shape = [%d][%d], want [3][8]", len(silkGainICDF), len(silkGainICDF[0]))
	}
	wantGain0 := []byte{224, 112, 44, 15, 3, 2, 1, 0}
	if !reflect.DeepEqual(silkGainICDF[0][:], wantGain0) {
		t.Errorf("silkGainICDF[0] = %v, want %v", silkGainICDF[0], wantGain0)
	}
	checkLen(t, "silkDeltaGainICDF", len(silkDeltaGainICDF), 41)

	if !reflect.DeepEqual(silkMaxPulsesTable, []byte{8, 10, 12, 16}) {
		t.Errorf("silkMaxPulsesTable = %v, want [8 10 12 16]", silkMaxPulsesTable)
	}
	if len(silkPulsesPerBlockICDF) != 10 || len(silkPulsesPerBlockICDF[0]) != 18 {
		t.Errorf("silkPulsesPerBlockICDF shape = [%d][%d], want [10][18]",
			len(silkPulsesPerBlockICDF), len(silkPulsesPerBlockICDF[0]))
	}
	if len(silkPulsesPerBlockBITSQ5) != 9 || len(silkPulsesPerBlockBITSQ5[0]) != 18 {
		t.Errorf("silkPulsesPerBlockBITSQ5 shape = [%d][%d], want [9][18]",
			len(silkPulsesPerBlockBITSQ5), len(silkPulsesPerBlockBITSQ5[0]))
	}
	for i, tbl := range [][]byte{silkShellCodeTable0, silkShellCodeTable1, silkShellCodeTable2, silkShellCodeTable3} {
		checkLenf(t, "silkShellCodeTable", i, len(tbl), 152)
	}
	checkLen(t, "silkShellCodeTableOffsets", len(silkShellCodeTableOffsets), 17)
	checkLen(t, "silkSignICDF", len(silkSignICDF), 42)
}

// TestPitchTables checks the pitch lag iCDF and the contour codebooks consumed
// by silk_decode_pitch (tables_pitch_lag.c, pitch_est_tables.c).
func TestPitchTables(t *testing.T) {
	checkLen(t, "silkPitchLagICDF", len(silkPitchLagICDF), 32)
	checkLen(t, "silkPitchContourICDF", len(silkPitchContourICDF), 34)
	checkLen(t, "silkPitchContourNBICDF", len(silkPitchContourNBICDF), 11)
	checkLen(t, "silkPitchContour10msICDF", len(silkPitchContour10msICDF), 12)
	checkLen(t, "silkPitchContour10msNBICDF", len(silkPitchContour10msNBICDF), 3)

	if len(silkCBLagsStage2) != 4 || len(silkCBLagsStage2[0]) != 11 {
		t.Errorf("silkCBLagsStage2 shape = [%d][%d], want [4][11]",
			len(silkCBLagsStage2), len(silkCBLagsStage2[0]))
	}
	if len(silkCBLagsStage3) != 4 || len(silkCBLagsStage3[0]) != 34 {
		t.Errorf("silkCBLagsStage3 shape = [%d][%d], want [4][34]",
			len(silkCBLagsStage3), len(silkCBLagsStage3[0]))
	}
	if len(silkCBLagsStage210ms) != 2 || len(silkCBLagsStage310ms) != 2 {
		t.Errorf("10 ms CB lag tables have wrong row counts")
	}
}

// TestOtherTables checks the stereo, transition and quantization-offset tables
// (tables_other.c).
func TestOtherTables(t *testing.T) {
	checkLen(t, "silkStereoPredQuantQ13", len(silkStereoPredQuantQ13), 16)
	if silkStereoPredQuantQ13[0] != -13732 || silkStereoPredQuantQ13[15] != 13732 {
		t.Errorf("silkStereoPredQuantQ13 ends = %d..%d, want -13732..13732",
			silkStereoPredQuantQ13[0], silkStereoPredQuantQ13[15])
	}
	wantTransB0 := [3]int32{250767114, 501534038, 250767114}
	if silkTransitionLPBQ28[0] != wantTransB0 {
		t.Errorf("silkTransitionLPBQ28[0] = %v, want %v", silkTransitionLPBQ28[0], wantTransB0)
	}
	if len(silkTransitionLPAQ28) != 5 || len(silkTransitionLPAQ28[0]) != 2 {
		t.Errorf("silkTransitionLPAQ28 shape = [%d][%d], want [5][2]",
			len(silkTransitionLPAQ28), len(silkTransitionLPAQ28[0]))
	}
	if len(silkQuantizationOffsetsQ10) != 2 || len(silkQuantizationOffsetsQ10[0]) != 2 {
		t.Errorf("silkQuantizationOffsetsQ10 shape wrong")
	}
	if !reflect.DeepEqual(silkLTPScalesTableQ14, []int16{15565, 12288, 8192}) {
		t.Errorf("silkLTPScalesTableQ14 = %v, want [15565 12288 8192]", silkLTPScalesTableQ14)
	}
}

func checkLen(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("len(%s) = %d, want %d", name, got, want)
	}
}

func checkLenf(t *testing.T, name string, idx, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("len(%s%d) = %d, want %d", name, idx, got, want)
	}
}
