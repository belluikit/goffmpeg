package goffmpeg

import (
	"os"
	"testing"
)

// openAtRate opens path and sets its audio output rate.
func openAtRate(t *testing.T, path string, rate int) IDecoder {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(dec.Close)
	if err := dec.SetAudioSampleRate(rate); err != nil {
		t.Fatalf("SetAudioSampleRate(%d): %v", rate, err)
	}
	return dec
}

// TestResampledAudioKeepsItsTail is the regression test for #24. A rate
// conversion leaves swresample holding a few samples that only a flush
// releases, and nothing flushed it, so resampled audio came out short: 17
// samples of the MP3 at 48kHz. The wanted byte counts are reference ffmpeg's
// `-ar <rate> -f s16le` output. Untagged, so purego runs it too.
func TestResampledAudioKeepsItsTail(t *testing.T) {
	cases := []struct {
		path       string
		rate       int
		audioBytes int
	}{
		{"testdata/sample_cover.mp3", 48000, 96000},
		{"testdata/sample_cover.mp3", 22050, 44100},
		{"testdata/sample.mp4", 48000, 384000},
		{"testdata/sample_51.m4a", 48000, 1152000},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			dec := openAtRate(t, c.path, c.rate)
			if _, got := decodeAll(t, dec); got != c.audioBytes {
				t.Errorf("at %dHz decoded %d audio bytes, want %d", c.rate, got, c.audioBytes)
			}
		})
	}
}

// TestNextEndsWithoutGetAudio checks that the tail does not hold Next open for
// a caller that stops asking for audio: the flush frame is offered once, taken
// or not. The first frames are taken so swresample has a tail to offer. A
// caller that never calls GetAudio feeds it nothing, has no tail, and would
// pass even if the flush frame came back forever.
func TestNextEndsWithoutGetAudio(t *testing.T) {
	dec := openAtRate(t, "testdata/sample_cover.mp3", 48000)
	taken := 0
	for i := 0; i < 10000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if mt == MediaTypeNone {
			return
		}
		if mt == MediaTypeAudio && taken < 5 {
			if _, err := dec.GetAudio(); err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			taken++
		}
	}
	t.Fatal("Next did not reach the end in 10000 calls once GetAudio stopped")
}

// TestSeekDiscardsResamplerTail checks that samples swresample buffered before
// a seek do not come out after it. Decoding part of the file, seeking to the
// start and decoding to the end must give exactly what a fresh decode does.
//
// It passes against a bridge with neither half of #24, because there the two
// faults cancel: the stale samples carried over the seek are as many as the
// unflushed tail drops. With the flush and without the reset it fails, 96034
// bytes against 96000.
func TestSeekDiscardsResamplerTail(t *testing.T) {
	dec := openAtRate(t, "testdata/sample_cover.mp3", 48000)
	for audio := 0; audio < 5; {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			t.Fatalf("Next before the seek: %v, %v", mt, err)
		}
		if mt == MediaTypeAudio {
			if _, err := dec.GetAudio(); err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			audio++
		}
	}
	if err := dec.Seek(0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, got := decodeAll(t, dec); got != 96000 {
		t.Errorf("decoded %d audio bytes after Seek(0), want 96000", got)
	}
}
