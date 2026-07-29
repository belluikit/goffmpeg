package goffmpeg

import (
	"os"
	"runtime"
	"testing"
)

// TestOpenDeviceDShow is the acceptance criterion from issue #3: OpenDevice
// must open a real camera on Windows.
//
// dshow selects by friendly name rather than by index, so unlike v4l2's
// /dev/videoN or avfoundation's "0" there is no positional form — the url is
// always video=<name>. Names vary per machine, hence the candidate list.
func TestOpenDeviceDShow(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("dshow is Windows-only")
	}
	// Names differ per machine and dshow offers no positional form, so try a
	// few known ones. GOFFMPEG_DSHOW_DEVICE overrides for anything else;
	// enumerate with: ffmpeg -f dshow -list_devices true -i dummy
	candidates := []string{
		"video=OBS Virtual Camera",
		"video=FaceTime HD Camera",
		"video=Logitech BRIO",
		"video=Integrated Camera",
	}
	if env := os.Getenv("GOFFMPEG_DSHOW_DEVICE"); env != "" {
		candidates = []string{env}
	}
	for _, url := range candidates {
		dec, err := OpenDevice("dshow", url, "")
		if err != nil {
			t.Logf("%s: %v", url, err)
			continue
		}
		defer dec.Close()
		info := dec.VideoInfo()
		t.Logf("opened %q: %dx%d @ %.2ffps", url, info.Width, info.Height, info.Framerate)
		if info.Width <= 0 || info.Height <= 0 {
			t.Fatalf("camera reported %dx%d, want a real frame size", info.Width, info.Height)
		}
		return
	}
	t.Skip("no dshow camera among the candidate names")
}
