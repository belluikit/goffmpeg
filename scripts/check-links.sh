#!/usr/bin/env bash
# check-links.sh TARGET BINARY SHARED_DIR [SET]
#
# Checks how a goffmpeg binary for TARGET links FFmpeg: it must import every
# shared FFmpeg library of codec set SET (media, the default, or audio for a
# goffmpeg_novideo build; #41) in SHARED_DIR by the name the loader looks for,
# none of the other set's, and define none of FFmpeg's code itself. A package
# carries one set, so a binary that named a library of the other would not
# load; the audio set has no libswscale or libavdevice to name.
#
# CI runs this on the test binaries it cross-links, for the targets whose tests
# it cannot run (iOS, Android). Where the tests run, TestLinksSharedFFmpeg and
# TestNoVideoImportsNoSwscale check the same from inside.
#
# Tools, by target: otool on Apple; READELF (default readelf) on Linux and
# Android; OBJDUMP (default objdump) on Windows; NM (default nm) for the
# defined-symbol check, skipped on Windows.
set -euo pipefail

target=$1 bin=$2 shared=$3 set=${4:-media}
here=$(cd "$(dirname "$0")" && pwd)
READELF=${READELF:-readelf} OBJDUMP=${OBJDUMP:-objdump} NM=${NM:-nm}

case "$target" in
    darwin_* | ios_* | iossimulator_*) imports=$(otool -L "$bin" | tail -n +2 | awk '{print $1}') ;;
    linux_* | android_*) imports=$("$READELF" -d "$bin" | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}') ;;
    windows_*) imports=$("$OBJDUMP" -p "$bin" | awk '/DLL Name:/ {print $3}') ;;
    *) echo "check-links: unknown target $target" >&2; exit 1 ;;
esac
# positive marker: every binary imports the C library or its equivalent
[ -n "$imports" ] || { echo "check-links: $bin imports nothing: the read found nothing" >&2; exit 1; }

# Read first, so a listing that fails stops the check here.
libs=$("$here/shared-libs.sh" "$target" "$shared" "$set")

# here-strings rather than pipes into grep -q: with pipefail, grep exiting at
# its first match can fail the writer with SIGPIPE, which reads as no match.
fail=0
while read -r name _ load; do
    if grep -qxF "$load" <<< "$imports"; then
        echo "ok   $target: imports $load"
    else
        echo "FAIL $target: does not import $load"; fail=1
    fi
done <<< "$libs"

# None of the other set's libraries: the audio set's names carry
# -goffmpeg-audio, the media set's -goffmpeg alone.
case $set in
    audio) other=$(grep -E -- '-goffmpeg' <<< "$imports" | grep -vF -- '-goffmpeg-audio' || true) ;;
    *)     other=$(grep -F -- '-goffmpeg-audio' <<< "$imports" || true) ;;
esac
if [ -n "$other" ]; then
    echo "FAIL $target: a $set-set binary imports the other set's $(echo $other)"; fail=1
else
    echo "ok   $target: imports nothing of the other set"
fi

case "$target" in
    windows_*) ;;
    *)
        defined=$("$NM" --defined-only "$bin" | awk '{print $NF}' | sed 's/^_//')
        grep -qx 'decoder_open' <<< "$defined" || {
            echo "check-links: decoder_open isn't defined in $bin: the symbol read found nothing" >&2; exit 1; }
        for s in avcodec_send_packet avformat_open_input swr_convert av_frame_alloc; do
            if grep -qx "$s" <<< "$defined"; then
                echo "FAIL $target: defines $s, so FFmpeg is linked in, not loaded"; fail=1
            fi
        done
        [ $fail -ne 0 ] || echo "ok   $target: defines none of FFmpeg's symbols" ;;
esac

exit $fail
