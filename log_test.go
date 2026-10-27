package goffmpeg

import (
	"bytes"
	"image"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// FFmpeg writes its log to file descriptor 2 from C, where a Go test cannot
// capture it, so the decode runs in a child process whose stderr is read.
const logHelperEnv = "GOFFMPEG_LOG_HELPER_LEVEL"

// TestLogHelper is the child: decode the sample with a seek, at the level the
// parent asked for ("" leaves the package default).
func TestLogHelper(t *testing.T) {
	level, ok := os.LookupEnv(logHelperEnv)
	if !ok {
		t.Skip("run by TestLogLevel")
	}
	if level != "" {
		n, err := strconv.Atoi(level)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetLogLevel(LogLevel(n)); err != nil {
			t.Fatal(err)
		}
	}
	dec, err := Open("testdata/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	decode := func() {
		for i, video := 0, 0; i < 400 && video < 10; i++ {
			mt, err := dec.Next()
			if err != nil || mt == MediaTypeNone {
				return
			}
			switch mt {
			case MediaTypeVideo:
				video++
				_ = dec.GetRGBA(rgba)
			case MediaTypeAudio:
				_, _ = dec.GetAudio()
			}
		}
	}
	decode()
	_ = dec.Seek(3) // the AAC decoder warns after this one on the sample
	decode()
}

// The package starts FFmpeg at LogError, and SetLogLevel moves it (#19). Read
// back from FFmpeg itself, so it holds on every runner whatever the decode
// below happens to log there.
func TestDefaultLogLevel(t *testing.T) {
	if got := logLevel(); got != LogError {
		t.Fatalf("FFmpeg's level when the package loads: %d, want LogError (%d)", got, LogError)
	}
	if err := SetLogLevel(LogWarning); err != nil {
		t.Fatal(err)
	}
	defer SetLogLevel(LogError)
	if got := logLevel(); got != LogWarning {
		t.Errorf("after SetLogLevel(LogWarning): %d", got)
	}
}

// What reaches stderr. The LogDebug run is the control: FFmpeg logs while
// probing any file there, so an empty capture would mean the capture misses
// FFmpeg's log. The decode's own warning is machine-dependent: on a developer
// Mac it prints the AAC decoder's "Could not update timestamps for discarded
// samples" at LogInfo, while GitHub's macOS and Windows runners print nothing
// at LogInfo for it, so the default run is a full check only where it warns,
// and TestDefaultLogLevel covers the rest.
func TestLogLevel(t *testing.T) {
	if _, err := os.Stat("testdata/sample.mp4"); err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	run := func(level string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestLogHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), logHelperEnv+"="+level)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("helper at level %q: %v\nstdout: %s\nstderr: %s", level, err, out, stderr.String())
		}
		return stderr.String()
	}

	if got := run(strconv.Itoa(int(LogDebug))); got == "" {
		t.Error("nothing printed at LogDebug: the capture is not seeing FFmpeg's log")
	}
	if got := run(""); got != "" {
		t.Errorf("FFmpeg printed at the default level:\n%s", got)
	}
	if got := run(strconv.Itoa(int(LogQuiet))); got != "" {
		t.Errorf("FFmpeg printed at LogQuiet:\n%s", got)
	}
}
