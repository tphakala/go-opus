#!/usr/bin/env bash
#
# gen_wrappers.sh regenerates the per-source cgo wrapper files (w_*.c) that
# compile the pinned libopus (../libopus) as the FIXED_POINT + DISABLE_FLOAT_API
# differential oracle.
#
# WHY PER-SOURCE WRAPPERS (one translation unit each):
# libopus is NOT a unity/amalgamation-safe codebase. include/opus_custom.h gates
# the opus_custom_{encoder,decoder}_{get_size,init} prototypes on per-file macros
# (CELT_ENCODER_C / CELT_DECODER_C), and celt/arch.h gates celt_fatal on CELT_C.
# Each .c self-defines its gating macro before including headers, but a single
# combined translation unit only honours the first includer because of the
# opus_custom.h include guard. Compiling every source as its own TU (exactly how
# libopus builds) is the only robust configuration and eliminates the whole class
# of unity-build macro-leak miscompiles that would silently corrupt the oracle.
#
# cgo compiles every .c file sitting in the package directory, so each wrapper is
# a thin one-liner that #includes the real source from the submodule. The
# //go:build refc constraint keeps them out of the pure-Go `go build ./...`.
#
# Re-run this after re-pinning the libopus submodule to a new tag:
#   cd internal/reftest/oracle && ./gen_wrappers.sh
#
# The source lists come straight from libopus's own *_sources.mk manifests, so a
# re-pin picks up added/removed files automatically. SIMD (x86/arm/mips), dnn/,
# multistream, projection, and the float-only paths are deliberately excluded.
set -euo pipefail

cd "$(dirname "$0")"
LIB=../libopus
[ -f "$LIB/celt_sources.mk" ] || { echo "libopus submodule not checked out at $LIB" >&2; exit 1; }

# extract MAKEVAR file.mk -> prints the .c paths in that make list
extract() {
  awk -v var="$1" '
    $0 ~ "^"var" *=" {grab=1}
    grab {
      line=$0; gsub(/\\/,"",line)
      n=split(line, a, /[ \t]+/)
      for (i=1;i<=n;i++) if (a[i] ~ /\.c$/) print a[i]
      if ($0 !~ /\\[ \t]*$/) grab=0
    }' "$2"
}

# Portable oracle source set (no SIMD, no dnn, no float API, no multistream).
SOURCES=$(
  extract CELT_SOURCES        "$LIB/celt_sources.mk"
  extract SILK_SOURCES        "$LIB/silk_sources.mk"
  extract SILK_SOURCES_FIXED  "$LIB/silk_sources.mk"
  # src/ subset: top-level API + framing only. Excludes analysis.c/mlp*.c
  # (OPUS_SOURCES_FLOAT, gone under DISABLE_FLOAT_API) and multistream/projection.
  printf '%s\n' \
    src/opus.c \
    src/opus_decoder.c \
    src/opus_encoder.c \
    src/extensions.c \
    src/repacketizer.c
)

# Remove previously generated wrappers so deletions upstream are reflected.
rm -f w_*.c

count=0
while IFS= read -r src; do
  [ -n "$src" ] || continue
  [ -f "$LIB/$src" ] || { echo "listed source missing: $LIB/$src" >&2; exit 1; }
  # w_<dir>_<file>.c, e.g. celt/bands.c -> w_celt_bands.c,
  # silk/fixed/autocorr_FIX.c -> w_silk_fixed_autocorr_FIX.c
  name="w_$(printf '%s' "$src" | sed 's,/,_,g')"
  if [ "$src" = "celt/celt_encoder.c" ]; then
    # celt_encoder.c is compiled SOLELY by celtenc_shim.h (celtenc_cgo.go), so the
    # CP8a encoder differential test can reach its file-static stage functions.
    # Compiling it here too would duplicate its external symbols and fail to link
    # (Option A). Emit a neutralized stub instead of the normal #include wrapper.
    {
      printf '//go:build refc\n\n'
      printf '// NEUTRALIZED wrapper for celt/celt_encoder.c. Compiled SOLELY by\n'
      printf '// celtenc_shim.h (celtenc_cgo.go) so the CP8a encoder differential test can\n'
      printf '// reach its file-static stage functions; a second TU compiling it here would\n'
      printf '// duplicate celt_encode_with_ec / celt_encoder_init / celt_preemphasis and fail\n'
      printf '// to link. celtdec_shim.h and opusenc_shim.h reference celt_encode_with_ec\n'
      printf '// extern and resolve against celtenc_shim.h at link time.\n'
      printf 'typedef int goopus_w_celt_celt_encoder_neutralized;\n'
    } > "$name"
    count=$((count + 1))
    continue
  fi
  if [ "$src" = "src/opus_encoder.c" ]; then
    # Same Option A treatment as celt/celt_encoder.c, for the same reason.
    # opus_encoder.c defines `struct OpusEncoder` at :76 and the stage statics
    # gen_toc / dc_reject / stereo_fade / user_bitrate_to_bitrate /
    # compute_equiv_rate INSIDE the .c: there is no header, so the struct is
    # opaque and the statics are unreachable from any other translation unit.
    # The CP9 top-level encoder differential test needs BOTH (a field-level state
    # dump and flat wrappers over the statics), so opusenc_shim.h (opusenc_cgo.go)
    # #includes opus_encoder.c and becomes its SOLE translation unit. A second TU
    # here would duplicate opus_encode / opus_encoder_create / opus_encoder_ctl /
    # opus_encoder_init / frame_size_select / compute_stereo_width /
    # is_digital_silence / downmix_int and fail to link.
    #
    # shim.c and opusdec_shim.h keep calling opus_encoder_create / opus_encode /
    # opus_encoder_ctl through the PUBLIC prototypes in opus.h; those resolve at
    # link time against the definitions inside opusenc_shim.h, exactly as
    # celtdec_shim.h resolves celt_encode_with_ec against celtenc_shim.h today.
    {
      printf '//go:build refc\n\n'
      printf '// NEUTRALIZED wrapper for src/opus_encoder.c. Compiled SOLELY by\n'
      printf '// opusenc_shim.h (opusenc_cgo.go) so the CP9 top-level encoder differential\n'
      printf '// test can reach `struct OpusEncoder` (defined in the .c, not in any header)\n'
      printf '// and the file statics gen_toc / dc_reject / stereo_fade /\n'
      printf '// user_bitrate_to_bitrate / compute_equiv_rate; a second TU compiling it here\n'
      printf '// would duplicate opus_encode / opus_encoder_create / opus_encoder_ctl /\n'
      printf '// opus_encoder_init / frame_size_select / downmix_int and fail to link.\n'
      printf '// shim.c and opusdec_shim.h call the public opus.h prototypes and resolve\n'
      printf '// against opusenc_shim.h at link time.\n'
      printf 'typedef int goopus_w_src_opus_encoder_neutralized;\n'
    } > "$name"
    count=$((count + 1))
    continue
  fi
  {
    printf '//go:build refc\n\n'
    printf '// Generated by gen_wrappers.sh. Compiles %s as its own translation\n' "$src"
    printf '// unit for the FIXED_POINT + DISABLE_FLOAT_API libopus oracle.\n'
    printf '#include "libopus/%s"\n' "$src"
  } > "$name"
  count=$((count + 1))
done <<< "$SOURCES"

echo "generated $count wrapper(s) under $(pwd)"
