#!/bin/sh
# linux-floor.sh SCRIPT
#
# Runs SCRIPT, a bash script, on Ubuntu 22.04, whose glibc 2.35 is the oldest
# goffmpeg supports (#44). CI runs goffmpeg's Linux tests this way, so they show
# the libraries link and load on the floor, not only on the runner's newer
# Ubuntu.
#
# There is no official Go image on Ubuntu 22.04, so the container installs the
# host's Go version (GO_VERSION, default `go env GOVERSION`) from go.dev,
# checked against its published SHA-256, plus gcc for cgo and -race.
#
# Run it from the checkout's top: the container installs its tools with
# scripts/apt-install.sh, which retries a mirror mid-sync (#54).
#
# The checkout and $RUNNER_TEMP (or TMPDIR) are mounted at their own paths, so
# a go.mod replace pointing into either resolves the same inside. The image's
# glibc version is printed first, as the marker that the floor is what ran.
# (sed, not head: under pipefail, head exiting early fails ldd with SIGPIPE.)
#
# On macOS, Docker Desktop's file sharing can show the container a go.mod
# edited moments before as it was, without a replace just appended: seen
# twice locally, gone on a rerun. Print the resolved module (go list -m) first
# when that matters. CI's Docker runs on Linux, where a mount is the file itself.
set -eu

script=$1
tmp=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
image=${LINUX_FLOOR_IMAGE:-ubuntu:22.04}
gover=${GO_VERSION:-$(go env GOVERSION)}

exec docker run --rm \
	-v "$PWD:$PWD" -v "$tmp:$tmp" -w "$PWD" \
	-e GOFLAGS=-buildvcs=false -e RUNNER_TEMP="$tmp" -e DEBIAN_FRONTEND=noninteractive \
	"$image" bash -euo pipefail -c "
		sh scripts/apt-install.sh ca-certificates curl gcc libc6-dev -- curl gcc
		tgz=$gover.linux-\$(dpkg --print-architecture).tar.gz
		curl -fsSL --retry 5 -o /tmp/\$tgz https://dl.google.com/go/\$tgz
		echo \"\$(curl -fsSL --retry 5 https://dl.google.com/go/\$tgz.sha256)  /tmp/\$tgz\" | sha256sum -c - >/dev/null
		tar -C /usr/local -xzf /tmp/\$tgz
		export PATH=/usr/local/go/bin:\$PATH
		ldd --version | sed -n 1p
		go version
		$script"
