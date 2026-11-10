#!/bin/sh
# apt-install.sh PACKAGE... -- COMMAND...
#
# Installs PACKAGEs in a Debian or Ubuntu container, then checks that each
# COMMAND exists. The Linux build container (make linux-docker) and the floor
# test container (scripts/linux-floor.sh) set themselves up with it (#54).
#
# The update and the install are retried together, a few times with a pause:
# an Ubuntu mirror in the middle of a sync serves index files whose size
# doesn't match, which fails apt-get update for a few minutes (CI run
# 42757722349). apt's errors stay in the log. A failure before the last && of
# a list doesn't stop a set -e shell, which is how a failed update once
# skipped the install and surfaced only as "make: command not found"; here
# the loop's condition carries it, and the command check catches the rest.
#
# APT_TRIES (default 5) and APT_DELAY (seconds, default 30) tune the retry.
set -eu

pkgs= cmds= after=
for a; do
	if [ "$a" = -- ]; then after=1; continue; fi
	if [ -n "$after" ]; then cmds="$cmds $a"; else pkgs="$pkgs $a"; fi
done
[ -n "$pkgs" ] || { echo "apt-install: no packages named" >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
tries=${APT_TRIES:-5} delay=${APT_DELAY:-30} i=1
# shellcheck disable=SC2086 # $pkgs is a list of package names.
until apt-get -o Acquire::Retries=3 update -qq &&
	apt-get install -y -qq --no-install-recommends $pkgs >/dev/null; do
	if [ "$i" -ge "$tries" ]; then
		echo "apt-install: installing$pkgs failed $i times" >&2
		exit 1
	fi
	echo "apt-install: try $i of $tries failed; again in ${delay}s" >&2
	i=$((i + 1))
	sleep "$delay"
done

for c in $cmds; do
	command -v "$c" >/dev/null || { echo "apt-install: $c is missing after installing$pkgs" >&2; exit 1; }
done
echo "apt-install: installed$pkgs (try $i)"
