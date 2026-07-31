package goffmpeg

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestNextResultClassifies pins the four outcomes Next has to keep distinct.
//
// The regression in #4 was that only AVERROR_EOF was recognised as "not a
// failure", so a device's EAGAIN — the ordinary state between frames on a live
// source — landed in the error arm and ended the capture. What makes it worth a
// table rather than one assertion is that "no data yet", "end of stream" and
// "the device is gone" all arrive as a single int and are trivial to conflate.
func TestNextResultClassifies(t *testing.T) {
	eagain := -errnoEAGAIN

	cases := []struct {
		name string
		res  int
		want MediaType
		err  error // sentinel to match with errors.Is, or nil
	}{
		{"video frame", 1, MediaTypeVideo, nil},
		{"audio frame", 2, MediaTypeAudio, nil},
		{"clean end of stream", 0, MediaTypeNone, nil},
		{"bridge says no data yet", goffmpegAgain, MediaTypeNone, ErrAgain},
		// The bare AVERROR arm: purego against a libgoffmpeg older than the
		// bridge fix returns the raw code, and it must classify the same.
		{"raw EAGAIN from an older bridge", eagain, MediaTypeNone, ErrAgain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mt, err := nextResult(tc.res)
			if mt != tc.want {
				t.Errorf("nextResult(%d) media = %v, want %v", tc.res, mt, tc.want)
			}
			switch {
			case tc.err == nil && err != nil:
				t.Errorf("nextResult(%d) err = %v, want nil", tc.res, err)
			case tc.err != nil && !errors.Is(err, tc.err):
				t.Errorf("nextResult(%d) err = %v, want errors.Is(..., %v)", tc.res, err, tc.err)
			}
		})
	}
}

// TestNextResultReportsGenuineFailure is the other half of the bargain: making
// EAGAIN benign must not make everything benign. EBUSY is the case that matters
// — it is what a v4l2 node returns when something else holds it (#14), and a
// caller that retried it forever would hang instead of reporting a lost device.
func TestNextResultReportsGenuineFailure(t *testing.T) {
	const ebusy = -16

	mt, err := nextResult(ebusy)
	if mt != MediaTypeNone {
		t.Errorf("media = %v, want MediaTypeNone", mt)
	}
	if err == nil {
		t.Fatal("a genuine failure reported no error")
	}
	if errors.Is(err, ErrAgain) {
		t.Error("EBUSY matched ErrAgain — a lost device would be retried forever")
	}

	// The point of the typed error: classify without parsing the message.
	var averr AVError
	if !errors.As(err, &averr) {
		t.Fatalf("errors.As could not recover an AVError from %v", err)
	}
	if int(averr) != ebusy {
		t.Errorf("recovered code %d, want %d", int(averr), ebusy)
	}
}

// TestAVErrorMessageNamesTheCode guards the readability half of #4: the old
// message was a bare "decoder error: -35", which is why the consumer had
// nothing better than a timeout to work with. An errno-derived code should say
// what it was.
func TestAVErrorMessageNamesTheCode(t *testing.T) {
	msg := AVError(-16).Error()
	if !strings.Contains(msg, "-16") {
		t.Errorf("message %q does not contain the code", msg)
	}
	// The wording comes from the platform, so assert only that something
	// beyond the number is there — and only where the platform will say.
	// Windows deliberately declines: its syscall.Errno is a Win32 code, not
	// the CRT errno FFmpeg negates, so naming -16 there would be wrong
	// rather than merely unhelpful.
	if errnoText(16) != "" {
		if strings.TrimSpace(strings.SplitN(msg, "-16", 2)[1]) == "" {
			t.Errorf("message %q names the code but not its meaning", msg)
		}
	}

	// A FFERRTAG-packed constant is far outside the errno range and must not
	// be run through syscall.Errno, which would invent a meaning for it.
	if msg := avErrorEOF.Error(); !strings.Contains(msg, "end of file") {
		t.Errorf("AVERROR_EOF message = %q, want it named as end of file", msg)
	}
}

// TestErrAgainIsDistinctFromEndOfStream is the distinction a capture loop turns
// on, and the one the old API could not express: both outcomes carry
// MediaTypeNone, so if ErrAgain were reported as a clean end (nil error) a
// caller would stop on the first gap between frames instead of waiting.
func TestErrAgainIsDistinctFromEndOfStream(t *testing.T) {
	_, eos := nextResult(0)
	if eos != nil {
		t.Fatalf("end of stream reported an error: %v", eos)
	}
	_, again := nextResult(goffmpegAgain)
	if again == nil {
		t.Fatal("ErrAgain reported as a clean end of stream — a capture would stop between frames")
	}
}

// TestCaptureSurvivesEmptyReads is the acceptance test for #4 against real
// hardware: a live device must keep delivering frames across the gaps between
// them, rather than ending the moment one read comes back empty.
//
// The measured failure was two frames in about 30ms on macOS/avfoundation, so
// the bar here is deliberately low — a handful of frames is already far past
// where the bug bit. It skips rather than fails without a camera, on the same
// reasoning as TestOpenDeviceV4L2: whether a camera opens is machine state.
func TestCaptureSurvivesEmptyReads(t *testing.T) {
	format, url := captureTarget(t)

	dec, opts, err := openCapture(t, format, url)
	if err != nil {
		t.Skipf("%s %q present but not currently openable: %v", format, url, err)
	}
	defer dec.Close()
	t.Logf("opened %s %q with options %q", format, url, opts)

	const wantFrames = 5
	deadline := time.Now().Add(10 * time.Second)

	var frames, empties int
	for frames < wantFrames && time.Now().Before(deadline) {
		mt, err := dec.Next()
		switch {
		case errors.Is(err, ErrAgain):
			// The whole point: not a failure, and not the end.
			empties++
			time.Sleep(5 * time.Millisecond)
		case err != nil:
			t.Fatalf("capture died after %d frame(s) and %d empty read(s): %v\n"+
				"this is #4 if the error is EAGAIN — an empty read from a live "+
				"device is the state between frames, not the end of the stream",
				frames, empties, err)
		case mt == MediaTypeNone:
			t.Fatalf("live device reported end of stream after %d frame(s)", frames)
		default:
			frames++
		}
	}

	if frames < wantFrames {
		t.Fatalf("got %d frame(s) in 10s (%d empty reads), want %d — the device "+
			"is not delivering", frames, empties, wantFrames)
	}
	t.Logf("%s %q: %d frames, %d empty reads tolerated", format, url, frames, empties)
}

// openCapture opens a camera, trying a few framerates before giving up, and
// reports which options worked.
//
// The retry is not incidental. avfoundation picks 29.97 when no framerate is
// given and refuses outright if the camera does not offer it — the machine this
// was written on only does 60 — so the default open fails on perfectly working
// hardware. FFmpeg prints the modes it would accept, but OpenDevice discards
// that and returns a bare -1 (#5), which is why this has to guess rather than
// read the supported list. The reporter's own repro for #4 passed framerate=60
// for the same reason.
//
// This is about reaching the read loop, which is what the test is actually
// about. Once #5 lands, the guessing can be replaced by the reported modes.
func openCapture(t *testing.T, format, url string) (IDecoder, string, error) {
	t.Helper()

	candidates := []string{"", "framerate=60", "framerate=30"}
	var lastErr error
	for _, opts := range candidates {
		dec, err := openWithinOpts(t, format, url, opts, openDeviceTimeout)
		if err == nil {
			return dec, opts, nil
		}
		lastErr = err
		t.Logf("open with options %q failed: %v", opts, err)
	}
	return nil, "", lastErr
}

// captureTarget picks a camera for the running platform, or skips. The url
// shapes differ by design: avfoundation is positional and cannot enumerate,
// v4l2 takes a node path, and dshow needs a name that only enumeration knows —
// which is why ListInputSources exists.
func captureTarget(t *testing.T) (format, url string) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return "avfoundation", "0"
	case "linux":
		devs, err := ListInputSources("v4l2")
		skipIfNoEnumeration(t, err)
		if err != nil {
			t.Skipf("ListInputSources(v4l2): %v", err)
		}
		for _, d := range devs {
			// Metadata nodes enumerate like any other and block forever
			// when opened, so this filter is load-bearing.
			if d.HasMedia(MediaTypeVideo) {
				if _, err := os.Stat(d.Name); err == nil {
					return "v4l2", d.Name
				}
			}
		}
		t.Skip("no v4l2 video-capture devices on this host")
	case "windows":
		devs, err := ListInputSources("dshow")
		skipIfNoEnumeration(t, err)
		if err != nil {
			t.Skipf("ListInputSources(dshow): %v", err)
		}
		for _, d := range devs {
			if d.HasMedia(MediaTypeVideo) {
				return "dshow", "video=" + d.Name
			}
		}
		t.Skip("no dshow video-capture devices on this host")
	}
	t.Skipf("no capture backend wired for %s", runtime.GOOS)
	return "", ""
}
