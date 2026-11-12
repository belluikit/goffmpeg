package goffmpeg

import (
	"errors"
	"image"
	"testing"
)

// testdata/sample_size_change.ts is two H.264 MPEG-TS clips joined end to end:
// thirty 64x48 frames, then five 96x64 frames. VideoInfo reports the first
// size; the first clip is long enough that probing the input stops inside it.

// A frame whose size no longer matches VideoInfo gets ErrFrameSizeChanged from
// GetRGBA, not nil, and leaves the buffer as it was (#63). Untagged, so it runs
// on the cgo and purego backends alike.
func TestGetRGBAFrameSizeChanged(t *testing.T) {
	dec, err := Open("testdata/sample_size_change.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	if info.Width != 64 || info.Height != 48 {
		t.Fatalf("VideoInfo = %dx%d, want 64x48", info.Width, info.Height)
	}

	rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	var results []error
	for i := 0; i < 100; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeVideo {
			continue
		}
		fill(rgba.Pix, 0x5a)
		err = dec.GetRGBA(rgba)
		results = append(results, err)
		if err != nil && !untouched(rgba.Pix, 0x5a) {
			t.Errorf("video frame %d: GetRGBA returned %v and wrote into the buffer", len(results)-1, err)
		}
		if err == nil && untouched(rgba.Pix, 0x5a) {
			t.Errorf("video frame %d: GetRGBA returned nil and wrote nothing", len(results)-1)
		}
	}

	if len(results) != 35 {
		t.Fatalf("decoded %d video frames, want 35", len(results))
	}
	for i, err := range results {
		switch {
		case i < 30 && err != nil:
			t.Errorf("video frame %d (64x48): GetRGBA = %v, want nil", i, err)
		case i >= 30 && !errors.Is(err, ErrFrameSizeChanged):
			t.Errorf("video frame %d (96x64): GetRGBA = %v, want ErrFrameSizeChanged", i, err)
		}
	}
}

// A buffer that does not match VideoInfo is refused before the bridge writes
// into it: the bridge writes VideoInfo's rows packed, whatever the image says.
func TestGetRGBARefusesMismatchedBuffer(t *testing.T) {
	dec, err := Open("testdata/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	for {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			t.Fatalf("no video frame: %v, %v", mt, err)
		}
		if mt == MediaTypeVideo {
			break
		}
	}
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, info.Width, info.Height-1), // too few rows
		image.Rect(0, 0, info.Width+1, info.Height), // wider stride
		image.Rect(0, 0, info.Width/2, info.Height), // narrower stride
	} {
		rgba := image.NewRGBA(r)
		fill(rgba.Pix, 0x5a)
		if err := dec.GetRGBA(rgba); err == nil {
			t.Errorf("GetRGBA(%v) = nil for VideoInfo %dx%d, want an error", r.Size(), info.Width, info.Height)
		}
		if !untouched(rgba.Pix, 0x5a) {
			t.Errorf("GetRGBA(%v) wrote into a buffer it refused", r.Size())
		}
	}
	if err := dec.GetRGBA(image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))); err != nil {
		t.Errorf("GetRGBA with a VideoInfo-sized buffer: %v", err)
	}
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

func untouched(b []byte, v byte) bool {
	for _, c := range b {
		if c != v {
			return false
		}
	}
	return true
}
