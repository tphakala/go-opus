//go:build refc

/*
 * silkresampler_shim.h - C-callable driver over the pinned libopus SILK
 * sample-rate converter (silk/resampler.c + silk/resampler_private_up2_HQ.c +
 * silk/resampler_private_IIR_FIR.c + silk/resampler_private_down_FIR.c +
 * silk/resampler_private_AR2.c + silk/resampler_rom.c) for the internal/silk
 * resampler.go / resampler_private.go / resampler_rom.go differential test.
 *
 * The resampler is a cross-call state machine (its allpass / AR2 / FIR filter
 * memory and the 1 ms delay buffer carry from one silk_resampler call to the next),
 * so the C state lives in a caller-owned silk_resampler_state_struct that the Go test
 * holds opaque and passes back on every block. oracle_resampler_init runs
 * silk_resampler_init and reports the derived configuration (Fs kHz, batchSize,
 * invRatio_Q16, FIR order/fracs, selected function, input delay);
 * oracle_resampler_process runs one block of silk_resampler and reads the full filter
 * state (sIIR, the sFIR union canonicalized, delayBuf) back into a plain struct the Go
 * side compares against the pure-Go ResamplerState after every block.
 *
 * The sFIR member is a C union { opus_int32 i32[36]; opus_int16 i16[36]; }. The
 * IIR_FIR path uses the i16 view (8 samples), every other path uses the i32 view; the
 * snapshot canonicalizes to int32[36] by reading through whichever view the selected
 * resampler function uses, which is exactly what the pure-Go SFIR array stores.
 *
 * This file never edits the shared oracle surface. Build flags (FIXED_POINT +
 * DISABLE_FLOAT_API) and include paths come from oracle_cgo.go; the resampler C
 * sources are compiled via their w_silk_resampler*.c wrappers.
 */
#ifndef GOOPUS_SILKRESAMPLER_SHIM_H
#define GOOPUS_SILKRESAMPLER_SHIM_H

#include <stdint.h>
#include <string.h>
#include "libopus/silk/main.h" /* silk_resampler_init + silk_resampler + silk_resampler_state_struct */

/* resampler_function value for USE_silk_resampler_private_IIR_FIR (silk/resampler.c,
 * a translation-unit-local #define not exported in any header). The IIR_FIR path is
 * the only one that reads the sFIR union through its i16 view. */
#define ORACLE_USE_silk_resampler_private_IIR_FIR 2

/* oracle_resampler_config mirrors the scalar configuration silk_resampler_init derives,
 * so the Go test can assert its own ResamplerInit produced the identical setup. */
typedef struct {
    int     Fs_in_kHz;
    int     Fs_out_kHz;
    int     batchSize;
    int32_t invRatio_Q16;
    int     FIR_Order;
    int     FIR_Fracs;
    int     resampler_function;
    int     inputDelay;
    int     ret; /* silk_resampler_init return value (0 ok, -1 unsupported pair) */
} oracle_resampler_config;

/* oracle_resampler_state is the persistent filter state read back after a block, in a
 * plain layout the Go test owns. sFIR is canonicalized to int32[36] (see file header). */
typedef struct {
    int32_t sIIR[6];
    int32_t sFIR[36];
    int16_t delayBuf[96];
} oracle_resampler_state;

/* oracle_resampler_read_state canonicalizes the live C resampler state into out. */
static void oracle_resampler_read_state(const silk_resampler_state_struct *S,
    oracle_resampler_state *out)
{
    int i;
    for (i = 0; i < 6; i++) {
        out->sIIR[i] = S->sIIR[i];
    }
    if (S->resampler_function == ORACLE_USE_silk_resampler_private_IIR_FIR) {
        for (i = 0; i < 36; i++) {
            out->sFIR[i] = (int32_t)S->sFIR.i16[i];
        }
    } else {
        for (i = 0; i < 36; i++) {
            out->sFIR[i] = S->sFIR.i32[i];
        }
    }
    for (i = 0; i < 96; i++) {
        out->delayBuf[i] = S->delayBuf[i];
    }
}

/* oracle_resampler_init clears/sets up S for the (Fs_Hz_in, Fs_Hz_out, forEnc) triple
 * and reports the derived configuration in cfg. */
static void oracle_resampler_init(silk_resampler_state_struct *S,
    int32_t Fs_Hz_in, int32_t Fs_Hz_out, int forEnc, oracle_resampler_config *cfg)
{
    cfg->ret               = silk_resampler_init(S, Fs_Hz_in, Fs_Hz_out, forEnc);
    cfg->Fs_in_kHz         = S->Fs_in_kHz;
    cfg->Fs_out_kHz        = S->Fs_out_kHz;
    cfg->batchSize         = S->batchSize;
    cfg->invRatio_Q16      = S->invRatio_Q16;
    cfg->FIR_Order         = S->FIR_Order;
    cfg->FIR_Fracs         = S->FIR_Fracs;
    cfg->resampler_function = S->resampler_function;
    cfg->inputDelay        = S->inputDelay;
}

/* oracle_resampler_process runs one block of silk_resampler (out receives
 * (inLen / Fs_in_kHz) * Fs_out_kHz samples) and reads the post-block filter state into
 * state_out. Returns silk_resampler's value (always 0). */
static int oracle_resampler_process(silk_resampler_state_struct *S,
    int16_t *out, const int16_t *in, int32_t inLen, oracle_resampler_state *state_out)
{
    int ret = silk_resampler(S, out, in, inLen);
    oracle_resampler_read_state(S, state_out);
    return ret;
}

#endif /* GOOPUS_SILKRESAMPLER_SHIM_H */
