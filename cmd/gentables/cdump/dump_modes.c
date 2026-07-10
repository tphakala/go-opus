/* dump_modes.c: emits the libopus CELT static mode data (mode48000_960) as a
 * flat token stream that cmd/gentables parses into Go source. It is compiled
 * and run by the generator, not by the normal Go toolchain (a bare .c file is
 * ignored by `go build`). See docs/hard-parts.md section 6.
 *
 * Build config mirrors the reftest oracle: FIXED_POINT + DISABLE_FLOAT_API,
 * non-CUSTOM_MODES, non-QEXT. Including modes.c pulls in both the eband5ms /
 * band_allocation tables (defined in modes.c) and, transitively, every array
 * from static_modes_fixed.h, so all values below come straight from source.
 *
 * Output format: one record per line, "KEY COUNT v0 v1 ... v(COUNT-1)", all
 * values decimal ints. The generator validates COUNT against its own schema.
 */

#include <stdio.h>
#include "modes.h"
#include "modes.c"

/* Structural lengths of the mode48000_960 arrays (see static_modes_fixed.h and
 * modes.h). Kept as literals matching the upstream declarations; the Go
 * generator re-asserts each one so a submodule reshape fails loudly. */
#define N_WINDOW 120
#define N_LOGN 21
#define N_CACHE_INDEX 105
#define N_CACHE_BITS 392
#define N_CACHE_CAPS 168
#define N_EBANDS 22   /* nbEBands + 1 */
#define N_ALLOC 231   /* nbAllocVectors * nbEBands = 11 * 21 */
#define N_TWIDDLE 480 /* kiss_twiddle_cpx pairs */
#define N_BITREV480 480
#define N_BITREV240 240
#define N_BITREV120 120
#define N_BITREV60 60
#define N_MDCT_TWIDDLES 1800
#define N_FACTORS 16 /* 2 * MAXFACTORS */

static void emit_i16(const char *key, const opus_int16 *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

static void emit_u8(const char *key, const unsigned char *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

static void emit_coef(const char *key, const celt_coef *a, int n) {
    printf("%s %d", key, n);
    for (int i = 0; i < n; i++) printf(" %d", (int)a[i]);
    printf("\n");
}

/* Emit one kiss_fft_state as 4 scalars followed by its 16 factors:
 * nfft scale scaleShift shift f0..f15. */
static void emit_fft_state(const char *key, const kiss_fft_state *s) {
    printf("%s %d", key, 4 + N_FACTORS);
    printf(" %d %d %d %d", s->nfft, (int)s->scale, s->scale_shift, s->shift);
    for (int i = 0; i < N_FACTORS; i++) printf(" %d", (int)s->factors[i]);
    printf("\n");
}

int main(void) {
    const CELTMode *m = &mode48000_960_120;

    /* Leaf data arrays, referenced by their upstream symbol names. */
    emit_coef("window120", window120, N_WINDOW);
    emit_i16("logN400", logN400, N_LOGN);
    emit_i16("cacheIndex50", cache_index50, N_CACHE_INDEX);
    emit_u8("cacheBits50", cache_bits50, N_CACHE_BITS);
    emit_u8("cacheCaps50", cache_caps50, N_CACHE_CAPS);
    emit_i16("eband5ms", eband5ms, N_EBANDS);
    emit_u8("bandAllocation", band_allocation, N_ALLOC);
    emit_coef("mdctTwiddles960", mdct_twiddles960, N_MDCT_TWIDDLES);
    emit_i16("fftBitrev480", fft_bitrev480, N_BITREV480);
    emit_i16("fftBitrev240", fft_bitrev240, N_BITREV240);
    emit_i16("fftBitrev120", fft_bitrev120, N_BITREV120);
    emit_i16("fftBitrev60", fft_bitrev60, N_BITREV60);

    /* FFT twiddles: N_TWIDDLE complex pairs flattened to r0 i0 r1 i1 ... */
    printf("fftTwiddles48000960 %d", 2 * N_TWIDDLE);
    for (int i = 0; i < N_TWIDDLE; i++) {
        printf(" %d %d", (int)fft_twiddles48000_960[i].r, (int)fft_twiddles48000_960[i].i);
    }
    printf("\n");

    /* The four kiss_fft_state instances used by the MDCT lookup. */
    emit_fft_state("fftState0", &fft_state48000_960_0);
    emit_fft_state("fftState1", &fft_state48000_960_1);
    emit_fft_state("fftState2", &fft_state48000_960_2);
    emit_fft_state("fftState3", &fft_state48000_960_3);

    /* Scalar mode fields, in CELTMode declaration order. */
    printf("preemph %d %d %d %d %d\n", 4,
        (int)m->preemph[0], (int)m->preemph[1], (int)m->preemph[2], (int)m->preemph[3]);
    printf("modeScalars %d", 11);
    printf(" %d", (int)m->Fs);
    printf(" %d", m->overlap);
    printf(" %d", m->nbEBands);
    printf(" %d", m->effEBands);
    printf(" %d", m->maxLM);
    printf(" %d", m->nbShortMdcts);
    printf(" %d", m->shortMdctSize);
    printf(" %d", m->nbAllocVectors);
    printf(" %d", m->cache.size);
    printf(" %d", m->mdct.n);
    printf(" %d", m->mdct.maxshift);
    printf("\n");

    return 0;
}
