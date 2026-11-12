//go:build cgo && goffmpeg_novideo

package goffmpeg

import (
	"errors"
	"image"
	"strings"
	"testing"
)

// testCodecSet is the codec set this test build links (#41): goffmpeg_novideo
// links the audio set. missing_video_test.go says "media".
const testCodecSet = "audio"

// An audio-only build decodes a file's audio and never its video: no video
// stream is opened, no frame arrives, and GetRGBA says why. The MP3 carries a
// cover picture, which no build decodes as video.
func TestNoVideoDecodesAudioOnly(t *testing.T) {
	dec, err := Open("testdata/sample_cover.mp3") // MP3 audio and a cover picture
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
	img := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	if _, err := dec.(YCbCrDecoder).GetYCbCr(img); !errors.Is(err, ErrVideoNotBuilt) {
		t.Errorf("GetYCbCr: %v, want ErrVideoNotBuilt", err)
	}
}

// An input with nothing an audio-only build decodes, a video with no audio,
// fails to open with a clear error instead of opening and yielding nothing
// (#41). The audio set has no MP4 demuxer, so it can't read the file at all:
// ErrUnrecognizedInput.
func TestNoVideoVideoOnlyInputFails(t *testing.T) {
	dec, err := Open("testdata/sample_video_cover.mp4") // video and cover art, no audio
	if err == nil {
		dec.Close()
		t.Fatal("a video-only file opened in an audio-only build")
	}
	if !errors.Is(err, ErrUnrecognizedInput) {
		t.Fatalf("%v, want ErrUnrecognizedInput", err)
	}
}

// The audio set reads no MP4 (#41): opening one says the build has no demuxer
// for it, which is what an app shows a user who picks a file it can't play.
func TestAudioSetRefusesMP4(t *testing.T) {
	dec, err := Open("testdata/sample.mp4")
	if err == nil {
		dec.Close()
		t.Fatal("the audio set opened an MP4")
	}
	if !errors.Is(err, ErrUnrecognizedInput) {
		t.Errorf("%v, want ErrUnrecognizedInput", err)
	}
}

// Nor HLS: the playlist is refused as a format the build doesn't read.
func TestAudioSetRefusesHLS(t *testing.T) {
	dec, err := Open("testdata/hls/stream.m3u8")
	if err == nil {
		dec.Close()
		t.Fatal("the audio set opened an HLS playlist")
	}
	if !errors.Is(err, ErrUnrecognizedInput) {
		t.Errorf("%v, want ErrUnrecognizedInput", err)
	}
}

// The audio set has no libavdevice, so an audio-only build has no capture:
// OpenDevice says so rather than failing some other way.
func TestNoVideoHasNoCapture(t *testing.T) {
	dec, err := OpenDevice("avfoundation", "0", "")
	if err == nil {
		dec.Close()
		t.Fatal("an audio-only build opened a capture device")
	}
	if !strings.Contains(err.Error(), "without libavdevice") {
		t.Errorf("%v, want the build-without-libavdevice error", err)
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

// The tag links the audio set (#41), whose libraries carry -goffmpeg-audio,
// and nothing of the media set: no libavdevice either.
func TestNoVideoLinksTheAudioSet(t *testing.T) {
	got := requireSharedFFmpeg(t)
	if !got["set:audio"] {
		t.Error("an audio-only build doesn't import the audio set's -goffmpeg-audio libraries")
	}
	if got["set:media"] {
		t.Error("an audio-only build imports the media set's libraries")
	}
	if got["avdevice"] {
		t.Error("an audio-only build imports libavdevice")
	}
}

// The audio set plays Opus in Ogg too (#78), beside MP3 and WAV.
func TestAudioSetPlaysOpus(t *testing.T) {
	dec, err := Open("testdata/sample.opus")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if ai := dec.AudioInfo(); ai.SampleRate != 48000 || ai.MissingDecoder != "" {
		t.Fatalf("audio %+v, want 48000 Hz and a decoder", ai)
	}
	samples := 0
	for {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if mt == MediaTypeNone {
			break
		}
		if b, err := dec.GetAudio(); err == nil {
			samples += len(b) / 2 / max(dec.AudioInfo().Channels, 1)
		}
	}
	if samples < 90000 || samples > 100000 {
		t.Errorf("%d samples, want about 96000 (two seconds at 48 kHz)", samples)
	}
}
