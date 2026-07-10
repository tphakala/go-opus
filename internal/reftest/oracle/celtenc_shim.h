//go:build refc

/*
 * celtenc_shim.h - C-callable wrappers over the pinned libopus CELT encoder
 * FRONT-HALF stage functions (celt/celt_encoder.c) for the phase-4 CP8a encoder
 * differential test of the pure-Go internal/celt encoder pipeline.
 *
 * SOLE TRANSLATION UNIT FOR celt_encoder.c (Option A). Most oracle sources are
 * compiled once each via a w_<dir>_<file>.c wrapper (see gen_wrappers.sh). The
 * stage functions this test drives (transient_analysis, run_prefilter,
 * compute_mdcts, tone_detect, patch_transient_decision) are FILE-STATIC in
 * celt_encoder.c, so they are only reachable by #include-ing the .c directly into
 * this translation unit. To avoid defining celt_encoder.c's external symbols
 * twice (which would fail to link), w_celt_celt_encoder.c is NEUTRALIZED and this
 * header is the only place celt_encoder.c is compiled. celtdec_shim.h and
 * w_src_opus_encoder.c reference celt_encode_with_ec (and celt_encoder_init /
 * opus_custom_encoder_ctl) extern; they resolve against the definitions here at
 * link time, since the whole oracle package links into one test binary.
 *
 * celt_encoder.c MUST be included FIRST, before any other opus header, so its own
 * `#define CELT_ENCODER_C` precedes the first include of opus_custom.h and the
 * CELT_ENCODER_C-gated prototypes are visible (this is the unity-build macro-leak
 * documented in gen_wrappers.sh; here it is deliberately the first include).
 *
 * Build config (oracle_cgo.go CFLAGS): FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64, non-CUSTOM_MODES, non-QEXT, non-RES24. So opus_res is
 * opus_int16, celt_sig / celt_norm / celt_ener / celt_glog are opus_int32,
 * opus_val16 / celt_coef are opus_int16. RES2SIG(a) = SHL32(EXTEND32(a),
 * SIG_SHIFT). This header only adds static glue; it never edits the shared oracle
 * surface (shim.h/shim.c/oracle_cgo.go).
 */
#ifndef GOOPUS_CELTENC_SHIM_H
#define GOOPUS_CELTENC_SHIM_H

/* The one and only compilation of celt_encoder.c. Included FIRST (see header). */
#include "libopus/celt/celt_encoder.c"

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

/* Fail the build loudly if the type config drifts from what these wrappers
   assume (opus_res int16, celt_sig/celt_glog int32). */
typedef char oracle_enc_assert_res_is_int16[(sizeof(opus_res) == 2) ? 1 : -1];
typedef char oracle_enc_assert_sig_is_int32[(sizeof(celt_sig) == 4) ? 1 : -1];
typedef char oracle_enc_assert_glog_is_int32[(sizeof(celt_glog) == 4) ? 1 : -1];

/* The frozen CELT-only 48 kHz / 960 mode. */
static const CELTMode *oracle_celtenc_mode(void)
{
   int e = 0;
   return opus_custom_mode_create(48000, 960, &e);
}

static int oracle_celtenc_overlap(void) { return oracle_celtenc_mode()->overlap; }
static int oracle_celtenc_nbebands(void) { return oracle_celtenc_mode()->nbEBands; }
static int oracle_celtenc_maxperiod(void) { return COMBFILTER_MAXPERIOD; }
static int oracle_celtenc_shortmdct(void) { return oracle_celtenc_mode()->shortMdctSize; }

/* --- celt_preemphasis (celt_encoder.c:557) -------------------------------- */

/* Pre-emphasize one channel of int16 PCM (read interleaved, stride CC) into inp
   (N celt_sig), threading the filter memory *mem_io. */
static void oracle_celtenc_preemphasis(const int16_t *pcm, int32_t *inp, int N,
      int CC, int upsample, int clip, int32_t *mem_io)
{
   const CELTMode *m = oracle_celtenc_mode();
   celt_sig mem = (celt_sig)*mem_io;
   celt_preemphasis((const opus_res *)pcm, (celt_sig *)inp, N, CC, upsample,
         m->preemph, &mem, clip);
   *mem_io = (int32_t)mem;
}

/* --- transient_analysis (celt_encoder.c:267) ------------------------------ */

static int oracle_celtenc_transient(const int32_t *in, int len, int C,
      int allow_weak, int tone_freq, int32_t toneishness,
      int32_t *tf_estimate_out, int *tf_chan_out, int *weak_transient_out)
{
   opus_val16 tf_estimate = 0;
   int tf_chan = 0;
   int weak = 0;
   int is_transient = transient_analysis((const opus_val32 *)in, len, C,
         &tf_estimate, &tf_chan, allow_weak, &weak,
         (opus_val16)tone_freq, (opus_val32)toneishness);
   *tf_estimate_out = (int32_t)tf_estimate;
   *tf_chan_out = tf_chan;
   *weak_transient_out = weak;
   return is_transient;
}

/* --- patch_transient_decision (celt_encoder.c:473) ------------------------ */

static int oracle_celtenc_patch_transient(int32_t *newE, int32_t *oldE,
      int nbEBands, int start, int end, int C)
{
   return patch_transient_decision((celt_glog *)newE, (celt_glog *)oldE,
         nbEBands, start, end, C);
}

/* --- compute_mdcts (celt_encoder.c:511) ----------------------------------- */

static void oracle_celtenc_compute_mdcts(int shortBlocks, int32_t *in,
      int32_t *out, int C, int CC, int LM, int upsample)
{
   const CELTMode *m = oracle_celtenc_mode();
   compute_mdcts(m, shortBlocks, (celt_sig *)in, (celt_sig *)out, C, CC, LM,
         upsample, 0);
}

/* --- tone_detect (celt_encoder.c:1362) ------------------------------------ */

static int oracle_celtenc_tone_detect(const int32_t *in, int CC, int N,
      int32_t *toneishness_out)
{
   const CELTMode *m = oracle_celtenc_mode();
   opus_val32 toneishness = 0;
   opus_val16 freq = tone_detect((const celt_sig *)in, CC, N, &toneishness, m->Fs);
   *toneishness_out = (int32_t)toneishness;
   return (int)freq;
}

/* --- remove_doubling (pitch.c:454) ---------------------------------------- */

/* remove_doubling is external (declared in pitch.h, linked via w_celt_pitch.c),
   so this wrapper just forwards. x is read-only (length (maxperiod+N)/2); *T0 is
   the coarse pitch estimate in/out. Returns the pitch gain (opus_val16). */
static int oracle_celtenc_remove_doubling(int16_t *x, int maxperiod, int minperiod,
      int N, int *T0, int prev_period, int prev_gain)
{
   int t0 = *T0;
   opus_val16 pg = remove_doubling((opus_val16 *)x, maxperiod, minperiod, N, &t0,
         prev_period, (opus_val16)prev_gain, 0);
   *T0 = t0;
   return (int)pg;
}

/* --- run_prefilter (celt_encoder.c:1404) ---------------------------------- */

/* Drive run_prefilter with a real CELTEncoder so every st field it reads is
   consistent. in (CC*(N+overlap)), in_mem (CC*overlap) and prefilter_mem
   (CC*max_period) are in/out. prefilter_period/gain/tapset seed the previous
   frame; prefilter_tapset_in is the current tapset_decision passed as the func's
   own parameter. prefilter_period_out captures the COMBFILTER_MINPERIOD floor
   side-effect run_prefilter applies to st->prefilter_period. */
static int oracle_celtenc_run_prefilter(int channels, int N,
      int complexity, int loss_rate,
      int prefilter_period, int prefilter_gain, int prefilter_tapset,
      int prefilter_tapset_in, int enabled, int tf_estimate, int nbAvailableBytes,
      int tone_freq, int32_t toneishness,
      int32_t *in, int32_t *in_mem, int32_t *prefilter_mem,
      int *pitch_out, int *gain_out, int *qgain_out, int *prefilter_period_out)
{
   int size = celt_encoder_get_size(channels);
   CELTEncoder *st = (CELTEncoder *)malloc((size_t)size);
   int overlap;
   int pitch_index = 0;
   int qg = 0;
   opus_val16 gain1 = 0;
   int pf_on;
   if (!st)
      return -1;
   celt_encoder_init(st, 48000, channels, 0);
   st->complexity = complexity;
   st->loss_rate = loss_rate;
   st->prefilter_period = prefilter_period;
   st->prefilter_gain = (opus_val16)prefilter_gain;
   st->prefilter_tapset = prefilter_tapset;
   overlap = st->mode->overlap;
   memcpy(st->in_mem, in_mem, sizeof(celt_sig) * (size_t)(channels * overlap));

   pf_on = run_prefilter(st, (celt_sig *)in, (celt_sig *)prefilter_mem, channels, N,
         prefilter_tapset_in, &pitch_index, &gain1, &qg, enabled, complexity,
         (opus_val16)tf_estimate, nbAvailableBytes, &st->analysis,
         (opus_val16)tone_freq, (opus_val32)toneishness);

   memcpy(in_mem, st->in_mem, sizeof(celt_sig) * (size_t)(channels * overlap));
   *pitch_out = pitch_index;
   *gain_out = (int)gain1;
   *qgain_out = qg;
   *prefilter_period_out = st->prefilter_period;
   free(st);
   return pf_on;
}

#endif /* GOOPUS_CELTENC_SHIM_H */
