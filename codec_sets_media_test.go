//go:build cgo && !goffmpeg_novideo

package goffmpeg

import "testing"

// An MP4 whose video no build decodes opens anyway, names the video's codec,
// and plays its AAC audio (#41). Every media set runs this, the desktop's,
// which has every codec, included. The video is EVC (MPEG-5), which FFmpeg
// decodes only through libxevd, which no build has: the fixture is
// sample_hevc.mp4 with its video sample entry relabelled from hvc1 to evc1, so
// its bytes are HEVC, but no decoder ever reads them.
func TestMediaSetNamesUndecodableVideoAndPlaysAudio(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open("testdata/sample_evc.mp4") // "EVC" video, AAC audio
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if got := dec.VideoInfo().MissingDecoder; got != "evc" {
		t.Errorf("VideoInfo().MissingDecoder = %q, want %q", got, "evc")
	}
	if got := dec.AudioInfo().MissingDecoder; got != "" {
		t.Errorf("AudioInfo().MissingDecoder = %q, want empty", got)
	}
	video, audio := drainFrames(t, dec, 500)
	if video != 0 {
		t.Errorf("%d video frames from a stream no build has a decoder for", video)
	}
	if audio == 0 {
		t.Error("decoded no audio")
	}
}

// The phone media set's motivating case for MissingDecoder (#41): an MP4 whose
// HEVC video the set doesn't decode opens anyway, names the video's codec, and
// plays its AAC audio. The desktop media set, which has every codec, decodes
// the HEVC video too.
func TestMediaSetNamesHEVCAndPlaysAudio(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open("testdata/sample_hevc.mp4") // HEVC video, AAC audio
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	wantMissing := "hevc"
	if everyCodec() {
		wantMissing = ""
	}
	if got := dec.VideoInfo().MissingDecoder; got != wantMissing {
		t.Errorf("VideoInfo().MissingDecoder = %q, want %q", got, wantMissing)
	}
	if got := dec.AudioInfo().MissingDecoder; got != "" {
		t.Errorf("AudioInfo().MissingDecoder = %q, want empty", got)
	}
	video, audio := drainFrames(t, dec, 500)
	if everyCodec() && video == 0 {
		t.Error("the desktop media set decoded no HEVC video")
	}
	if !everyCodec() && video != 0 {
		t.Errorf("%d video frames from a stream the phone media set has no decoder for", video)
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

// drainFrames reads up to max frames and counts the video frames and the
// audio frames that carried samples.
func drainFrames(t *testing.T, dec IDecoder, max int) (video, audio int) {
	t.Helper()
	for i := 0; i < max; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		switch mt {
		case MediaTypeNone:
			return video, audio
		case MediaTypeVideo:
			video++
		case MediaTypeAudio:
			if b, err := dec.GetAudio(); err == nil && len(b) > 0 {
				audio++
			}
		}
	}
	return video, audio
}
