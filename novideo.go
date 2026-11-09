//go:build cgo && goffmpeg_novideo

package goffmpeg

// The goffmpeg_novideo tag builds goffmpeg for audio only, against the audio
// codec set (#41): MP3 and WAV, in goffmpeg-libs' shared-audio/, with no
// libswscale and no libavdevice. The bridge opens no video stream, never calls
// libswscale and has no capture: OpenDevice and ListInputSources report that
// the build has no libavdevice. Video frames never arrive, and GetRGBA returns
// ErrVideoNotBuilt. It changes the cgo build only: a purego build loads a
// prebuilt libgoffmpeg, which links the media set.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_VIDEO -DGOFFMPEG_NO_AVDEVICE
*/
import "C"
