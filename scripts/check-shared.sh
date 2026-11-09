#!/bin/sh
# check-shared.sh TARGET DIR
#
# Verifies a shared FFmpeg build laid out by stage-shared.sh, the way its
# loader will see it. For every library: it is there, its own name (install
# name, soname, DLL name) is its file name, and everything it links is either
# beside it or part of the OS. Android adds no text relocations and 16KB
# alignment; Apple adds a valid signature. Prints one line per library read,
# and fails on an empty read rather than passing it.
set -eu

target=$1 dir=$2
READELF=${READELF:-readelf}
OBJDUMP=${OBJDUMP:-objdump}
fail=0
err() { echo "check-shared: $target: $*" >&2; fail=1; }

case $target in
android_*|ios_*|iossimulator_*) libs="avutil swresample swscale avcodec avformat" ;;
*) libs="avutil swresample swscale avcodec avformat avdevice" ;;
esac

checked=0
for l in $libs; do
	case $target in
	darwin_*)
		set -- "$dir"/lib"$l"-*.dylib
		f=$1
		[ -f "$f" ] || { err "no lib$l"; continue; }
		id=$(otool -D "$f" | sed -n 2p)
		[ "$id" = "@rpath/${f##*/}" ] || err "${f##*/}: install name is '$id', not @rpath/${f##*/}"
		for dep in $(otool -L "$f" | awk 'NR > 2 { print $1 }'); do
			case $dep in
			/usr/lib/*|/System/Library/*) ;;
			@rpath/*) [ -f "$dir/${dep#@rpath/}" ] || err "${f##*/} links $dep, which is not in $dir" ;;
			*) err "${f##*/} links $dep" ;;
			esac
		done
		# arm64 Macs load only signed code, and Apple's linker signs arm64
		# output ad hoc. It does not sign x86_64, which runs unsigned. A
		# signature that is there must verify on either.
		if ! v=$(codesign --verify "$f" 2>&1); then
			case "$target:$v" in
			darwin_amd64:*"not signed at all"*) ;;
			*) err "${f##*/}: signature does not verify: $v" ;;
			esac
		fi
		echo "  ${f##*/}: $id"
		;;

	ios_*|iossimulator_*)
		set -- "$dir"/lib"$l"-*.framework
		fw=$1
		[ -d "$fw" ] || { err "no lib$l framework"; continue; }
		name=${fw##*/}
		name=${name%.framework}
		f="$fw/$name"
		[ -f "$f" ] || { err "$name.framework has no binary $name"; continue; }
		id=$(otool -D "$f" | sed -n 2p)
		[ "$id" = "@rpath/$name.framework/$name" ] || err "$name: install name is '$id'"
		for dep in $(otool -L "$f" | awk 'NR > 2 { print $1 }'); do
			case $dep in
			/usr/lib/*|/System/Library/*) ;;
			@rpath/*) [ -f "$dir/${dep#@rpath/}" ] || err "$name links $dep, which is not in $dir" ;;
			*) err "$name links $dep" ;;
			esac
		done
		plutil -lint -s "$fw/Info.plist" || err "$name: Info.plist does not parse"
		[ "$(plutil -extract CFBundleExecutable raw "$fw/Info.plist")" = "$name" ] || err "$name: CFBundleExecutable is not $name"
		[ "$(plutil -extract MinimumOSVersion raw "$fw/Info.plist")" = "${IOS_MIN:?}" ] || err "$name: MinimumOSVersion is not $IOS_MIN"
		codesign --verify "$fw" 2>/dev/null || err "$name.framework: signature does not verify"
		echo "  $name.framework: $id"
		;;

	linux_*|android_*)
		case $target in
		linux_*) set -- "$dir"/lib"$l"-*.so.* ;;
		*) set -- "$dir"/lib"$l"-*.so ;;
		esac
		f=$1
		[ -f "$f" ] || { err "no lib$l"; continue; }
		dyn=$("$READELF" -d "$f")
		soname=$(echo "$dyn" | sed -n 's/.*(SONAME).*\[\(.*\)\]/\1/p')
		[ "$soname" = "${f##*/}" ] || err "${f##*/}: soname is '$soname'"
		echo "$dyn" | grep -qE '\((RPATH|RUNPATH)\)' && err "${f##*/} carries an rpath"
		echo "$dyn" | grep -qE '\(TEXTREL\)|TEXTREL' && err "${f##*/} has text relocations"
		for dep in $(echo "$dyn" | sed -n 's/.*(NEEDED).*\[\(.*\)\]/\1/p'); do
			case $dep in
			lib*-goffmpeg.so*) [ -f "$dir/$dep" ] || err "${f##*/} needs $dep, which is not in $dir" ;;
			*)
				case $target in
				linux_*) allowed='^(libc\.so\.6|libm\.so\.6|libpthread\.so\.0|libdl\.so\.2|libssl\.so\.3|libcrypto\.so\.3|ld-linux-[a-z0-9_-]+\.so\.[0-9])$' ;;
				*) allowed='^(libc|libm|libdl|liblog|libandroid|libmediandk)\.so$' ;;
				esac
				echo "$dep" | grep -qE "$allowed" || err "${f##*/} needs $dep"
				;;
			esac
		done
		if [ "${target%%_*}" = android ]; then
			# Every LOAD segment aligned to at least 16KB (0x4000).
			aligns=$("$READELF" -lW "$f" | awk '$1 == "LOAD" { print $NF }')
			[ -n "$aligns" ] || err "${f##*/}: read no LOAD segments"
			for a in $aligns; do
				[ $((a)) -ge 16384 ] || err "${f##*/}: LOAD segment aligned to $a, not 16KB"
			done
		fi
		echo "  ${f##*/}: soname $soname"
		;;

	windows_*)
		set -- "$dir"/"$l"-*.dll
		f=$1
		[ -f "$f" ] || { err "no $l DLL"; continue; }
		base=${f##*/}
		[ -f "$dir/lib${base%-*}.dll.a" ] || err "$base has no import library lib${base%-*}.dll.a"
		imports=$("$OBJDUMP" -p "$f" | awk '/DLL Name:/ { print $3 }' | sort -fu)
		[ -n "$imports" ] || err "$base: read no imports"
		for dep in $imports; do
			case $dep in
			*-goffmpeg-*.dll) [ -f "$dir/$dep" ] || err "$base imports $dep, which is not in $dir" ;;
			*)
				# The bridge's list (build.yml), plus what FFmpeg's own DLLs
				# import: every one ships with Windows.
				echo "$dep" | grep -qiE '^(api-ms-win-crt-[a-z0-9-]+|advapi32|avicap32|bcrypt|crypt32|gdi32|kernel32|mfplat|msvcrt|ncrypt|ole32|oleaut32|psapi|secur32|shell32|shlwapi|user32|ws2_32)\.dll$' \
					|| err "$base imports $dep, which Windows does not ship"
				;;
			esac
		done
		echo "  $base: imports $(echo "$imports" | tr "\n" " ")"
		;;

	*)
		echo "check-shared: unknown target $target" >&2
		exit 1
		;;
	esac
	checked=$((checked + 1))
done

[ "$checked" -gt 0 ] || err "checked no libraries in $dir"
[ "$fail" -eq 0 ] || exit 1
echo "==> $target: $checked shared libraries check out"
