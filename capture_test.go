package goffmpeg

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

// NOTE: TestOpenDeviceV4L2Reopen is declared first on purpose — Go runs tests
// in declaration order, and it needs the first device open of the process to be
// its own. The bug it guards leaves the node unopenable after one open/close,
// so any earlier test touching the same camera would poison it and turn a real
// regression into "not openable, skipped".

// v4l2ReopenCycles is how many times TestOpenDeviceV4L2Reopen opens the same
// node. Three matches the reproducer in #14; the failure shows up on the
// second, so the third is only there to catch a leak that takes more than one
// cycle to bite.
const v4l2ReopenCycles = 3

// reopenVerdict records, on stderr, whether the reopen assertion actually ran.
//
// `go test` without -v cannot distinguish "reopened a real camera three times"
// from "no camera present, skipped" — and it buffers a package's output and
// discards it when the package passes, so writing to stderr does not rescue
// this either. (reportDevices' output is only ever seen because the runs that
// showed it had failed.) CI therefore gives this test its own `-v` step; these
// lines are what that step prints.
//
// The distinction is the whole value of the test. A run went green while the
// machine had no camera at all and the regression was never exercised.
func reopenVerdict(t *testing.T, format string, args ...any) {
	t.Helper()
	line := "[reopen] " + fmt.Sprintf(format, args...)
	t.Log(line)
	fmt.Fprintln(os.Stderr, line)
}

// TestOpenDeviceV4L2Reopen is the regression test for #14: a v4l2 node must be
// openable more than once in the same process.
//
// It was not. FFmpeg's mmap_free loops `while (--n > 0)`, unmapping buffers
// n-1 down to 1 and never index 0, so one mapping of the device survived every
// close. A mapped buffer keeps the driver's allocation alive even after the
// descriptor is gone, so the next VIDIOC_S_FMT returned EBUSY — reported by
// device_try_init without a log line, hence the bare -1 with no explanation.
// patches/0001 carries the one-line fix.
//
// The asymmetry between failing and skipping is deliberate. Whether *any*
// camera opens is machine state — a virtual camera such as OBS's needs OBS
// running — so no successful first open means skip. Once the first open works,
// every later one on the same node must too, and a failure there is the bug
// rather than the weather.
func TestOpenDeviceV4L2Reopen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("v4l2 is Linux-only")
	}
	devs, err := ListInputSources("v4l2")
	skipIfNoEnumeration(t, err)
	if err != nil {
		t.Fatalf("ListInputSources(v4l2): %v", err)
	}

	// Video-capture nodes only. A UVC camera's metadata node enumerates like
	// any other and blocks forever when opened, so HasMedia is load-bearing.
	var video []DeviceInfo
	for _, d := range devs {
		if d.HasMedia(MediaTypeVideo) {
			video = append(video, d)
		}
	}
	if len(video) == 0 {
		reopenVerdict(t, "SKIPPED — no v4l2 video-capture devices on this host; #14 not exercised")
		t.Skip("no v4l2 video-capture devices on this host")
	}
	sort.SliceStable(video, func(i, j int) bool {
		return v4l2NodeNumber(video[i].Name) < v4l2NodeNumber(video[j].Name)
	})

	for _, d := range video {
		dec, err := openWithin(t, "v4l2", d.Name, openDeviceTimeout)
		if err != nil {
			time.Sleep(time.Second)
			dec, err = openWithin(t, "v4l2", d.Name, openDeviceTimeout)
		}
		if err != nil {
			t.Logf("%s: not openable, trying the next node: %v", d.Name, err)
			continue
		}
		dec.Close()
		t.Logf("%s: open/close cycle 1 of %d ok", d.Name, v4l2ReopenCycles)

		// The device is known good from here, so every remaining cycle must
		// succeed. No retry and no continue: this is the assertion.
		for i := 2; i <= v4l2ReopenCycles; i++ {
			dec, err := openWithin(t, "v4l2", d.Name, openDeviceTimeout)
			if err != nil {
				t.Fatalf("%s: open %d of %d failed after a successful first open: %v\n"+
					"this is #14 — a buffer left mapped by close keeps the node busy",
					d.Name, i, v4l2ReopenCycles, err)
			}
			dec.Close()
			t.Logf("%s: open/close cycle %d of %d ok", d.Name, i, v4l2ReopenCycles)
		}
		reopenVerdict(t, "PASSED — %s reopened %d times; #14 exercised for real", d.Name, v4l2ReopenCycles)
		return
	}

	reopenVerdict(t, "SKIPPED — %d node(s) found, none openable; #14 not exercised", len(video))
	t.Skipf("found %d video-capture node(s), none currently openable (see log)", len(video))
}

// TestOpenDeviceV4L2 is the acceptance criterion from issue #2: OpenDevice must
// open a real camera on Linux by node path, not merely reject an unknown
// format, and without needing enumeration first.
//
// A failed open skips rather than fails, which is the same bargain
// TestListInputSourcesV4L2 and TestListInputSourcesDShow already strike.
// Whether a camera opens is machine state — a former CI machine's /dev/video0
// was an OBS Virtual Camera, which refuses unless OBS is running, and it failed
// a PR that could not reach any device code. Reporting a broken build for an idle
// camera costs more than it catches.
//
// What is genuinely ours still fails: the geometry once the open succeeds. The
// skip is loud, so a camera that stops working is visible in the log rather
// than silent.
//
// openWithin rather than a bare OpenDevice: a v4l2 node can block forever
// instead of failing, and a hang here takes the whole suite down with it.
func TestOpenDeviceV4L2(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("v4l2 is Linux-only")
	}
	const dev = "/dev/video0"
	if _, err := os.Stat(dev); err != nil {
		t.Skipf("no v4l2 capture device: %v", err)
	}

	dec, err := openWithin(t, "v4l2", dev, openDeviceTimeout)
	if err != nil {
		t.Skipf("%s present but not currently openable: %v", dev, err)
	}
	defer dec.Close()

	info := dec.VideoInfo()
	t.Logf("opened %s: %dx%d @ %.2ffps", dev, info.Width, info.Height, info.Framerate)
	if info.Width <= 0 || info.Height <= 0 {
		t.Fatalf("camera reported %dx%d, want a real frame size", info.Width, info.Height)
	}
}
