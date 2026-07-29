//go:build !cgo

package goffmpeg

import (
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

type cDecoder struct {
	fmt_ctx        uintptr
	v_ctx          uintptr
	v_frame        uintptr
	sws_ctx        uintptr
	v_stream_idx   int32
	width          int32
	height         int32
	framerate      float64
	a_ctx          uintptr
	a_frame        uintptr
	swr_ctx        uintptr
	a_stream_idx   int32
	sample_rate    int32
	channels       int32
	audio_buf      uintptr
	audio_buf_size int32
	pkt            uintptr
	position       float64
	duration       float64
	// Mirrors the custom-IO tail of the C Decoder struct. The purego build has
	// no reader support, so these stay zero — but they must exist, because the
	// C side memsets sizeof(Decoder) bytes over this Go allocation and would
	// otherwise write past the end of it.
	avio_ctx      uintptr
	avio_buf      uintptr
	reader_handle int64
}

// The bridge returns C int, which is 32 bits. These must be declared int32,
// not int: purego reads the full 64-bit return register, and on arm64 a C int
// comes back in w0 with the upper half of x0 unspecified — so a negative value
// arrives zero-extended. Declaring int turns a -1 into 4294967295 and silently
// kills every `if res < 0` check. Convert at the call site instead.
var (
	decoder_open        func(d *cDecoder, url *byte) int32
	decoder_open_opts   func(d *cDecoder, url *byte, options *byte) int32
	decoder_open_device func(d *cDecoder, format *byte, url *byte, options *byte) int32
	decoder_next        func(d *cDecoder) int32
	decoder_get_rgba    func(d *cDecoder, buffer *byte) int32
	decoder_get_audio   func(d *cDecoder, buffer **byte) int32
	decoder_close       func(d *cDecoder)
	decoder_seek        func(d *cDecoder, seconds float64) int32

	decoder_set_audio_channels func(d *cDecoder, channels int32) int32

	// These mirror the C signatures exactly, int32 for C int: the return
	// register's upper half is not sign-extended, so binding a C int as Go int
	// turns -2 into 4294967294. Convert after the call, not in the signature.
	device_list_open        func(format *byte, list *uintptr) int32
	device_list_count       func(list uintptr) int32
	device_list_name        func(list uintptr, i int32) string
	device_list_description func(list uintptr, i int32) string
	device_list_media_types func(list uintptr, i int32) int32
	device_list_default     func(list uintptr) int32
	device_list_free        func(list uintptr)
)

func init() {
	libName := "libgoffmpeg.so"
	if runtime.GOOS == "darwin" {
		libName = "libgoffmpeg.dylib"
	} else if runtime.GOOS == "windows" {
		libName = "libgoffmpeg.dll"
	}

	// Artifacts are laid out per platform (see BUILDING.md), so the directory
	// carries the target triple. This path is relative to the working
	// directory, which really only holds while developing inside this module;
	// a deployed binary is expected to find the library through the system
	// search path, or to use the cgo backend instead.
	target := runtime.GOOS + "_" + runtime.GOARCH
	libPath, _ := filepath.Abs(filepath.Join("internal", "libav", "dist", target, "lib", libName))
	lib, err := loadLibrary(libPath)
	if err != nil {
		lib, err = loadLibrary(libName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", libName, err)
			return
		}
	}

	purego.RegisterLibFunc(&decoder_open, lib, "decoder_open")
	purego.RegisterLibFunc(&decoder_open_device, lib, "decoder_open_device")
	purego.RegisterLibFunc(&decoder_next, lib, "decoder_next")
	purego.RegisterLibFunc(&decoder_get_rgba, lib, "decoder_get_rgba")
	purego.RegisterLibFunc(&decoder_get_audio, lib, "decoder_get_audio")
	purego.RegisterLibFunc(&decoder_close, lib, "decoder_close")
	purego.RegisterLibFunc(&decoder_seek, lib, "decoder_seek")
	purego.RegisterLibFunc(&decoder_set_audio_channels, lib, "decoder_set_audio_channels")

	// Per-open options arrived after enumeration and have the same problem: a
	// library from an older release does not export the symbol, and
	// RegisterLibFunc panics rather than reporting that. Probed separately from
	// the enumeration block below so a library that has one but not the other
	// still binds what it has.
	if hasSymbol(lib, "decoder_open_opts") {
		purego.RegisterLibFunc(&decoder_open_opts, lib, "decoder_open_opts")
	}

	// Device enumeration arrived after the shared bridge started shipping
	// prebuilt in the goffmpeg-libs companion modules, so a library from an
	// older release exports none of these symbols. RegisterLibFunc panics on a
	// missing symbol, hence the probe: an old library leaves the bindings nil
	// and ListInputSources reports that, rather than taking the process down.
	if !hasSymbol(lib, "device_list_open") {
		return
	}
	purego.RegisterLibFunc(&device_list_open, lib, "device_list_open")
	purego.RegisterLibFunc(&device_list_count, lib, "device_list_count")
	purego.RegisterLibFunc(&device_list_name, lib, "device_list_name")
	purego.RegisterLibFunc(&device_list_description, lib, "device_list_description")
	purego.RegisterLibFunc(&device_list_media_types, lib, "device_list_media_types")
	purego.RegisterLibFunc(&device_list_default, lib, "device_list_default")
	purego.RegisterLibFunc(&device_list_free, lib, "device_list_free")
}

// listInputSources mirrors the cgo backend, reading the list through the
// bridge's accessors — purego cannot walk AVDeviceInfoList itself, which is
// why the C side hands back a handle instead of the struct.
func listInputSources(format string) ([]DeviceInfo, error) {
	if device_list_open == nil {
		return nil, ErrDeviceListUnavailable
	}
	cFormat := []byte(format + "\x00")

	var list uintptr
	if rc := device_list_open(&cFormat[0], &list); rc < 0 {
		return nil, deviceListError(format, int(rc))
	}
	defer device_list_free(list)

	// The count comes from the list rather than from device_list_open's return
	// value; both report it, but reading the list cannot disagree with it.
	n := device_list_count(list)
	def := device_list_default(list)
	out := make([]DeviceInfo, 0, n)
	for i := int32(0); i < n; i++ {
		media, known := mediaTypesFromMask(int(device_list_media_types(list, i)))
		out = append(out, DeviceInfo{
			Name:        device_list_name(list, i),
			Description: device_list_description(list, i),
			MediaTypes:  media,
			MediaKnown:  known,
			Default:     i == def,
		})
	}
	return out, nil
}

type puregoDecoder struct {
	d cDecoder
}

func openDecoder(url, options string) (IDecoder, error) {
	if decoder_open == nil {
		return nil, errors.New("library not loaded")
	}
	// A library too old to take options can still open the url. Say so only
	// when options were actually asked for — silently dropping them would turn
	// a missing timeout into a hang with no explanation.
	if decoder_open_opts == nil && options != "" {
		return nil, ErrOpenOptionsUnavailable
	}

	d := &puregoDecoder{}
	cUrl := []byte(url + "\x00")

	var res int
	if decoder_open_opts != nil {
		cOpts := []byte(options + "\x00")
		res = int(decoder_open_opts(&d.d, &cUrl[0], &cOpts[0]))
	} else {
		res = int(decoder_open(&d.d, &cUrl[0]))
	}
	if res != 0 {
		if res == -8 {
			return nil, fmt.Errorf("goffmpeg: malformed open options %q", options)
		}
		return nil, fmt.Errorf("open failed: %d", res)
	}
	return d, nil
}

// openReaderDecoder is unsupported on the purego backend: feeding libavformat
// from an io.Reader means C calling back into Go, which the cgo build does via
// //export. bridge_reader.c is therefore left out of the shared library, and
// libgoffmpeg exports no decoder_open_reader symbol to bind here.
func openReaderDecoder(r io.Reader, options string) (IDecoder, error) {
	return nil, errors.New("goffmpeg: OpenReader requires cgo — build with CGO_ENABLED=1, or use Open with a path or URL")
}

func openDeviceDecoder(format, url, options string) (IDecoder, error) {
	if decoder_open_device == nil {
		return nil, errors.New("library not loaded")
	}
	d := &puregoDecoder{}
	cFormat := []byte(format + "\x00")
	cURL := []byte(url + "\x00")
	cOpts := []byte(options + "\x00")
	res := int(decoder_open_device(&d.d, &cFormat[0], &cURL[0], &cOpts[0]))
	if res != 0 {
		return nil, fmt.Errorf("open device %q %q failed: %d", format, url, res)
	}
	return d, nil
}

func (d *puregoDecoder) VideoInfo() VideoInfo {
	return VideoInfo{Width: int(d.d.width), Height: int(d.d.height), Framerate: d.d.framerate}
}

func (d *puregoDecoder) AudioInfo() AudioInfo {
	return AudioInfo{SampleRate: int(d.d.sample_rate), Channels: int(d.d.channels)}
}

func (d *puregoDecoder) Position() float64 {
	return d.d.position
}

func (d *puregoDecoder) Next() (MediaType, error) {
	res := int(decoder_next(&d.d))
	if res < 0 {
		return MediaTypeNone, fmt.Errorf("decoder error: %d", res)
	}
	return MediaType(res), nil
}

func (d *puregoDecoder) GetRGBA(rgba *image.RGBA) error {
	if rgba == nil || len(rgba.Pix) == 0 {
		return errors.New("invalid buffer")
	}
	decoder_get_rgba(&d.d, &rgba.Pix[0])
	return nil
}

func (d *puregoDecoder) GetAudio() ([]byte, error) {
	var ptr *byte
	size := int(decoder_get_audio(&d.d, &ptr))
	if size < 0 {
		return nil, fmt.Errorf("audio error: %d", size)
	}
	if size == 0 {
		return nil, nil
	}
	out := make([]byte, size)
	copy(out, unsafe.Slice(ptr, size))
	return out, nil
}

func (d *puregoDecoder) Close() {
	decoder_close(&d.d)
}

func (d *puregoDecoder) Duration() float64 {
	return d.d.duration
}

func (d *puregoDecoder) SetAudioChannels(n int) error {
	if decoder_set_audio_channels == nil {
		return errors.New("library not loaded")
	}
	if res := int(decoder_set_audio_channels(&d.d, int32(n))); res < 0 {
		return fmt.Errorf("set audio channels to %d failed: %d", n, res)
	}
	return nil
}

func (d *puregoDecoder) Seek(seconds float64) error {
	if decoder_seek == nil {
		return errors.New("library not loaded")
	}
	if res := int(decoder_seek(&d.d, seconds)); res < 0 {
		return fmt.Errorf("seek failed: %d", res)
	}
	return nil
}
