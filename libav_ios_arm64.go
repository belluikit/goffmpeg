//go:build ios && arm64 && !iossimulator

package goffmpeg

// iOS devices. Built without avdevice — capture goes through the
// platform APIs — but VideoToolbox hardware decode is available.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_AVDEVICE -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/ios_arm64"
