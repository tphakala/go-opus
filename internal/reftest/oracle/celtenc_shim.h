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

/* ==========================================================================
 * CP8c wave 1: compute_vbr, hysteresis_decision, and the CELT ENCODER HANDLE
 * (create / configure / encode-one-frame / dump-state / destroy) that lets a Go
 * test drive N frames on the SAME C encoder and compare the evolved state after
 * every frame.
 *
 * The handle exists because struct OpusCustomEncoder is defined INSIDE
 * celt_encoder.c and is therefore an opaque incomplete type everywhere else;
 * only this TU (the sole one that #includes celt_encoder.c) can read its fields.
 * That is also why oracle_celtenc_create/encode/destroy in celtdec_shim.h cannot
 * be extended with a state dump: they see CELTEncoder only as a forward
 * declaration. Those stay as they are (CBR-only packet producers for the DECODER
 * test); the encoder differential test uses the handle below.
 * ========================================================================== */

/* --- compute_vbr (celt_encoder.c:1604, static) ---------------------------- */

/* Flat-scalar wrapper over compute_vbr. ARG_QEXT(x) expands to nothing without
   ENABLE_QEXT (celt.h:49), so there is NO enable_qext parameter in this build,
   and under DISABLE_FLOAT_API the AnalysisInfo* reduces to `(void)analysis;`
   (:1671) -- a zeroed struct is passed to keep it inert.
   stereo_saving and tf_estimate are opus_val16 (int16) and are truncated to that
   on the way in, exactly as the C call site at :2456-2464 does; maxDepth,
   surround_masking and temporal_vbr are celt_glog (int32). Returns the target in
   8th bits per frame. compute_vbr touches NO st-> field: it is a pure function
   of these arguments. */
static int32_t oracle_celtenc_compute_vbr(int32_t base_target, int LM, int32_t bitrate,
      int lastCodedBands, int C, int intensity, int constrained_vbr,
      int stereo_saving, int tot_boost, int tf_estimate, int pitch_change,
      int32_t maxDepth, int lfe, int has_surround_mask, int32_t surround_masking,
      int32_t temporal_vbr)
{
   const CELTMode *m = oracle_celtenc_mode();
   AnalysisInfo analysis;
   memset(&analysis, 0, sizeof(analysis));
   return (int32_t)compute_vbr(m, &analysis, (opus_int32)base_target, LM,
         (opus_int32)bitrate, lastCodedBands, C, intensity, constrained_vbr,
         (opus_val16)stereo_saving, tot_boost, (opus_val16)tf_estimate,
         pitch_change, (celt_glog)maxDepth, lfe, has_surround_mask,
         (celt_glog)surround_masking, (celt_glog)temporal_vbr);
}

/* --- hysteresis_decision (bands.c:46, NON-static) -------------------------- */

/* thresholds and hysteresis are N opus_val16 (int16). val is opus_val16 too; it
   arrives as an int and is truncated to 16 bits here, matching the encoder's own
   `(opus_val16)(equiv_rate/1000)` cast at :2403. */
static int oracle_celtenc_hysteresis_decision(int val, const int16_t *thresholds,
      const int16_t *hysteresis, int N, int prev)
{
   return hysteresis_decision((opus_val16)val, (const opus_val16 *)thresholds,
         (const opus_val16 *)hysteresis, N, prev);
}

/* --- the encoder handle --------------------------------------------------- */

/* Number of SCALAR words in the state dump. See oracle_celtenc_h_dump for the
   canonical order, which is the declaration order of the struct's reset region
   (celt_encoder.c:91-127) and MUST match celt.EncoderState (Go). */
#define ORACLE_ENC_NSCALARS 23

/* nbEBands is 21 for the frozen 48 kHz / 960 mode; 32 leaves headroom for the
   C*nbEBands energy_mask copy the handle owns. */
#define ORACLE_ENC_MAX_EBANDS 32

typedef struct {
   CELTEncoder *st;
   int channels;    /* CC, the allocation channel count */
   int nbEBands;
   int overlap;
   /* st->energy_mask is a BORROWED pointer that must outlive the encode call, so
      the handle owns the storage. Only the multistream/surround encoder ever
      sets it; on the frozen CELT-only path it stays NULL. */
   celt_glog energy_mask[2 * ORACLE_ENC_MAX_EBANDS];
} OracleEncHandle;

/* Create an encoder in the state celt_encoder_init leaves it in (no ctls
   applied): CBR, bitrate OPUS_BITRATE_MAX, complexity 5, lsb_depth 24, start 0,
   end effEBands, constrained_vbr 1, clip 1, signalling 1. Call
   oracle_celtenc_h_configure next. */
static void *oracle_celtenc_h_create(int channels)
{
   OracleEncHandle *H;
   int size;
   if (channels < 1 || channels > 2)
      return NULL;
   H = (OracleEncHandle *)calloc(1, sizeof(OracleEncHandle));
   if (!H)
      return NULL;
   size = celt_encoder_get_size(channels);
   H->st = (CELTEncoder *)malloc((size_t)size);
   if (!H->st) {
      free(H);
      return NULL;
   }
   if (celt_encoder_init(H->st, 48000, channels, 0) != OPUS_OK) {
      free(H->st);
      free(H);
      return NULL;
   }
   H->channels = channels;
   H->nbEBands = H->st->mode->nbEBands;
   H->overlap = H->st->mode->overlap;
   return H;
}

/* Apply the full ctl set. Returns 0 on success, or -(1-based ctl index) for the
   first request opus_custom_encoder_ctl rejected, so a Go test failure points at
   the exact ctl.

   force_intra and disable_prefilter are set on the struct DIRECTLY: the only ctl
   that reaches them is CELT_SET_PREDICTION (:2972), which couples them
   (disable_pf = value<=1; force_intra = value==0) and so cannot express the two
   flags independently. The Go celt.Encoder exposes SetForceIntra/SetDisablePf
   for the same reason. Every other field goes through the real ctl, including
   its validation and the OPUS_SET_BITRATE clamp to 750000*channels (:3006). */
static int oracle_celtenc_h_configure(void *h, int stream_channels, int complexity,
      int vbr, int vbr_constraint, int32_t bitrate, int lfe, int force_intra,
      int packet_loss, int lsb_depth, int disable_prefilter, int start, int end,
      int disable_inv)
{
   OracleEncHandle *H = (OracleEncHandle *)h;
   CELTEncoder *st = H->st;
   if (opus_custom_encoder_ctl(st, CELT_SET_CHANNELS_REQUEST, (opus_int32)stream_channels) != OPUS_OK)
      return -1;
   if (opus_custom_encoder_ctl(st, OPUS_SET_COMPLEXITY_REQUEST, (opus_int32)complexity) != OPUS_OK)
      return -2;
   if (opus_custom_encoder_ctl(st, OPUS_SET_VBR_REQUEST, (opus_int32)vbr) != OPUS_OK)
      return -3;
   if (opus_custom_encoder_ctl(st, OPUS_SET_VBR_CONSTRAINT_REQUEST, (opus_int32)vbr_constraint) != OPUS_OK)
      return -4;
   if (opus_custom_encoder_ctl(st, OPUS_SET_BITRATE_REQUEST, (opus_int32)bitrate) != OPUS_OK)
      return -5;
   if (opus_custom_encoder_ctl(st, OPUS_SET_LFE_REQUEST, (opus_int32)lfe) != OPUS_OK)
      return -6;
   if (opus_custom_encoder_ctl(st, OPUS_SET_PACKET_LOSS_PERC_REQUEST, (opus_int32)packet_loss) != OPUS_OK)
      return -7;
   if (opus_custom_encoder_ctl(st, OPUS_SET_LSB_DEPTH_REQUEST, (opus_int32)lsb_depth) != OPUS_OK)
      return -8;
   if (opus_custom_encoder_ctl(st, CELT_SET_START_BAND_REQUEST, (opus_int32)start) != OPUS_OK)
      return -9;
   if (opus_custom_encoder_ctl(st, CELT_SET_END_BAND_REQUEST, (opus_int32)end) != OPUS_OK)
      return -10;
   if (opus_custom_encoder_ctl(st, OPUS_SET_PHASE_INVERSION_DISABLED_REQUEST, (opus_int32)disable_inv) != OPUS_OK)
      return -11;
   st->force_intra = force_intra;
   st->disable_pf = disable_prefilter;
   return 0;
}

/* Install (a copy of) the surround masking curve, C*nbEBands celt_glog, or clear
   it with len==0. Returns 0, or -1 if len exceeds the handle's storage. The
   frozen single-stream CELT config never sets it (only OPUS_SET_ENERGY_MASK from
   the multistream encoder does), so the surround-masking block of
   celt_encode_with_ec (:2108-2185) is inert unless a test calls this. */
static int oracle_celtenc_h_set_energy_mask(void *h, const int32_t *mask, int len)
{
   OracleEncHandle *H = (OracleEncHandle *)h;
   int i;
   if (len < 0 || len > 2 * ORACLE_ENC_MAX_EBANDS)
      return -1;
   if (len == 0) {
      (void)opus_custom_encoder_ctl(H->st, OPUS_SET_ENERGY_MASK_REQUEST, (celt_glog *)NULL);
      return 0;
   }
   for (i = 0; i < len; i++)
      H->energy_mask[i] = (celt_glog)mask[i];
   (void)opus_custom_encoder_ctl(H->st, OPUS_SET_ENERGY_MASK_REQUEST, H->energy_mask);
   return 0;
}

/* Encode ONE frame (enc==NULL, so celt_encode_with_ec runs its own ec_enc over
   buf, which is the path the Go driver takes). Returns the celt_encode_with_ec
   return value (packet length in bytes, or a negative OPUS_* error) and reports
   st->rng, i.e. OPUS_GET_FINAL_RANGE. */
static int oracle_celtenc_h_encode(void *h, const int16_t *pcm, int frame_size,
      int nb_bytes, unsigned char *buf, uint32_t *rng_out)
{
   OracleEncHandle *H = (OracleEncHandle *)h;
   int ret;
   memset(buf, 0, (size_t)nb_bytes);
   ret = celt_encode_with_ec(H->st, (const opus_res *)pcm, frame_size, buf,
         nb_bytes, NULL);
   *rng_out = (uint32_t)H->st->rng;
   return ret;
}

/* Full flat state dump, in the CANONICAL ORDER (identical to Go's
   celt.EncoderState, whose doc comment carries the same list). It is the
   declaration order of the reset region (celt_encoder.c:91-127), skipping the
   fields that are dead in the frozen CELT-only config (analysis, silk_info) and
   the energy_mask POINTER (an input, not evolved state), followed by the six
   trailing VLA arrays:

     scalars[ 0] rng               scalars[12] preemph_memE[1]
     scalars[ 1] spread_decision   scalars[13] preemph_memD[0]
     scalars[ 2] delayedIntra      scalars[14] preemph_memD[1]
     scalars[ 3] tonal_average     scalars[15] vbr_reservoir
     scalars[ 4] lastCodedBands    scalars[16] vbr_drift
     scalars[ 5] hf_average        scalars[17] vbr_offset
     scalars[ 6] tapset_decision   scalars[18] vbr_count
     scalars[ 7] prefilter_period  scalars[19] overlap_max
     scalars[ 8] prefilter_gain    scalars[20] stereo_saving
     scalars[ 9] prefilter_tapset  scalars[21] intensity
     scalars[10] consec_transient  scalars[22] spec_avg
     scalars[11] preemph_memE[0]

   then in_mem, prefilter_mem, oldBandE, oldLogE, oldLogE2, energyError.

   prefilter_gain (opus_val16) and stereo_saving (opus_val16) are sign-extended
   into int32_t here; the Go side stores them back into int16 fields, so the
   16-bit truncation semantics are preserved where they matter (in the encoder,
   not in the transport).

   Buffer sizes the caller must provide: scalars[ORACLE_ENC_NSCALARS],
   in_mem[CC*overlap], prefilter_mem[CC*COMBFILTER_MAXPERIOD], and
   oldBandE/oldLogE/oldLogE2/energyError[CC*nbEBands] each. */
static void oracle_celtenc_h_dump(void *h, int32_t *scalars, int32_t *in_mem,
      int32_t *prefilter_mem, int32_t *oldBandE, int32_t *oldLogE,
      int32_t *oldLogE2, int32_t *energyError)
{
   OracleEncHandle *H = (OracleEncHandle *)h;
   CELTEncoder *st = H->st;
   int CC = H->channels;
   int nbEBands = H->nbEBands;
   int overlap = H->overlap;
   /* Same pointer arithmetic as celt_encode_with_ec:1854-1858 (QEXT_SCALE is the
      identity without ENABLE_QEXT). */
   celt_sig *c_prefilter_mem = st->in_mem + CC * overlap;
   celt_glog *c_oldBandE = (celt_glog *)(st->in_mem + CC * (overlap + COMBFILTER_MAXPERIOD));
   celt_glog *c_oldLogE = c_oldBandE + CC * nbEBands;
   celt_glog *c_oldLogE2 = c_oldLogE + CC * nbEBands;
   celt_glog *c_energyError = c_oldLogE2 + CC * nbEBands;
   int i;
   int k = 0;

   scalars[k++] = (int32_t)st->rng;
   scalars[k++] = (int32_t)st->spread_decision;
   scalars[k++] = (int32_t)st->delayedIntra;
   scalars[k++] = (int32_t)st->tonal_average;
   scalars[k++] = (int32_t)st->lastCodedBands;
   scalars[k++] = (int32_t)st->hf_average;
   scalars[k++] = (int32_t)st->tapset_decision;
   scalars[k++] = (int32_t)st->prefilter_period;
   scalars[k++] = (int32_t)st->prefilter_gain;
   scalars[k++] = (int32_t)st->prefilter_tapset;
   scalars[k++] = (int32_t)st->consec_transient;
   scalars[k++] = (int32_t)st->preemph_memE[0];
   scalars[k++] = (int32_t)st->preemph_memE[1];
   scalars[k++] = (int32_t)st->preemph_memD[0];
   scalars[k++] = (int32_t)st->preemph_memD[1];
   scalars[k++] = (int32_t)st->vbr_reservoir;
   scalars[k++] = (int32_t)st->vbr_drift;
   scalars[k++] = (int32_t)st->vbr_offset;
   scalars[k++] = (int32_t)st->vbr_count;
   scalars[k++] = (int32_t)st->overlap_max;
   scalars[k++] = (int32_t)st->stereo_saving;
   scalars[k++] = (int32_t)st->intensity;
   scalars[k++] = (int32_t)st->spec_avg;
   celt_assert(k == ORACLE_ENC_NSCALARS);

   for (i = 0; i < CC * overlap; i++)
      in_mem[i] = (int32_t)st->in_mem[i];
   for (i = 0; i < CC * COMBFILTER_MAXPERIOD; i++)
      prefilter_mem[i] = (int32_t)c_prefilter_mem[i];
   for (i = 0; i < CC * nbEBands; i++) {
      oldBandE[i] = (int32_t)c_oldBandE[i];
      oldLogE[i] = (int32_t)c_oldLogE[i];
      oldLogE2[i] = (int32_t)c_oldLogE2[i];
      energyError[i] = (int32_t)c_energyError[i];
   }
}

static void oracle_celtenc_h_destroy(void *h)
{
   OracleEncHandle *H = (OracleEncHandle *)h;
   if (!H)
      return;
   free(H->st);
   free(H);
}

/* --- resampling_factor (celt/celt.c:62) ------------------------------------
   The ONE place a sample rate enters the CELT layer: celt_encoder_init sets
   st->upsample = resampling_factor(Fs) (celt_encoder.c:255) and the mode stays the
   48 kHz / 960 one. resampling_factor is a non-static symbol in celt.c (compiled by
   w_celt_celt.c) and is declared in celt.h, which celt_encoder.c pulls in above. */
static int32_t oracle_celt_resampling_factor(int32_t rate)
{
   return (int32_t)resampling_factor((opus_int32)rate);
}

/* Compile-time QCONST/GCONST constants, evaluated by the C compiler with EXACTLY the
   literal each call site in celt_encoder.c / bands.c writes. A literal with an "f"
   suffix is a float and is rounded in float32 before the shift; an unsuffixed literal
   is a double. The two disagree by a few ULPs, which is enough to flip a comparison
   and diverge the whole packet, so the Go port has to pick the matching helper per
   site. These wrappers pin that choice against the C rather than against a comment. */
static int32_t oracle_const_q29_098f(void)      { return QCONST32(.98f, 29); }        /* celt_encoder.c:447, :2242 */
static int32_t oracle_const_q29_099f(void)      { return QCONST32(.99f, 29); }        /* celt_encoder.c:1440 */
static int32_t oracle_const_q29_1_999999f(void) { return QCONST32(1.999999f, 29); }   /* celt_encoder.c:1354 */
static int32_t oracle_const_q29_3_999999d(void) { return QCONST32(3.999999, 29); }    /* celt_encoder.c:1387, DOUBLE (no f) */
static int32_t oracle_const_q31_sqrt2inv(void)  { return QCONST32(.70710678f, 31); }  /* bands.c:631 */
static int32_t oracle_const_gconst_31_9(void)   { return GCONST(31.9f); }             /* celt_encoder.c:1055 */
static int32_t oracle_const_gconst_0_0062(void) { return GCONST(.0062f); }            /* celt_encoder.c:1108 */

#endif /* GOOPUS_CELTENC_SHIM_H */
