package goffmpeg

import (
	"os"
	"testing"
)

// coverArtMedia is 1s of mono 44.1kHz MP3 carrying a 64x64 JPEG as an
// attached picture: the cover art an MP3, M4A or FLAC commonly embeds.
// Reference ffmpeg decodes its audio to 44100 samples.
const coverArtMedia = "testdata/sample_cover.mp3"

// TestCoverArtIsNotVideo is the regression test for #21. av_find_best_stream
// returns an attached-picture stream as the video stream, so the file
// reported a 64x64 video that yields one frame and no more, and a player
// waited for frames that never came. Deliberately untagged: the fix is in
// bridge.c, so purego only has it once libgoffmpeg is re-released.
func TestCoverArtIsNotVideo(t *testing.T) {
	if _, err := os.Stat(coverArtMedia); err != nil {
		t.Skipf("cover-art fixture unavailable: %v", err)
	}
	dec, err := Open(coverArtMedia)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()

	if got := dec.VideoInfo(); got != (VideoInfo{}) {
		t.Errorf("VideoInfo() = %+v, want zero: the cover is not a video stream", got)
	}
	if got := dec.AudioInfo(); got.SampleRate != 44100 || got.Channels != 1 {
		t.Fatalf("AudioInfo() = %+v, want 44100Hz mono", got)
	}

	var video, samples int
	for i := 0; i < 1000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if mt == MediaTypeNone {
			break
		}
		switch mt {
		case MediaTypeVideo:
			video++
		case MediaTypeAudio:
			b, err := dec.GetAudio()
			if err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			samples += len(b) / 2 // S16 mono
		}
	}
	if video != 0 {
		t.Errorf("decoded %d video frames, want 0", video)
	}
	if samples != 44100 {
		t.Errorf("decoded %d audio samples, want 44100 (all of the stream)", samples)
	}
}

// TestCoverArtBesideVideo checks the other side of #21: hiding attached
// pictures from the stream choice must not hide real video. The fixture is a
// 32x24 MPEG-4 video of 10 frames plus a 64x64 cover.
func TestCoverArtBesideVideo(t *testing.T) {
	const path = "testdata/sample_video_cover.mp4"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("video-and-cover fixture unavailable: %v", err)
	}
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()

	if info := dec.VideoInfo(); info.Width != 32 || info.Height != 24 {
		t.Fatalf("video = %dx%d, want 32x24 (the video, not the cover)", info.Width, info.Height)
	}
}
