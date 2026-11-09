#!/usr/bin/env bash
# shared-libs.sh TARGET DIR
#
# Lists the shared FFmpeg libraries of TARGET found in DIR (a dist/<target>/shared
# directory, or a goffmpeg-libs module's shared/), one per line:
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

[ -d "$dir" ] || { echo "shared-libs: no directory $dir" >&2; exit 1; }

case "$target" in
    darwin_* | linux_* | windows_*) names="avdevice avformat avcodec swresample swscale avutil" ;;
    android_* | ios_* | iossimulator_*) names="avformat avcodec swresample swscale avutil" ;;
    *) echo "shared-libs: unknown target $target" >&2; exit 1 ;;
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
            f=$(one "lib$n-goffmpeg.*.dylib")
            echo "$n $f @rpath/$f" ;;
        ios_* | iossimulator_*)
            f=$(one "lib$n-goffmpeg.framework")
            echo "$n $f @rpath/$f/lib$n-goffmpeg" ;;
        linux_*)
            f=$(one "lib$n-goffmpeg.so.*")
            echo "$n $f $f" ;;
        android_*)
            f=$(one "lib$n-goffmpeg.so")
            echo "$n $f $f" ;;
        windows_*)
            f=$(one "lib$n-goffmpeg.dll.a")
            dll=$(one "$n-goffmpeg-*.dll")
            echo "$n $f $dll" ;;
    esac
done
