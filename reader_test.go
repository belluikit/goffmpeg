//go:build cgo

package goffmpeg

import (
	"bytes"
	"errors"
	"image"
	"io"
	"os"
	"testing"
)

// Small H.264/AAC fixtures (4s, 320x240, 1s GOP) checked in under testdata so
// the suite is self-contained.
const (
	// sampleMedia keeps its moov index at the end of the file, the default MP4
	// layout — fine when seeking is available.
	sampleMedia = "testdata/sample.mp4"
	// streamMedia is the same content muxed with +faststart, so the index
	// precedes the media data and the file can be played forward-only.
	streamMedia = "testdata/sample_stream.mp4"
)

func openSample(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(sampleMedia)
	if err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestOpenReaderSeekable checks that a seekable reader yields the same stream
// properties as opening the identical file by path.
func TestOpenReaderSeekable(t *testing.T) {
	byPath, err := Open(sampleMedia)
	if err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	defer byPath.Close()

	dec, err := OpenReader(openSample(t))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer dec.Close()

	wantV, gotV := byPath.VideoInfo(), dec.VideoInfo()
	if wantV.Width != gotV.Width || wantV.Height != gotV.Height {
		t.Errorf("video size = %dx%d, want %dx%d", gotV.Width, gotV.Height, wantV.Width, wantV.Height)
	}
	if gotV.Width == 0 || gotV.Height == 0 {
		t.Fatal("no video stream detected through custom IO")
	}
	if wantA, gotA := byPath.AudioInfo(), dec.AudioInfo(); wantA.SampleRate != gotA.SampleRate {
		t.Errorf("sample rate = %d, want %d", gotA.SampleRate, wantA.SampleRate)
	}
	// A seekable reader must produce a real duration, not the 0 you get when
	// libavformat cannot reach the trailing index.
	if got, want := dec.Duration(), byPath.Duration(); got <= 0 || got != want {
		t.Errorf("duration = %v, want %v (>0)", got, want)
	}
}

// TestOpenReaderDecodesFrames drives the full decode loop through custom IO and
// confirms real pixels and PCM come out the other side.
func TestOpenReaderDecodesFrames(t *testing.T) {
	dec, err := OpenReader(openSample(t))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer dec.Close()

	info := dec.VideoInfo()
	rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))

	var video, audio, audioBytes int
	for video < 30 && video+audio < 3000 {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		switch mt {
		case MediaTypeVideo:
			if err := dec.GetRGBA(rgba); err != nil {
				t.Fatalf("GetRGBA: %v", err)
			}
			video++
		case MediaTypeAudio:
			b, err := dec.GetAudio()
			if err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			audio++
			audioBytes += len(b)
		}
	}

	if video < 30 {
		t.Errorf("decoded %d video frames, want 30", video)
	}
	if audio == 0 || audioBytes == 0 {
		t.Errorf("decoded %d audio packets (%d bytes), want non-zero", audio, audioBytes)
	}
	// An all-zero buffer would mean sws_scale wrote nothing.
	if allZero(rgba.Pix) {
		t.Error("last decoded frame is entirely zero — no pixels were written")
	}
	if pos := dec.Position(); pos <= 0 {
		t.Errorf("position = %v, want > 0", pos)
	}
}

// TestOpenReaderSeek verifies the AVIO seek callback repositions the stream.
func TestOpenReaderSeek(t *testing.T) {
	dec, err := OpenReader(openSample(t))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer dec.Close()

	if err := dec.Seek(2); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	for i := 0; i < 200; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			t.Fatalf("no frame after seek (err=%v)", err)
		}
		if mt == MediaTypeVideo {
			break
		}
	}
	// av_seek_frame lands on the keyframe at or before the target, so allow
	// slack below 2s but reject a decoder still sitting at the start.
	if pos := dec.Position(); pos <= 0 {
		t.Errorf("position after seeking to 2s = %v, want > 0", pos)
	}
}

// TestOpenReaderNonSeekable covers the forward-only path: no io.Seeker, so the
// seek callback is disabled and libavformat must work from probing alone. It
// uses the +faststart fixture, whose moov atom sits ahead of the media data —
// the layout progressive download and HTTP streaming rely on.
func TestOpenReaderNonSeekable(t *testing.T) {
	data, err := os.ReadFile(streamMedia)
	if err != nil {
		t.Skipf("stream fixture unavailable: %v", err)
	}
	// A bare Reader — bytes.Reader would satisfy io.Seeker and defeat the test.
	dec, err := OpenReader(struct{ io.Reader }{bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("OpenReader on a faststart stream: %v", err)
	}
	defer dec.Close()

	info := dec.VideoInfo()
	if info.Width != 320 || info.Height != 240 {
		t.Errorf("video size = %dx%d, want 320x240", info.Width, info.Height)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))

	var frames int
	for i := 0; i < 200 && frames < 5; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		if mt == MediaTypeVideo {
			if err := dec.GetRGBA(rgba); err != nil {
				t.Fatalf("GetRGBA: %v", err)
			}
			frames++
		}
	}
	if frames < 5 {
		t.Errorf("decoded %d frames from a non-seekable stream, want 5", frames)
	}
	if allZero(rgba.Pix) {
		t.Error("decoded frame is entirely zero")
	}
}

// TestOpenReaderTrailingMoovDoesNotAbort is a regression test. An unfragmented
// MP4 keeps its moov index at the end of the file, so reading it forward-only
// leaves libavformat unable to resolve the pixel format. Passing that
// AV_PIX_FMT_NONE straight to sws_getContext trips an internal assertion that
// abort()s the whole process; the scaler is now built from the decoded frame
// instead. Whether this input opens or errors is FFmpeg's business — the point
// is that the process survives and reports failure through the API.
func TestOpenReaderTrailingMoovDoesNotAbort(t *testing.T) {
	data, err := os.ReadFile(sampleMedia)
	if err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	dec, err := OpenReader(struct{ io.Reader }{bytes.NewReader(data)})
	if err != nil {
		return // Refusing to open is a fine outcome.
	}
	defer dec.Close()

	info := dec.VideoInfo()
	rgba := image.NewRGBA(image.Rect(0, 0, max(info.Width, 1), max(info.Height, 1)))
	for i := 0; i < 50; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			return // Errored out cleanly rather than crashing.
		}
		if mt == MediaTypeVideo {
			_ = dec.GetRGBA(rgba) // Must return an error, never abort.
		}
	}
}

// TestOpenReaderErrors covers the failure paths.
func TestOpenReaderErrors(t *testing.T) {
	if _, err := OpenReader(nil); err == nil {
		t.Error("expected error for nil reader")
	}
	if _, err := OpenReader(bytes.NewReader([]byte("not media at all"))); err == nil {
		t.Error("expected error for non-media bytes")
	}
	if _, err := OpenReader(errReader{}); err == nil {
		t.Error("expected error when the reader fails")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestOpenReaderRegistryReleased ensures Close drops the registry entry so
// long-running processes do not leak a readerState per opened stream.
func TestOpenReaderRegistryReleased(t *testing.T) {
	before := registrySize()
	dec, err := OpenReader(openSample(t))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if got := registrySize(); got != before+1 {
		t.Errorf("registry size = %d, want %d while open", got, before+1)
	}
	dec.Close()
	if got := registrySize(); got != before {
		t.Errorf("registry size = %d after Close, want %d", got, before)
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
