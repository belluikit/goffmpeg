package goffmpeg

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

// v4l2NodeNumber extracts the trailing integer of a /dev/videoN node so the
// candidates can be tried in the order a real caller would use. readdir order
// is arbitrary, so /dev/video10 can otherwise precede /dev/video0.
func v4l2NodeNumber(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(name[i:])
	if err != nil {
		return int(^uint(0) >> 1) // no trailing number: sort last
	}
	return n
}

// reportDevices writes the listing to stderr as well as the test log.
//
// t.Logf output is buffered until the test ends, so when an open hangs and the
// suite dies on its global timeout, the listing that would explain *why* is
// lost with it — which is exactly what happened twice while getting the v4l2
// path working. Stderr survives the panic.
func reportDevices(t *testing.T, format string, devs []DeviceInfo) {
	t.Helper()
	for _, d := range devs {
		line := fmt.Sprintf("%s %q (%s) media=%v known=%v default=%v",
			format, d.Name, d.Description, d.MediaTypes, d.MediaKnown, d.Default)
		t.Log(line)
		fmt.Fprintln(os.Stderr, line)
	}
}

// openWithin opens a device on its own goroutine and gives up after d.
//
// Opening a capture device can block indefinitely rather than fail — a v4l2
// metadata node is the known case — and a test that calls OpenDevice directly
// takes the whole suite down with a 10-minute timeout and no indication of
// which device did it. The goroutine is left running: a blocked open cannot be
// cancelled, so it closes the decoder itself if it ever returns.
func openWithin(t *testing.T, format, url string, d time.Duration) (IDecoder, error) {
	t.Helper()
	return openWithinOpts(t, format, url, "", d)
}

// openWithinOpts is openWithin with per-open device options. Separate because
// some devices reject the default settings and can only be opened by naming
// one they support — see TestCaptureSurvivesEmptyReads.
func openWithinOpts(t *testing.T, format, url, options string, d time.Duration) (IDecoder, error) {
	t.Helper()
	type result struct {
		dec IDecoder
		err error
	}
	ch := make(chan result, 1)
	go func() {
		dec, err := OpenDevice(format, url, options)
		ch <- result{dec, err}
	}()
	select {
	case r := <-ch:
		return r.dec, r.err
	case <-time.After(d):
		go func() {
			if r := <-ch; r.dec != nil {
				r.dec.Close()
			}
		}()
		return nil, fmt.Errorf("OpenDevice(%s, %s) still blocked after %s", format, url, d)
	}
}

// openDeviceTimeout bounds a single device open. Generous: a cold USB camera
// can take a second or two to hand over its first frame, and this is only
// meant to catch a block that would otherwise never end.
const openDeviceTimeout = 15 * time.Second

// skipIfNoEnumeration skips when the loaded backend cannot enumerate at all.
// The purego leg of CI deliberately stages the *published* libgoffmpeg, so
// until a release carrying the device_list_* symbols ships, that library
// reports ErrDeviceListUnavailable for everything — a real answer about the
// artifact, but not one these tests can assert format behaviour through.
func skipIfNoEnumeration(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrDeviceListUnavailable) {
		t.Skipf("backend cannot enumerate: %v", err)
	}
}

// TestListInputSourcesUnknownFormat covers the one failure every platform can
// reproduce: a format libav has never heard of is an error, and specifically
// not ErrDeviceListUnsupported — callers treat that sentinel as "fall back to
// the platform default", which would be wrong for a typo.
func TestListInputSourcesUnknownFormat(t *testing.T) {
	devs, err := ListInputSources("definitely-not-a-device")
	if err == nil {
		t.Fatalf("ListInputSources(bogus) = %v, want an error", devs)
	}
	skipIfNoEnumeration(t, err)
	if errors.Is(err, ErrDeviceListUnsupported) {
		t.Fatalf("unknown format reported as unsupported enumeration: %v", err)
	}
}

// TestListInputSourcesAVFoundation pins the macOS answer: avfoundation
// implements no get_device_list callback, so enumeration is unsupported rather
// than empty. Callers must keep selecting macOS cameras positionally ("0").
func TestListInputSourcesAVFoundation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("avfoundation is macOS-only")
	}
	devs, err := ListInputSources("avfoundation")
	skipIfNoEnumeration(t, err)
	if !errors.Is(err, ErrDeviceListUnsupported) {
		t.Fatalf("ListInputSources(avfoundation) = %v, %v; want ErrDeviceListUnsupported", devs, err)
	}
}

// TestListInputSourcesV4L2 is the Linux acceptance criterion: enumeration finds
// the machine's cameras, correctly separates capture nodes from metadata ones,
// and the Name it reports opens directly — that round trip is the point of the
// API.
//
// The split of what is asserted follows what this code actually controls.
// Enumeration and capability determination are ours, so a wrong answer fails.
// Whether a camera will open is machine state — busy, switched off, wedged
// passthrough — so failing to open any of them logs the reasons and skips.
func TestListInputSourcesV4L2(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("v4l2 is Linux-only")
	}
	devs, err := ListInputSources("v4l2")
	skipIfNoEnumeration(t, err)
	if err != nil {
		t.Fatalf("ListInputSources(v4l2): %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no v4l2 capture devices on this host")
	}
	reportDevices(t, "v4l2", devs)

	// Only nodes determined to do video capture may be opened. This is not
	// tidiness: a UVC camera's metadata node enumerates like any other, and
	// opening it blocks indefinitely instead of failing — it hung this test for
	// the full 10-minute timeout before the bridge learned to query V4L2_CAP_*.
	var video []DeviceInfo
	for _, d := range devs {
		if d.HasMedia(MediaTypeVideo) {
			video = append(video, d)
		}
	}
	if len(video) == 0 {
		t.Skip("v4l2 devices present but none does video capture")
	}

	// Lowest node first, as a real caller orders them: it is conventionally the
	// primary camera, and trying a wedged one first costs openDeviceTimeout.
	sort.SliceStable(video, func(i, j int) bool {
		return v4l2NodeNumber(video[i].Name) < v4l2NodeNumber(video[j].Name)
	})

	for _, d := range video {
		dec, err := openWithin(t, "v4l2", d.Name, openDeviceTimeout)
		if err != nil {
			// One retry: v4l2 does not always release a device promptly ("Some
			// buffers are still owned by the caller on close"), so a node the
			// previous test just closed can still be busy here.
			time.Sleep(time.Second)
			dec, err = openWithin(t, "v4l2", d.Name, openDeviceTimeout)
		}
		if err != nil {
			t.Logf("%s: %v", d.Name, err)
			continue
		}
		defer dec.Close()
		info := dec.VideoInfo()
		t.Logf("opened %s: %dx%d @ %.2ffps", d.Name, info.Width, info.Height, info.Framerate)
		if info.Width <= 0 || info.Height <= 0 {
			t.Fatalf("%s reported %dx%d, want a real frame size", d.Name, info.Width, info.Height)
		}
		return
	}

	// Enumeration is this test's contract and it held — the listing above is
	// the evidence. Whether any of those cameras will *open* is machine state:
	// the runner has an OBS virtual camera that fails unless OBS is running,
	// and a passed-through FaceTime camera that blocks. Failing on that would
	// report a broken build for an idle camera, so it skips, loudly.
	t.Skipf("enumerated %d video-capture node(s), none currently openable (see log)", len(video))
}

// TestListInputSourcesDShow is the Windows acceptance criterion, and the reason
// the API exists: dshow has no positional device form, so before this there was
// no way to name a camera without knowing it in advance. Enumeration must find
// one, and its Name must open once prefixed with "video=".
//
// Same split as the v4l2 test: enumeration is asserted, openability is
// best-effort and skips with the reasons logged.
func TestListInputSourcesDShow(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("dshow is Windows-only")
	}
	devs, err := ListInputSources("dshow")
	skipIfNoEnumeration(t, err)
	if err != nil {
		t.Fatalf("ListInputSources(dshow): %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no dshow capture devices on this host")
	}
	reportDevices(t, "dshow", devs)

	// dshow lists audio sources alongside video ones, so filter before opening.
	var video []DeviceInfo
	for _, d := range devs {
		if d.HasMedia(MediaTypeVideo) {
			video = append(video, d)
		}
	}
	if len(video) == 0 {
		t.Skip("dshow found devices but no video source")
	}
	for _, d := range video {
		dec, err := openWithin(t, "dshow", "video="+d.Name, openDeviceTimeout)
		if err != nil {
			t.Logf("%s: %v", d.Name, err)
			continue
		}
		defer dec.Close()
		info := dec.VideoInfo()
		t.Logf("opened %q (%s): %dx%d @ %.2ffps", d.Description, d.Name, info.Width, info.Height, info.Framerate)
		if info.Width <= 0 || info.Height <= 0 {
			t.Fatalf("%s reported %dx%d, want a real frame size", d.Name, info.Width, info.Height)
		}
		return
	}
	// As with v4l2: enumeration is the contract, and it held. A camera that is
	// switched off or already in use is machine state, not a broken build.
	t.Skipf("enumerated %d dshow video device(s), none currently openable (see log)", len(video))
}

// TestHasMediaUnknown guards the deliberate asymmetry in DeviceInfo.HasMedia,
// where both directions carry real consequences: an undetermined device must
// still be tried (guessing "no" would drop a working camera), and a device
// determined to do no video must be skipped (on Linux that is the metadata
// node, which blocks forever when opened rather than failing).
func TestHasMediaUnknown(t *testing.T) {
	unknown := DeviceInfo{Name: "/dev/video0"}
	if !unknown.HasMedia(MediaTypeVideo) || !unknown.HasMedia(MediaTypeAudio) {
		t.Error("a device whose media could not be determined should not be filtered out")
	}

	// Determined, and it does neither — the metadata-node shape.
	neither := DeviceInfo{Name: "/dev/video1", MediaKnown: true}
	if neither.HasMedia(MediaTypeVideo) {
		t.Error("a device determined to do no video must be skipped, not tried")
	}

	videoOnly := DeviceInfo{Name: "cam", MediaTypes: []MediaType{MediaTypeVideo}, MediaKnown: true}
	if !videoOnly.HasMedia(MediaTypeVideo) {
		t.Error("video device should report video")
	}
	if videoOnly.HasMedia(MediaTypeAudio) {
		t.Error("video-only device should not report audio")
	}
}
