//go:build android && arm64

package goffmpeg

// Android arm64-v8a. The media set has MediaCodec's H.264 decoder, reached
// through the NDK, which the bridge opens for H.264 (#65).
//
// The FFmpeg headers are shared by every target and live in this module; only
// the shared libraries differ, and those come from the companion module
// imported below, whose link files load them. See BUILDING.md.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_AVDEVICE -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/android_arm64"
