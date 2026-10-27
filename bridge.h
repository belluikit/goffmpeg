#ifndef GOFFMPEG_BRIDGE_H
#define GOFFMPEG_BRIDGE_H

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
// Capture devices are desktop-only: the mobile targets configure FFmpeg with
// --disable-avdevice, so this header does not exist there. Those builds define
// GOFFMPEG_NO_AVDEVICE via their per-platform cgo CFLAGS.
#ifndef GOFFMPEG_NO_AVDEVICE
#include <libavdevice/avdevice.h>
#endif
#include <libavutil/frame.h>
#include <libavutil/imgutils.h>
#include <libswscale/swscale.h>
#include <libswresample/swresample.h>

typedef struct {
    AVFormatContext *fmt_ctx;
    
    // Video
    AVCodecContext *v_ctx;
    AVFrame *v_frame;
    struct SwsContext *sws_ctx;
    int v_stream_idx;
    int width, height; double framerate;

    // Audio
    AVCodecContext *a_ctx;
    AVFrame *a_frame;
    struct SwrContext *swr_ctx;
    int a_stream_idx;
    int sample_rate, channels;
    uint8_t *audio_buf;
    int audio_buf_size;

    AVPacket *pkt;
    double position;
    double duration;

    // Custom-IO playback (see bridge_reader.c). Only decoder_open_reader
    // populates these; the url and device entry points leave them NULL.
    AVIOContext *avio_ctx;
    unsigned char *avio_buf;
    int64_t reader_handle;

    // Set once decoder_next has handed out swresample's buffered tail at the
    // end of the input; cleared by a seek or a resampler rebuild (#24).
    int swr_flushed;
} Decoder;

// NOTE: decoder_purego.go mirrors this struct field-for-field in Go, and the C
// side memsets it by sizeof(Decoder) over that Go allocation. Any field added
// here MUST be added to cDecoder as well, or the memset overruns the Go object.
// decoder_struct_size reports sizeof(Decoder) so a purego test can catch the
// two drifting apart.
int decoder_struct_size(void);

// decoder_setup_streams is shared by every entry point below; it is exposed
// (rather than static) so bridge_reader.c can reuse it.
int decoder_setup_streams(Decoder *d);

// decoder_open_opts opens a file or URL with per-open demuxer and protocol
// options: a comma-separated "key=value" list in the same shape
// decoder_open_device takes, or NULL/"" for none.
//
// It exists for network inputs. http, https and HLS are the cases that need
// tuning and cannot be tuned any other way — without rw_timeout a stalled
// server hangs the open indefinitely, and without reconnect a dropped
// connection ends playback. user_agent, headers, http_persistent,
// analyzeduration and probesize are the other options that matter in practice.
//
// Options an input does not recognise are ignored rather than rejected, so one
// string can be handed to both a URL and a local path. A malformed string
// (something av_dict_parse_string cannot split) returns -8.
int decoder_open_opts(Decoder *d, const char *url, const char *options);
// decoder_open is decoder_open_opts with no options.
int decoder_open(Decoder *d, const char *url);
// decoder_open_device opens a live capture device (e.g. format="avfoundation",
// url="0") via libavdevice. options is a comma-separated "key=value" list
// passed to the input device (e.g. "framerate=30,pixel_format=nv12"), or "".
int decoder_open_device(Decoder *d, const char *format, const char *url, const char *options);
// decoder_open_device_err is decoder_open_device with the AVERROR reported.
//
// On a -1 return (avformat_open_input failed) *averr carries the code libav
// actually gave, which is the difference between "no such device", "device
// busy" and "the device refused this configuration" — the last being
// AVERROR(EINVAL) on avfoundation, and the one a caller can act on by asking
// device_modes_open what the device would accept instead.
//
// It is a separate out-param rather than the return value because the codes
// above already occupy the small negatives that errno-derived AVERRORs use
// (#5). *averr is set to 0 on success and on every non-open failure.
int decoder_open_device_err(Decoder *d, const char *format, const char *url,
                            const char *options, int *averr);
// decoder_open_reader feeds libavformat from a Go io.Reader through a custom
// AVIOContext. handle identifies the reader in the Go-side registry; pass
// seekable=0 for a pure stream, which disables the seek callback and limits
// format probing to what can be read forward. Defined in bridge_reader.c,
// which is compiled only into the cgo build — the purego shared library does
// not export this symbol.
//
// decoder_open_reader_opts takes the same options string as
// decoder_open_opts. analyzeduration and probesize matter more here than
// anywhere else: a non-seekable stream is exactly where format probing
// struggles, and raising them is the only remedy.
int decoder_open_reader_opts(Decoder *d, int64_t handle, int seekable, const char *options);
int decoder_open_reader(Decoder *d, int64_t handle, int seekable);
int decoder_seek(Decoder *d, double seconds);
// decoder_set_audio_channels rebuilds the resampler to emit `channels`
// interleaved S16 channels; 0 keeps the stream's native layout. Call before
// playback begins — rebuilding drops whatever swresample had buffered.
int decoder_set_audio_channels(Decoder *d, int channels);
// GOFFMPEG_AGAIN means no packet was ready yet and decoder_next should be
// called again. It is not an error and not an end of stream.
//
// A live capture device read non-blockingly reports "nothing yet" as
// AVERROR(EAGAIN) from av_read_frame, and on a camera that is the ordinary
// state between frames — folding it into the negative error range ended a
// capture within two frames (#4).
//
// The mapping is done here rather than in Go because EAGAIN is platform
// dependent — 11 on Linux, 35 on macOS — so only the C side, where the errno
// macro is the right one for the build, can recognise it reliably.
#define GOFFMPEG_AGAIN 3
int decoder_next(Decoder *d); // 1=Video, 2=Audio, 0=EOF, 3=no data yet, <0=Error
int decoder_get_rgba(Decoder *d, uint8_t *buffer);
int decoder_get_audio(Decoder *d, uint8_t **buffer);
void decoder_close(Decoder *d);

// Device enumeration.
//
// device_list_open asks an input device for the capture sources it can see
// (avdevice_list_input_sources) and hands back an opaque list the accessors
// below index into; device_list_free releases it. The list is returned as a
// handle rather than a marshalled blob so the cgo and purego backends read it
// the same way — purego cannot walk AVDeviceInfoList itself.
//
// Not every input format implements enumeration: v4l2 and dshow do,
// avfoundation does not, and the mobile builds have no libavdevice at all.
// Those cases are distinguishable return codes, not generic failures, because
// callers fall back to a platform default rather than giving up.
#define GOFFMPEG_DEVICE_LIST_FAILED      (-1) // enumeration ran and failed
#define GOFFMPEG_DEVICE_LIST_NO_FORMAT   (-2) // no such input format
#define GOFFMPEG_DEVICE_LIST_UNSUPPORTED (-3) // format implements no enumeration
#define GOFFMPEG_DEVICE_LIST_NO_AVDEVICE (-4) // built without libavdevice
// GOFFMPEG_DEVICE_LIST_EMPTY means the host produced no device list at all.
//
// It exists to separate "this machine has nothing to enumerate" from "our
// enumeration is broken", which the bare FAILED code cannot express. dshow is
// the case that needs it: it reports AVERROR(EIO) for a cycle that finds
// nothing, saying "Could not enumerate %s devices (or none found)" — the two
// are one code upstream, and there is no way to tell them apart from here.
//
// Two details of dshow_get_device_list make this broader than "no camera":
// the *video* cycle's EIO is deliberately tolerated and the function returns
// the *audio* cycle's result, so a machine with cameras but no microphone
// lands here too; and libavdevice frees the partially built list on any
// negative return, so devices already collected are gone regardless.
//
// So this is a statement about the host, not about a particular device class.
#define GOFFMPEG_DEVICE_LIST_EMPTY       (-5) // nothing enumerable on this host

// device_list_open returns the device count (>= 0) and stores the list in
// *list, or one of the GOFFMPEG_DEVICE_LIST_* codes with *list left NULL.
int device_list_open(const char *format, void **list);
int device_list_count(void *list);
// device_list_name returns the selector to pass to decoder_open_device as its
// url ("/dev/video0" under v4l2, a DirectShow unique name under dshow — which
// still wants the "video="/"audio=" prefix the caller adds); NULL if i is out
// of range. The string belongs to the list and dies with it.
const char *device_list_name(void *list, int i);
const char *device_list_description(void *list, int i); // human-friendly name
// device_list_media_types returns a bitmask of what the device can provide:
// bit 0 video, bit 1 audio. GOFFMPEG_DEVICE_MEDIA_UNKNOWN means nothing could
// be determined, which is a different answer from 0 ("determined: neither") —
// a caller must try an unknown device and must skip a known-non-video one.
//
// dshow fills the media types in itself. v4l2 does not, so the bridge queries
// each node's V4L2_CAP_* directly: its listing includes metadata nodes that
// block forever when opened, and this is what lets a caller avoid them.
#define GOFFMPEG_DEVICE_MEDIA_UNKNOWN (-1)
int device_list_media_types(void *list, int i);
int device_list_default(void *list); // index of the default device, or -1
void device_list_free(void *list);

// Capture-mode enumeration.
//
// device_modes_open asks one device what configurations it will accept, so a
// caller refused by OpenDevice can find out what to ask for instead. This is
// the information libavdevice already has and only prints: avfoundation logs
// "Supported modes:" and a list, then fails the open with nothing a program
// can read (#5).
//
// It is separate from device_list_* because it is per device rather than per
// format, and because libavdevice cannot supply it. None of avfoundation, v4l2
// or dshow implements FFmpeg's create_device_capabilities, so each backend is
// queried natively here: v4l2 by ioctl, avfoundation through AVCaptureDevice
// (bridge_avf_darwin.m), dshow through IAMStreamConfig.
//
// url is the same selector decoder_open_device takes — "0" for avfoundation,
// "/dev/video0" for v4l2, a dshow name with its "video=" prefix.
//
// A mode is a resolution, a framerate range and a pixel format. Ranges rather
// than single rates because that is what the hardware reports: a camera
// commonly offers 1920x1080 at anything from 1 to 60fps as one entry, and
// flattening that would invent modes it never advertised.
#define GOFFMPEG_DEVICE_MODES_FAILED      (-1) // query ran and failed
#define GOFFMPEG_DEVICE_MODES_NO_FORMAT   (-2) // no such input format
#define GOFFMPEG_DEVICE_MODES_UNSUPPORTED (-3) // no mode query for this format
#define GOFFMPEG_DEVICE_MODES_NO_AVDEVICE (-4) // built without libavdevice
#define GOFFMPEG_DEVICE_MODES_NO_DEVICE   (-5) // no such device, or not readable

// device_modes_open returns the mode count (>= 0) and stores the list in
// *modes, or one of the codes above with *modes left NULL. A device that opens
// but advertises nothing is 0 with an empty list, not an error.
int device_modes_open(const char *format, const char *url, void **modes);
int device_modes_count(void *modes);
int device_modes_width(void *modes, int i);
int device_modes_height(void *modes, int i);
// The framerate bounds are equal for a fixed-rate mode. 0 means the backend
// reported no rate for this entry, which v4l2 does for a device whose driver
// omits VIDIOC_ENUM_FRAMEINTERVALS.
double device_modes_min_fps(void *modes, int i);
double device_modes_max_fps(void *modes, int i);
// The pixel format as libav names it ("nv12", "yuyv422"), or NULL if the
// backend's format has no libav equivalent. The string belongs to the list and
// dies with it.
const char *device_modes_pixel_format(void *modes, int i);
// device_modes_is_codec reports whether device_modes_pixel_format named a
// decoder rather than a pixel layout — "mjpeg" and "h264" rather than "nv12".
// A caller has to know, because the option that selects one is not the option
// that selects the other on every backend.
int device_modes_is_codec(void *modes, int i);
void device_modes_free(void *modes);

// Internal to the mode backends, not part of the Go-facing surface.
//
// device_modes_append adds one mode to a list device_modes_open is building,
// ignoring an exact duplicate. It is declared here rather than kept static in
// bridge.c because the AVFoundation query lives in its own translation unit
// (bridge_avf_darwin.m) and has to reach it. Returns 0, or -1 if the list
// could not grow.
int device_modes_append(void *modes, int width, int height,
                        double min_fps, double max_fps, const char *pixfmt,
                        int is_codec);

#if defined(__APPLE__) && !defined(GOFFMPEG_NO_AVDEVICE)
// goffmpeg_avf_collect_modes fills modes from AVCaptureDevice. Implemented in
// bridge_avf_darwin.m; returns 0, or a GOFFMPEG_DEVICE_MODES_* code.
int goffmpeg_avf_collect_modes(const char *url, void *modes);
#endif

#endif
