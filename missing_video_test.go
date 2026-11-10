//go:build !goffmpeg_novideo

package goffmpeg

import "testing"

// testCodecSet is the codec set this test build links (#41): a build without
// goffmpeg_novideo links the media set. novideo_test.go says "audio".
const testCodecSet = "media"

// A build that decodes every stream of a file reports no MissingDecoder: the
// field is empty for a stream that decodes, not only for one that is absent.
// TestMediaSetNamesUndecodableVideoAndPlaysAudio is the case where a stream
// doesn't decode.
func TestMissingDecoderEmptyWhenDecoded(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open("testdata/sample.mp4") // H.264 video and AAC audio
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	v, a := dec.VideoInfo(), dec.AudioInfo()
	if v.Width == 0 || a.SampleRate == 0 {
		t.Fatalf("video %+v, audio %+v: expected both to decode", v, a)
	}
	if v.MissingDecoder != "" || a.MissingDecoder != "" {
		t.Errorf("MissingDecoder video %q, audio %q, want both empty", v.MissingDecoder, a.MissingDecoder)
	}
}
