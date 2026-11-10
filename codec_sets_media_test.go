//go:build cgo && !goffmpeg_novideo

package goffmpeg

import "testing"

// The media set's motivating case for MissingDecoder (#41): an MP4 whose
// HEVC video the set doesn't decode opens anyway, names the video's codec,
// and plays its AAC audio.
func TestMediaSetNamesHEVCAndPlaysAudio(t *testing.T) {
	requireMissingDecoderSupport(t)
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

// The media set plays HLS (owner, #41), which bellui's x/mmedia streams: a
// local playlist of MPEG-TS segments with H.264 and AAC, through FFmpeg's hls
// and mpegts demuxers.
func TestMediaSetPlaysHLS(t *testing.T) {
	dec, err := Open("testdata/hls/stream.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	v, a := dec.VideoInfo(), dec.AudioInfo()
	if v.Width != 320 || v.Height != 240 || a.SampleRate == 0 {
		t.Fatalf("video %+v, audio %+v: want 320x240 video and audio", v, a)
	}
	if v.MissingDecoder != "" || a.MissingDecoder != "" {
		t.Errorf("MissingDecoder video %q, audio %q, want both empty", v.MissingDecoder, a.MissingDecoder)
	}
	var video, audio int
	for i := 0; i < 1000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		if mt == MediaTypeNone {
			break
		}
		switch mt {
		case MediaTypeVideo:
			video++
		case MediaTypeAudio:
			audio++
		}
	}
	// 2.08 s across three segments: 52 video frames, as ffprobe counts them.
	if video != 52 {
		t.Errorf("decoded %d video frames, want 52", video)
	}
	if audio == 0 {
		t.Error("decoded no audio")
	}
}
