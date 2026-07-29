package goffmpeg

import (
	"errors"
	"fmt"
	"image"
	"io"
)

type MediaType int

const (
	MediaTypeNone  MediaType = 0
	MediaTypeVideo MediaType = 1
	MediaTypeAudio MediaType = 2
)

type VideoInfo struct {
	Width     int
	Height    int
	Framerate float64
}

type AudioInfo struct {
	SampleRate int
	Channels   int
}

type IDecoder interface {
	VideoInfo() VideoInfo
	AudioInfo() AudioInfo
	Position() float64
	// Duration returns the total media duration in seconds (0 if unknown).
	Duration() float64
	// Seek jumps to the given position in seconds.
	Seek(seconds float64) error
	// SetAudioChannels rebuilds the resampler to emit n interleaved S16
	// channels; n <= 0 restores the stream's native layout. Call before
	// decoding starts — rebuilding discards buffered resampler state.
	// AudioInfo reports the resulting count, which may differ from n.
	SetAudioChannels(n int) error
	Next() (MediaType, error)
	GetRGBA(rgba *image.RGBA) error
	GetAudio() ([]byte, error)
	Close()
}

func Open(url string) (IDecoder, error) {
	return openDecoder(url, "")
}

// OpenWithOptions is Open with per-open demuxer and protocol options, given as
// a comma-separated "key=value" list — the same shape OpenDevice takes. Pass
// "" for none.
//
// The string is handed to libavformat as-is, so anything FFmpeg accepts works
// without this package knowing about it. What matters for a network source:
//
//   - rw_timeout — without it a stalled server hangs the open indefinitely.
//   - reconnect, reconnect_streamed, reconnect_delay_max — without them any
//     dropped connection ends playback.
//   - user_agent, headers — some CDNs reject the default agent, and cookies,
//     auth and referer have nowhere else to go.
//   - http_persistent — HLS otherwise reopens a connection per segment.
//   - analyzeduration, probesize — slow or failed probing on sparse streams.
//
// Those two timeouts are what separate a working demo from unattended playback
// of a real stream:
//
//	d, err := goffmpeg.OpenWithOptions(u,
//		"rw_timeout=5000000,reconnect=1,reconnect_streamed=1,reconnect_delay_max=5")
//
// An option the input does not recognise is ignored rather than rejected, so
// the same string can be used for a URL and a local path. A malformed string
// is an error.
//
// Under CGO_ENABLED=0 this needs a libgoffmpeg new enough to export the entry
// point; an older one returns ErrOpenOptionsUnavailable for a non-empty
// options string and behaves exactly like Open for an empty one.
func OpenWithOptions(url, options string) (IDecoder, error) {
	return openDecoder(url, options)
}

// OpenReader decodes directly from r, without staging the media on disk.
//
// If r implements io.Seeker the returned decoder supports Seek and reports an
// accurate Duration; a plain reader is forward-only, and formats that need to
// read a trailing index (an unfragmented MP4 with its moov atom at the end,
// for example) may fail to open at all.
//
// Requires cgo. Under CGO_ENABLED=0 this returns an error — use Open with a
// path or URL instead.
func OpenReader(r io.Reader) (IDecoder, error) {
	return openReaderDecoder(r, "")
}

// OpenReaderWithOptions is OpenReader with the same options string
// OpenWithOptions takes.
//
// The protocol options do nothing here — there is no protocol, only r — but
// the probing ones matter more than anywhere else: a forward-only reader is
// exactly the case where libavformat struggles to identify the format, and
// raising analyzeduration and probesize is the only remedy.
//
// Requires cgo, as OpenReader does.
func OpenReaderWithOptions(r io.Reader, options string) (IDecoder, error) {
	return openReaderDecoder(r, options)
}

// OpenDevice opens a live capture device (camera/microphone) via libavdevice
// and returns a decoder over its stream. Read frames with Next + GetRGBA just
// like a file; Duration is 0 and Seek is unsupported on a live device.
//
//   - format: the input device name — "avfoundation" (macOS), "v4l2" (Linux),
//     "dshow" (Windows).
//   - url: the device selector — "0" (default avfoundation video device),
//     "/dev/video0" (v4l2), or a dshow "video=<name>".
//   - options: a comma-separated "key=value" list forwarded to the device
//     (e.g. "framerate=30,pixel_format=nv12"), or "" for defaults.
func OpenDevice(format, url, options string) (IDecoder, error) {
	return openDeviceDecoder(format, url, options)
}

// DeviceInfo is one capture device reported by ListInputSources.
type DeviceInfo struct {
	// Name is the selector to hand OpenDevice as its url: "/dev/video0" under
	// v4l2, a DirectShow unique name under dshow — dshow additionally wants the
	// "video="/"audio=" prefix, which it does not include.
	Name string
	// Description is the human-friendly name ("Logitech BRIO"). dshow accepts
	// it as a selector too, but it is not unique across identical cameras.
	Description string
	// MediaTypes is what the device can provide. Meaningful only when
	// MediaKnown is true; use HasMedia rather than reading it directly.
	MediaTypes []MediaType
	// MediaKnown reports whether MediaTypes was determined at all. False means
	// the backend could not say — not that the device provides nothing.
	MediaKnown bool
	// Default reports whether the format nominated this as its default device.
	Default bool
}

// HasMedia reports whether the device can provide mt. A device whose media
// could not be determined answers true: the caller must try it, since guessing
// "no" would drop a working camera.
//
// The inverse matters just as much. On Linux a node that is determined *not*
// to be video capture — the metadata node a UVC camera exposes alongside its
// real one — must be skipped, because opening it blocks indefinitely rather
// than failing.
func (d DeviceInfo) HasMedia(mt MediaType) bool {
	if !d.MediaKnown {
		return true
	}
	for _, t := range d.MediaTypes {
		if t == mt {
			return true
		}
	}
	return false
}

// ErrDeviceListUnsupported is returned by ListInputSources when the input
// format has no enumeration callback. avfoundation is the case that matters:
// macOS cameras are selected positionally ("0") instead, so a caller seeing
// this should fall back to a platform default rather than report no camera.
var ErrDeviceListUnsupported = errors.New("goffmpeg: input format does not support device enumeration")

// ErrDeviceListUnavailable is returned when the backend itself cannot
// enumerate, whatever the format. It is specific to purego: that backend
// dlopens the prebuilt libgoffmpeg the companion modules ship, and a library
// from a release before enumeration existed exports none of the symbols. The
// cgo backend compiles the bridge from source and never hits this.
//
// It is separate from ErrDeviceListUnsupported because the remedy differs —
// rebuild or update the shared library, rather than fall back to positional
// device selection.
var ErrDeviceListUnavailable = errors.New("goffmpeg: the loaded libgoffmpeg exports no device enumeration — rebuild the shared library, or build with cgo")

// ErrOpenOptionsUnavailable is returned by OpenWithOptions under purego when
// the loaded libgoffmpeg predates per-open options and exports no
// decoder_open_opts. Same cause and same remedy as ErrDeviceListUnavailable.
//
// It is reported only for a non-empty options string: with no options to lose,
// OpenWithOptions falls back to the plain entry point and succeeds.
var ErrOpenOptionsUnavailable = errors.New("goffmpeg: the loaded libgoffmpeg accepts no open options — rebuild the shared library, or build with cgo")

// ListInputSources enumerates the capture devices an input device format can
// see — the programmatic form of `ffmpeg -f dshow -list_devices true -i dummy`.
//
// It exists because the device url is not uniform across platforms and only
// one of the shapes is guessable: v4l2 takes a device node and dshow takes a
// name with no positional form at all, so on Windows a camera cannot be opened
// without first being enumerated.
//
// Both video and audio sources come back (dshow lists both); filter with
// HasMedia. Returns ErrDeviceListUnsupported for a format that cannot
// enumerate, and an error for an unknown format or a failed enumeration. A
// successful call with no devices is an empty slice and a nil error.
func ListInputSources(format string) ([]DeviceInfo, error) {
	return listInputSources(format)
}

// deviceListError maps the bridge's GOFFMPEG_DEVICE_LIST_* codes onto Go
// errors. Shared by both backends so they fail identically.
func deviceListError(format string, rc int) error {
	switch rc {
	case -2:
		return fmt.Errorf("goffmpeg: unknown input format %q", format)
	case -3:
		return ErrDeviceListUnsupported
	case -4:
		return errors.New("goffmpeg: built without libavdevice")
	default:
		return fmt.Errorf("goffmpeg: listing %q devices failed: %d", format, rc)
	}
}

// The bridge reports a device's media as a bitmask; these mirror the bit
// positions device_list_media_types sets, plus its "could not determine"
// answer, which is distinct from a mask of 0.
const (
	deviceMediaVideo   = 1 << 0
	deviceMediaAudio   = 1 << 1
	deviceMediaUnknown = -1
)

// mediaTypesFromMask expands that bitmask into what DeviceInfo carries: the
// media types, and whether they were determined at all.
func mediaTypesFromMask(mask int) ([]MediaType, bool) {
	if mask == deviceMediaUnknown {
		return nil, false
	}
	var out []MediaType
	if mask&deviceMediaVideo != 0 {
		out = append(out, MediaTypeVideo)
	}
	if mask&deviceMediaAudio != 0 {
		out = append(out, MediaTypeAudio)
	}
	return out, true
}
