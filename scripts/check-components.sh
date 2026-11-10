#!/bin/sh
# check-components.sh BUILD_DIR
#
# Holds a configured FFmpeg build to its codec set (#41): the decoders,
# demuxers, parsers, hwaccels and input devices config_components.h enables
# must be exactly DECODERS, DEMUXERS, PARSERS, HWACCELS and INDEVS
# (space-separated FFmpeg names).
# configure turns on whatever an enabled component selects, so a flag that
# asks for one decoder can bring others with it: videotoolbox's hwaccels
# brought hevc, av1 and vp9 back. This is the test that fails when a set's
# build carries a codec the set doesn't list.
#
# Prints each kind's list and fails on a difference, or on a header that
# enables no decoder at all, which would be a read that found nothing.
set -eu

dir=$1
header=$dir/config_components.h
[ -f "$header" ] || { echo "check-components: no $header: configure has not run" >&2; exit 1; }

# enabled KIND prints the components of KIND (DECODER, DEMUXER, ...) the header
# enables, lowercased and sorted, one per line.
enabled() {
	sed -n "s/^#define CONFIG_\\([A-Z0-9_]*\\)_$1 1\$/\\1/p" "$header" | tr 'A-Z' 'a-z' | sort
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
for kind in DECODER DEMUXER PARSER HWACCEL INDEV; do
	case $kind in
	DECODER) want=${DECODERS:-} ;;
	DEMUXER) want=${DEMUXERS:-} ;;
	PARSER)  want=${PARSERS:-} ;;
	HWACCEL) want=${HWACCELS:-} ;;
	INDEV)   want=${INDEVS:-} ;;
	esac
	got=$(enabled $kind)
	want=$(printf '%s\n' $want | sed '/^$/d' | sort)
	if [ "$got" = "$want" ]; then
		echo "  ${kind}s: $(echo $got)"
	else
		echo "check-components: ${kind}s differ from the set (#41):" >&2
		printf '%s\n' "$got" | sed '/^$/d' > "$tmp/got"
		printf '%s\n' "$want" | sed '/^$/d' > "$tmp/want"
		extra=$(comm -23 "$tmp/got" "$tmp/want")
		missing=$(comm -13 "$tmp/got" "$tmp/want")
		[ -z "$extra" ] || echo "    built but not in the set: $(echo $extra)" >&2
		[ -z "$missing" ] || echo "    in the set but not built: $(echo $missing)" >&2
		fail=1
	fi
done
[ -n "$(enabled DECODER)" ] || { echo "check-components: $header enables no decoder: the read found nothing" >&2; fail=1; }
exit $fail
