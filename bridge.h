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
} Decoder;

// NOTE: decoder_purego.go mirrors this struct field-for-field in Go, and the C
// side memsets it by sizeof(Decoder) over that Go allocation. Any field added
// here MUST be added to cDecoder as well, or the memset overruns the Go object.

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

#endif
