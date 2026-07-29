//go:build android && arm64

package goffmpeg

// Android arm64-v8a. MediaCodec provides hardware decode via JNI.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_AVDEVICE -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/android_arm64"
