package goffmpeg

import (
	"errors"
	"fmt"
	"image"
)

// ColorMatrix is the matrix that turns a video frame's YUV into RGB.
type ColorMatrix int

const (
	// MatrixBT601 is standard-definition video's matrix (SMPTE 170M, BT.470BG).
	MatrixBT601 ColorMatrix = iota + 1
	// MatrixBT709 is high-definition video's matrix.
	MatrixBT709
	// MatrixBT2020 is ultra-high-definition video's matrix (non-constant
	// luminance).
	MatrixBT2020
)

func (m ColorMatrix) String() string {
	switch m {
	case MatrixBT601:
		return "BT.601"
	case MatrixBT709:
		return "BT.709"
	case MatrixBT2020:
		return "BT.2020"
	}
	return fmt.Sprintf("ColorMatrix(%d)", int(m))
}

// YCbCrInfo describes how to turn a frame read by GetYCbCr into RGB.
type YCbCrInfo struct {
	Matrix ColorMatrix
	// FullRange is set for 0–255 luma and chroma (JPEG range). Unset, the
	// frame is limited range: luma 16–235, chroma 16–240, as nearly all video.
	FullRange bool
	// MatrixGuessed is set when the stream did not say which matrix it uses,
	// which most files do not, and Matrix is the conventional guess from the
	// frame's height: BT.709 above 576 lines, BT.601 at or below.
	MatrixGuessed bool
}

// YCbCrDecoder is an optional capability of an IDecoder: it hands out the
// current video frame's planes as decoded, without converting them to RGBA.
// A caller that converts in a GPU shader then skips libswscale's work on the
// processor and uploads 1.5 bytes per pixel instead of 4 (#64). Every decoder
// this package opens implements it; assert for it, because IDecoder does not
// carry it:
//
//	if yd, ok := dec.(goffmpeg.YCbCrDecoder); ok { ... }
type YCbCrDecoder interface {
	// GetYCbCr copies the frame most recently returned by Next into dst,
	// which must be 4:2:0 (image.YCbCrSubsampleRatio420) and the size
	// VideoInfo reports. Only 8-bit 4:2:0 frames are copied, which is what
	// nearly all H.264, HEVC and VP9 decodes to: planar (yuv420p, yuvj420p),
	// or nv12, whose interleaved chroma the phones' hardware decoders deliver
	// (#65) and which is split into dst's Cb and Cr as it is copied. Any other
	// format returns an error wrapping ErrPixelFormat that names it, and
	// GetRGBA still converts it.
	GetYCbCr(dst *image.YCbCr) (YCbCrInfo, error)
}

// ErrPixelFormat is GetYCbCr's answer for a frame it does not copy: one that
// is not 8-bit 4:2:0, planar or nv12. The error wrapping it names the format.
var ErrPixelFormat = errors.New("goffmpeg: frame is not 8-bit 4:2:0")

// ErrFrameSize is the answer for a frame whose size is not the size the
// decoder opened with, which a stream that changes resolution partway through
// brings. Nothing is written into the caller's buffer.
var ErrFrameSize = errors.New("goffmpeg: frame size no longer matches VideoInfo")

// ErrNoFrame is the answer when there is no decoded video frame to read.
var ErrNoFrame = errors.New("goffmpeg: no decoded video frame")

// errYUVUnavailable is GetYCbCr's answer under CGO_ENABLED=0 against a
// libgoffmpeg built before decoder_get_yuv existed.
var errYUVUnavailable = errors.New("goffmpeg: this libgoffmpeg predates GetYCbCr (#64); GetRGBA still works")

// checkYCbCr reports whether dst can take a w x h frame.
func checkYCbCr(dst *image.YCbCr, w, h int) error {
	if dst == nil {
		return errors.New("goffmpeg: nil destination")
	}
	if dst.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		return fmt.Errorf("goffmpeg: destination is %v, want 4:2:0", dst.SubsampleRatio)
	}
	if dst.Rect.Dx() != w || dst.Rect.Dy() != h {
		return fmt.Errorf("goffmpeg: destination is %dx%d, frame %dx%d", dst.Rect.Dx(), dst.Rect.Dy(), w, h)
	}
	cw, ch := (w+1)/2, (h+1)/2
	if dst.YStride < w || dst.CStride < cw || len(dst.Y) < dst.YStride*(h-1)+w ||
		len(dst.Cb) < dst.CStride*(ch-1)+cw || len(dst.Cr) < dst.CStride*(ch-1)+cw {
		return errors.New("goffmpeg: destination planes too small")
	}
	return nil
}

// yuvResult turns decoder_get_yuv's result into an error.
func yuvResult(res int, pixFmt string) error {
	switch res {
	case 0:
		return nil
	case -1:
		return ErrNoFrame
	case -2:
		return ErrFrameSize
	case -3:
		return fmt.Errorf("%w: %s", ErrPixelFormat, pixFmt)
	case -8:
		return ErrVideoNotBuilt
	}
	return fmt.Errorf("goffmpeg: reading the frame's planes failed: %d", res)
}

// FFmpeg's AVColorSpace values that name a matrix (libavutil/pixfmt.h).
const (
	avcolSpcBT709     = 1
	avcolSpcFCC       = 4
	avcolSpcBT470BG   = 5
	avcolSpcSMPTE170M = 6
	avcolSpcSMPTE240M = 7
	avcolSpcBT2020NCL = 9
)

// colorInfo builds a YCbCrInfo from the frame's AVColorSpace, its range and
// its height.
func colorInfo(colorspace int, fullRange bool, height int) YCbCrInfo {
	info := YCbCrInfo{FullRange: fullRange}
	switch colorspace {
	case avcolSpcBT709:
		info.Matrix = MatrixBT709
	case avcolSpcFCC, avcolSpcBT470BG, avcolSpcSMPTE170M, avcolSpcSMPTE240M:
		info.Matrix = MatrixBT601
	case avcolSpcBT2020NCL:
		info.Matrix = MatrixBT2020
	default:
		info.MatrixGuessed = true
		info.Matrix = MatrixBT601
		if height > 576 {
			info.Matrix = MatrixBT709
		}
	}
	return info
}
