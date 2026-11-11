#!/usr/bin/env bash
# shared-libs.sh TARGET DIR [SET]
#
# Lists the shared FFmpeg libraries of TARGET's codec set SET (media, the
# default, or audio; #41) found in DIR (dist/<target>/shared or shared-audio,
# or the same directory of a goffmpeg-libs module), one per line:
#
#   <name> <file> <load name>
#
# name is avcodec, avformat and so on. file is what a link names, relative to
# DIR: the library itself, the framework on iOS, the import library on
# Windows. load name is what a linked binary records and the loader looks for:
# the install name on Apple, the soname on Linux and Android, the DLL on
# Windows.
#
# The version numbers in the names come from the files, so nothing that links
# them has to be edited when an FFmpeg bump moves a library's major version.
# Each library must be there exactly once; a missing or ambiguous one is an
# error, because a link line generated from a partial list would fail later and
# further from its cause.
set -euo pipefail

target=$1
dir=$2
set=${3:-media}

[ -d "$dir" ] || { echo "shared-libs: no directory $dir" >&2; exit 1; }

case "$target" in
    darwin_* | linux_* | windows_*) names="avdevice avformat avcodec swresample swscale avutil" ;;
    android_* | ios_* | iossimulator_*) names="avformat avcodec swresample swscale avutil" ;;
    *) echo "shared-libs: unknown target $target" >&2; exit 1 ;;
esac
# The audio set has neither libswscale nor libavdevice, and its own suffix.
case "$set" in
    media) suffix=-goffmpeg ;;
    audio) suffix=-goffmpeg-audio names="avformat avcodec swresample avutil" ;;
    *) echo "shared-libs: unknown set $set" >&2; exit 1 ;;
esac

# one prints the single entry of DIR matching a glob, or fails.
one() {
    local pattern=$1 matches=()
    shopt -s nullglob
    matches=("$dir"/$pattern)
    shopt -u nullglob
    if [ ${#matches[@]} -ne 1 ]; then
        echo "shared-libs: $target: expected one $pattern in $dir, found ${#matches[@]}" >&2
        exit 1
    fi
    basename "${matches[0]}"
}

for n in $names; do
    case "$target" in
        darwin_*)
            f=$(one "lib$n$suffix.*.dylib")
            echo "$n $f @rpath/$f" ;;
        ios_* | iossimulator_*)
            f=$(one "lib$n$suffix.framework")
            echo "$n $f @rpath/$f/lib$n$suffix" ;;
        linux_*)
            f=$(one "lib$n$suffix-[0-9]*.so")
            echo "$n $f $f" ;;
        android_*)
            f=$(one "lib$n$suffix.so")
            echo "$n $f $f" ;;
        windows_*)
            f=$(one "lib$n$suffix.dll.a")
            dll=$(one "$n$suffix-[0-9]*.dll")
            echo "$n $f $dll" ;;
    esac
done
