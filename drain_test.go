package goffmpeg

import (
	"image"
	"os"
	"testing"
)

// decodeAll runs Next to the end of the stream and counts what it yields:
// video frames, and audio bytes as GetAudio returns them.
func decodeAll(t *testing.T, dec IDecoder) (frames, audioBytes int) {
	t.Helper()
	var rgba *image.RGBA
	if info := dec.VideoInfo(); info.Width > 0 {
		rgba = image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	}
	for i := 0; i < 10000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch mt {
		case MediaTypeNone:
			return frames, audioBytes
		case MediaTypeVideo:
			if err := dec.GetRGBA(rgba); err != nil {
				t.Fatalf("GetRGBA: %v", err)
			}
			frames++
		case MediaTypeAudio:
			b, err := dec.GetAudio()
			if err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			audioBytes += len(b)
		}
	}
	t.Fatal("Next did not reach the end of the stream in 10000 calls")
	return
}

// TestNextYieldsEveryFrame is the regression test for #22. Next stopped at
// the end of the input without draining the decoders, and frame threading
// holds back about a frame per thread, so every video lost its last frames:
// 83 of 100 on a 16-core Mac, and all 10 of a 10-frame clip. The wanted
// counts are reference ffmpeg's: ffprobe -count_frames for video, and the
// bytes of `ffmpeg -f s16le` for audio. Untagged, so purego runs it too.
func TestNextYieldsEveryFrame(t *testing.T) {
	cases := []struct {
		path       string
		frames     int
		audioBytes int
	}{
		{"testdata/sample.mp4", 100, 352800},
		{"testdata/sample_video_cover.mp4", 10, 0},
		{"testdata/sample_51.m4a", 0, 1058400},
		{"testdata/sample_cover.mp3", 0, 88200},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if _, err := os.Stat(c.path); err != nil {
				t.Skipf("fixture unavailable: %v", err)
			}
			dec, err := Open(c.path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer dec.Close()
			frames, audioBytes := decodeAll(t, dec)
			if frames != c.frames {
				t.Errorf("decoded %d video frames, want %d", frames, c.frames)
			}
			if audioBytes != c.audioBytes {
				t.Errorf("decoded %d audio bytes, want %d", audioBytes, c.audioBytes)
			}
		})
	}
}

// TestSeekAfterDrain checks that a decoder drained at the end of the input
// decodes again after Seek: draining leaves it refusing input until flushed.
func TestSeekAfterDrain(t *testing.T) {
	const path = "testdata/sample.mp4"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()

	decodeAll(t, dec)
	if err := dec.Seek(0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if frames, _ := decodeAll(t, dec); frames != 100 {
		t.Errorf("decoded %d video frames after Seek(0), want 100", frames)
	}
}
