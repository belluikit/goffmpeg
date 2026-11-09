//go:build cgo && goffmpeg_novideo

package goffmpeg

import (
	"errors"
	"image"
	"testing"
)

// An audio-only build decodes a file's audio and never its video: no video
// stream is opened, no frame arrives, and GetRGBA says why.
func TestNoVideoDecodesAudioOnly(t *testing.T) {
	dec, err := Open("testdata/sample.mp4") // H.264 video and AAC audio
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if info := dec.VideoInfo(); info.Width != 0 || info.Height != 0 {
		t.Errorf("video = %dx%d, want none", info.Width, info.Height)
	}
	var video, audio int
	for i := 0; i < 2000; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		switch mt {
		case MediaTypeVideo:
			video++
		case MediaTypeAudio:
			if b, err := dec.GetAudio(); err == nil && len(b) > 0 {
				audio++
			}
		}
	}
	if video != 0 {
		t.Errorf("%d video frames in an audio-only build", video)
	}
	if audio == 0 {
		t.Error("decoded no audio")
	}
	if err := dec.GetRGBA(image.NewRGBA(image.Rect(0, 0, 2, 2))); !errors.Is(err, ErrVideoNotBuilt) {
		t.Errorf("GetRGBA: %v, want ErrVideoNotBuilt", err)
	}
}

// The point of the tag: nothing of libswscale is linked.
func TestNoVideoLinksNoSwscale(t *testing.T) {
	names := linkedSymbols(t)
	for _, s := range []string{"sws_scale", "sws_getCachedContext", "sws_freeContext"} {
		if names[s] {
			t.Errorf("%s is linked in an audio-only build", s)
		}
	}
}

// With FFmpeg linked dynamically, no symbol is not enough: a library linked by
// path is imported even when nothing calls it, and a package without
// libswscale would then not load (#40). The audio-only build must not import it.
func TestNoVideoImportsNoSwscale(t *testing.T) {
	got := requireSharedFFmpeg(t)
	if !got["avcodec"] {
		t.Fatal("libavcodec-goffmpeg isn't imported: the read found no FFmpeg")
	}
	if got["swscale"] {
		t.Error("an audio-only build imports libswscale-goffmpeg")
	}
}
