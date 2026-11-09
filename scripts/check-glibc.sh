#!/bin/sh
# check-glibc.sh FLOOR FILE...
#
# Fails when a Linux library or executable needs a newer glibc than FLOOR, the
# oldest goffmpeg supports (#44). A shared library records the glibc symbol
# versions it was linked against, so it needs at least the glibc it was built
# with, whatever the consumer has. That is why the Linux targets are built in a
# container with the floor's glibc rather than on a newer CI image.
#
# Prints the highest version each file needs, and fails on a file whose
# dynamic symbols it cannot read rather than passing it. OBJDUMP picks the tool
# (default objdump).
set -eu

floor=$1
shift
OBJDUMP=${OBJDUMP:-objdump}
fail=0

for f in "$@"; do
	# A file that reads no dynamic symbols would pass as needing no glibc.
	syms=$("$OBJDUMP" -T "$f") || { echo "check-glibc: cannot read $f" >&2; fail=1; continue; }
	echo "$syms" | grep -q 'DYNAMIC SYMBOL TABLE' || { echo "check-glibc: no dynamic symbols in $f" >&2; fail=1; continue; }
	need=$(echo "$syms" | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -n 1)
	[ -n "$need" ] || need=none
	if [ "$need" != none ] && [ "$(printf '%s\n%s\n' "$floor" "$need" | sort -V | tail -n 1)" != "$floor" ]; then
		echo "check-glibc: ${f##*/} needs glibc $need, above the floor $floor:" >&2
		echo "$syms" | grep -E "GLIBC_$need( |\)|$)" | awk '{ print "    " $NF }' | sort -u | head -n 10 >&2
		fail=1
	else
		echo "  ${f##*/}: glibc $need (floor $floor)"
	fi
done
exit $fail
