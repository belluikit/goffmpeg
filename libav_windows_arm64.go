//go:build windows && arm64

package goffmpeg

// Windows on ARM.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the shared libraries differ, and those come from the companion module
// imported below, whose link files load them. See BUILDING.md.

/*
#cgo CFLAGS: -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/windows_arm64"
