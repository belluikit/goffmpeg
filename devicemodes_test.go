package goffmpeg

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// modesVerdict records, on stderr, whether the hardware test below actually
// queried a device. Same reasoning as reopenVerdict and dshowVerdict: a
// machine with no camera skips, and skipping and passing look identical
// without -v, so which one happened has to be in the log.
func modesVerdict(t *testing.T, format string, args ...any) {
	t.Helper()
	line := "[modes] " + fmt.Sprintf(format, args...)
	t.Log(line)
	fmt.Fprintln(os.Stderr, line)
}

// TestDeviceModeOptions is the test that matters most in this file, because
// Options is the whole point of #5: a mode is useful only if the string it
// renders is one OpenDevice accepts. Every case here was established against
// real hardware, and three of them are corrections to a first version that
// looked obviously right and did not open.
func TestDeviceModeOptions(t *testing.T) {
	cases := []struct {
		name string
		mode DeviceMode
		want string
	}{
		{
			// The avfoundation case from the issue. bgr0 rather than bgra is
			// the correction: pixel_format names what avfoundation delivers,
			// and its table maps 32BGRA to BGR0 because the alpha is dropped.
			name: "avfoundation raw",
			mode: DeviceMode{Format: "avfoundation", Width: 1920, Height: 1080,
				MinFPS: 60, MaxFPS: 60, PixelFormat: "bgr0"},
			want: "video_size=1920x1080,framerate=60,pixel_format=bgr0",
		},
		{
			// dshow validates pixel_format strictly as a pixel layout, so a
			// compressed mode has to go through vcodec instead. Sending
			// pixel_format=mjpeg here fails the open outright.
			name: "dshow compressed uses vcodec",
			mode: DeviceMode{Format: "dshow", Width: 1920, Height: 1080,
				MinFPS: 60, MaxFPS: 60, Codec: "mjpeg"},
			want: "video_size=1920x1080,framerate=60,vcodec=mjpeg",
		},
		{
			// v4l2 declares pixel_format and input_format at one offset and
			// resolves a codec name before a pixel format, so the same option
			// carries both and mjpeg belongs there rather than in vcodec.
			name: "v4l2 compressed uses pixel_format",
			mode: DeviceMode{Format: "v4l2", Width: 1920, Height: 1080,
				MinFPS: 60, MaxFPS: 60, Codec: "mjpeg"},
			want: "video_size=1920x1080,framerate=60,pixel_format=mjpeg",
		},
		{
			name: "v4l2 raw",
			mode: DeviceMode{Format: "v4l2", Width: 640, Height: 480,
				MinFPS: 30, MaxFPS: 30, PixelFormat: "yuyv422"},
			want: "video_size=640x480,framerate=30,pixel_format=yuyv422",
		},
		{
			// A rate the backend did not report is left out rather than
			// written as framerate=0, which no device would accept.
			name: "no framerate reported",
			mode: DeviceMode{Format: "v4l2", Width: 1280, Height: 720,
				PixelFormat: "nv12"},
			want: "video_size=1280x720,pixel_format=nv12",
		},
		{
			// Likewise a format with no libav name: the resolution is still
			// worth opening with.
			name: "no pixel format known",
			mode: DeviceMode{Format: "avfoundation", Width: 1280, Height: 720,
				MinFPS: 30, MaxFPS: 30},
			want: "video_size=1280x720,framerate=30",
		},
		{
			// A range renders at its top end; a caller wanting a slower rate
			// within it builds the string itself.
			name: "range takes the maximum",
			mode: DeviceMode{Format: "v4l2", Width: 1920, Height: 1080,
				MinFPS: 1, MaxFPS: 30, PixelFormat: "nv12"},
			want: "video_size=1920x1080,framerate=30,pixel_format=nv12",
		},
		{
			// 29.97 must survive verbatim rather than becoming 29.970001 or
			// 30 — dshow stores intervals in 100ns units and the rounding
			// that corrects for it has to leave the broadcast rates alone.
			name: "fractional rate is preserved",
			mode: DeviceMode{Format: "dshow", Width: 720, Height: 480,
				MinFPS: 29.97, MaxFPS: 29.97, PixelFormat: "yuyv422"},
			want: "video_size=720x480,framerate=29.97,pixel_format=yuyv422",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mode.Options(); got != tc.want {
				t.Errorf("Options() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeviceModeString covers the human-facing rendering, which is what a
// device picker shows and what makes a log comparable with the "Supported
// modes" list FFmpeg prints.
func TestDeviceModeString(t *testing.T) {
	cases := []struct {
		mode DeviceMode
		want string
	}{
		{DeviceMode{Width: 1920, Height: 1080, MinFPS: 60, MaxFPS: 60, PixelFormat: "nv12"}, "1920x1080@60fps nv12"},
		{DeviceMode{Width: 1920, Height: 1080, MinFPS: 1, MaxFPS: 60}, "1920x1080@[1 60]fps"},
		{DeviceMode{Width: 640, Height: 480}, "640x480"},
		{DeviceMode{Width: 1280, Height: 720, MinFPS: 30, MaxFPS: 30, Codec: "mjpeg"}, "1280x720@30fps mjpeg"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// TestDeviceModesErrorCodes pins the bridge's GOFFMPEG_DEVICE_MODES_* codes to
// the errors callers match on, both directions — that the named codes reach
// their sentinels, and that a generic failure reaches neither. It runs
// everywhere, which matters because most of these codes need hardware, or a
// specific platform, to produce for real.
func TestDeviceModesErrorCodes(t *testing.T) {
	cases := []struct {
		rc   int
		want error
	}{
		{-3, ErrDeviceModesUnsupported},
		{-5, ErrDeviceNotFound},
		{-1, nil}, // query ran and failed
		{-2, nil}, // unknown format
		{-4, nil}, // built without libavdevice
	}
	for _, tc := range cases {
		err := deviceModesError("v4l2", "/dev/video0", tc.rc)
		if err == nil {
			t.Errorf("deviceModesError(%d) = nil, want an error", tc.rc)
			continue
		}
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("deviceModesError(%d) = %v, want errors.Is(..., %v)", tc.rc, err, tc.want)
			}
			continue
		}
		if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrDeviceModesUnsupported) {
			t.Errorf("deviceModesError(%d) = %v, must not match a sentinel", tc.rc, err)
		}
	}
}

// TestOpenDeviceErrorCodes covers the other half of #5: a failed open must say
// more than -1. The AVERROR is reachable with errors.As rather than only
// legible in a formatted string.
func TestOpenDeviceErrorCodes(t *testing.T) {
	// -16 is EBUSY, which is what a v4l2 node held by another process gives.
	err := openDeviceError("v4l2", "/dev/video0", -1, -16)
	var averr AVError
	if !errors.As(err, &averr) {
		t.Fatalf("errors.As could not recover an AVError from %v", err)
	}
	if int(averr) != -16 {
		t.Errorf("recovered %d, want -16", int(averr))
	}

	// An older purego libgoffmpeg reports no AVERROR at all. That must still
	// produce a usable error rather than claim AVError(0).
	old := openDeviceError("v4l2", "/dev/video0", -1, 0)
	if old == nil {
		t.Fatal("no error for a failed open with no AVERROR")
	}
	if errors.As(old, &averr) {
		t.Errorf("fabricated an AVError (%d) where the bridge reported none", int(averr))
	}

	// The bridge's own codes keep their own meanings.
	if got := openDeviceError("nope", "0", -3, 0).Error(); !strings.Contains(got, "unknown input format") {
		t.Errorf("rc=-3 gave %q, want it to name the unknown format", got)
	}
}

// TestListDeviceModesUnknownFormat is the one query every platform can run: a
// format libavdevice has never heard of is an error, and specifically not the
// "no query for this format" answer, which would send a caller looking for a
// backend to write rather than at their typo.
func TestListDeviceModesUnknownFormat(t *testing.T) {
	_, err := ListDeviceModes("definitely-not-a-real-format", "0")
	if err == nil {
		t.Fatal("expected an error for an unknown input format")
	}
	if errors.Is(err, ErrDeviceModesUnavailable) {
		t.Skipf("backend cannot query modes: %v", err)
	}
	if errors.Is(err, ErrDeviceModesUnsupported) {
		t.Errorf("unknown format reported as unsupported: %v", err)
	}
}

// TestDeviceModesRoundTrip is the acceptance test for #5 against real
// hardware: a mode the device advertises should open with the options that
// mode renders, because enumeration that cannot be acted on is the bug the
// issue was about wearing a different hat.
//
// What it can assert is narrower than that, though, and the first version got
// this wrong in both directions. Whether a camera opens *at all* is machine
// state — a macOS LaunchAgent has no camera access, and a camera passed
// through to a VM can block on open (both seen on former CI machines) — so a refused
// open is not by itself evidence of anything. Worse, a blocked open cannot be
// cancelled and keeps the device, so one such attempt turns every later mode
// into EBUSY and manufactures a pile of failures from a single machine fact.
//
// So the failure it reports is the specific one it can attribute: a mode
// refused with EINVAL, which is what a wrong option *name* produces —
// pixel_format=mjpeg on dshow, the real bug found while writing this. A device
// that opens nothing is a skip, and contention after the first success is
// logged rather than failed.
//
// The gap that leaves: avfoundation collapses every open failure to EIO, so a
// bad option there is indistinguishable from an absent camera and this test
// cannot catch it. v4l2 and dshow do report EINVAL, and the local development
// loop covers macOS.
func TestDeviceModesRoundTrip(t *testing.T) {
	format, url := captureTarget(t)

	modes, err := ListDeviceModes(format, url)
	if errors.Is(err, ErrDeviceModesUnavailable) || errors.Is(err, ErrDeviceModesUnsupported) {
		modesVerdict(t, "SKIPPED — %s has no mode query here: %v", format, err)
		t.Skipf("no mode query for %s: %v", format, err)
	}
	// On macOS captureTarget names device "0" without enumerating, so a host
	// with no camera at all (a hosted CI runner) lands here rather than in an
	// enumeration skip. No device is a machine fact, not a defect.
	if errors.Is(err, ErrDeviceNotFound) {
		modesVerdict(t, "SKIPPED — %s %q names no capture device on this host; round trip not exercised", format, url)
		t.Skipf("no capture device %q for %s on this host", url, format)
	}
	if err != nil {
		t.Fatalf("ListDeviceModes(%s, %s): %v", format, url, err)
	}
	if len(modes) == 0 {
		modesVerdict(t, "SKIPPED — %s %q advertised no modes; round trip not exercised", format, url)
		t.Skip("device advertised no modes")
	}

	// Bounded: a device can advertise dozens and each open costs an
	// acquisition. Four is enough to cover a raw and a compressed mode, which
	// is where the option name differs.
	const maxModes = 4
	var opened, refused int

	for i, m := range modes {
		if i >= maxModes {
			break
		}
		opts := m.Options()
		dec, err := openWithinOpts(t, format, url, opts, openDeviceTimeout)
		if err == nil {
			opened++
			info := dec.VideoInfo()
			t.Logf("opened  %-46s -> %dx%d", opts, info.Width, info.Height)
			dec.Close()
			continue
		}
		refused++

		// EINVAL is the signature of an option this backend will not parse,
		// which is a fault in what the mode rendered rather than in the
		// machine. Everything else — a timeout, EBUSY, avfoundation's
		// catch-all EIO — is the device being unavailable.
		var averr AVError
		if errors.As(err, &averr) && int(averr) == -22 {
			t.Errorf("%s reported mode %s but rejected %q as invalid: %v\n"+
				"this is #5 again — the option name or its value is not one "+
				"this backend accepts",
				format, m, opts, err)
			continue
		}

		t.Logf("skipped %-46s (device unavailable: %v)", opts, err)
		// A blocked open still holds the device, so every further attempt
		// would report EBUSY and say nothing about this code.
		if strings.Contains(err.Error(), "still blocked") {
			t.Logf("giving up on further modes: the blocked open still holds %q", url)
			break
		}
	}

	if opened == 0 {
		modesVerdict(t, "SKIPPED — %s %q: %d mode(s) advertised, none openable here; round trip not exercised",
			format, url, len(modes))
		t.Skipf("%s %q advertised %d mode(s), none currently openable", format, url, len(modes))
	}
	modesVerdict(t, "PASSED — %s %q: %d mode(s) advertised, %d opened with their own options, %d unavailable",
		format, url, len(modes), opened, refused)
}

// TestListDeviceModesNoSuchDevice checks the selector-not-found path on the
// one platform where a definitely-absent selector is cheap to name. v4l2 takes
// a path, so a node that does not exist is unambiguous; avfoundation's
// positional selector and dshow's names have no equivalent.
func TestListDeviceModesNoSuchDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a device selector that is definitely absent; v4l2-only")
	}
	_, err := ListDeviceModes("v4l2", "/dev/video-does-not-exist")
	if errors.Is(err, ErrDeviceModesUnavailable) {
		t.Skipf("backend cannot query modes: %v", err)
	}
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("ListDeviceModes on an absent node = %v, want ErrDeviceNotFound", err)
	}
}
