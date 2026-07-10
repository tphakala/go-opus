/* dump_silk.c: emits the libopus SILK static data tables as a flat token
 * stream that cmd/gentables parses into internal/silk/tables_gen.go. Like
 * cdump/dump_modes.c it is compiled and run by the generator, not by the
 * normal Go toolchain (a bare .c file is ignored by `go build`).
 *
 * Build config mirrors the reftest oracle and dump_modes.c: FIXED_POINT +
 * DISABLE_FLOAT_API. The SILK table translation units are #included directly
 * so that every array is compiled into this TU, including the file-static
 * sub-arrays that upstream only exposes through the public pointer tables
 * (silk_LTP_*_ptrs, silk_LBRR_flags_iCDF_ptr) and the two NLSF codebook
 * structs. All values printed below therefore come straight from source.
 *
 * Output format matches dump_modes.c: one record per line,
 * "KEY COUNT v0 v1 ... v(COUNT-1)", all values decimal ints. The Go generator
 * validates COUNT against its own schema so a submodule reshape fails loudly.
 *
 * Only the tables the SILK decoder needs are dumped. The encoder-only pitch
 * search parameter tables (silk_Lag_range_stage3*, silk_nb_cbk_searchs_stage3)
 * are intentionally skipped; the pitch contour codebooks that silk_decode_pitch
 * consumes (silk_CB_lags_stage2/3, incl. the 10 ms variants) are included.
 */

#include <stdio.h>

#include "tables.h"
#include "pitch_est_defines.h"

/* Pull in the table definitions (and their file-static sub-arrays). */
#include "tables_NLSF_CB_NB_MB.c"
#include "tables_NLSF_CB_WB.c"
#include "tables_gain.c"
#include "tables_LTP.c"
#include "tables_other.c"
#include "tables_pitch_lag.c"
#include "tables_pulses_per_block.c"
#include "table_LSF_cos.c"
#include "pitch_est_tables.c"

static void emit_u8(const char *key, const opus_uint8 *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

static void emit_i8(const char *key, const opus_int8 *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

static void emit_i16(const char *key, const opus_int16 *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

static void emit_i32(const char *key, const opus_int32 *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

int main(void) {
    /* --- Gain tables (tables_gain.c) --- */
    emit_u8("gainICDF", &silk_gain_iCDF[0][0], 3 * 8);
    emit_u8("deltaGainICDF", silk_delta_gain_iCDF,
        MAX_DELTA_GAIN_QUANT - MIN_DELTA_GAIN_QUANT + 1);

    /* --- LTP tables (tables_LTP.c) --- */
    emit_u8("ltpPerIndexICDF", silk_LTP_per_index_iCDF, 3);
    emit_u8("ltpGainICDF0", silk_LTP_gain_iCDF_ptrs[0], 8);
    emit_u8("ltpGainICDF1", silk_LTP_gain_iCDF_ptrs[1], 16);
    emit_u8("ltpGainICDF2", silk_LTP_gain_iCDF_ptrs[2], 32);
    emit_u8("ltpGainBITS0", silk_LTP_gain_BITS_Q5_ptrs[0], 8);
    emit_u8("ltpGainBITS1", silk_LTP_gain_BITS_Q5_ptrs[1], 16);
    emit_u8("ltpGainBITS2", silk_LTP_gain_BITS_Q5_ptrs[2], 32);
    emit_i8("ltpVQ0", silk_LTP_vq_ptrs_Q7[0], 8 * 5);
    emit_i8("ltpVQ1", silk_LTP_vq_ptrs_Q7[1], 16 * 5);
    emit_i8("ltpVQ2", silk_LTP_vq_ptrs_Q7[2], 32 * 5);
    emit_u8("ltpVQGain0", silk_LTP_vq_gain_ptrs_Q7[0], 8);
    emit_u8("ltpVQGain1", silk_LTP_vq_gain_ptrs_Q7[1], 16);
    emit_u8("ltpVQGain2", silk_LTP_vq_gain_ptrs_Q7[2], 32);
    emit_i8("ltpVQSizes", silk_LTP_vq_sizes, NB_LTP_CBKS);

    /* --- Pitch lag / contour iCDFs (tables_pitch_lag.c) --- */
    emit_u8("pitchLagICDF", silk_pitch_lag_iCDF,
        2 * (PITCH_EST_MAX_LAG_MS - PITCH_EST_MIN_LAG_MS));
    emit_u8("pitchDeltaICDF", silk_pitch_delta_iCDF, 21);
    emit_u8("pitchContourICDF", silk_pitch_contour_iCDF, 34);
    emit_u8("pitchContourNBICDF", silk_pitch_contour_NB_iCDF, 11);
    emit_u8("pitchContour10msICDF", silk_pitch_contour_10_ms_iCDF, 12);
    emit_u8("pitchContour10msNBICDF", silk_pitch_contour_10_ms_NB_iCDF, 3);

    /* --- Pitch contour codebooks consumed by silk_decode_pitch
     * (pitch_est_tables.c). Row-major flattened. --- */
    emit_i8("cbLagsStage2", &silk_CB_lags_stage2[0][0],
        PE_MAX_NB_SUBFR * PE_NB_CBKS_STAGE2_EXT);
    emit_i8("cbLagsStage3", &silk_CB_lags_stage3[0][0],
        PE_MAX_NB_SUBFR * PE_NB_CBKS_STAGE3_MAX);
    emit_i8("cbLagsStage2_10ms", &silk_CB_lags_stage2_10_ms[0][0],
        (PE_MAX_NB_SUBFR >> 1) * PE_NB_CBKS_STAGE2_10MS);
    emit_i8("cbLagsStage3_10ms", &silk_CB_lags_stage3_10_ms[0][0],
        (PE_MAX_NB_SUBFR >> 1) * PE_NB_CBKS_STAGE3_10MS);

    /* --- Pulses-per-block / shell coding (tables_pulses_per_block.c) --- */
    emit_u8("maxPulsesTable", silk_max_pulses_table, 4);
    emit_u8("pulsesPerBlockICDF", &silk_pulses_per_block_iCDF[0][0],
        N_RATE_LEVELS * (SILK_MAX_PULSES + 2));
    emit_u8("pulsesPerBlockBITSQ5", &silk_pulses_per_block_BITS_Q5[0][0],
        (N_RATE_LEVELS - 1) * (SILK_MAX_PULSES + 2));
    emit_u8("rateLevelsICDF", &silk_rate_levels_iCDF[0][0],
        2 * (N_RATE_LEVELS - 1));
    emit_u8("rateLevelsBITSQ5", &silk_rate_levels_BITS_Q5[0][0],
        2 * (N_RATE_LEVELS - 1));
    emit_u8("shellCodeTable0", silk_shell_code_table0, 152);
    emit_u8("shellCodeTable1", silk_shell_code_table1, 152);
    emit_u8("shellCodeTable2", silk_shell_code_table2, 152);
    emit_u8("shellCodeTable3", silk_shell_code_table3, 152);
    emit_u8("shellCodeTableOffsets", silk_shell_code_table_offsets,
        SILK_MAX_PULSES + 1);
    emit_u8("signICDF", silk_sign_iCDF, 42);

    /* --- Stereo, LBRR, misc entropy tables (tables_other.c) --- */
    emit_i16("stereoPredQuantQ13", silk_stereo_pred_quant_Q13,
        STEREO_QUANT_TAB_SIZE);
    emit_u8("stereoPredJointICDF", silk_stereo_pred_joint_iCDF, 25);
    emit_u8("stereoOnlyCodeMidICDF", silk_stereo_only_code_mid_iCDF, 2);
    emit_u8("lbrrFlags2ICDF", silk_LBRR_flags_iCDF_ptr[0], 3);
    emit_u8("lbrrFlags3ICDF", silk_LBRR_flags_iCDF_ptr[1], 7);
    emit_u8("lsbICDF", silk_lsb_iCDF, 2);
    emit_u8("ltpScaleICDF", silk_LTPscale_iCDF, 3);
    emit_u8("typeOffsetVADICDF", silk_type_offset_VAD_iCDF, 4);
    emit_u8("typeOffsetNoVADICDF", silk_type_offset_no_VAD_iCDF, 2);
    emit_u8("nlsfInterpolationFactorICDF", silk_NLSF_interpolation_factor_iCDF, 5);
    emit_i16("quantizationOffsetsQ10", &silk_Quantization_Offsets_Q10[0][0], 4);
    emit_i16("ltpScalesTableQ14", silk_LTPScales_table_Q14, 3);
    emit_u8("uniform3ICDF", silk_uniform3_iCDF, 3);
    emit_u8("uniform4ICDF", silk_uniform4_iCDF, 4);
    emit_u8("uniform5ICDF", silk_uniform5_iCDF, 5);
    emit_u8("uniform6ICDF", silk_uniform6_iCDF, 6);
    emit_u8("uniform8ICDF", silk_uniform8_iCDF, 8);
    emit_u8("nlsfEXTICDF", silk_NLSF_EXT_iCDF, 7);
    emit_i32("transitionLPBQ28", &silk_Transition_LP_B_Q28[0][0],
        TRANSITION_INT_NUM * TRANSITION_NB);
    emit_i32("transitionLPAQ28", &silk_Transition_LP_A_Q28[0][0],
        TRANSITION_INT_NUM * TRANSITION_NA);

    /* --- LSF cosine table (table_LSF_cos.c) --- */
    emit_i16("lsfCosTabFIXQ12", silk_LSFCosTab_FIX_Q12, LSF_COS_TAB_SZ_FIX + 1);

    /* --- NLSF NB/MB codebook (tables_NLSF_CB_NB_MB.c). Scalars first, then
     * every pointed-to sub-array at its upstream-declared length. --- */
    printf("nbmbScalars 4 %d %d %d %d\n",
        (int)silk_NLSF_CB_NB_MB.nVectors, (int)silk_NLSF_CB_NB_MB.order,
        (int)silk_NLSF_CB_NB_MB.quantStepSize_Q16,
        (int)silk_NLSF_CB_NB_MB.invQuantStepSize_Q6);
    emit_u8("nbmbCB1NLSFQ8", silk_NLSF_CB_NB_MB.CB1_NLSF_Q8, 320);
    emit_i16("nbmbCB1WghtQ9", silk_NLSF_CB_NB_MB.CB1_Wght_Q9, 320);
    emit_u8("nbmbCB1ICDF", silk_NLSF_CB_NB_MB.CB1_iCDF, 64);
    emit_u8("nbmbPredQ8", silk_NLSF_CB_NB_MB.pred_Q8, 18);
    emit_u8("nbmbEcSel", silk_NLSF_CB_NB_MB.ec_sel, 160);
    emit_u8("nbmbEcICDF", silk_NLSF_CB_NB_MB.ec_iCDF, 72);
    emit_u8("nbmbEcRatesQ5", silk_NLSF_CB_NB_MB.ec_Rates_Q5, 72);
    emit_i16("nbmbDeltaMinQ15", silk_NLSF_CB_NB_MB.deltaMin_Q15, 11);

    /* --- NLSF WB codebook (tables_NLSF_CB_WB.c). --- */
    printf("wbScalars 4 %d %d %d %d\n",
        (int)silk_NLSF_CB_WB.nVectors, (int)silk_NLSF_CB_WB.order,
        (int)silk_NLSF_CB_WB.quantStepSize_Q16,
        (int)silk_NLSF_CB_WB.invQuantStepSize_Q6);
    emit_u8("wbCB1NLSFQ8", silk_NLSF_CB_WB.CB1_NLSF_Q8, 512);
    emit_i16("wbCB1WghtQ9", silk_NLSF_CB_WB.CB1_Wght_Q9, 512);
    emit_u8("wbCB1ICDF", silk_NLSF_CB_WB.CB1_iCDF, 64);
    emit_u8("wbPredQ8", silk_NLSF_CB_WB.pred_Q8, 30);
    emit_u8("wbEcSel", silk_NLSF_CB_WB.ec_sel, 256);
    emit_u8("wbEcICDF", silk_NLSF_CB_WB.ec_iCDF, 72);
    emit_u8("wbEcRatesQ5", silk_NLSF_CB_WB.ec_Rates_Q5, 72);
    emit_i16("wbDeltaMinQ15", silk_NLSF_CB_WB.deltaMin_Q15, 17);

    return 0;
}
