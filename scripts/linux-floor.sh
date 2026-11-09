#!/bin/sh
# linux-floor.sh SCRIPT
#
# Runs SCRIPT, a bash script, in a Go image on Debian 12, whose glibc 2.36 is
# the oldest goffmpeg supports (#44). CI runs goffmpeg's Linux tests this way,
# so they show the libraries link and load on the floor, not only on the
# runner's newer Ubuntu.
#
# The checkout and $RUNNER_TEMP (or TMPDIR) are mounted at their own paths, so
# a go.mod replace pointing into either resolves the same inside. The image's
# glibc version is printed first, as the marker that the floor is what ran.
# (sed, not head: under pipefail, head exiting early fails ldd with SIGPIPE.)
set -eu

script=$1
tmp=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
image=${LINUX_FLOOR_IMAGE:-golang:bookworm}

exec docker run --rm \
	-v "$PWD:$PWD" -v "$tmp:$tmp" -w "$PWD" \
	-e GOFLAGS=-buildvcs=false -e RUNNER_TEMP="$tmp" \
	"$image" bash -euo pipefail -c "ldd --version | sed -n 1p; go version; $script"
