//go:build !goffmpeg_novideo

package goffmpeg

import (
	"errors"
	"image"
	"testing"
)

// The royalty-free formats play through the media set on every platform
// (#78): VP9 and AV1 video, Opus audio, in WebM, MP4 and Ogg. The clips are two
// seconds of sample.mp4, 320x240 at 25 fps with 48 kHz mono Opus:
//
//	sample_vp9.webm  VP9 profile 0 and Opus, WebM
//	sample_av1.mp4   AV1 Main and Opus, MP4
//	sample.opus      Opus, Ogg
func TestRoyaltyFreeFormats(t *testing.T) {
	for _, c := range []struct {
		path      string
		video     bool
		wantVideo int
	}{
		{"testdata/sample_vp9.webm", true, 50},
		{"testdata/sample_av1.mp4", true, 50},
		{"testdata/sample.opus", false, 0},
	} {
		t.Run(c.path, func(t *testing.T) {
			dec, err := Open(c.path)
			if errors.Is(err, ErrUnrecognizedInput) || errors.Is(err, ErrNoDecoder) {
				t.Skipf("the loaded FFmpeg predates #78: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			vi, ai := dec.VideoInfo(), dec.AudioInfo()
			if vi.MissingDecoder != "" || ai.MissingDecoder != "" {
				t.Skipf("the loaded FFmpeg predates #78: no decoder for video %q, audio %q", vi.MissingDecoder, ai.MissingDecoder)
			}
			if c.video && (vi.Width != 320 || vi.Height != 240) {
				t.Errorf("video %dx%d, want 320x240", vi.Width, vi.Height)
			}
			if ai.SampleRate != 48000 {
				t.Errorf("audio at %d Hz, want 48000", ai.SampleRate)
			}
			video, samples := 0, 0
			for i := 0; i < 2000; i++ {
				mt, err := dec.Next()
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if mt == MediaTypeNone {
					break
				}
				switch mt {
				case MediaTypeVideo:
					rgba := image.NewRGBA(image.Rect(0, 0, vi.Width, vi.Height))
					if err := dec.GetRGBA(rgba); err != nil {
						t.Fatalf("video frame %d: GetRGBA: %v", video, err)
					}
					img := image.NewYCbCr(image.Rect(0, 0, vi.Width, vi.Height), image.YCbCrSubsampleRatio420)
					if _, err := dec.(YCbCrDecoder).GetYCbCr(img); err != nil {
						t.Fatalf("video frame %d: GetYCbCr: %v", video, err)
					}
					video++
				case MediaTypeAudio:
					b, err := dec.GetAudio()
					if err != nil {
						t.Fatalf("GetAudio: %v", err)
					}
					samples += len(b) / 2 / max(ai.Channels, 1)
				}
			}
			name := dec.(interface{ videoDecoder() string }).videoDecoder()
			t.Logf("%d video frames (%q), %d audio samples", video, name, samples)
			if video != c.wantVideo {
				t.Errorf("%d video frames, want %d", video, c.wantVideo)
			}
			// Two seconds at 48 kHz, give or take Opus's priming and padding.
			if samples < 90000 || samples > 100000 {
				t.Errorf("%d audio samples, want about 96000", samples)
			}
		})
	}
}
