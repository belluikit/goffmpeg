//go:build cgo && !goffmpeg_novideo

package goffmpeg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"math"
	"os"
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

// androidH264 reports whether name is MediaCodec decoding H.264 on Android:
// the hardware decoder, or Android's own software decoder, which the bridge
// opens by name for a stream the hardware refuses (#77 decision B).
func androidH264(name string) bool {
	return name == "h264_mediacodec" || name == "h264_mediacodec (c2.android.avc.decoder)"
}

// sameAsReference fails unless got's planes are h264Reference's for path: the
// check for a build with no software h264 to compare with (#77).
func sameAsReference(t *testing.T, what, path string, got planes) {
	t.Helper()
	ref, ok := h264Reference[path]
	if !ok {
		t.Fatalf("no h264Reference for %s", path)
	}
	h := sha256.New()
	for _, f := range got.frames {
		h.Write(f)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if len(got.frames) != ref.frames || sum != ref.planes || len(got.infos) == 0 || got.infos[0] != ref.info {
		t.Errorf("%s: %d frames, planes %s, %+v; software h264 gives %d, %s, %+v", what, len(got.frames), sum, got.infos, ref.frames, ref.planes, ref.info)
		return
	}
	t.Logf("%s: %d frames from %q, the same planes as software h264's", what, len(got.frames), got.decoder)
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
	if !softwareH264() {
		// The phones (#77): their own decoder is the only one.
		hw := hwPlanes(t, "testdata/sample.mp4", "")
		sameAsReference(t, "default", "testdata/sample.mp4", hw)
		requireHardware(t, decoded{decoder: hw.decoder, last: hw.decoder})
		return
	}
	sw := hwDecodeAll(t, "testdata/sample.mp4", "0")
	if sw.decoder != "h264" {
		t.Fatalf("GOFFMPEG_HWDECODE=0 decoded with %q, want h264", sw.decoder)
	}
	// force: on Android sample.mp4 (320x240) is below MediaCodec's size
	// threshold, and this test is about the hardware path's frames.
	hw := hwDecodeAll(t, "testdata/sample.mp4", "force")
	sameFrames(t, "GOFFMPEG_HWDECODE=force", hw, sw)
	requireHardware(t, hw)
}

// hwDecodeUntilError is hwDecodeAll for a stream that may end in an error:
// it returns the frames decoded before it, and the error.
func hwDecodeUntilError(t *testing.T, path, mode string) (decoded, error) {
	t.Helper()
	t.Setenv("GOFFMPEG_HWDECODE", mode)
	dec, err := Open(path)
	if err != nil {
		return decoded{}, err
	}
	defer dec.Close()
	info := dec.VideoInfo()
	var out decoded
	for i := 0; i < 5000; i++ {
		mt, err := dec.Next()
		if err != nil {
			return out, err
		}
		if mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeVideo {
			continue
		}
		rgba := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
		if err := dec.GetRGBA(rgba); err != nil {
			return out, err
		}
		name := dec.(interface{ videoDecoder() string }).videoDecoder()
		if out.frames == nil {
			out.decoder = name
		}
		out.last = name
		out.frames = append(out.frames, rgba)
	}
	return out, nil
}

// A stream MediaCodec cannot decode (High 4:4:4) is left to the software
// decoder. By default the profile check keeps MediaCodec from it. With
// "force", MediaCodec opens it and the software decoder takes over with the
// packets MediaCodec was sent: every frame still arrives. VideoToolbox either
// decodes it or hands it to software itself; the frames are the same either
// way.
//
// MediaCodec gives up in one of two ways. Qualcomm's (OnePlus Pad 2) fails at
// the first packet. MediaTek's (moto g power 2024) takes every packet and
// returns nothing, with no error (#81). The bridge replaces a silent one at
// the end of the input, which the 30-frame clip reaches first, or after
// HW_PENDING_MAX (64) packets, which the 100-frame clip reaches first. With
// only the first, the long clip would lose its frames past the 64th.
//
// Samsung's Exynos decoder (Pixel 9a) does not give up: it decodes High 4:4:4
// to 4:2:0 frames and leaves some out, 26 of 30 and 91 of 100, the same each
// run (#86). That is what "force" asks for, the hardware with no profile
// check, and there is no fallback to check, so the forced half skips there.
func TestHardwareDecodeRefusedProfile(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	for _, c := range []struct {
		path   string
		frames int
	}{
		{"testdata/sample_444.mp4", 30},       // H.264 High 4:4:4, yuv444p, 320x240
		{"testdata/sample_444_long.mp4", 100}, // the same, past HW_PENDING_MAX
	} {
		t.Run(c.path, func(t *testing.T) {
			if !softwareH264() {
				// Android with no FFmpeg h264 (#77): a MediaCodec that
				// fails before its first frame has nothing behind it, so
				// Next ends in the error decision A asks for, naming it.
				// One that decodes High 4:4:4 (the Pixel's Exynos) just
				// does.
				got, err := hwDecodeUntilError(t, c.path, "")
				t.Logf("default: %d frames from %q, then %v", len(got.frames), got.decoder, err)
				switch {
				case err == nil && len(got.frames) > 0 && androidH264(got.decoder):
					t.Logf("this device's MediaCodec decodes High 4:4:4: %d of %d frames", len(got.frames), c.frames)
				case errors.Is(err, ErrNoDecoder) && strings.Contains(err.Error(), "h264_mediacodec") && len(got.frames) == 0:
				default:
					t.Errorf("%d frames from %q, then %v; want frames from MediaCodec, or none and an error wrapping ErrNoDecoder that names h264_mediacodec",
						len(got.frames), got.decoder, err)
				}
				return
			}
			sw := hwDecodeAll(t, c.path, "0")
			if len(sw.frames) != c.frames {
				t.Fatalf("software decoded %d frames of %s, want %d", len(sw.frames), c.path, c.frames)
			}
			def := hwDecodeAll(t, c.path, "1")
			sameFrames(t, "GOFFMPEG_HWDECODE=1", def, sw)
			if def.decoder != "h264" || def.last != "h264" {
				t.Errorf("GOFFMPEG_HWDECODE=1: decoded with %q to %q, want h264 (the profile check)", def.decoder, def.last)
			}
			forced := hwDecodeAll(t, c.path, "force")
			if runtime.GOOS == "android" && forced.decoder == hwName() && forced.last == hwName() {
				t.Skipf("GOFFMPEG_HWDECODE=force: this device's MediaCodec decoded High 4:4:4 itself (%d frames, software %d), so there is no fallback to check",
					len(forced.frames), len(sw.frames))
			}
			sameFrames(t, "GOFFMPEG_HWDECODE=force", forced, sw)
			if runtime.GOOS == "android" {
				// MediaCodec opens a 320x240 High 4:4:4 stream (it refuses
				// to configure a smaller one), so this is the fallback
				// that sends the kept packets again.
				const fell = "h264 (after h264_mediacodec failed)"
				if forced.decoder != fell || forced.last != fell {
					t.Errorf("force: decoded with %q to %q, want %q", forced.decoder, forced.last, fell)
				}
			}
		})
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
	if !softwareH264() {
		// The phones (#77): held to software h264's recorded frame.
		t.Setenv("GOFFMPEG_HWDECODE", "")
		pos, sum, name := seekPlanes(t)
		t.Logf("after Seek(2): %q at %.3f s", name, pos)
		if pos != h264SeekReference.position || sum != h264SeekReference.planes {
			t.Errorf("after Seek(2): at %v, planes %s; software h264 gives %v, %s",
				pos, sum, h264SeekReference.position, h264SeekReference.planes)
		}
		return
	}
	swImg, swPos, _ := after("0")
	hwImg, hwPos, name := after("force") // as in TestHardwareDecodeMatchesSoftware
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
		// 32x24, which goes to Android's software decoder on every phone
		// (#101); checked only where there is no software h264.
		"testdata/sample_video_cover.mp4",
	} {
		if path == "testdata/sample_video_cover.mp4" && softwareH264() {
			continue
		}
		if !softwareH264() {
			// The phones (#77): held to software h264's recorded planes.
			// MediaCodec's hardware decoder on the OnePlus Pad 2 refuses
			// 64x64, and Android's software decoder decodes it (decision B).
			hw := hwPlanes(t, path, "")
			sameAsReference(t, "default", path, hw)
			if runtime.GOOS == "android" && !androidH264(hw.decoder) {
				t.Errorf("%s: decoded by %q, want MediaCodec", path, hw.decoder)
			}
			continue
		}
		sw := hwPlanes(t, path, "0")
		hw := hwPlanes(t, path, "force") // as in TestHardwareDecodeMatchesSoftware
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

// On Android, MediaCodec decodes VP9 and AV1 only larger than 1280x720 (#65):
// below that, it costs more than the software decoding it replaces. H.264 has
// no software decoder there any more (#77 decision B), so it goes to MediaCodec
// at every size. VideoToolbox has no threshold. Unset GOFFMPEG_HWDECODE is the
// default; "1" applies the same rules, so it is checked too.
func TestHardwareDecodeSizeThreshold(t *testing.T) {
	if runtime.GOOS != "android" && runtime.GOOS != "ios" && !(runtime.GOOS == "darwin" && !onVM()) {
		t.Skip("no default hardware path to check on " + runtime.GOOS)
	}
	for _, c := range []struct {
		path  string
		large bool
	}{
		{"testdata/sample.mp4", false},     // 320x240
		{"testdata/sample_1080.mp4", true}, // 1920x1080
	} {
		modes := []string{"1"}
		if runtime.GOOS != "darwin" {
			modes = append(modes, "") // unset: the default, which is software on macOS
		}
		for _, mode := range modes {
			// The bridge reads an empty GOFFMPEG_HWDECODE as unset.
			got := hwDecodeAll(t, c.path, mode)
			want := hwName()
			if runtime.GOOS == "android" && !c.large && softwareH264() {
				want = "h264"
			}
			t.Logf("%s, GOFFMPEG_HWDECODE=%q: %d frames from %q", c.path, mode, len(got.frames), got.decoder)
			if got.decoder != want || got.last != want {
				t.Errorf("%s, GOFFMPEG_HWDECODE=%q: decoded with %q to %q, want %q", c.path, mode, got.decoder, got.last, want)
			}
		}
	}
}

// VP9 and AV1 through the hardware path (#78): with GOFFMPEG_HWDECODE=force
// the frames are software's (FFmpeg's vp9, and dav1d for AV1), byte for byte,
// whichever decoder the platform has. The hardware varies by device, the iPad
// (10th generation) has none for either, so this logs which decoder ran
// rather than requiring one.
func TestHardwareDecodeRoyaltyFree(t *testing.T) {
	if !hwPlatform() {
		t.Skip("no hardware decoder in the bridge on " + runtime.GOOS)
	}
	for _, path := range []string{"testdata/sample_vp9.webm", "testdata/sample_av1.mp4"} {
		sw := hwDecodeAll(t, path, "0")
		hw := hwDecodeAll(t, path, "force")
		sameFrames(t, path+" GOFFMPEG_HWDECODE=force", hw, sw)
		t.Logf("%s: software %q, hardware path %q", path, sw.decoder, hw.decoder)
	}
}
