//go:build darwin && arm64 && !ios

package goffmpeg

// macOS on Apple silicon. avdevice provides avfoundation capture;
// VideoToolbox provides hardware decode.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/darwin_arm64"
