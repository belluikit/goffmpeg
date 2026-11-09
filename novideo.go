//go:build cgo && goffmpeg_novideo

package goffmpeg

// The goffmpeg_novideo tag builds goffmpeg for audio only: the bridge opens no
// video stream and never calls libswscale, so a static link pulls in nothing
// of it (about 1 MB of code on android/arm64, goffmpeg-libs v0.3.5). Video
// frames never arrive, and GetRGBA returns ErrVideoNotBuilt. It changes the cgo
// build only: a purego build loads a prebuilt libgoffmpeg, which has video.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_VIDEO
*/
import "C"
