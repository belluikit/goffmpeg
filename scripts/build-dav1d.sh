#!/bin/bash
# build-dav1d.sh TARGET SRC BUILD PREFIX
#
# Builds dav1d, VideoLAN's AV1 decoder, for TARGET as a static library with
# its assembly, and installs it under PREFIX for FFmpeg's configure to find
# through pkg-config (#78). FFmpeg's own av1 decoder has no software path, so
# without dav1d AV1 plays only where the hardware decodes it.
#
# The compiler is the one the Makefile builds FFmpeg's smoke test with for
# TARGET, passed as DAV1D_CC (with its flags), and DAV1D_AR its archiver.
# dav1d is linked into libavcodec, so no dav1d library ships on its own.
#
# A build without the assembly would decode AV1 several times slower, and
# nothing else would notice, so that fails here.
set -euo pipefail
target=$1 src=$2 build=$3 prefix=$4
: "${DAV1D_CC:?DAV1D_CC is the C compiler for $target}"
DAV1D_AR=${DAV1D_AR:-ar}

case $target in
	darwin_*|ios_*|iossimulator_*) system=darwin ;;
	android_*) system=android ;;
	linux_*) system=linux ;;
	windows_*) system=windows ;;
	*) echo "build-dav1d: unknown target $target" >&2; exit 1 ;;
esac
case $target in
	*_arm64) cpu_family=aarch64; cpu=aarch64 ;;
	*_amd64) cpu_family=x86_64; cpu=x86_64 ;;
	android_arm) cpu_family=arm; cpu=armv7 ;;
	*) echo "build-dav1d: no CPU for $target" >&2; exit 1 ;;
esac

# Meson takes the compiler as a list: the command, then its flags.
cc=""
for w in $DAV1D_CC; do cc="$cc'$w', "; done
mkdir -p "$(dirname "$build")"
cross="$build.ini"
cat > "$cross" <<INI
[binaries]
c = [${cc%, }]
ar = '$DAV1D_AR'
[host_machine]
system = '$system'
cpu_family = '$cpu_family'
cpu = '$cpu'
endian = 'little'
INI

rm -rf "$build" "$prefix"
# A shell's CFLAGS and LDFLAGS (Homebrew's, say) would reach the cross
# compiler; the Makefile's FFmpeg build ignores them too.
env -u CFLAGS -u CPPFLAGS -u LDFLAGS meson setup "$build" "$src" \
	--cross-file "$cross" --prefix "$prefix" --libdir lib \
	--default-library static --buildtype release -Db_staticpic=true \
	-Denable_tools=false -Denable_tests=false
ninja -C "$build" install

if ! grep -q '^#define HAVE_ASM 1' "$build/config.h"; then
	echo "build-dav1d: dav1d for $target was built without its assembly" >&2
	exit 1
fi
echo "==> dav1d $(sed -n 's/^Version: //p' "$prefix/lib/pkgconfig/dav1d.pc") for $target in $prefix (with assembly)"
