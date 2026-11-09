//go:build cgo && !goffmpeg_novideo

package goffmpeg

import "testing"

// A build with video links libswscale; the audio-only build's check
// (TestNoVideoLinksNoSwscale) relies on the same scan finding it here.
func TestVideoLinksSwscale(t *testing.T) {
	if names := linkedSymbols(t); !names["sws_scale"] {
		t.Error("sws_scale isn't linked in a build with video")
	}
}

// The twin of TestNoVideoImportsNoSwscale: a build with video imports
// libswscale, so the audio-only check reads a list that would show it.
func TestVideoImportsSwscale(t *testing.T) {
	if got := requireSharedFFmpeg(t); !got["swscale"] {
		t.Error("a build with video doesn't import libswscale-goffmpeg")
	}
}
