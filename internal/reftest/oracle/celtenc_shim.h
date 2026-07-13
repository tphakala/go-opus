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

/* ==========================================================================
 * CP8b: the five ANALYSIS-stage statics (tf_analysis, tf_encode,
 * dynalloc_analysis, alloc_trim_analysis, stereo_analysis).
 *
 * All signatures are flattened to scalars + flat arrays; no Go struct is ever
 * marshalled into C. The CELTMode is always the frozen 48 kHz / 960 mode, so
 * m / m->logN / m->eBands are supplied here rather than passed from Go.
 *
 * The `arch` argument is pinned to 0 (C fallback) and `AnalysisInfo *analysis`
 * is a zeroed local: under DISABLE_FLOAT_API both dynalloc_analysis (:1223) and
 * alloc_trim_analysis (:934) reduce the analysis pointer to `(void)analysis;`,
 * so it is inert. A zeroed struct (valid==0) keeps it inert even if the float
 * API were ever re-enabled, and avoids passing NULL into a deref-shaped param.
 *
 * ARG_QEXT(arg) expands to nothing without ENABLE_QEXT (celt.h:49), so
 * dynalloc_analysis has NO qext_scale parameter in this build.
 * ========================================================================== */

/* --- tf_analysis (celt_encoder.c:663) ------------------------------------- */

/* X is C*N0 celt_norm (read-only: tf_analysis copies each band into a scratch
   buffer). importance[0..len) is read; tf_res[0..len) is written. Returns
   tf_select. NOTE: the C reads importance[i] over [0,len), while
   dynalloc_analysis only defines importance over [start,end) -- do not drive
   this with start>0 (see CP8b brief; unreachable in the frozen config). */
static int oracle_celtenc_tf_analysis(int len, int isTransient, int *tf_res,
      int lambda, const int32_t *X, int N0, int LM, int tf_estimate, int tf_chan,
      const int *importance)
{
   const CELTMode *m = oracle_celtenc_mode();
   return tf_analysis(m, len, isTransient, tf_res, lambda, (celt_norm *)X, N0, LM,
         (opus_val16)tf_estimate, tf_chan, (int *)importance);
}

/* --- tf_encode (celt_encoder.c:824) --------------------------------------- */

/* Range-coder writer. Drives a real ec_enc over buf (buflen bytes, so
   enc->storage == buflen and the unsigned `budget = storage*8` arithmetic is
   exercised for real), optionally advancing the coder first with prefill_bits
   ec_enc_bit_logp(bit,1) calls (bit i = (prefill_pat >> (i&31)) & 1) so the test
   can sweep the starting `tell` and hit the budget-exhaustion path at :849.

   tf_res[start..end) is MUTATED IN PLACE (unaffordable bands forced to curr,
   then every entry remapped through tf_select_table at :859-860); the caller
   must compare it. buf receives the FINALIZED bitstream (ec_enc_done is called
   last), while tell/rng/val are captured BEFORE ec_enc_done. */
static void oracle_celtenc_tf_encode(int start, int end, int isTransient,
      int *tf_res, int LM, int tf_select, int prefill_bits, uint32_t prefill_pat,
      unsigned char *buf, int buflen, int *tell_before_out, int *tell_out,
      uint32_t *rng_out, uint32_t *val_out, int *err_out)
{
   ec_enc enc;
   int i;
   memset(buf, 0, (size_t)buflen);
   ec_enc_init(&enc, buf, (opus_uint32)buflen);
   for (i = 0; i < prefill_bits; i++)
      ec_enc_bit_logp(&enc, (int)((prefill_pat >> (i & 31)) & 1u), 1);
   *tell_before_out = ec_tell(&enc);

   tf_encode(start, end, isTransient, tf_res, LM, tf_select, &enc);

   *tell_out = ec_tell(&enc);
   *rng_out = (uint32_t)enc.rng;
   *val_out = (uint32_t)enc.val;
   *err_out = ec_get_error(&enc);
   ec_enc_done(&enc);
}

/* --- dynalloc_analysis (celt_encoder.c:1049) ------------------------------ */

/* bandLogE / bandLogE2 / oldBandE are C*nbEBands celt_glog; surround_dynalloc is
   nbEBands celt_glog (read-only). offsets (nbEBands, OPUS_CLEAR'd inside),
   importance (written only over [start,end)) and spread_weight (written over
   [0,end)) are out-params. logN and eBands come from the frozen mode, matching
   the celt_encode_with_ec call site (:2248-2250). Returns maxDepth; *tot_boost
   is the second output. */
static int32_t oracle_celtenc_dynalloc_analysis(const int32_t *bandLogE,
      const int32_t *bandLogE2, const int32_t *oldBandE, int nbEBands, int start,
      int end, int C, int *offsets, int lsb_depth, int isTransient, int vbr,
      int constrained_vbr, int LM, int effectiveBytes, int32_t *tot_boost_out,
      int lfe, const int32_t *surround_dynalloc, int *importance,
      int *spread_weight, int tone_freq, int32_t toneishness)
{
   const CELTMode *m = oracle_celtenc_mode();
   AnalysisInfo analysis;
   opus_int32 tot_boost = 0;
   celt_glog maxDepth;
   memset(&analysis, 0, sizeof(analysis));
   maxDepth = dynalloc_analysis((const celt_glog *)bandLogE,
         (const celt_glog *)bandLogE2, (const celt_glog *)oldBandE, nbEBands,
         start, end, C, offsets, lsb_depth, m->logN, isTransient, vbr,
         constrained_vbr, m->eBands, LM, effectiveBytes, &tot_boost, lfe,
         (celt_glog *)surround_dynalloc, &analysis, importance, spread_weight,
         (opus_val16)tone_freq, (opus_val32)toneishness);
   *tot_boost_out = (int32_t)tot_boost;
   return (int32_t)maxDepth;
}

/* --- alloc_trim_analysis (celt_encoder.c:865) ----------------------------- */

/* X is C*N0 celt_norm, bandLogE is C*nbEBands celt_glog, both read-only.
   stereo_saving_io is the C's `opus_val16 *stereo_saving` in/out (:919, C==2
   only) widened to int32_t: it is truncated back to opus_val16 (int16) before
   the call and sign-extended on the way out, so a Go caller can thread it across
   a multi-frame sequence. Returns trim_index (0..10). */
static int oracle_celtenc_alloc_trim_analysis(const int32_t *X,
      const int32_t *bandLogE, int end, int LM, int C, int N0,
      int32_t *stereo_saving_io, int tf_estimate, int intensity,
      int32_t surround_trim, int32_t equiv_rate)
{
   const CELTMode *m = oracle_celtenc_mode();
   AnalysisInfo analysis;
   opus_val16 stereo_saving = (opus_val16)*stereo_saving_io;
   int trim_index;
   memset(&analysis, 0, sizeof(analysis));
   trim_index = alloc_trim_analysis(m, (const celt_norm *)X,
         (const celt_glog *)bandLogE, end, LM, C, N0, &analysis, &stereo_saving,
         (opus_val16)tf_estimate, intensity, (celt_glog)surround_trim,
         (opus_int32)equiv_rate, 0);
   *stereo_saving_io = (int32_t)stereo_saving;
   return trim_index;
}

/* --- stereo_analysis (celt_encoder.c:957) --------------------------------- */

/* X is 2*N0 celt_norm (stereo only; the loop is hard-coded to i<13 and reads
   X[N0+j]). Returns the dual_stereo decision (0/1). */
static int oracle_celtenc_stereo_analysis(const int32_t *X, int LM, int N0)
{
   return stereo_analysis(oracle_celtenc_mode(), (const celt_norm *)X, LM, N0);
}

#endif /* GOOPUS_CELTENC_SHIM_H */
