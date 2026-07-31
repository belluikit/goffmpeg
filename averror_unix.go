//go:build !windows

package goffmpeg

import "syscall"

// errnoEAGAIN is C's EAGAIN for this platform — 11 on Linux and Android, 35 on
// macOS and iOS. FFmpeg builds AVERROR(EAGAIN) by negating that value, so this
// is what an errno-derived decoder result has to be compared against.
//
// On unix Go's syscall constants are the platform's own errno numbers, so
// taking it from syscall keeps the two definitions from drifting.
const errnoEAGAIN = int(syscall.EAGAIN)

// errnoText names an errno-derived AVERROR in the platform's own wording.
func errnoText(n int) string { return syscall.Errno(n).Error() }
