//go:build ios && arm64 && iossimulator

package goffmpeg

// The iOS simulator on Apple silicon. Go has no separate GOOS for it,
// so build with -tags iossimulator; linking the device libraries into a
// simulator binary fails on a platform mismatch.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_AVDEVICE -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/iossimulator_arm64"
