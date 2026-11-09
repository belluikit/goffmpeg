#!/bin/sh
# smoke-shared.sh TARGET DIR WORK SAMPLE
#
# Links testdata/sharedsmoke/smoke.c against the shared FFmpeg libraries in DIR
# and nothing else, then runs it on SAMPLE when this machine can execute
# TARGET. SMOKE_CC is the target's C compiler and INCLUDE the FFmpeg headers.
# SMOKE_SET picks the codec set's checks (media or audio, below).
# SMOKE_RUN is 1 (must run), 0 (link only) or auto (run on a matching host).
#
# It ends with one line saying which it did, "ran" or "linked", so a log can
# tell a run from a link.
set -eu

target=$1 dir=$2 work=$3 sample=$4
src=$(cd "$(dirname "$0")/.." && pwd)/testdata/sharedsmoke/smoke.c
: "${SMOKE_CC:?SMOKE_CC is not set}" "${INCLUDE:?INCLUDE is not set}"
run=${SMOKE_RUN:-auto}

mkdir -p "$work"
exe="$work/smoke"
# SMOKE_SET is the codec set (#41): media decodes video and links avdevice on
# desktop; audio decodes audio, and has neither libswscale nor libavdevice.
defs=
case ${SMOKE_SET:-media}:$target in
media:darwin_*|media:linux_*|media:windows_*) defs=-DSMOKE_AVDEVICE ;;
audio:*) defs=-DSMOKE_AUDIO ;;
esac

# Each library by its path, so the link can only use the shared build.
case $target in
darwin_*)
	set -- "$dir"/lib*.dylib -Wl,-rpath,"$dir"
	;;
ios_*|iossimulator_*)
	set -- -F"$dir" -Wl,-rpath,@executable_path/Frameworks
	for fw in "$dir"/*.framework; do
		n=${fw##*/}
		set -- "$@" -framework "${n%.framework}"
	done
	;;
linux_*)
	set -- "$dir"/lib*.so.* -Wl,-rpath,"$dir"
	;;
android_*)
	set -- "$dir"/lib*.so
	;;
windows_*)
	exe="$work/smoke.exe"
	set -- "$dir"/lib*.dll.a
	;;
esac

# shellcheck disable=SC2086 # SMOKE_CC is a command line.
$SMOKE_CC $defs -I"$INCLUDE" -o "$exe" "$src" "$@"

if [ "$run" = auto ]; then
	run=0
	host="$(uname -s)_$(uname -m)"
	case "$target:$host" in
	darwin_arm64:Darwin_arm64|linux_amd64:Linux_x86_64|linux_arm64:Linux_aarch64) run=1 ;;
	darwin_amd64:Darwin_*) arch -x86_64 /usr/bin/true 2>/dev/null && run=1 ;;
	esac
fi

if [ "$run" = 1 ]; then
	case $target in
	windows_*)
		# Windows looks for a DLL in the executable's own directory first,
		# which is where an app ships them.
		cp "$dir"/*.dll "$work/"
		"$exe" "$sample"
		;;
	iossimulator_*)
		# A simulator runs a command-line binary from the host's disk. The
		# executable's rpath is @executable_path/Frameworks, as in an app, so
		# the frameworks go there. Never auto: booting one takes a minute.
		rm -rf "$work/Frameworks"
		cp -R "$dir" "$work/Frameworks"
		udid=${SMOKE_SIM:-$(xcrun simctl list devices available | grep 'iPhone' |
			grep -oE '[0-9A-F]{8}-([0-9A-F]{4}-){3}[0-9A-F]{12}' | head -1)}
		[ -n "$udid" ] || { echo "smoke-shared: no iPhone simulator available" >&2; exit 1; }
		xcrun simctl boot "$udid" 2>/dev/null || true # already booted is fine
		status=0
		xcrun simctl spawn "$udid" "$exe" "$sample" || status=$?
		xcrun simctl shutdown "$udid" 2>/dev/null || true
		[ "$status" -eq 0 ] || exit "$status"
		;;
	*) "$exe" "$sample" ;;
	esac
	echo "==> smoke-shared: ran $target"
else
	echo "==> smoke-shared: linked $target (not run on this host)"
fi
