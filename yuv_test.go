package goffmpeg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"os"
	"testing"
)

// firstYCbCr opens path and reads its first video frame with GetYCbCr into a
// 4:2:0 image whose strides are padded past the frame's width, so the copy is
// seen to honour both strides.
func firstYCbCr(t *testing.T, path string) (*image.YCbCr, YCbCrInfo, error) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("test media unavailable: %v", err)
	}
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	t.Cleanup(dec.Close)
	vi := dec.VideoInfo()
	img := paddedYCbCr(vi.Width, vi.Height, 16)
	for i := 0; i < 200; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			t.Fatalf("%s: no video frame (%v)", path, err)
		}
		if mt != MediaTypeVideo {
			continue
		}
		info, err := dec.(YCbCrDecoder).GetYCbCr(img)
		if errors.Is(err, errYUVUnavailable) {
			t.Skip(err)
		}
		return img, info, err
	}
	t.Fatalf("%s: no video frame in 200 reads", path)
	return nil, YCbCrInfo{}, nil
}

// paddedYCbCr is a 4:2:0 image whose rows are pad bytes wider than its
// planes, with the padding filled so a copy that ignores the strides shows.
func paddedYCbCr(w, h, pad int) *image.YCbCr {
	cw, ch := (w+1)/2, (h+1)/2
	img := &image.YCbCr{
		Y: make([]byte, (w+pad)*h), Cb: make([]byte, (cw+pad)*ch), Cr: make([]byte, (cw+pad)*ch),
		YStride: w + pad, CStride: cw + pad, SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect: image.Rect(0, 0, w, h),
	}
	for _, p := range [][]byte{img.Y, img.Cb, img.Cr} {
		for i := range p {
			p[i] = 0xEE
		}
	}
	return img
}

// packed returns the planes of img without stride padding, Y then Cb then Cr:
// the layout of FFmpeg's `-f rawvideo -pix_fmt yuv420p`.
func packed(img *image.YCbCr) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	cw, ch := (w+1)/2, (h+1)/2
	var out []byte
	for y := 0; y < h; y++ {
		out = append(out, img.Y[y*img.YStride:y*img.YStride+w]...)
	}
	for _, p := range [][]byte{img.Cb, img.Cr} {
		for y := 0; y < ch; y++ {
			out = append(out, p[y*img.CStride:y*img.CStride+cw]...)
		}
	}
	return out
}

// The planes are the decoder's own, byte for byte: the first frame of
// sample.mp4 hashes the same as FFmpeg's raw output for it,
//
//	ffmpeg -i testdata/sample.mp4 -frames:v 1 -f rawvideo -pix_fmt yuv420p - | shasum -a 256
//
// (H.264 decoding is bit-exact, so the hash does not depend on the FFmpeg
// that made it.) The padding past each row is left alone.
func TestGetYCbCrMatchesFFmpeg(t *testing.T) {
	img, info, err := firstYCbCr(t, "testdata/sample.mp4")
	if err != nil {
		t.Fatalf("GetYCbCr: %v", err)
	}
	sum := sha256.Sum256(packed(img))
	if got, want := hex.EncodeToString(sum[:]), "558e046fe1488bd533a0553278ffd3f3d787cbc517dd05945b31c1414e421143"; got != want {
		t.Errorf("planes hash %s, want FFmpeg's %s", got, want)
	}
	if img.Y[img.Rect.Dx()] != 0xEE || img.Cb[img.Rect.Dx()/2] != 0xEE {
		t.Error("the copy wrote into the stride padding")
	}
	// sample.mp4 does not say its matrix, and is 240 lines.
	if want := (YCbCrInfo{Matrix: MatrixBT601, MatrixGuessed: true}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
}

// The matrix and range come from the stream when it states them, and are
// guessed from the height when it does not.
func TestGetYCbCrColorInfo(t *testing.T) {
	for _, tc := range []struct {
		path string
		want YCbCrInfo
	}{
		{"testdata/yuv/bt709_tv.mp4", YCbCrInfo{Matrix: MatrixBT709}},
		{"testdata/yuv/smpte170m_pc.mp4", YCbCrInfo{Matrix: MatrixBT601, FullRange: true}},
		{"testdata/yuv/untagged_720.mp4", YCbCrInfo{Matrix: MatrixBT709, MatrixGuessed: true}},
	} {
		_, info, err := firstYCbCr(t, tc.path)
		if err != nil {
			t.Errorf("%s: GetYCbCr: %v", tc.path, err)
			continue
		}
		if info != tc.want {
			t.Errorf("%s: info = %+v, want %+v", tc.path, info, tc.want)
		}
	}
}

// A format GetYCbCr does not copy is refused by name, not converted.
func TestGetYCbCrRefusesOtherFormats(t *testing.T) {
	_, _, err := firstYCbCr(t, "testdata/yuv/yuv444.mp4")
	if !errors.Is(err, ErrPixelFormat) {
		t.Fatalf("err = %v, want ErrPixelFormat", err)
	}
	if got := err.Error(); got != ErrPixelFormat.Error()+": yuv444p" {
		t.Errorf("err = %q, want it to name yuv444p", got)
	}
}

// A stream that changes resolution partway through: frames of the new size
// are refused with ErrFrameSize, and nothing is written into the buffer sized
// for the old one.
func TestGetYCbCrRefusesAFrameOfAnotherSize(t *testing.T) {
	const path = "testdata/yuv/size_change.ts"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("test media unavailable: %v", err)
	}
	dec, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()
	yd := dec.(YCbCrDecoder)
	var ok, refused int
	for i := 0; i < 400; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeVideo {
			continue
		}
		img := paddedYCbCr(64, 64, 0)
		_, err = yd.GetYCbCr(img)
		switch {
		case errors.Is(err, errYUVUnavailable):
			t.Skip(err)
		case err == nil:
			ok++
		case errors.Is(err, ErrFrameSize):
			refused++
			for _, b := range img.Y {
				if b != 0xEE {
					t.Fatal("a refused frame wrote into the buffer")
				}
			}
		default:
			t.Fatalf("GetYCbCr: %v", err)
		}
	}
	if ok == 0 || refused == 0 {
		t.Errorf("%d frames read and %d refused, want both: the clip is 64x64 then 96x96", ok, refused)
	}
}

// A destination GetYCbCr cannot fill is refused before anything is copied.
func TestGetYCbCrChecksTheDestination(t *testing.T) {
	for name, img := range map[string]*image.YCbCr{
		"nil":    nil,
		"4:4:4":  image.NewYCbCr(image.Rect(0, 0, 320, 240), image.YCbCrSubsampleRatio444),
		"size":   image.NewYCbCr(image.Rect(0, 0, 320, 200), image.YCbCrSubsampleRatio420),
		"stride": {Y: make([]byte, 320*240), Cb: make([]byte, 80*120), Cr: make([]byte, 80*120), YStride: 320, CStride: 80, SubsampleRatio: image.YCbCrSubsampleRatio420, Rect: image.Rect(0, 0, 320, 240)},
	} {
		if err := checkYCbCr(img, 320, 240); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkYCbCr(image.NewYCbCr(image.Rect(0, 0, 321, 241), image.YCbCrSubsampleRatio420), 321, 241); err != nil {
		t.Errorf("an odd-sized 4:2:0 image was refused: %v", err)
	}
}
