//go:build cgo && !goffmpeg_novideo

package goffmpeg

import (
	"image"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The hardware decoder (#65). These tests set GOFFMPEG_HWDECODE, which the
// bridge reads with getenv at each open, so they need cgo: without it
// os.Setenv does not reach the C environment.

// hwPlatform reports whether this platform has a hardware path in the bridge.
func hwPlatform() bool {
	switch runtime.GOOS {
	case "android", "ios", "darwin":
		return true
	}
	return false
}

// hwName is the decoder name the hardware path reports on this platform.
func hwName() string {
	if runtime.GOOS == "android" {
		return "h264_mediacodec"
	}
	return "h264 (videotoolbox)"
}

type decoded struct {
	frames  []*image.RGBA
	decoder string // what decoded the first frame
	last    string // what decoded the last frame
}

// hwDecodeAll opens path with GOFFMPEG_HWDECODE=mode and converts every video
// frame, failing the test on any error: a hardware frame whose size differs
// from VideoInfo would show here as ErrFrameSizeChanged.
func hwDecodeAll(t *testing.T, path, mode string) decoded {
	t.Helper()
	t.Setenv("GOFFMPEG_HWDECODE", mode)
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) with GOFFMPEG_HWDECODE=%s: %v", path, mode, err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	var out decoded
	for i := 0; i < 5000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("GOFFMPEG_HWDECODE=%s: Next: %v", mode, err)
		}
		if mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeVideo {
			continue
		}
		rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
		if err := dec.GetRGBA(rgba); err != nil {
			t.Fatalf("GOFFMPEG_HWDECODE=%s: video frame %d: GetRGBA: %v", mode, len(out.frames), err)
		}
		name := dec.(interface{ videoDecoder() string }).videoDecoder()
		if out.frames == nil {
			out.decoder = name
		}
		out.last = name
		out.frames = append(out.frames, rgba)
	}
	return out
}

// psnr is the peak signal-to-noise ratio of b against a, in dB; +Inf when
// they are equal.
func psnr(a, b []byte) float64 {
	var se float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		se += d * d
	}
	if se == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/(se/float64(len(a))))
}

// onVM reports a macOS virtual machine, such as a GitHub runner.
func onVM() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("sysctl", "-n", "kern.hv_vmm_present").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// checkFrame fails unless got is want byte for byte, and returns the PSNR.
// A conforming H.264 decoder is bit-exact, and every hardware decoder measured
// is: VideoToolbox on an M-series Mac and on an iPad (10th generation),
// MediaCodec on the OPD2403. VideoToolbox inside GitHub's macOS VM is not: its
// frames are 29.0 dB from software's (runs 43169957531 and 43171074560),
// where the iPad gives the same bytes for the same clips. In a VM the PSNR is
// only logged, and the tests check what the VM can answer: frame counts,
// positions and colour info.
func checkFrame(t *testing.T, what string, got, want []byte) float64 {
	t.Helper()
	p := psnr(got, want)
	switch {
	case len(got) != len(want):
		t.Errorf("%s: %d bytes, software %d", what, len(got), len(want))
	case onVM():
	case !math.IsInf(p, 1):
		t.Errorf("%s: differs from software's (%.1f dB), want the same bytes", what, p)
	}
	return p
}

// sameFrames fails unless got has as many frames as want and each is the
// same picture (checkFrame).
func sameFrames(t *testing.T, what string, got, want decoded) {
	t.Helper()
	if len(got.frames) != len(want.frames) {
		t.Fatalf("%s: %d video frames, software %d", what, len(got.frames), len(want.frames))
	}
	worst := math.Inf(1)
	for i := range got.frames {
		worst = math.Min(worst, checkFrame(t, what, got.frames[i].Pix, want.frames[i].Pix))
	}
	t.Logf("%s: %d frames from %q (last %q); worst PSNR against software %.1f dB (VM: %v)", what, len(got.frames), got.decoder, got.last, worst, onVM())
}

// requireHardware fails unless the hardware decoded the frames. A CI runner
// may have no hardware decoder (a macOS VM), so there it skips, after the
// frames have been checked.
func requireHardware(t *testing.T, got decoded) {
	t.Helper()
	if got.decoder == hwName() && got.last == hwName() {
		return
	}
	msg := "the hardware decoded no frame: first " + got.decoder + ", last " + got.last + ", want " + hwName()
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip(msg + " (CI runner without VideoToolbox?)")
	}
	t.Error(msg)
}

// The hardware path gives the same frames as software decoding, every one of
// them: none lost at the start or in the drain at the end.
func TestHardwareDecodeMatchesSoftware(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	sw := hwDecodeAll(t, "testdata/sample.mp4", "0")
	if sw.decoder != "h264" {
		t.Fatalf("GOFFMPEG_HWDECODE=0 decoded with %q, want h264", sw.decoder)
	}
	hw := hwDecodeAll(t, "testdata/sample.mp4", "1")
	sameFrames(t, "GOFFMPEG_HWDECODE=1", hw, sw)
	requireHardware(t, hw)
}

// A stream MediaCodec cannot decode (High 4:4:4) is left to the software
// decoder. By default the profile check keeps MediaCodec from it. With
// "force", MediaCodec opens it and fails before its first frame, and the
// software decoder takes over with the packets MediaCodec was sent: every
// frame still arrives. VideoToolbox either decodes it or hands it to software
// itself; the frames are the same either way.
func TestHardwareDecodeRefusedProfile(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	const path = "testdata/sample_444.mp4" // H.264 High 4:4:4, yuv444p, 320x240, 30 frames
	sw := hwDecodeAll(t, path, "0")
	if len(sw.frames) != 30 {
		t.Fatalf("software decoded %d frames of %s, want 30", len(sw.frames), path)
	}
	def := hwDecodeAll(t, path, "1")
	sameFrames(t, "GOFFMPEG_HWDECODE=1", def, sw)
	forced := hwDecodeAll(t, path, "force")
	sameFrames(t, "GOFFMPEG_HWDECODE=force", forced, sw)
	if def.decoder != "h264" || def.last != "h264" {
		t.Errorf("GOFFMPEG_HWDECODE=1: decoded with %q to %q, want h264 (the profile check)", def.decoder, def.last)
	}
	if runtime.GOOS == "android" {
		// MediaCodec opens a 320x240 High 4:4:4 stream and fails at the
		// first frame (it refuses to configure a smaller one), so this is
		// the fallback that sends the kept packets again.
		const fell = "h264 (after h264_mediacodec failed)"
		if forced.decoder != fell || forced.last != fell {
			t.Errorf("force: decoded with %q to %q, want %q", forced.decoder, forced.last, fell)
		}
	}
}

// Seeking with the hardware decoder lands where software decoding does.
func TestHardwareDecodeSeek(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	after := func(mode string) (*image.RGBA, float64, string) {
		t.Setenv("GOFFMPEG_HWDECODE", mode)
		dec, err := Open("testdata/sample.mp4")
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		info := dec.VideoInfo()
		rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
		frames := 0
		next := func() {
			for {
				mt, err := dec.Next()
				if err != nil || mt == MediaTypeNone {
					t.Fatalf("GOFFMPEG_HWDECODE=%s: no video frame: %v, %v", mode, mt, err)
				}
				if mt == MediaTypeVideo {
					if err := dec.GetRGBA(rgba); err != nil {
						t.Fatalf("GOFFMPEG_HWDECODE=%s: GetRGBA: %v", mode, err)
					}
					frames++
					return
				}
			}
		}
		for frames < 5 {
			next()
		}
		if err := dec.Seek(2); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		next()
		return rgba, dec.Position(), dec.(interface{ videoDecoder() string }).videoDecoder()
	}
	swImg, swPos, _ := after("0")
	hwImg, hwPos, name := after("1")
	p := checkFrame(t, "first frame after Seek(2)", hwImg.Pix, swImg.Pix)
	t.Logf("after Seek(2): software at %.3f s, %q at %.3f s, %.1f dB", swPos, name, hwPos, p)
	if hwPos != swPos {
		t.Errorf("position after Seek(2): %.3f with the hardware, %.3f in software", hwPos, swPos)
	}
	if strings.TrimSpace(name) == "" {
		t.Error("no decoder name after the seek")
	}
}

type planes struct {
	frames  [][]byte
	infos   []YCbCrInfo
	decoder string
}

// hwPlanes opens path with GOFFMPEG_HWDECODE=mode and reads every video frame
// with GetYCbCr.
func hwPlanes(t *testing.T, path, mode string) planes {
	t.Helper()
	t.Setenv("GOFFMPEG_HWDECODE", mode)
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	var out planes
	for i := 0; i < 5000; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("GOFFMPEG_HWDECODE=%s %s: Next: %v", mode, path, err)
		}
		if mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeVideo {
			continue
		}
		img := image.NewYCbCr(image.Rect(0, 0, info.Width, info.Height), image.YCbCrSubsampleRatio420)
		yi, err := dec.(YCbCrDecoder).GetYCbCr(img)
		if err != nil {
			t.Fatalf("GOFFMPEG_HWDECODE=%s %s: frame %d: GetYCbCr: %v", mode, path, len(out.frames), err)
		}
		if out.frames == nil {
			out.decoder = dec.(interface{ videoDecoder() string }).videoDecoder()
		}
		out.frames = append(out.frames, packed(img))
		out.infos = append(out.infos, yi)
	}
	return out
}

// GetYCbCr hands out a hardware decoder's frames too: they are nv12, whose
// interleaved chroma is split into Cb and Cr, and they come out byte for byte
// as software decoding gives them, with the same matrix and range.
func TestHardwareDecodeYCbCr(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	for _, path := range []string{
		"testdata/sample.mp4",           // 320x240, untagged
		"testdata/yuv/bt709_tv.mp4",     // 64x64, BT.709 limited range
		"testdata/yuv/smpte170m_pc.mp4", // 64x64, BT.601 full range (yuvj420p)
		// MediaCodec refuses 64x64, so the tagged clips again at 320x240.
		"testdata/yuv/bt709_tv_320.mp4",
		"testdata/yuv/smpte170m_pc_320.mp4",
	} {
		sw := hwPlanes(t, path, "0")
		hw := hwPlanes(t, path, "1")
		t.Logf("%s: %d frames, hardware run decoded by %q", path, len(hw.frames), hw.decoder)
		if len(hw.frames) != len(sw.frames) {
			t.Errorf("%s: %d frames with the hardware, %d in software", path, len(hw.frames), len(sw.frames))
			continue
		}
		worst := math.Inf(1)
		for i := range sw.frames {
			worst = math.Min(worst, checkFrame(t, path+": planes", hw.frames[i], sw.frames[i]))
			if hw.infos[i] != sw.infos[i] {
				t.Errorf("%s: frame %d: %+v with the hardware, %+v in software", path, i, hw.infos[i], sw.infos[i])
				break
			}
		}
		t.Logf("%s: worst PSNR of the planes against software %.1f dB (VM: %v)", path, worst, onVM())
		if path == "testdata/sample.mp4" {
			requireHardware(t, decoded{decoder: hw.decoder, last: hw.decoder})
		}
	}
}
