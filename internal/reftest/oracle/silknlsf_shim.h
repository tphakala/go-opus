//go:build refc

/*
 * silknlsf_shim.h - C-callable wrappers over the pinned libopus SILK NLSF decode
 * chain (silk/NLSF_decode.c, silk/NLSF_unpack.c, silk/NLSF_stabilize.c,
 * silk/NLSF2A.c, silk/LPC_fit.c, silk/LPC_inv_pred_gain.c, silk/bwexpander.c,
 * silk/bwexpander_32.c, silk/sort.c) for the internal/silk differential test.
 *
 * This chain is a pure function of the (already range-decoded) NLSF codebook path
 * indices: silk_NLSF_decode dequantizes the Q15 NLSF vector from the indices and
 * the codebook and stabilizes it, and silk_NLSF2A turns that into the Q12 LPC
 * whitening filter. No range coder is involved, so silknlsf_test.go can feed the
 * SAME index vectors / coefficient arrays to both C and Go and assert the outputs
 * are bit-identical.
 *
 * Everything is static so the header pulls straight into silknlsf_cgo.go's
 * preamble; the SILK functions and the two NLSF codebook objects
 * (silk_NLSF_CB_NB_MB / silk_NLSF_CB_WB, declared in silk/tables.h) plus the LSF
 * cosine table are linked in via the existing w_silk_NLSF_decode.c /
 * w_silk_NLSF_unpack.c / w_silk_NLSF_stabilize.c / w_silk_NLSF2A.c /
 * w_silk_LPC_fit.c / w_silk_LPC_inv_pred_gain.c / w_silk_bwexpander.c /
 * w_silk_bwexpander_32.c / w_silk_sort.c / w_silk_tables_NLSF_CB_NB_MB.c /
 * w_silk_tables_NLSF_CB_WB.c / w_silk_table_LSF_cos.c wrappers. This file never
 * edits the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 *
 * Build flags (FIXED_POINT + DISABLE_FLOAT_API + OPUS_FAST_INT64) and include
 * paths come from oracle_cgo.go; silk/main.h declares silk_NLSF_decode /
 * silk_NLSF_unpack and pulls in SigProc_FIX.h (silk_NLSF2A, silk_NLSF_stabilize,
 * silk_bwexpander, silk_bwexpander_32, silk_LPC_inverse_pred_gain_c), define.h
 * (MAX_LPC_ORDER) and tables.h (the codebook objects).
 */
#ifndef GOOPUS_SILKNLSF_SHIM_H
#define GOOPUS_SILKNLSF_SHIM_H

#include <stdint.h>
#include <string.h>
#include "libopus/silk/main.h" /* silk_NLSF_decode/_unpack + SigProc_FIX/define/tables */

/*
 * oracle_silk_NLSF_decode selects the NLSF codebook by filter order (16 -> the
 * wideband codebook silk_NLSF_CB_WB, otherwise the NB/MB codebook
 * silk_NLSF_CB_NB_MB, order 10) and runs silk_NLSF_decode over a mutable copy of
 * the caller's index path (NLSFIndices is [order+1]; silk_NLSF_decode takes it as
 * a non-const pointer). pNLSF_Q15 receives the stabilized Q15 NLSF vector.
 */
static void oracle_silk_NLSF_decode(int order, const int8_t *NLSFIndices,
    int16_t *pNLSF_Q15)
{
    const silk_NLSF_CB_struct *psNLSF_CB = (order == 16) ? &silk_NLSF_CB_WB : &silk_NLSF_CB_NB_MB;
    opus_int8 idx[ MAX_LPC_ORDER + 1 ];
    int i;
    for (i = 0; i < order + 1; i++) {
        idx[i] = (opus_int8)NLSFIndices[i];
    }
    silk_NLSF_decode((opus_int16 *)pNLSF_Q15, idx, psNLSF_CB);
}

/*
 * oracle_silk_NLSF2A runs silk_NLSF2A: convert the Q15 NLSF vector (length d) into
 * the monic Q12 LPC whitening filter a_Q12 (length d). arch is 0 (scalar path).
 */
static void oracle_silk_NLSF2A(const int16_t *NLSF, int d, int16_t *a_Q12)
{
    silk_NLSF2A((opus_int16 *)a_Q12, (const opus_int16 *)NLSF, d, 0);
}

/*
 * oracle_silk_LPC_inverse_pred_gain runs silk_LPC_inverse_pred_gain_c over the Q12
 * coefficients A_Q12 (length order), returning the inverse prediction gain (Q30),
 * or 0 if the filter is unstable.
 */
static int32_t oracle_silk_LPC_inverse_pred_gain(const int16_t *A_Q12, int order)
{
    return (int32_t)silk_LPC_inverse_pred_gain_c((const opus_int16 *)A_Q12, order);
}

/*
 * oracle_silk_bwexpander chirps (bandwidth-expands) the Q12 int16 filter ar
 * (length d) in place by chirp_Q16 via silk_bwexpander.
 */
static void oracle_silk_bwexpander(int16_t *ar, int d, int32_t chirp_Q16)
{
    silk_bwexpander((opus_int16 *)ar, d, (opus_int32)chirp_Q16);
}

/*
 * oracle_silk_bwexpander_32 chirps the unscaled int32 filter ar (length d) in
 * place by chirp_Q16 via silk_bwexpander_32.
 */
static void oracle_silk_bwexpander_32(int32_t *ar, int d, int32_t chirp_Q16)
{
    silk_bwexpander_32((opus_int32 *)ar, d, (opus_int32)chirp_Q16);
}

/*
 * oracle_silk_NLSF_stabilize stabilizes NLSF_Q15 (length L) in place given the
 * [L+1] minimum-distance vector NDeltaMin_Q15 via silk_NLSF_stabilize.
 */
static void oracle_silk_NLSF_stabilize(int16_t *NLSF_Q15,
    const int16_t *NDeltaMin_Q15, int L)
{
    silk_NLSF_stabilize((opus_int16 *)NLSF_Q15, (const opus_int16 *)NDeltaMin_Q15, L);
}

#endif /* GOOPUS_SILKNLSF_SHIM_H */
