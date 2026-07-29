package goffmpeg

import (
	"image"
	"os"
	"testing"
)

// TestDecodeSmoke decodes real frames through whichever backend is compiled in.
//
// Deliberately untagged so it runs under cgo *and* purego. The purego case is
// the point: without it, a missing or unloadable libgoffmpeg shared object goes
// unnoticed, because Open then fails with "library not loaded" and a test that
// only asserts "opening a bad path errors" still passes.
func TestDecodeSmoke(t *testing.T) {
	const path = "testdata/sample.mp4"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}

	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v (under purego this usually means libgoffmpeg failed to load)", err)
	}
	defer dec.Close()

	info := dec.VideoInfo()
	if info.Width != 320 || info.Height != 240 {
		t.Fatalf("video = %dx%d, want 320x240", info.Width, info.Height)
	}

	rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	var video, audio int
	for i := 0; i < 400 && video < 10; i++ {
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
			if b, err := dec.GetAudio(); err == nil && len(b) > 0 {
				audio++
			}
		}
	}

	if video < 10 {
		t.Errorf("decoded %d video frames, want 10", video)
	}
	if audio == 0 {
		t.Error("decoded no audio")
	}
	allZero := true
	for _, v := range rgba.Pix {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("decoded frame is entirely zero — no pixels were written")
	}
}
