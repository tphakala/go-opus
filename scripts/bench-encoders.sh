#!/usr/bin/env bash
#
# bench-encoders.sh - whole-file encoder comparison: go-opus vs libopus as it ships.
#
# Times cmd/wav2opus (pure Go, fixed-point, scalar) against opusenc and
# ffmpeg -c:a libopus (C, float, SIMD) on one WAV, and reports wall time, CPU
# seconds, peak RSS, input throughput, the achieved output bitrate, and -- the
# part that makes the comparison mean anything -- the coding mode each encoder
# actually used, measured from the bitstream.
#
# THE FAIRNESS PROBLEM
#
#   go-opus's encoder is CELT-only. libopus in its default mode picks SILK,
#   CELT, or hybrid per frame; those are different algorithms with different
#   costs. Timing our CELT against whatever libopus happened to choose measures
#   nothing. So every row carries a Mode column parsed out of the Opus TOC bytes
#   (scripts/opus-modes.py, RFC 6716 s3.1). If a row does not say CELT 100%, that
#   row is not comparable and the table says so instead of hiding it.
#
#   ffmpeg can be forced CELT-only with -application lowdelay
#   (OPUS_APPLICATION_RESTRICTED_LOWDELAY). opusenc has NO such flag: it always
#   builds its encoder with OPUS_APPLICATION_AUDIO, and its --set-ctl-int escape
#   hatch whitelists only the public OPUS_SET_* (4xxx) requests, so it rejects
#   OPUS_SET_FORCE_MODE (11002). The opusenc row is therefore mode-matched only
#   if libopus's own mode decision lands on CELT -- which at music-like content
#   and >=96 kbit/s stereo it does. The Mode column is what proves it, per run.
#
# usage: scripts/bench-encoders.sh [input.wav]
#
#   With no argument, synthesizes a deterministic tone+noise mix with ffmpeg at a
#   fixed seed, so the run reproduces across machines.
#
# environment overrides:
#   BITRATE=96000     target bitrate, bits/sec
#   COMPLEXITY=10     encoder complexity 0..10
#   DURATION=300      synthesized input length, seconds
#   CHANNELS=2        synthesized input channels (1 or 2)
#   RATE=48000        synthesized input sample rate
#   SEED=42           synthesized noise seed
#   KEEP=1            keep the work directory instead of deleting it

set -euo pipefail

BITRATE=${BITRATE:-96000}
COMPLEXITY=${COMPLEXITY:-10}
DURATION=${DURATION:-300}
CHANNELS=${CHANNELS:-2}
RATE=${RATE:-48000}
SEED=${SEED:-42}
KEEP=${KEEP:-}

BITRATE_K=$((BITRATE / 1000))

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
modes_py="$repo_root/scripts/opus-modes.py"

work=$(mktemp -d "${TMPDIR:-/tmp}/bench-encoders.XXXXXX")
cleanup() { [ -n "$KEEP" ] || rm -rf "$work"; }
trap cleanup EXIT

note() { printf '  note: %s\n' "$*" >&2; }
die() { printf 'bench-encoders: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# time(1): BSD and GNU disagree on both the flags and the units of max RSS.
# BSD  (/usr/bin/time -l)          reports "maximum resident set size" in BYTES.
# GNU  (/usr/bin/time -f '%M')     reports Maxrss                     in KILOBYTES.
# Getting this wrong is a silent 1024x error in one column, so detect, don't guess.
# ---------------------------------------------------------------------------
if command -v gtime >/dev/null 2>&1; then
    TIME_BIN=$(command -v gtime); TIME_KIND=gnu
elif /usr/bin/time -f '%e' true >/dev/null 2>&1; then
    TIME_BIN=/usr/bin/time; TIME_KIND=gnu
elif /usr/bin/time -l true >/dev/null 2>&1; then
    TIME_BIN=/usr/bin/time; TIME_KIND=bsd
else
    die "no usable time(1): need GNU time (-f) or BSD time (-l)"
fi

# run_timed <metrics-file> <cmd>...
# Writes "wall user sys rss_kb" to the metrics file. Returns the command's status.
run_timed() {
    local metrics=$1; shift
    local raw="$work/time.raw"
    local wall user sys rss_kb rss_b status

    if [ "$TIME_KIND" = gnu ]; then
        # -o keeps the timing report out of the tool's own stderr.
        status=0
        "$TIME_BIN" -f '%e %U %S %M' -o "$raw" "$@" >/dev/null 2>>"$work/tool.log" || status=$?
        [ "$status" -eq 0 ] || return "$status"
        read -r wall user sys rss_kb < "$raw"
    else
        # BSD time has no -o: its report and the tool's stderr share fd 2, so we
        # capture both and pick the timing lines back out by their shape.
        status=0
        "$TIME_BIN" -l "$@" >/dev/null 2>"$raw" || status=$?
        cat "$raw" >> "$work/tool.log"
        [ "$status" -eq 0 ] || return "$status"
        wall=$(awk '$2=="real"{v=$1} END{print v}' "$raw")
        user=$(awk '$4=="user"{v=$3} END{print v}' "$raw")
        sys=$(awk '$6=="sys"{v=$5} END{print v}' "$raw")
        rss_b=$(awk '/maximum resident set size/{v=$1} END{print v}' "$raw")
        rss_kb=$((rss_b / 1024))
    fi

    printf '%s %s %s %s\n' "$wall" "$user" "$sys" "$rss_kb" > "$metrics"
}

# ---------------------------------------------------------------------------
# Input: a caller-supplied WAV, or a deterministic synthetic one.
# ---------------------------------------------------------------------------
input=${1:-}
if [ -n "$input" ]; then
    [ -f "$input" ] || die "no such file: $input"
else
    command -v ffmpeg >/dev/null 2>&1 || die "ffmpeg is required to synthesize the input; pass a WAV instead"
    input="$work/input.wav"
    # Tone + noise, decorrelated per channel (independent seeded noise sources),
    # so stereo coding is not trivially mid/side. Fixed seed => reproducible.
    ffmpeg -nostdin -y -hide_banner -loglevel error \
        -f lavfi -i "sine=frequency=440:sample_rate=$RATE:duration=$DURATION" \
        -f lavfi -i "sine=frequency=659:sample_rate=$RATE:duration=$DURATION" \
        -f lavfi -i "anoisesrc=sample_rate=$RATE:duration=$DURATION:color=pink:seed=$SEED:amplitude=0.30" \
        -f lavfi -i "anoisesrc=sample_rate=$RATE:duration=$DURATION:color=brown:seed=$((SEED + 1)):amplitude=0.30" \
        -filter_complex \
          "[0][2]amix=inputs=2:duration=shortest:weights=0.7 0.6[l];\
           [1][3]amix=inputs=2:duration=shortest:weights=0.7 0.6[r];\
           [l][r]join=inputs=2:channel_layout=stereo,volume=0.8[a]" \
        -map "[a]" -ac "$CHANNELS" -ar "$RATE" -c:a pcm_s16le "$input"
fi

in_bytes=$(wc -c < "$input" | tr -d ' ')

# Probe the input we are ACTUALLY encoding rather than echoing the synth
# defaults back: a caller-supplied WAV need not match them, and the duration
# drives the achieved-bitrate column.
if command -v python3 >/dev/null 2>&1; then
    read -r in_rate in_ch in_secs < <(python3 - "$input" <<'PY'
import sys, wave
with wave.open(sys.argv[1]) as w:
    print(w.getframerate(), w.getnchannels(), w.getnframes() / w.getframerate())
PY
)
elif command -v ffprobe >/dev/null 2>&1; then
    read -r in_rate in_ch in_secs < <(ffprobe -v error -select_streams a:0 \
        -show_entries stream=sample_rate,channels -show_entries format=duration \
        -of default=nw=1:nk=1 "$input" | tr '\n' ' ')
else
    die "need python3 or ffprobe to read the input format"
fi

in_mb=$(awk -v b="$in_bytes" 'BEGIN{printf "%.2f", b/1e6}')

have_modes=1
command -v python3 >/dev/null 2>&1 || { have_modes=0; note "python3 not found: the Mode column will read 'n/a', so NO row is mode-verified"; }

# ---------------------------------------------------------------------------
# Build the Go encoder under test.
# ---------------------------------------------------------------------------
command -v go >/dev/null 2>&1 || die "go toolchain not found"
wav2opus="$work/wav2opus"
(cd "$repo_root" && go build -o "$wav2opus" ./cmd/wav2opus) || die "building cmd/wav2opus failed"

# ---------------------------------------------------------------------------
# Header
# ---------------------------------------------------------------------------
echo "# go-opus encoder benchmark"
echo
# Every probe here needs a fallback: sysctl lives in /usr/sbin and nproc is not
# universal, and a benchmark script must not die in its own banner.
if [ "$(uname -s)" = Darwin ]; then
    cpu=$(/usr/sbin/sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
    ncpu=$(/usr/sbin/sysctl -n hw.ncpu 2>/dev/null || echo '?')
    os="$(sw_vers -productName 2>/dev/null || echo macOS) $(sw_vers -productVersion 2>/dev/null || uname -r)"
else
    cpu=$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)
    [ -n "$cpu" ] || cpu=unknown
    os=$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || uname -sr)
    ncpu=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo '?')
fi
printf 'machine    : %s (%s cores), %s/%s\n' "$cpu" "$ncpu" "$(uname -s)" "$(uname -m)"
printf 'os         : %s\n' "$os"
printf 'go         : %s\n' "$(go version | awk '{print $3, $4}')"
printf 'go-opus    : %s\n' "$(cd "$repo_root" && git describe --always --dirty 2>/dev/null || echo unknown)"
if command -v opusenc >/dev/null 2>&1; then
    printf 'opusenc    : %s\n' "$(opusenc --version 2>&1 | head -1)"
else
    printf 'opusenc    : NOT INSTALLED\n'
fi
if command -v ffmpeg >/dev/null 2>&1; then
    printf 'ffmpeg     : %s\n' "$(ffmpeg -version 2>&1 | head -1 | cut -c1-60)"
else
    printf 'ffmpeg     : NOT INSTALLED\n'
fi
printf 'time(1)    : %s (%s; max RSS in %s)\n' "$TIME_BIN" "$TIME_KIND" \
    "$([ "$TIME_KIND" = bsd ] && echo bytes || echo kilobytes)"
echo
printf 'input      : %s\n' "$input"
printf '             %s bytes (%s MB), %.2f s, %d Hz, %d ch, s16le\n' \
    "$in_bytes" "$in_mb" "$in_secs" "$in_rate" "$in_ch"
printf 'settings   : bitrate %d bit/s, complexity %d, VBR, 20 ms frames, single-threaded\n' \
    "$BITRATE" "$COMPLEXITY"
echo

# ---------------------------------------------------------------------------
# The runs. Each appends one markdown row to $rows.
# ---------------------------------------------------------------------------
rows="$work/rows.md"
: > "$rows"
: > "$work/tool.log"
modes_detail="$work/modes.txt"
: > "$modes_detail"

# bench <label> <output-name> <cmd>...
bench() {
    local label=$1 outname=$2; shift 2
    local out="$work/$outname.opus"
    local metrics="$work/$outname.metrics"

    printf 'running    : %s\n' "$label" >&2
    if ! run_timed "$metrics" "$@"; then
        note "$label FAILED (see $work/tool.log); recording it as a failed row"
        printf '| %s | FAILED | FAILED | FAILED | FAILED | FAILED | FAILED |\n' "$label" >> "$rows"
        return 0
    fi

    read -r wall user sys rss_kb < "$metrics"
    local out_bytes; out_bytes=$(wc -c < "$out" | tr -d ' ')

    local mode="n/a"
    if [ "$have_modes" = 1 ]; then
        mode=$(python3 "$modes_py" --summary "$out" 2>/dev/null || echo "parse failed")
        { printf '=== %s ===\n' "$label"; python3 "$modes_py" "$out" 2>&1 || true; echo; } >> "$modes_detail"
    fi

    awk -v label="$label" -v wall="$wall" -v user="$user" -v sys="$sys" \
        -v rss="$rss_kb" -v inb="$in_bytes" -v outb="$out_bytes" -v secs="$in_secs" \
        -v mode="$mode" 'BEGIN{
            cpu = user + sys;
            thru = (wall > 0) ? (inb / 1e6) / wall : 0;
            kbps = (secs > 0) ? (outb * 8) / secs / 1000 : 0;
            printf "| %s | %.2f | %.2f | %.1f | %.1f | %.1f | %s |\n",
                   label, wall, cpu, rss/1024, thru, kbps, mode;
        }' >> "$rows"
}

# go-opus. CELT-only by construction: there is no other mode in this encoder.
bench "go-opus wav2opus" goopus \
    "$wav2opus" -bitrate "$BITRATE" -complexity "$COMPLEXITY" "$input" "$work/goopus.opus"

# opusenc. No way to force CELT-only (see the header comment); we measure what it chose.
if command -v opusenc >/dev/null 2>&1; then
    bench "opusenc (auto mode)" opusenc \
        opusenc --quiet --bitrate "$BITRATE_K" --comp "$COMPLEXITY" --vbr \
                "$input" "$work/opusenc.opus"
else
    note "opusenc not installed: skipping (row omitted, not silently dropped)"
    printf '| opusenc | SKIPPED (not installed) | | | | | |\n' >> "$rows"
fi

# ffmpeg, CELT forced. This is the row that is mode-matched by construction.
if command -v ffmpeg >/dev/null 2>&1; then
    bench "ffmpeg libopus (lowdelay=CELT)" ffceltx \
        ffmpeg -nostdin -y -hide_banner -loglevel error -threads 1 -i "$input" \
               -c:a libopus -b:a "$BITRATE" -vbr on \
               -application lowdelay -frame_duration 20 -compression_level "$COMPLEXITY" \
               -f ogg "$work/ffceltx.opus"

    # ffmpeg, default application. Mode NOT forced; the Mode column reports what it picked.
    bench "ffmpeg libopus (auto mode)" ffauto \
        ffmpeg -nostdin -y -hide_banner -loglevel error -threads 1 -i "$input" \
               -c:a libopus -b:a "$BITRATE" -vbr on \
               -frame_duration 20 -compression_level "$COMPLEXITY" \
               -f ogg "$work/ffauto.opus"
else
    note "ffmpeg not installed: skipping (rows omitted, not silently dropped)"
    printf '| ffmpeg libopus | SKIPPED (not installed) | | | | | |\n' >> "$rows"
fi

# ---------------------------------------------------------------------------
# Table
# ---------------------------------------------------------------------------
echo
echo "| Encoder | Wall (s) | CPU (s) | Peak RSS (MB) | Throughput (MB/s) | Achieved (kbit/s) | Mode (measured from TOC) |"
echo "| ------- | -------: | ------: | ------------: | ----------------: | ----------------: | :----------------------- |"
cat "$rows"
echo
echo "Throughput is input PCM (10^6 bytes) per wall second. Achieved bitrate is the whole"
echo "Ogg file over the input duration, so it includes container overhead."
echo "Mode is parsed from every packet's TOC byte (RFC 6716 s3.1), not assumed from flags:"
echo "a row that does not read CELT 100% is NOT comparable with go-opus, which is CELT-only."
echo
echo "Caveats, both of which flatter go-opus -- read the ratio as \"what you get if you swap"
echo "opusenc for go-opus\", not as a per-sample codec-core ratio:"
echo "  * go-opus is a DISABLE_FLOAT_API fixed-point port, so it never runs libopus's"
echo "    tonality analysis (run_analysis/analysis.c; see internal/opusenc/encode.go:258)."
echo "    The float libopus that opusenc and ffmpeg ship DOES run it at complexity >= 7."
echo "    They are therefore doing per-frame work go-opus simply does not do."
echo "  * ffmpeg's CPU time exceeds its wall time: it uses helper threads for demux/mux"
echo "    despite -threads 1, so its wall time gets parallelism go-opus does not have."
echo "    opusenc (CPU ~= wall) is the honest single-threaded reference."
echo

# Sanity: if the achieved bitrates disagree wildly, the comparison is broken and
# a pretty table would be a lie. Check the spread across the rows that succeeded.
# Row shape is "| label | wall | cpu | rss | thru | kbps | mode |", so splitting on
# "|" puts the achieved bitrate in $7 (a leading empty field precedes the label).
awk -F'|' 'NF>=9 && $7+0 > 0 {
        k=$7+0; n++;
        if (n==1 || k<lo) lo=k;
        if (n==1 || k>hi) hi=k;
    }
    END{
        if (n < 2) exit 0;
        spread = (hi - lo) / lo * 100;
        printf "bitrate spread across encoders: %.1f - %.1f kbit/s (%.1f%%)\n", lo, hi, spread;
        if (spread > 25)
            printf "WARNING: the encoders did not land on comparable bitrates. The throughput\n" \
                   "         numbers above are NOT an apples-to-apples comparison.\n";
        else
            printf "OK: all encoders landed within 25%% of each other, so the rates are comparable.\n";
    }' "$rows"

echo
echo "## Measured mode distribution (full detail)"
echo
cat "$modes_detail"

[ -n "$KEEP" ] && echo "work dir kept: $work"
exit 0
