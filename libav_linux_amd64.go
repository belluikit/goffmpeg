//go:build linux && amd64 && !android

package goffmpeg

// Linux x86_64. OpenSSL provides https; avdevice provides v4l2 capture.
//
// The FFmpeg headers are shared by every target and live in this module; only
// the static libraries differ, and those come from the companion module
// imported below. See BUILDING.md.

/*
#cgo CFLAGS: -I${SRCDIR}/internal/libav/include
*/
import "C"

import _ "github.com/belluikit/goffmpeg-libs/linux_amd64"
