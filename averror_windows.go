package goffmpeg

// errnoEAGAIN is C's EAGAIN as the CRT that FFmpeg is built against defines it:
// 11, the same as Linux.
//
// It is spelled out rather than taken from syscall because on Windows
// syscall.EAGAIN is not this number and never was. That package invents its
// errno constants starting at APPLICATION_ERROR (1<<29), so syscall.EAGAIN is
// 536870918 — a Go-internal value with no relationship to the CRT errno FFmpeg
// negates. Comparing against it would silently never match, leaving a live
// capture on Windows with exactly the bug #4 describes.
const errnoEAGAIN = 11

// errnoText declines to name the code on Windows. syscall.Errno there is a
// Win32 error code, not a CRT errno, so running an FFmpeg errno through it
// would print a confident and unrelated message — Win32 16 is "the directory
// cannot be removed", while FFmpeg's -16 is EBUSY.
func errnoText(int) string { return "" }
