#!/bin/sh
# stage-shared.sh TARGET STAGE OUT
#
# Copies FFmpeg's shared install tree for one target (STAGE, the DESTDIR'd
# prefix) into the layout goffmpeg-libs publishes (OUT). Run by the Makefile's
# dist-shared; check-shared.sh then verifies the result.
#
# Each library is kept under the one name its loader looks for: the install
# name on macOS, the soname on Linux and Android, the DLL name on Windows.
# FFmpeg installs the other names as symlinks, and a Go module zip leaves
# symlinks out, so they are not copied. On iOS each library becomes a
# framework, which is what an app embeds and signs.
set -eu

target=$1 stage=$2 out=$3
READELF=${READELF:-readelf}

rm -rf "$out"
mkdir -p "$out"

case $target in
darwin_*)
	for f in "$stage"/lib/lib*.dylib; do
		[ -L "$f" ] && continue
		id=$(otool -D "$f" | sed -n 2p)
		cp "$f" "$out/${id##*/}"
	done
	;;

ios_*|iossimulator_*)
	case $target in
	ios_*) platform=iPhoneOS ;;
	*) platform=iPhoneSimulator ;;
	esac
	for f in "$stage"/lib/lib*.dylib; do
		[ -L "$f" ] && continue
		file=${f##*/}                       # libavcodec-goffmpeg.62.21.100.dylib
		name=${file%%.*}                    # libavcodec-goffmpeg
		version=${file#"$name".}
		version=${version%.dylib}           # 62.21.100
		fw="$out/$name.framework"
		mkdir -p "$fw"
		cp "$f" "$fw/$name"
		install_name_tool -id "@rpath/$name.framework/$name" "$fw/$name"
		# The other FFmpeg libraries it links, @rpath/libavutil-goffmpeg.60.dylib
		# and so on, are frameworks now too.
		for dep in $(otool -L "$fw/$name" | awk 'NR > 1 && $1 ~ /^@rpath\/lib.*\.dylib$/ { print $1 }'); do
			d=${dep##*/}
			d=${d%%.*}
			install_name_tool -change "$dep" "@rpath/$d.framework/$d" "$fw/$name"
		done
		cat > "$fw/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key><string>en</string>
	<key>CFBundleExecutable</key><string>$name</string>
	<key>CFBundleIdentifier</key><string>com.trendvidia.goffmpeg.${name%-*}</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleName</key><string>$name</string>
	<key>CFBundlePackageType</key><string>FMWK</string>
	<key>CFBundleShortVersionString</key><string>$version</string>
	<key>CFBundleVersion</key><string>$version</string>
	<key>CFBundleSupportedPlatforms</key><array><string>$platform</string></array>
	<key>MinimumOSVersion</key><string>${IOS_MIN:?IOS_MIN is not set}</string>
</dict>
</plist>
PLIST
		# install_name_tool voids the linker's ad-hoc signature. Sign again,
		# ad hoc: an app's packager re-signs the framework with its own identity
		# when it embeds it.
		codesign --force --sign - "$fw" >/dev/null
	done
	;;

linux_*)
	for f in "$stage"/lib/lib*.so*; do
		[ -L "$f" ] && continue
		soname=$("$READELF" -d "$f" | sed -n 's/.*(SONAME).*\[\(.*\)\]/\1/p')
		[ -n "$soname" ] || { echo "stage-shared: no SONAME in $f" >&2; exit 1; }
		cp "$f" "$out/$soname"
	done
	;;

android_*)
	# FFmpeg's Android build names each library by its soname already, with
	# no version: an APK installs only lib*.so from lib/<abi>.
	for f in "$stage"/lib/lib*.so; do
		[ -L "$f" ] && continue
		cp "$f" "$out/"
	done
	;;

windows_*)
	# The DLLs install to bin/; lib/ holds each one's import library.
	cp "$stage"/bin/*.dll "$out/"
	cp "$stage"/lib/lib*.dll.a "$out/"
	;;

*)
	echo "stage-shared: unknown target $target" >&2
	exit 1
	;;
esac

ls -l "$out"
