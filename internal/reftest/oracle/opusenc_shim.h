//go:build refc

/*
 * opusenc_shim.h - C-callable wrappers over the pinned libopus TOP-LEVEL Opus
 * ENCODER (src/opus_encoder.c) for the phase-4 CP9 differential test of the
 * pure-Go internal/opusenc port.
 *
 * SOLE TRANSLATION UNIT FOR src/opus_encoder.c (Option A, exactly as
 * celtenc_shim.h is the sole TU for celt/celt_encoder.c). Two things the CP9 gate
 * needs are unreachable from any other translation unit:
 *
 *   1. `struct OpusEncoder` is DEFINED IN THE .c (opus_encoder.c:76), not in any
 *      header. opus.h only forward-declares it, so the struct is OPAQUE
 *      everywhere else and a field-level state dump is impossible from outside.
 *   2. gen_toc (:330), dc_reject (:479), stereo_fade (:548),
 *      user_bitrate_to_bitrate (:733) and compute_equiv_rate (:1027) are
 *      file-static.
 *
 * So this header #includes opus_encoder.c directly, w_src_opus_encoder.c is
 * NEUTRALIZED by gen_wrappers.sh, and ONLY opusenc_cgo.go may include this
 * header. A second cgo TU including it would duplicate opus_encode /
 * opus_encoder_create / opus_encoder_ctl / opus_encoder_init / frame_size_select
 * / compute_stereo_width / is_digital_silence / downmix_int and fail to link.
 *
 * shim.c and opusdec_shim.h keep calling opus_encoder_create / opus_encode /
 * opus_encoder_ctl through the PUBLIC prototypes in opus.h; they resolve at link
 * time against the definitions compiled here, since the whole oracle package
 * links into one test binary. That is the same arrangement by which celtdec_shim.h
 * resolves celt_encode_with_ec against celtenc_shim.h today. NOTHING in shim.c
 * changes.
 *
 * Build config (oracle_cgo.go CFLAGS): FIXED_POINT + DISABLE_FLOAT_API +
 * OPUS_FAST_INT64, non-CUSTOM_MODES, non-QEXT, non-RES24, no DRED. So opus_res is
 * opus_int16, opus_val32 (hp_mem) is opus_int32, celt_coef is opus_int16, and
 * MAX_ENCODER_BUFFER is 480. This header only adds static glue; it never edits
 * the shared oracle surface (shim.h/shim.c/oracle_cgo.go).
 */
#ifndef GOOPUS_OPUSENC_SHIM_H
#define GOOPUS_OPUSENC_SHIM_H

/* The one and only compilation of src/opus_encoder.c. */
#include "libopus/src/opus_encoder.c"

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

/* Fail the build loudly if the type/geometry config drifts from what these
   wrappers assume. */
typedef char oracle_topenc_assert_res_is_int16[(sizeof(opus_res) == 2) ? 1 : -1];
typedef char oracle_topenc_assert_val32_is_int32[(sizeof(opus_val32) == 4) ? 1 : -1];
typedef char oracle_topenc_assert_coef_is_int16[(sizeof(celt_coef) == 2) ? 1 : -1];
typedef char oracle_topenc_assert_max_enc_buf[(MAX_ENCODER_BUFFER == 480) ? 1 : -1];

/* ==========================================================================
 * Flat wrappers over the file statics.
 * ========================================================================== */

/* gen_toc (opus_encoder.c:330). Returns the TOC byte (0..255). NOTE the call
   site at :2551 passes st->stream_channels, NOT st->channels. */
static int oracle_topenc_gen_toc(int mode, int framerate, int bandwidth, int channels)
{
   return (int)gen_toc(mode, framerate, bandwidth, channels);
}

/* dc_reject (opus_encoder.c:479, the FIXED_POINT variant). in/out are interleaved
   opus_res (int16) of len*channels; hp_mem is 4 opus_val32 (int32) threaded across
   frames. The Opus encoder always calls it with cutoff_Hz == 3 (:2008). */
static void oracle_topenc_dc_reject(const int16_t *in, int32_t cutoff_Hz, int16_t *out,
      int32_t *hp_mem, int len, int channels, int32_t Fs)
{
   dc_reject((const opus_res *)in, (opus_int32)cutoff_Hz, (opus_res *)out,
         (opus_val32 *)hp_mem, len, channels, (opus_int32)Fs);
}

/* stereo_fade (opus_encoder.c:548). The overlap48 and window arguments always
   come from the CELT mode (:2344), so they are resolved here rather than passed:
   the frozen 48 kHz / 960 mode has overlap 120. */
static void oracle_topenc_stereo_fade(const int16_t *in, int16_t *out,
      int16_t g1, int16_t g2, int frame_size, int channels, int32_t Fs)
{
   int e = 0;
   const CELTMode *m = opus_custom_mode_create(48000, 960, &e);
   stereo_fade((const opus_res *)in, (opus_res *)out, (opus_val16)g1, (opus_val16)g2,
         m->overlap, frame_size, channels, m->window, (opus_int32)Fs);
}

/* user_bitrate_to_bitrate (opus_encoder.c:733). It reads only st->Fs,
   st->user_bitrate_bps and st->channels, so a zeroed scratch OpusEncoder with
   those three fields set reproduces it exactly. */
static int32_t oracle_topenc_user_bitrate_to_bitrate(int32_t Fs, int channels,
      int32_t user_bitrate_bps, int frame_size, int max_data_bytes)
{
   OpusEncoder tmp;
   memset(&tmp, 0, sizeof(tmp));
   tmp.Fs = (opus_int32)Fs;
   tmp.channels = channels;
   tmp.user_bitrate_bps = (opus_int32)user_bitrate_bps;
   return (int32_t)user_bitrate_to_bitrate(&tmp, frame_size, max_data_bytes);
}

/* compute_equiv_rate (opus_encoder.c:1027). */
static int32_t oracle_topenc_compute_equiv_rate(int32_t bitrate, int channels,
      int frame_rate, int vbr, int mode, int complexity, int loss)
{
   return (int32_t)compute_equiv_rate((opus_int32)bitrate, channels, frame_rate,
         vbr, mode, complexity, loss);
}

/* bits_to_bitrate / bitrate_to_bits (celt/celt.h:147,151), the two static inlines
   the CBR byte-budget derivation (:1330-1333) is built out of. */
static int32_t oracle_topenc_bits_to_bitrate(int32_t bits, int32_t Fs, int32_t frame_size)
{
   return (int32_t)bits_to_bitrate((opus_int32)bits, (opus_int32)Fs, (opus_int32)frame_size);
}

static int32_t oracle_topenc_bitrate_to_bits(int32_t bitrate, int32_t Fs, int32_t frame_size)
{
   return (int32_t)bitrate_to_bits((opus_int32)bitrate, (opus_int32)Fs, (opus_int32)frame_size);
}

/* frame_size_select (opus_encoder.c:827) is extern (declared in opus_private.h),
   but wrapping it here keeps the CP9 rejection-rule test on one surface. */
static int32_t oracle_topenc_frame_size_select(int application, int32_t frame_size,
      int variable_duration, int32_t Fs)
{
   return (int32_t)frame_size_select(application, (opus_int32)frame_size,
         variable_duration, (opus_int32)Fs);
}

/* opus_packet_pad (src/repacketizer.c:359), which the CBR path calls at
   opus_encoder.c:2646 (apply_padding = !use_vbr). Pads data[0:len] in place up to
   new_len bytes and returns OPUS_OK, or a negative OPUS_* error. data must have
   room for new_len bytes. */
static int oracle_topenc_packet_pad(unsigned char *data, int len, int new_len)
{
   return opus_packet_pad(data, (opus_int32)len, (opus_int32)new_len);
}

/* ==========================================================================
 * Handle triple: create / encode-frame (+ state dump) / destroy.
 * ========================================================================== */

/* oracle_topenc_cfg is the full ctl set applied right after opus_encoder_create.
   Every field is applied unconditionally, so the Go driver must supply the
   opus_encoder_init defaults (opus_encoder.c:293-317) for the ones it does not
   care about; passing the defaults back through the ctls is idempotent. */
typedef struct {
    int32_t Fs;
    int32_t channels;
    int32_t application;           /* OPUS_APPLICATION_* (passed to create) */
    int32_t force_mode;            /* OPUS_SET_FORCE_MODE: OPUS_AUTO or MODE_* */
    int32_t bitrate;               /* OPUS_SET_BITRATE: OPUS_AUTO/OPUS_BITRATE_MAX/bps */
    int32_t complexity;            /* OPUS_SET_COMPLEXITY: 0..10 */
    int32_t vbr;                   /* OPUS_SET_VBR */
    int32_t vbr_constraint;        /* OPUS_SET_VBR_CONSTRAINT */
    int32_t force_channels;        /* OPUS_SET_FORCE_CHANNELS */
    int32_t max_bandwidth;         /* OPUS_SET_MAX_BANDWIDTH */
    int32_t bandwidth;             /* OPUS_SET_BANDWIDTH */
    int32_t signal_type;           /* OPUS_SET_SIGNAL */
    int32_t lsb_depth;             /* OPUS_SET_LSB_DEPTH: 8..24 */
    int32_t dtx;                   /* OPUS_SET_DTX */
    int32_t inband_fec;            /* OPUS_SET_INBAND_FEC */
    int32_t packet_loss_perc;      /* OPUS_SET_PACKET_LOSS_PERC */
    int32_t prediction_disabled;   /* OPUS_SET_PREDICTION_DISABLED */
    int32_t lfe;                   /* OPUS_SET_LFE */
    int32_t expert_frame_duration; /* OPUS_SET_EXPERT_FRAME_DURATION */
} oracle_topenc_cfg;

/* oracle_topenc_state is the FIELD-LEVEL dump of the cross-frame OpusEncoder
 * state, in the CANONICAL ORDER the Go internal/opusenc State()/Hash() uses.
 * Everything here lives at or after OPUS_ENCODER_RESET_START (opus_encoder.c:111),
 * i.e. it is exactly what OPUS_RESET_STATE clears.
 *
 * DELIBERATELY EXCLUDED: st->width_mem (StereoWidthState, mutated by
 * compute_stereo_width at :1322) and st->peak_signal_energy (mutated at :1317).
 * Both are EXECUTED but OUTPUT-DEAD in the frozen forced-CELT-only config:
 * stereo_width is consumed only inside the user_forced_mode==OPUS_AUTO mode
 * decision, and peak_signal_energy is consumed only by DTX. The Go port omits
 * both computations, so comparing them would compare a value the port provably
 * does not need. Enabling DTX makes peak_signal_energy LIVE and this exclusion
 * must be revisited then.
 *
 * Also excluded, and not state: silk_mode.stereoWidth_Q14 (recomputed from
 * equiv_rate every frame at :2320-2327 before any read), nonfinal_frame (only set
 * inside the deferred multiframe path), silk_bw_switch and nb_no_activity_ms_Q1
 * (SILK/DTX only, never written in CELT-only).
 */
typedef struct {
    int32_t  stream_channels;
    int32_t  hybrid_stereo_width_Q14;
    int32_t  variable_HP_smth2_Q15;
    int32_t  prev_HB_gain;
    int32_t  hp_mem[4];
    int32_t  mode;
    int32_t  prev_mode;
    int32_t  prev_channels;
    int32_t  prev_framesize;
    int32_t  bandwidth;
    int32_t  auto_bandwidth;
    int32_t  first;
    int32_t  bitrate_bps;
    uint32_t rangeFinal;
    /* delay_len = encoder_buffer*channels: the VALID prefix of delay_buffer. The
       C allocation is shortened by MAX_ENCODER_BUFFER*sizeof(opus_res) for mono
       (opus_encoder.c:235), so reading past it would read past the malloc. */
    int32_t  delay_len;
    int16_t  delay_buffer[MAX_ENCODER_BUFFER * 2];
} oracle_topenc_state;

typedef struct {
    OpusEncoder *enc;
    int channels;
    int Fs;
} oracle_topenc_h;

/* oracle_topenc_create builds an encoder and applies the whole ctl set. On
   failure it returns NULL and writes the opus error code to *err. */
static oracle_topenc_h *oracle_topenc_create(const oracle_topenc_cfg *cfg, int *err)
{
   oracle_topenc_h *h;
   int e = OPUS_OK;

   if (err) *err = OPUS_OK;
   h = (oracle_topenc_h *)calloc(1, sizeof(*h));
   if (h == NULL) {
      if (err) *err = OPUS_ALLOC_FAIL;
      return NULL;
   }
   h->channels = (int)cfg->channels;
   h->Fs = (int)cfg->Fs;

   h->enc = opus_encoder_create((opus_int32)cfg->Fs, (int)cfg->channels,
         (int)cfg->application, &e);
   if (h->enc == NULL || e != OPUS_OK) {
      if (err) *err = (e != OPUS_OK) ? e : OPUS_ALLOC_FAIL;
      if (h->enc) opus_encoder_destroy(h->enc);
      free(h);
      return NULL;
   }

#define ORACLE_OPUSENC_CTL(call)                       \
   do {                                                \
      e = opus_encoder_ctl(h->enc, call);              \
      if (e != OPUS_OK) {                              \
         if (err) *err = e;                            \
         opus_encoder_destroy(h->enc);                 \
         free(h);                                      \
         return NULL;                                  \
      }                                                \
   } while (0)

   ORACLE_OPUSENC_CTL(OPUS_SET_FORCE_MODE((opus_int32)cfg->force_mode));
   ORACLE_OPUSENC_CTL(OPUS_SET_BITRATE((opus_int32)cfg->bitrate));
   ORACLE_OPUSENC_CTL(OPUS_SET_COMPLEXITY((opus_int32)cfg->complexity));
   ORACLE_OPUSENC_CTL(OPUS_SET_VBR((opus_int32)cfg->vbr));
   ORACLE_OPUSENC_CTL(OPUS_SET_VBR_CONSTRAINT((opus_int32)cfg->vbr_constraint));
   ORACLE_OPUSENC_CTL(OPUS_SET_FORCE_CHANNELS((opus_int32)cfg->force_channels));
   ORACLE_OPUSENC_CTL(OPUS_SET_MAX_BANDWIDTH((opus_int32)cfg->max_bandwidth));
   ORACLE_OPUSENC_CTL(OPUS_SET_BANDWIDTH((opus_int32)cfg->bandwidth));
   ORACLE_OPUSENC_CTL(OPUS_SET_SIGNAL((opus_int32)cfg->signal_type));
   ORACLE_OPUSENC_CTL(OPUS_SET_LSB_DEPTH((opus_int32)cfg->lsb_depth));
   ORACLE_OPUSENC_CTL(OPUS_SET_DTX((opus_int32)cfg->dtx));
   ORACLE_OPUSENC_CTL(OPUS_SET_INBAND_FEC((opus_int32)cfg->inband_fec));
   ORACLE_OPUSENC_CTL(OPUS_SET_PACKET_LOSS_PERC((opus_int32)cfg->packet_loss_perc));
   ORACLE_OPUSENC_CTL(OPUS_SET_PREDICTION_DISABLED((opus_int32)cfg->prediction_disabled));
   ORACLE_OPUSENC_CTL(OPUS_SET_LFE((opus_int32)cfg->lfe));
   ORACLE_OPUSENC_CTL(OPUS_SET_EXPERT_FRAME_DURATION((opus_int32)cfg->expert_frame_duration));

#undef ORACLE_OPUSENC_CTL

   return h;
}

/* oracle_topenc_reset applies OPUS_RESET_STATE (opus_encoder.c:3243). */
static int oracle_topenc_reset(oracle_topenc_h *h)
{
   return opus_encoder_ctl(h->enc, OPUS_RESET_STATE);
}

/* oracle_topenc_get_state dumps the cross-frame state in canonical order. */
static void oracle_topenc_get_state(oracle_topenc_h *h, oracle_topenc_state *s)
{
   OpusEncoder *st = h->enc;
   int i, n;

   memset(s, 0, sizeof(*s));
   s->stream_channels         = st->stream_channels;
   s->hybrid_stereo_width_Q14 = st->hybrid_stereo_width_Q14;
   s->variable_HP_smth2_Q15   = st->variable_HP_smth2_Q15;
   s->prev_HB_gain            = st->prev_HB_gain;
   for (i = 0; i < 4; i++)
      s->hp_mem[i] = (int32_t)st->hp_mem[i];
   s->mode           = st->mode;
   s->prev_mode      = st->prev_mode;
   s->prev_channels  = st->prev_channels;
   s->prev_framesize = st->prev_framesize;
   s->bandwidth      = st->bandwidth;
   s->auto_bandwidth = st->auto_bandwidth;
   s->first          = st->first;
   s->bitrate_bps    = st->bitrate_bps;
   s->rangeFinal     = st->rangeFinal;

   n = st->encoder_buffer * st->channels;
   if (n > MAX_ENCODER_BUFFER * 2) n = MAX_ENCODER_BUFFER * 2;
   if (n < 0) n = 0;
   s->delay_len = n;
   for (i = 0; i < n; i++)
      s->delay_buffer[i] = (int16_t)st->delay_buffer[i];
}

/* oracle_topenc_encode encodes one frame and dumps the resulting state. Returns
   opus_encode's return value verbatim (a packet length, or a negative OPUS_*
   error). On the error path the state dump still runs, so the Go test can assert
   that a rejected frame left the state untouched. */
static int oracle_topenc_encode(oracle_topenc_h *h, const int16_t *pcm, int frame_size,
      unsigned char *data, int max_data_bytes, oracle_topenc_state *s)
{
   int ret = opus_encode(h->enc, (const opus_int16 *)pcm, frame_size,
         data, (opus_int32)max_data_bytes);
   oracle_topenc_get_state(h, s);
   return ret;
}

/* oracle_topenc_encode_bare is oracle_topenc_encode WITHOUT the state dump: it is
   opus_encode and nothing else.

   It exists ONLY for the Go-vs-C benchmark. oracle_topenc_get_state walks the whole
   OpusEncoder and copies out ~40 fields including the delay_buffer, which costs real
   time per frame. Timing the encode entry point the differential tests use would
   therefore charge libopus for work libopus does not do, making the C look slower
   than it is and the Go port look better than it is by exactly the margin we are
   trying to measure. The differential path above is deliberately left alone: it
   still dumps state, because that is what it is for. */
static int oracle_topenc_encode_bare(oracle_topenc_h *h, const int16_t *pcm, int frame_size,
      unsigned char *data, int max_data_bytes)
{
   return opus_encode(h->enc, (const opus_int16 *)pcm, frame_size,
         data, (opus_int32)max_data_bytes);
}

static void oracle_topenc_destroy(oracle_topenc_h *h)
{
   if (h == NULL) return;
   if (h->enc) opus_encoder_destroy(h->enc);
   free(h);
}

/* Compile-time float-literal constant, pinned the same way as the celt QCONST32 /
   GCONST wrappers in celtenc_shim.h. SILK_FIX_CONST, like QCONST32, evaluates its
   product IN THE PRECISION OF THE LITERAL: VARIABLE_HP_SMTH_COEF2 is 0.015f
   (silk/tuning_parameters.h:63), an "f"-suffixed FLOAT, so the int64 shift is
   converted to float rather than the literal to double. Here the two readings
   happen to agree, but that is a fact to be VERIFIED against the C compiler, not
   assumed: this is the exact class of constant that produced two real packet
   divergences in CP8c. Used at opus_encoder.c:1975. */
static int32_t oracle_const_silk_variable_hp_smth_coef2_q16(void)
{
   return (int32_t)SILK_FIX_CONST(VARIABLE_HP_SMTH_COEF2, 16);
}

/* OPUS_GET_LOOKAHEAD (opus_encoder.c:3082). Returns Fs/400 plus, for every application
   except the RESTRICTED ones, delay_compensation. This value never reaches a packet
   byte, so the bit-exact packet gate cannot see an error in it, but the Ogg Opus
   pre-skip field is derived from it: getting it wrong misaligns every decoded stream. */
static int32_t oracle_topenc_lookahead(int32_t fs, int channels, int application)
{
   int err = 0;
   int32_t v = -1;
   OpusEncoder *e = opus_encoder_create(fs, channels, application, &err);
   if (e == NULL || err != OPUS_OK) return -1;
   if (opus_encoder_ctl(e, OPUS_GET_LOOKAHEAD(&v)) != OPUS_OK) v = -1;
   opus_encoder_destroy(e);
   return v;
}

#endif /* GOOPUS_OPUSENC_SHIM_H */
