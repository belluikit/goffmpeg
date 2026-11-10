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
	"sync"
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
	// missing is decoder_missing's answer at open, video then audio: the
	// codecs of streams this build could not decode.
	missing [2]string

	// mu orders Interrupt against Close, which frees the flag Interrupt sets.
	// Nothing else takes it: Interrupt is the only method that may run
	// concurrently with the rest.
	mu     sync.Mutex
	closed bool

	interrupted interruptState
}

func openDecoder(url, options string) (IDecoder, error) {
	d := &cgoDecoder{}
	cUrl := C.CString(url)
	cOpts := C.CString(options)
	defer C.free(unsafe.Pointer(cUrl))
	defer C.free(unsafe.Pointer(cOpts))

	var averr C.int
	res := C.decoder_open_opts_err(&d.d, cUrl, cOpts, &averr)
	d.missing = cMissing(&d.d)
	if res != 0 {
		// A failed open can leave the interrupt flag, and a format context
		// whose streams could not be read, behind.
		C.decoder_close(&d.d)
		return nil, openError(fmt.Sprintf("%q", url), int(res), int(averr), options, d.missing)
	}
	return d, nil
}

// cMissing reads decoder_missing for video and audio. It has to run before
// decoder_close, which frees the format context the names come from.
func cMissing(d *C.Decoder) [2]string {
	var m [2]string
	if s := C.decoder_missing(d, 1); s != nil {
		m[0] = C.GoString(s)
	}
	if s := C.decoder_missing(d, 2); s != nil {
		m[1] = C.GoString(s)
	}
	return m
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
	d.missing = cMissing(&d.d)
	if res != 0 {
		C.decoder_close(&d.d)
		if res == -9 {
			return nil, openError(fmt.Sprintf("device %q %q", format, url), int(res), 0, options, d.missing)
		}
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
		MissingDecoder: d.missing[0],
	}
}

func (d *cgoDecoder) AudioInfo() AudioInfo {
	return AudioInfo{
		SampleRate:     int(d.d.sample_rate),
		Channels:       int(d.d.channels),
		MissingDecoder: d.missing[1],
	}
}

func (d *cgoDecoder) Position() float64 {
	return float64(d.d.position)
}

func (d *cgoDecoder) Next() (MediaType, error) {
	return d.interrupted.nextResult(d.bridgeNext())
}

func (d *cgoDecoder) bridgeNext() int { return int(C.decoder_next(&d.d)) }

func (d *cgoDecoder) GetRGBA(rgba *image.RGBA) error {
	if rgba == nil || len(rgba.Pix) == 0 {
		return errors.New("invalid buffer")
	}
	if C.decoder_get_rgba(&d.d, (*C.uint8_t)(unsafe.Pointer(&rgba.Pix[0]))) == -8 {
		return ErrVideoNotBuilt
	}
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

func (d *cgoDecoder) Interrupt() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	// A reader-backed decoder is stopped on the Go side too: custom IO never
	// consults FFmpeg's interrupt callback, so the reader callback has to.
	if d.handle != 0 {
		interruptReader(d.handle)
	}
	C.decoder_interrupt(&d.d)
	d.interrupted.set.Store(true)
}

func (d *cgoDecoder) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
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
	return d.interrupted.seekResult(d.bridgeSeek(seconds))
}

func (d *cgoDecoder) bridgeSeek(seconds float64) int {
	return int(C.decoder_seek(&d.d, C.double(seconds)))
}

// missingDecoderSupported reports whether this backend can report missing
// decoders (#41). The cgo build compiles the bridge, so it always can.
func missingDecoderSupported() bool { return true }
