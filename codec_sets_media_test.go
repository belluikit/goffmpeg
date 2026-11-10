//go:build cgo && !goffmpeg_novideo

package goffmpeg

import "testing"

// The media set's motivating case for MissingDecoder (#41): an MP4 whose
// HEVC video the set doesn't decode opens anyway, names the video's codec,
// and plays its AAC audio.
func TestMediaSetNamesHEVCAndPlaysAudio(t *testing.T) {
	requireMissingDecoderSupport(t)
	requireCodecSets(t)
	dec, err := Open("testdata/sample_hevc.mp4") // HEVC video, AAC audio
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if got := dec.VideoInfo().MissingDecoder; got != "hevc" {
		t.Errorf("VideoInfo().MissingDecoder = %q, want hevc", got)
	}
	if got := dec.AudioInfo().MissingDecoder; got != "" {
		t.Errorf("AudioInfo().MissingDecoder = %q, want empty", got)
	}
	audio := 0
	for i := 0; i < 500; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		if mt == MediaTypeNone {
			break
		}
		if mt == MediaTypeVideo {
			t.Fatal("a video frame arrived from a stream with no decoder")
		}
		if b, err := dec.GetAudio(); err == nil && len(b) > 0 {
			audio++
		}
	}
	if audio == 0 {
		t.Error("decoded no audio")
	}
}
