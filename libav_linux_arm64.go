//go:build linux && arm64 && !android

package goffmpeg

// Linux arm64.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/linux_arm64"
