//go:build cgo && goffmpeg_novideo

package goffmpeg

// The goffmpeg_novideo tag builds goffmpeg for audio only: the bridge opens no
// video stream and never calls libswscale, and goffmpeg-libs' link files leave
// libswscale out under the same tag (#40), so the binary doesn't load it and
// an app ships without it (about 1.2 MB on android/arm64). Video frames never
// arrive, and GetRGBA returns ErrVideoNotBuilt. It changes the cgo build only:
// a purego build loads a prebuilt libgoffmpeg, which has video.

/*
#cgo CFLAGS: -DGOFFMPEG_NO_VIDEO
*/
import "C"
