//go:build cgo

package goffmpeg

/*
// Include and library paths are per-platform; see libav_<goos>_<goarch>.go.

#include "bridge.h"

#include <stdlib.h>
*/
import "C"
import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

type cgoDecoder struct {
	d C.Decoder
	// handle is non-zero only for reader-backed decoders; it keys the Go-side
	// reader registry that the AVIO callbacks look up (see reader_cgo.go).
	handle int64
	// audio holds the caller's channel and rate requests, so rebuilding the
	// resampler for one keeps the other.
	audio audioRequest
}

func openDecoder(url, options string) (IDecoder, error) {
	d := &cgoDecoder{}
	cUrl := C.CString(url)
	cOpts := C.CString(options)
	defer C.free(unsafe.Pointer(cUrl))
	defer C.free(unsafe.Pointer(cOpts))

	res := C.decoder_open_opts(&d.d, cUrl, cOpts)
	if res != 0 {
		return nil, openError(res, options)
	}
	return d, nil
}

// openError turns a bridge open code into a Go error. -8 is the one worth
// naming: it means the options string itself would not parse, which is a
// caller mistake rather than anything about the media.
func openError(res C.int, options string) error {
	if res == -8 {
		return fmt.Errorf("goffmpeg: malformed open options %q", options)
	}
	return fmt.Errorf("failed to open decoder: %d", int(res))
}

func openDeviceDecoder(format, url, options string) (IDecoder, error) {
	d := &cgoDecoder{}
	cFormat := C.CString(format)
	cURL := C.CString(url)
	cOpts := C.CString(options)
	defer C.free(unsafe.Pointer(cFormat))
	defer C.free(unsafe.Pointer(cURL))
	defer C.free(unsafe.Pointer(cOpts))

	var averr C.int
	res := C.decoder_open_device_err(&d.d, cFormat, cURL, cOpts, &averr)
	if res != 0 {
		return nil, openDeviceError(format, url, int(res), int(averr))
	}
	return d, nil
}

// listInputSources walks the AVDeviceInfoList the bridge opened, copying each
// device out before device_list_free drops the C strings it points at.
func listInputSources(format string) ([]DeviceInfo, error) {
	cFormat := C.CString(format)
	defer C.free(unsafe.Pointer(cFormat))

	var list unsafe.Pointer
	if rc := int(C.device_list_open(cFormat, &list)); rc < 0 {
		return nil, deviceListError(format, rc)
	}
	defer C.device_list_free(list)

	// The count comes from the list rather than from device_list_open's return
	// value; both report it, but reading the list cannot disagree with it.
	n := int(C.device_list_count(list))
	def := int(C.device_list_default(list))
	out := make([]DeviceInfo, 0, n)
	for i := 0; i < n; i++ {
		media, known := mediaTypesFromMask(int(C.device_list_media_types(list, C.int(i))))
		out = append(out, DeviceInfo{
			Name:        C.GoString(C.device_list_name(list, C.int(i))),
			Description: C.GoString(C.device_list_description(list, C.int(i))),
			MediaTypes:  media,
			MediaKnown:  known,
			Default:     i == def,
		})
	}
	return out, nil
}

// listDeviceModes reads the mode list the bridge built, copying each entry out
// before device_modes_free drops the pixel-format strings it points at — the
// same ownership rule listInputSources follows.
func listDeviceModes(format, url string) ([]DeviceMode, error) {
	cFormat := C.CString(format)
	cURL := C.CString(url)
	defer C.free(unsafe.Pointer(cFormat))
	defer C.free(unsafe.Pointer(cURL))

	var modes unsafe.Pointer
	if rc := int(C.device_modes_open(cFormat, cURL, &modes)); rc < 0 {
		return nil, deviceModesError(format, url, rc)
	}
	defer C.device_modes_free(modes)

	n := int(C.device_modes_count(modes))
	out := make([]DeviceMode, 0, n)
	for i := 0; i < n; i++ {
		m := DeviceMode{
			Format: format,
			Width:  int(C.device_modes_width(modes, C.int(i))),
			Height: int(C.device_modes_height(modes, C.int(i))),
			MinFPS: float64(C.device_modes_min_fps(modes, C.int(i))),
			MaxFPS: float64(C.device_modes_max_fps(modes, C.int(i))),
		}
		name := C.GoString(C.device_modes_pixel_format(modes, C.int(i)))
		if C.device_modes_is_codec(modes, C.int(i)) != 0 {
			m.Codec = name
		} else {
			m.PixelFormat = name
		}
		out = append(out, m)
	}
	return out, nil
}

func (d *cgoDecoder) VideoInfo() VideoInfo {
	return VideoInfo{
		Width:  int(d.d.width),
		Height: int(d.d.height), Framerate: float64(d.d.framerate),
	}
}

func (d *cgoDecoder) AudioInfo() AudioInfo {
	return AudioInfo{
		SampleRate: int(d.d.sample_rate),
		Channels:   int(d.d.channels),
	}
}

func (d *cgoDecoder) Position() float64 {
	return float64(d.d.position)
}

func (d *cgoDecoder) Next() (MediaType, error) {
	return nextResult(int(C.decoder_next(&d.d)))
}

func (d *cgoDecoder) GetRGBA(rgba *image.RGBA) error {
	if rgba == nil || len(rgba.Pix) == 0 {
		return errors.New("invalid buffer")
	}
	C.decoder_get_rgba(&d.d, (*C.uint8_t)(unsafe.Pointer(&rgba.Pix[0])))
	return nil
}

func (d *cgoDecoder) GetAudio() ([]byte, error) {
	var ptr *C.uint8_t
	size := C.decoder_get_audio(&d.d, &ptr)
	if size < 0 {
		return nil, fmt.Errorf("audio error: %d", size)
	}
	if size == 0 {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(ptr), C.int(size)), nil
}

func (d *cgoDecoder) Close() {
	C.decoder_close(&d.d)
	// Drop the reader from the registry only after the C side has torn down
	// its AVIOContext, so no in-flight callback can miss it.
	if d.handle != 0 {
		unregisterReader(d.handle)
		d.handle = 0
	}
}

func (d *cgoDecoder) Duration() float64 {
	return float64(d.d.duration)
}

func (d *cgoDecoder) SetAudioChannels(n int) error {
	if res := C.decoder_set_audio_channels(&d.d, C.int(n)); res < 0 {
		return fmt.Errorf("set audio channels to %d failed: %d", n, int(res))
	}
	d.audio.channels = n
	return nil
}

func (d *cgoDecoder) SetAudioSampleRate(rate int) error {
	return d.audio.setRate(rate, (*int32)(unsafe.Pointer(&d.d.sample_rate)), func(channels int) int {
		return int(C.decoder_set_audio_channels(&d.d, C.int(channels)))
	})
}

func (d *cgoDecoder) Seek(seconds float64) error {
	if res := C.decoder_seek(&d.d, C.double(seconds)); res < 0 {
		return fmt.Errorf("seek failed: %d", res)
	}
	return nil
}
