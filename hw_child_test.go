//go:build !goffmpeg_novideo

package goffmpeg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// onVM reports a macOS virtual machine, such as a GitHub runner.
func onVM() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("sysctl", "-n", "kern.hv_vmm_present").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// The hardware decode path (#65), checked in a child process so it runs under
// purego too (#74). The bridge reads GOFFMPEG_HWDECODE with getenv, and under
// purego os.Setenv does not reach the C library's environment, which is why
// hw_decode_test.go is cgo-only. A child started with the variable in its
// environment has it from the start, under either backend.
const hwChildEnv = "GOFFMPEG_HW_CHILD"

// TestHardwareDecodeChild is the child: it decodes sample_video.mp4 through
// GetYCbCr and prints one line, the decoder of the last frame, the frame count
// and a hash of every frame's planes. sample_video.mp4 is sample.mp4's video
// stream, copied without its AAC audio (ffmpeg -map 0:v -c copy), so the
// child's decode is the video's alone. On Android AAC decodes on MediaCodec
// too (#77), and TestHardwareDecodeStall's child runs with no binder pool,
// where a Pixel 9a's AAC decoder stalls as its video decoder does, and with
// the audio Open did not return in 60 s.
func TestHardwareDecodeChild(t *testing.T) {
	if os.Getenv(hwChildEnv) == "" {
		t.Skip("run by TestHardwareDecodeInChild")
	}
	dec, err := Open("testdata/sample_video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	img := image.NewYCbCr(image.Rect(0, 0, info.Width, info.Height), image.YCbCrSubsampleRatio420)
	h := sha256.New()
	frames := 0
	for {
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
		if _, err := dec.(YCbCrDecoder).GetYCbCr(img); errors.Is(err, errYUVUnavailable) {
			fmt.Printf("HWCHILD skip=%q\n", err.Error())
			return
		} else if err != nil {
			t.Fatalf("frame %d: GetYCbCr: %v", frames, err)
		}
		h.Write(packed(img))
		frames++
	}
	name := dec.(interface{ videoDecoder() string }).videoDecoder()
	fmt.Printf("HWCHILD frames=%d sha256=%s decoder=%q\n", frames, hex.EncodeToString(h.Sum(nil)), name)
}

// TestHardwareDecodeInChild runs the child with the hardware forced on and off.
// The hardware run must have decoded on VideoToolbox, and its frames must be
// software's, byte for byte. In a VM VideoToolbox is not bit-exact
// (hw_decode_test.go), and only macOS has a purego bridge with a hardware
// path: goffmpeg-libs ships no libgoffmpeg for Android, and iOS runs no go
// test.
func TestHardwareDecodeInChild(t *testing.T) {
	if runtime.GOOS != "darwin" || onVM() {
		t.Skip("needs VideoToolbox on real hardware, and a purego bridge: macOS, not in a VM")
	}
	// run returns the child's frames and hash ("frames=100 sha256=…") and the
	// decoder it reported, or skips the test when the child could not run.
	run := func(mode string) (frames, decoder string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHardwareDecodeChild$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), hwChildEnv+"=1", "GOFFMPEG_HWDECODE="+mode)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("child with GOFFMPEG_HWDECODE=%s: %v\n%s", mode, err, out.String())
		}
		for _, line := range strings.Split(out.String(), "\n") {
			line, ok := strings.CutPrefix(line, "HWCHILD ")
			if !ok {
				continue
			}
			if reason, ok := strings.CutPrefix(line, "skip="); ok {
				t.Skip("the loaded libgoffmpeg cannot run this: " + reason)
			}
			frames, decoder, ok = strings.Cut(line, " decoder=")
			if !ok {
				t.Fatalf("child with GOFFMPEG_HWDECODE=%s printed %q", mode, line)
			}
			return frames, decoder
		}
		t.Fatalf("child with GOFFMPEG_HWDECODE=%s printed no result:\n%s", mode, out.String())
		return "", ""
	}
	swFrames, swDecoder := run("0")
	hwFrames, hwDecoder := run("force")
	t.Logf("software: %s decoder=%s", swFrames, swDecoder)
	t.Logf("hardware: %s decoder=%s", hwFrames, hwDecoder)
	if swDecoder != `"h264"` {
		t.Errorf("GOFFMPEG_HWDECODE=0 decoded with %s, want h264", swDecoder)
	}
	if hwDecoder == `""` {
		t.Skip("the loaded libgoffmpeg predates decoder_video_decoder (#65): rebuild it, or use a goffmpeg-libs release that has it")
	}
	if hwDecoder != `"h264 (videotoolbox)"` {
		t.Errorf("GOFFMPEG_HWDECODE=force decoded with %s, want h264 (videotoolbox)", hwDecoder)
	}
	if hwFrames != swFrames {
		t.Errorf("the hardware's frames are not software's: %s, software %s", hwFrames, swFrames)
	}
}
