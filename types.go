package goffmpeg

import (
	"errors"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
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
	// Next advances to the next decoded frame and reports which stream it
	// came from. There are four outcomes, and a live source produces all
	// four:
	//
	//	MediaTypeVideo, nil  a video frame is ready — GetRGBA it
	//	MediaTypeAudio, nil  an audio frame is ready — GetAudio it
	//	MediaTypeNone,  nil  end of stream, cleanly
	//	MediaTypeNone,  err  ErrAgain if no data is ready yet, else a failure
	//
	// ErrAgain is not a failure: see its documentation for the loop shape a
	// capture device needs.
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

// ErrDeviceListNoDevices is returned when the host produced no device list at
// all, which in practice means it has no capture hardware attached.
//
// It is separate from a generic failure because the remedy is different and
// because a caller usually wants to carry on: no camera on this machine is an
// ordinary state, not a fault, and it should be as distinguishable as
// avfoundation's "cannot enumerate" already is.
//
// Only dshow produces it. v4l2 walks /dev and reports an empty list with a nil
// error when it finds nothing; avfoundation cannot enumerate at all and
// reports ErrDeviceListUnsupported. dshow instead answers AVERROR(EIO) with
// "Could not enumerate %s devices (or none found)", conflating an absent
// device set with a broken enumerator — so this cannot promise the enumerator
// was healthy, only that nothing came back.
//
// It is deliberately not "no camera". dshow_get_device_list tolerates the
// video cycle's EIO and returns the audio cycle's result, so a machine with
// working cameras but no microphone reports this too, and libavdevice discards
// the devices it had already collected on the way out.
var ErrDeviceListNoDevices = errors.New("goffmpeg: no capture devices could be enumerated on this host")

// ErrAgain reports that no data was ready yet and Next should simply be called
// again. It is neither a failure nor an end of stream.
//
// It comes from live capture: a device read non-blockingly answers "no packet
// yet" with EAGAIN, which on a camera is the ordinary state between frames.
// Treated as fatal it ends a capture almost immediately — the run in #4 got two
// frames in ~30ms before the stream was declared over.
//
// A capture loop therefore has to distinguish three things:
//
//	for {
//		mt, err := dec.Next()
//		switch {
//		case errors.Is(err, ErrAgain):
//			time.Sleep(5 * time.Millisecond) // nothing yet; see below
//			continue
//		case err != nil:
//			return err // the device really is gone
//		case mt == MediaTypeNone:
//			return nil // end of stream
//		}
//		// ... mt is MediaTypeVideo or MediaTypeAudio
//	}
//
// The sleep is the caller's to choose and is not optional: Next does not block
// waiting for a frame, so a bare `continue` busy-waits a core between frames.
var ErrAgain = errors.New("goffmpeg: no data available yet")

// AVError is an FFmpeg AVERROR code reported by the decoder.
//
// It exists so a caller can classify a failure without parsing a formatted
// string, which used to be the only handle on it (#4):
//
//	var averr goffmpeg.AVError
//	if errors.As(err, &averr) && int(averr) == -16 { /* EBUSY */ }
//
// The value is FFmpeg's own convention: an errno-derived code is the negated
// errno (AVERROR(EBUSY) is -16), and the rest are FFmpeg's packed FFERRTAG
// constants, which are large negatives.
type AVError int

func (e AVError) Error() string {
	// FFERRTAG-packed constants are large negatives; errno-derived ones are
	// small, and for those the platform can usually name the code.
	if n := -int(e); n > 0 && n < 1024 {
		if s := errnoText(n); s != "" {
			return fmt.Sprintf("goffmpeg: decoder error: %d (%s)", int(e), s)
		}
	}
	if e == avErrorEOF {
		return fmt.Sprintf("goffmpeg: decoder error: %d (end of file)", int(e))
	}
	return fmt.Sprintf("goffmpeg: decoder error: %d", int(e))
}

// avErrorEOF mirrors AVERROR_EOF, which FFmpeg builds with FFERRTAG('E','O','F',' ').
// decoder_next maps it to a clean 0 before it reaches Go, so this is only for
// naming the value if it ever arrives by another route.
const avErrorEOF AVError = -('E' | 'O'<<8 | 'F'<<16 | ' '<<24)

// goffmpegAgain mirrors GOFFMPEG_AGAIN in bridge.h.
const goffmpegAgain = 3

// nextResult maps decoder_next's return code onto what Next reports. Both
// backends go through it so they classify identically — the same reason
// deviceListError exists.
func nextResult(res int) (MediaType, error) {
	// A bridge new enough to know about #4 has already folded EAGAIN into
	// goffmpegAgain. The raw-code arm below is for purego against an older
	// prebuilt libgoffmpeg, which returns the bare AVERROR: that backend
	// dlopens whatever the companion module shipped, so it can be paired
	// with a bridge predating this fix. Normalising both here means callers
	// have one thing to check, and errnoEAGAIN is the platform-correct value
	// on every target (11 on Linux, 35 on macOS).
	switch {
	case res == goffmpegAgain, res == -errnoEAGAIN:
		return MediaTypeNone, ErrAgain
	case res < 0:
		return MediaTypeNone, AVError(res)
	default:
		return MediaType(res), nil
	}
}

// openDeviceError maps the bridge's device-open result onto a Go error. Shared
// by both backends so they fail identically, as deviceListError already is.
//
// rc is the bridge's own code; averr is the AVERROR from avformat_open_input,
// or 0 when there was none to report — which is also what an older purego
// libgoffmpeg gives, since it predates the out-param entirely.
//
// There is deliberately no "the device refused this configuration" sentinel
// here, because libavdevice does not preserve enough to build an honest one.
// Only v4l2 propagates the real errno from the ioctl that refused. avfoundation
// discards it — add_video_device's return is tested for truthiness and thrown
// away, so its fail path answers AVERROR(EIO) for a rejected framerate and for
// "no capture device found" alike — and dshow answers EIO for nearly
// everything too. A sentinel keyed on EIO would fire on unrelated failures, and
// one keyed on EINVAL would never fire at all on the backend #5 was reported
// against. ListDeviceModes is the reliable answer to "what would this device
// accept"; the AVERROR here is the honest amount of detail about why the open
// failed.
func openDeviceError(format, url string, rc, averr int) error {
	switch {
	case rc == -3:
		return fmt.Errorf("goffmpeg: unknown input format %q", format)
	case rc == -7:
		return errors.New("goffmpeg: built without libavdevice")
	case rc == -2:
		return fmt.Errorf("goffmpeg: opened device %q %q, but could not read its streams", format, url)
	case averr != 0:
		return fmt.Errorf("goffmpeg: opening device %q %q: %w", format, url, AVError(averr))
	default:
		// No AVERROR to report: either the failure was not an open failure, or
		// the loaded bridge predates decoder_open_device_err.
		return fmt.Errorf("goffmpeg: opening device %q %q failed: %d", format, url, rc)
	}
}

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

// DeviceMode is one configuration a capture device will accept.
type DeviceMode struct {
	// Format is the input format this mode came from — "v4l2", "dshow",
	// "avfoundation". Options needs it: the option that selects a compressed
	// format is not spelled the same on every backend.
	Format string
	Width  int
	Height int
	// MinFPS and MaxFPS bound the framerates this mode supports. They are
	// equal for a fixed-rate mode, and both 0 when the backend reported no
	// rate — v4l2 does that for a driver without VIDIOC_ENUM_FRAMEINTERVALS.
	// A range is kept as a range because that is what the hardware says:
	// flattening 1–60fps into sixty modes would invent entries.
	MinFPS float64
	MaxFPS float64
	// PixelFormat is libav's name for the raw layout ("nv12", "yuyv422").
	// Empty when the mode is compressed, or when the device's format has no
	// libav equivalent — the mode is still usable, just not by that name.
	PixelFormat string
	// Codec is set instead of PixelFormat when the device delivers the mode
	// compressed ("mjpeg", "h264"). At most one of the two is ever set.
	//
	// They are separate fields because the distinction is not cosmetic:
	// dshow validates pixel_format strictly as a pixel layout and rejects
	// "mjpeg" outright, so the two need different options. v4l2 happens to
	// accept either through one option, and avfoundation never delivers
	// compressed at all.
	Codec string
}

// Options renders the mode as an options string OpenDevice accepts:
//
//	video_size=1920x1080,framerate=60,pixel_format=nv12
//
// The framerate is MaxFPS, the fastest the mode allows; a caller wanting a
// different rate within the range should build the string itself. Fields the
// device did not report are left out rather than guessed at.
func (m DeviceMode) Options() string {
	var parts []string
	if m.Width > 0 && m.Height > 0 {
		parts = append(parts, fmt.Sprintf("video_size=%dx%d", m.Width, m.Height))
	}
	if m.MaxFPS > 0 {
		parts = append(parts, "framerate="+strconv.FormatFloat(m.MaxFPS, 'g', -1, 64))
	}
	switch {
	case m.Codec != "":
		// dshow keeps these apart; v4l2 resolves pixel_format as a codec
		// descriptor before a pixel format, so one name serves both there.
		if m.Format == "dshow" {
			parts = append(parts, "vcodec="+m.Codec)
		} else {
			parts = append(parts, "pixel_format="+m.Codec)
		}
	case m.PixelFormat != "":
		parts = append(parts, "pixel_format="+m.PixelFormat)
	}
	return strings.Join(parts, ",")
}

// String renders the mode the way FFmpeg prints its "Supported modes" list.
func (m DeviceMode) String() string {
	s := fmt.Sprintf("%dx%d", m.Width, m.Height)
	if m.MaxFPS > 0 {
		if m.MinFPS == m.MaxFPS {
			s += fmt.Sprintf("@%gfps", m.MaxFPS)
		} else {
			s += fmt.Sprintf("@[%g %g]fps", m.MinFPS, m.MaxFPS)
		}
	}
	if m.PixelFormat != "" {
		s += " " + m.PixelFormat
	} else if m.Codec != "" {
		s += " " + m.Codec
	}
	return s
}

// ErrDeviceModesUnsupported is returned by ListDeviceModes for a format with
// no mode query behind it. Every format libavdevice offers is in this position
// by default: none of avfoundation, v4l2 or dshow implements FFmpeg's
// create_device_capabilities, so each supported backend is queried natively,
// and one that has not been written yet says so here.
var ErrDeviceModesUnsupported = errors.New("goffmpeg: no capture-mode query for this input format")

// ErrDeviceModesUnavailable is the purego counterpart of
// ErrDeviceListUnavailable: the loaded libgoffmpeg predates mode enumeration
// and exports none of the symbols. Rebuild the shared library, or use cgo.
var ErrDeviceModesUnavailable = errors.New("goffmpeg: the loaded libgoffmpeg exports no capture-mode query — rebuild the shared library, or build with cgo")

// ErrDeviceNotFound is returned by ListDeviceModes when the selector names no
// device it can reach — a v4l2 node that does not exist or cannot be opened,
// an avfoundation index past the end of the camera list.
var ErrDeviceNotFound = errors.New("goffmpeg: no such capture device")

// ListDeviceModes reports the configurations one capture device will accept.
//
// It answers the question a failed OpenDevice cannot: libavdevice knows what
// the device supports, prints it to the log, and then fails with a code that
// carries none of it (#5). Rather than guess at a framerate the device might
// take, ask:
//
//	modes, err := goffmpeg.ListDeviceModes("avfoundation", "0")
//	if err == nil && len(modes) > 0 {
//		dec, err = goffmpeg.OpenDevice("avfoundation", "0", modes[0].Options())
//	}
//
// url is the same selector OpenDevice takes: "0" under avfoundation,
// "/dev/video0" under v4l2, a "video=<name>" under dshow.
//
// A device that opens but advertises nothing is an empty slice and a nil
// error. Returns ErrDeviceModesUnsupported for a format with no query behind
// it, and ErrDeviceNotFound when the selector matches no device.
func ListDeviceModes(format, url string) ([]DeviceMode, error) {
	return listDeviceModes(format, url)
}

// deviceModesError maps the bridge's GOFFMPEG_DEVICE_MODES_* codes onto Go
// errors. Shared by both backends, as deviceListError is.
func deviceModesError(format, url string, rc int) error {
	switch rc {
	case -2:
		return fmt.Errorf("goffmpeg: unknown input format %q", format)
	case -3:
		return ErrDeviceModesUnsupported
	case -4:
		return errors.New("goffmpeg: built without libavdevice")
	case -5:
		return fmt.Errorf("goffmpeg: %q %q: %w", format, url, ErrDeviceNotFound)
	default:
		return fmt.Errorf("goffmpeg: querying %q %q modes failed: %d", format, url, rc)
	}
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
	case -5:
		return ErrDeviceListNoDevices
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
