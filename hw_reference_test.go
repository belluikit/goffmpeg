//go:build cgo && !goffmpeg_novideo

package goffmpeg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"os"
	"slices"
	"sort"
	"testing"
)

// softwareH264 reports whether the loaded FFmpeg has its software h264
// decoder. The phones' media sets have none (#77): H.264 decodes only on
// Apple's and Android's own decoders there, and the hardware tests compare
// those with h264Reference instead.
func softwareH264() bool {
	decoders, _ := builtComponents()
	return slices.Contains(decoders, "h264")
}

// h264Ref is what FFmpeg's software h264 decodes a clip to: the frame count,
// a hash of every frame's planes through GetYCbCr (packed), and GetYCbCr's
// colour information. The planes are the decoder's own output, the same on
// every CPU. GetRGBA's are not: libswscale converts with different SIMD code
// on x86 and on arm64, and the RGBA of the same planes differs between a
// linux_amd64 runner and an arm64 Mac (#100's first CI run).
type h264Ref struct {
	frames int
	planes string
	info   YCbCrInfo
}

// h264Reference is FFmpeg's software h264 on macOS (arm64), recorded so the
// phones, which have no software H.264 decoder, can be held to it (#77). A
// conforming H.264 decoder is bit-exact, and the phones' were measured so
// against software decoding before it left them (#65, #78). TestH264Reference
// holds desktop software decoding to these values on every run, so a change
// in FFmpeg's output shows there first; GOFFMPEG_TEST_PRINT_REFERENCE=1 prints
// them again.
var h264Reference = map[string]h264Ref{
	"testdata/sample_video_cover.mp4":   {frames: 10, planes: "ba1498a41abd606f78e3d3fde095a6c2ff721a6aba4e391b850145ffe7797184", info: YCbCrInfo{Matrix: MatrixBT601, FullRange: false, MatrixGuessed: true}},
	"testdata/sample.mp4":               {frames: 100, planes: "8b1e28d66435d798e464f511a824a4acafcc330acc9b818427b0d1b203c8d577", info: YCbCrInfo{Matrix: MatrixBT601, FullRange: false, MatrixGuessed: true}},
	"testdata/sample_video.mp4":         {frames: 100, planes: "8b1e28d66435d798e464f511a824a4acafcc330acc9b818427b0d1b203c8d577", info: YCbCrInfo{Matrix: MatrixBT601, FullRange: false, MatrixGuessed: true}},
	"testdata/yuv/bt709_tv.mp4":         {frames: 3, planes: "2d5b14213ee4c21a4c39463a565cad464cc7945651c65ffad538823b94c06c58", info: YCbCrInfo{Matrix: MatrixBT709, FullRange: false, MatrixGuessed: false}},
	"testdata/yuv/bt709_tv_320.mp4":     {frames: 3, planes: "abdd5cb4b6fbabe3cf9631609a2f2cdd064548aa2ba0d7b3a603e4df5e30022c", info: YCbCrInfo{Matrix: MatrixBT709, FullRange: false, MatrixGuessed: false}},
	"testdata/yuv/smpte170m_pc.mp4":     {frames: 3, planes: "ee59ae0b0422ba4c3dd7650b01a9c19ab4602bc0b958362850d8d307df424af3", info: YCbCrInfo{Matrix: MatrixBT601, FullRange: true, MatrixGuessed: false}},
	"testdata/yuv/smpte170m_pc_320.mp4": {frames: 3, planes: "4a146bcfc3e47977d332a22b575e5fb2a48cf1fd6e96163471ef9c2a78f4233f", info: YCbCrInfo{Matrix: MatrixBT601, FullRange: true, MatrixGuessed: false}},
}

// h264SeekReference is the first frame after Seek(2) in sample.mp4 (after
// five frames from the start, as TestHardwareDecodeSeek decodes it): its
// position and a hash of its planes through GetYCbCr.
var h264SeekReference = struct {
	position float64
	planes   string
}{2, "63ad7c376624829b4cb4937757fb3ed17c136c7ccd5074b983adab6656ce6f52"}

// referenceClips are the clips h264Reference covers.
var referenceClips = []string{
	"testdata/sample.mp4",
	"testdata/sample_video.mp4",
	"testdata/yuv/bt709_tv.mp4",
	"testdata/yuv/smpte170m_pc.mp4",
	"testdata/yuv/bt709_tv_320.mp4",
	"testdata/yuv/smpte170m_pc_320.mp4",
	// 32x24: below ANDROID_AVC_MIN_SIDE, so Android's software decoder (#101)
	"testdata/sample_video_cover.mp4",
}

// decodeReference decodes path with whatever decoder this build opens for it
// and returns what h264Ref records, and the decoder of the last frame.
func decodeReference(t *testing.T, path string) (h264Ref, string) {
	t.Helper()
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	ph := sha256.New()
	var ref h264Ref
	for {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("%s: Next: %v", path, err)
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
			t.Fatalf("%s: frame %d: GetYCbCr: %v", path, ref.frames, err)
		}
		ph.Write(packed(img))
		if ref.frames == 0 {
			ref.info = yi
		}
		ref.frames++
	}
	ref.planes = hex.EncodeToString(ph.Sum(nil))
	return ref, dec.(interface{ videoDecoder() string }).videoDecoder()
}

// seekPlanes decodes five frames of sample.mp4, seeks to 2 s and returns the
// next video frame's position, a hash of its planes and the decoder that
// decoded it, as TestHardwareDecodeSeek does.
func seekPlanes(t *testing.T) (float64, string, string) {
	t.Helper()
	dec, err := Open("testdata/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	info := dec.VideoInfo()
	img := image.NewYCbCr(image.Rect(0, 0, info.Width, info.Height), image.YCbCrSubsampleRatio420)
	next := func() {
		for {
			mt, err := dec.Next()
			if err != nil || mt == MediaTypeNone {
				t.Fatalf("no video frame: %v, %v", mt, err)
			}
			if mt == MediaTypeVideo {
				if _, err := dec.(YCbCrDecoder).GetYCbCr(img); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	for i := 0; i < 5; i++ {
		next()
	}
	if err := dec.Seek(2); err != nil {
		t.Fatal(err)
	}
	next()
	h := sha256.Sum256(packed(img))
	return dec.Position(), hex.EncodeToString(h[:]), dec.(interface{ videoDecoder() string }).videoDecoder()
}

// Desktop software decoding gives h264Reference, which the phones are held
// to. It runs where FFmpeg's h264 is in the build and the hardware is off.
func TestH264Reference(t *testing.T) {
	if !softwareH264() {
		t.Skip("no software h264 in this build: the phones compare their own decoders with the recorded reference")
	}
	t.Setenv("GOFFMPEG_HWDECODE", "0")
	got := map[string]h264Ref{}
	for _, path := range referenceClips {
		ref, decoder := decodeReference(t, path)
		if decoder != "h264" {
			t.Fatalf("%s decoded with %q, want FFmpeg's h264", path, decoder)
		}
		got[path] = ref
	}
	pos, seekHash, _ := seekPlanes(t)
	if os.Getenv("GOFFMPEG_TEST_PRINT_REFERENCE") != "" {
		paths := make([]string, 0, len(got))
		for p := range got {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			r := got[p]
			fmt.Printf("\t%q: {frames: %d, planes: %q, info: YCbCrInfo{Matrix: %s, FullRange: %v, MatrixGuessed: %v}},\n",
				p, r.frames, r.planes, matrixName(r.info.Matrix), r.info.FullRange, r.info.MatrixGuessed)
		}
		fmt.Printf("seek: position %v, planes %q\n", pos, seekHash)
		return
	}
	for _, path := range referenceClips {
		if got[path] != h264Reference[path] {
			t.Errorf("%s: software h264 gives %+v, the recorded reference is %+v", path, got[path], h264Reference[path])
		}
	}
	if pos != h264SeekReference.position || seekHash != h264SeekReference.planes {
		t.Errorf("after Seek(2): software at %v with %s, the recorded reference %v with %s",
			pos, seekHash, h264SeekReference.position, h264SeekReference.planes)
	}
}

// matrixName is m as a Go expression, for printing the reference.
func matrixName(m ColorMatrix) string {
	switch m {
	case MatrixBT601:
		return "MatrixBT601"
	case MatrixBT709:
		return "MatrixBT709"
	case MatrixBT2020:
		return "MatrixBT2020"
	}
	return fmt.Sprintf("ColorMatrix(%d)", int(m))
}
