#include "bridge.h"
#include <libavutil/mem.h>
#include <libavutil/channel_layout.h>

// find_video_stream is av_find_best_stream for video, minus attached pictures.
// The cover art an MP3, M4A or FLAC embeds is a one-packet video stream, and
// av_find_best_stream ranks it like any other: picked, it reports the cover's
// size and yields one frame, so a player waits for video that never comes.
// Those streams are hidden for the call rather than ranked by hand, so a file
// with real video still gets FFmpeg's own choice among the rest. They are
// marked AVMEDIA_TYPE_NB, a type no real stream has, so the restore finds
// exactly these.
static int find_video_stream(AVFormatContext *fmt) {
    for (unsigned i = 0; i < fmt->nb_streams; i++) {
        AVStream *st = fmt->streams[i];
        if (st->codecpar->codec_type == AVMEDIA_TYPE_VIDEO &&
            (st->disposition & AV_DISPOSITION_ATTACHED_PIC))
            st->codecpar->codec_type = AVMEDIA_TYPE_NB;
    }
    int idx = av_find_best_stream(fmt, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    for (unsigned i = 0; i < fmt->nb_streams; i++) {
        if (fmt->streams[i]->codecpar->codec_type == AVMEDIA_TYPE_NB)
            fmt->streams[i]->codecpar->codec_type = AVMEDIA_TYPE_VIDEO;
    }
    return idx;
}

// decoder_setup_streams finds the best video/audio streams, opens their
// decoders, and prepares the swscale/swresample converters. Shared by
// decoder_open (files/URLs), decoder_open_device (capture devices) and
// decoder_open_reader (custom IO) so every entry point produces identical
// decoding behaviour.
int decoder_setup_streams(Decoder *d) {
    if (avformat_find_stream_info(d->fmt_ctx, NULL) < 0) return -2;

    // Video stream
    d->v_stream_idx = find_video_stream(d->fmt_ctx);
    if (d->v_stream_idx >= 0) {
        AVStream *st = d->fmt_ctx->streams[d->v_stream_idx];
        const AVCodec *codec = avcodec_find_decoder(st->codecpar->codec_id);
        if (codec) {
            d->v_ctx = avcodec_alloc_context3(codec);
            avcodec_parameters_to_context(d->v_ctx, st->codecpar);
            d->v_ctx->thread_count = 0;
            if (avcodec_open2(d->v_ctx, codec, NULL) >= 0) {
                d->width = d->v_ctx->width;
                d->height = d->v_ctx->height;
                d->framerate = av_q2d(st->avg_frame_rate);

                d->v_frame = av_frame_alloc();
                // The scaler is built lazily in decoder_get_rgba from the
                // frame that actually arrives. Probing does not always settle
                // the pixel format up front — a forward-only stream whose
                // index sits at the end of the file reports AV_PIX_FMT_NONE —
                // and handing that to sws_getContext trips an assertion that
                // abort()s the process instead of returning an error.
            }
        }
    }

    // Audio stream
    d->a_stream_idx = av_find_best_stream(d->fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, NULL, 0);
    if (d->a_stream_idx >= 0) {
        AVStream *st = d->fmt_ctx->streams[d->a_stream_idx];
        const AVCodec *codec = avcodec_find_decoder(st->codecpar->codec_id);
        if (codec) {
            d->a_ctx = avcodec_alloc_context3(codec);
            avcodec_parameters_to_context(d->a_ctx, st->codecpar);
            if (avcodec_open2(d->a_ctx, codec, NULL) >= 0) {
                d->sample_rate = d->a_ctx->sample_rate;
                d->a_frame = av_frame_alloc();
                // 0 = keep the stream's own layout. Callers that need a
                // specific count (because the audio device is already open at
                // one) ask for it via decoder_set_audio_channels.
                decoder_set_audio_channels(d, 0);
            }
        }
    }

    if (d->fmt_ctx->duration != AV_NOPTS_VALUE) {
        d->duration = (double)d->fmt_ctx->duration / AV_TIME_BASE;
    }

    d->pkt = av_packet_alloc();
    return 0;
}

// decoder_set_audio_channels builds (or rebuilds) the resampler so decoded
// audio comes out with `channels` interleaved S16 channels. Pass 0 to keep the
// stream's native layout — 5.1 stays 5.1.
//
// swresample performs the mix itself, which applies the proper downmix
// coefficients rather than the naive channel-dropping a hand-rolled mixer
// would. Rebuilding discards whatever swr had buffered, so call this before
// playback starts rather than mid-stream.
int decoder_set_audio_channels(Decoder *d, int channels) {
    if (!d->a_ctx) return -1;

    AVChannelLayout out_layout;
    memset(&out_layout, 0, sizeof(out_layout));
    if (channels > 0) {
        av_channel_layout_default(&out_layout, channels);
    } else if (d->a_ctx->ch_layout.nb_channels > 0) {
        av_channel_layout_copy(&out_layout, &d->a_ctx->ch_layout);
    } else {
        // Decoder never reported a layout; stereo is the safe assumption.
        av_channel_layout_default(&out_layout, 2);
    }

    if (d->swr_ctx) swr_free(&d->swr_ctx);

    int ret = swr_alloc_set_opts2(&d->swr_ctx,
                                  &out_layout, AV_SAMPLE_FMT_S16, d->sample_rate,
                                  &d->a_ctx->ch_layout, d->a_ctx->sample_fmt, d->a_ctx->sample_rate,
                                  0, NULL);
    if (ret < 0) {
        av_channel_layout_uninit(&out_layout);
        return ret;
    }
    ret = swr_init(d->swr_ctx);
    if (ret < 0) {
        swr_free(&d->swr_ctx);
        av_channel_layout_uninit(&out_layout);
        return ret;
    }

    d->channels = out_layout.nb_channels;
    av_channel_layout_uninit(&out_layout);
    return 0;
}

int decoder_open_opts(Decoder *d, const char *url, const char *options) {
    memset(d, 0, sizeof(Decoder));
    d->v_stream_idx = -1;
    d->a_stream_idx = -1;

    AVDictionary *opts = NULL;
    if (options && options[0] &&
        av_dict_parse_string(&opts, options, "=", ",", 0) < 0) {
        av_dict_free(&opts);
        return -8;
    }
    // Entries libavformat does not recognise are left in opts rather than
    // rejected, and that is the right behaviour here: an option is consumed by
    // whichever layer claims it, so "reconnect=1" is meaningful for http and
    // simply unused for a local file. The caller passes one string for both.
    int rc = avformat_open_input(&d->fmt_ctx, url, NULL, &opts);
    av_dict_free(&opts);
    if (rc < 0) return -1;

    return decoder_setup_streams(d);
}

int decoder_open(Decoder *d, const char *url) {
    return decoder_open_opts(d, url, NULL);
}

int decoder_open_device_err(Decoder *d, const char *format, const char *url,
                            const char *options, int *averr) {
    if (averr) *averr = 0;
#ifdef GOFFMPEG_NO_AVDEVICE
    (void)d; (void)format; (void)url; (void)options;
    return -7; // built without libavdevice
#else
    memset(d, 0, sizeof(Decoder));
    d->v_stream_idx = -1;
    d->a_stream_idx = -1;

    avdevice_register_all();
    const AVInputFormat *ifmt = av_find_input_format(format);
    if (!ifmt) return -3;

    AVDictionary *opts = NULL;
    if (options && options[0]) av_dict_parse_string(&opts, options, "=", ",", 0);
    int rc = avformat_open_input(&d->fmt_ctx, url, ifmt, &opts);
    av_dict_free(&opts);
    if (rc < 0) {
        // The AVERROR goes out through averr rather than the return value.
        // This function's own codes occupy -1, -2, -3, -7 and -8, and errno
        // derived AVERRORs land on exactly those numbers — AVERROR(ENOENT) is
        // -2, which a device open produces readily — so the two cannot share
        // one channel without becoming ambiguous (#5).
        if (averr) *averr = rc;
        return -1;
    }

    return decoder_setup_streams(d);
#endif
}

int decoder_open_device(Decoder *d, const char *format, const char *url, const char *options) {
    return decoder_open_device_err(d, format, url, options, NULL);
}

int decoder_seek(Decoder *d, double seconds) {
    if (seconds < 0) seconds = 0;
    int64_t ts = (int64_t)(seconds * AV_TIME_BASE);
    int ret = av_seek_frame(d->fmt_ctx, -1, ts, AVSEEK_FLAG_BACKWARD);
    if (ret < 0) return ret;
    if (d->v_ctx) avcodec_flush_buffers(d->v_ctx);
    if (d->a_ctx) avcodec_flush_buffers(d->a_ctx);
    d->position = seconds;
    return 0;
}

int decoder_next(Decoder *d) {
    int ret;
    while ((ret = av_read_frame(d->fmt_ctx, d->pkt)) >= 0) {
        if (d->pkt->stream_index == d->v_stream_idx) {
            ret = avcodec_send_packet(d->v_ctx, d->pkt);
            av_packet_unref(d->pkt);
            if (ret < 0) return ret;
            ret = avcodec_receive_frame(d->v_ctx, d->v_frame);
            if (ret == AVERROR(EAGAIN)) continue;
            if (ret == 0) {
                // Adopt the real geometry once a frame arrives; probing may
                // not have resolved it, and callers size their buffers from
                // the values reported here.
                if (d->width <= 0 || d->height <= 0) {
                    d->width = d->v_frame->width;
                    d->height = d->v_frame->height;
                }
                AVStream *st = d->fmt_ctx->streams[d->v_stream_idx];
                int64_t pts = d->v_frame->best_effort_timestamp;
                if (pts == AV_NOPTS_VALUE) pts = d->v_frame->pts;
                if (pts == AV_NOPTS_VALUE) pts = 0;
                d->position = (double)pts * av_q2d(st->time_base);
                return 1;
            }
        } else if (d->pkt->stream_index == d->a_stream_idx) {
            ret = avcodec_send_packet(d->a_ctx, d->pkt);
            av_packet_unref(d->pkt);
            if (ret < 0) return ret;
            ret = avcodec_receive_frame(d->a_ctx, d->a_frame);
            if (ret == AVERROR(EAGAIN)) continue;
            if (ret == 0) {
                AVStream *st = d->fmt_ctx->streams[d->a_stream_idx];
                int64_t pts = d->a_frame->best_effort_timestamp;
                if (pts == AV_NOPTS_VALUE) pts = d->a_frame->pts;
                if (pts == AV_NOPTS_VALUE) pts = 0;
                d->position = (double)pts * av_q2d(st->time_base);
                return 2;
            }
        } else {
            av_packet_unref(d->pkt);
        }
    }
    // The loop above exits only on a negative av_read_frame. EOF is the clean
    // end of a file; EAGAIN is a non-blocking device saying "not yet" and must
    // not be confused with either an end or a failure (#4).
    if (ret == AVERROR_EOF) return 0;
    if (ret == AVERROR(EAGAIN)) return GOFFMPEG_AGAIN;
    return ret;
}

int decoder_get_rgba(Decoder *d, uint8_t *buffer) {
    if (!d->v_frame) return -1;
    if (d->v_frame->width <= 0 || d->v_frame->height <= 0) return -1;
    if (d->v_frame->format == AV_PIX_FMT_NONE) return -1;

    // Build the scaler from the decoded frame rather than from the format
    // guessed at open time. sws_getCachedContext reuses the existing context
    // when nothing changed and rebuilds it when the stream switches size or
    // pixel format mid-playback.
    d->sws_ctx = sws_getCachedContext(d->sws_ctx,
                                      d->v_frame->width, d->v_frame->height, d->v_frame->format,
                                      d->v_frame->width, d->v_frame->height, AV_PIX_FMT_RGBA,
                                      SWS_FAST_BILINEAR, NULL, NULL, NULL);
    if (!d->sws_ctx) return -1;

    // The caller sized its buffer from VideoInfo, so a late resolution change
    // would overrun it; report the mismatch instead of scaling into it.
    if (d->v_frame->width != d->width || d->v_frame->height != d->height) return -2;

    uint8_t *dst_data[4] = { buffer, NULL, NULL, NULL };
    int dst_linesize[4] = { d->width * 4, 0, 0, 0 };
    return sws_scale(d->sws_ctx, (const uint8_t * const*)d->v_frame->data, d->v_frame->linesize,
                     0, d->v_frame->height, dst_data, dst_linesize);
}

int decoder_get_audio(Decoder *d, uint8_t **buffer) {
    if (!d->swr_ctx || !d->a_frame) return -1;
    if (d->channels <= 0) return -1;
    int out_samples = av_rescale_rnd(swr_get_delay(d->swr_ctx, d->a_ctx->sample_rate) + d->a_frame->nb_samples,
                                     d->sample_rate, d->a_ctx->sample_rate, AV_ROUND_UP);
    int out_size = av_samples_get_buffer_size(NULL, d->channels, out_samples, AV_SAMPLE_FMT_S16, 0);
    if (out_size < 0) return out_size;
    if (out_size > d->audio_buf_size) {
        av_freep(&d->audio_buf);
        d->audio_buf = av_malloc(out_size);
        if (!d->audio_buf) {
            d->audio_buf_size = 0;
            return -1;
        }
        d->audio_buf_size = out_size;
    }
    int ret = swr_convert(d->swr_ctx, &d->audio_buf, out_samples,
                          (const uint8_t **)d->a_frame->data, d->a_frame->nb_samples);
    if (ret < 0) return ret;
    *buffer = d->audio_buf;
    // Bytes produced: samples x channels x 2 (S16). This was hardcoded to
    // stereo, which would have truncated every multichannel buffer.
    return ret * d->channels * 2;
}

void decoder_close(Decoder *d) {
    if (d->sws_ctx) sws_freeContext(d->sws_ctx);
    if (d->swr_ctx) swr_free(&d->swr_ctx);
    if (d->v_frame) av_frame_free(&d->v_frame);
    if (d->a_frame) av_frame_free(&d->a_frame);
    if (d->audio_buf) av_freep(&d->audio_buf);
    if (d->v_ctx) avcodec_free_context(&d->v_ctx);
    if (d->a_ctx) avcodec_free_context(&d->a_ctx);
    if (d->fmt_ctx) avformat_close_input(&d->fmt_ctx);
    if (d->pkt) av_packet_free(&d->pkt);
    // Custom IO is not owned by fmt_ctx, so avformat_close_input leaves it
    // behind. Free the AVIOContext's current buffer rather than the one we
    // handed it — avio may have reallocated it while probing.
    if (d->avio_ctx) {
        av_freep(&d->avio_ctx->buffer);
        avio_context_free(&d->avio_ctx);
        d->avio_buf = NULL;
    }
}

// ---------------------------------------------------------------------------
// Device enumeration (see bridge.h for the contract).
// ---------------------------------------------------------------------------

#ifdef GOFFMPEG_NO_AVDEVICE

int device_list_open(const char *format, void **list) {
    (void)format;
    if (list) *list = NULL;
    return GOFFMPEG_DEVICE_LIST_NO_AVDEVICE;
}
int device_list_count(void *list) { (void)list; return 0; }
const char *device_list_name(void *list, int i) { (void)list; (void)i; return NULL; }
const char *device_list_description(void *list, int i) { (void)list; (void)i; return NULL; }
int device_list_media_types(void *list, int i) { (void)list; (void)i; return 0; }
int device_list_default(void *list) { (void)list; return -1; }
void device_list_free(void *list) { (void)list; }

int device_modes_open(const char *format, const char *url, void **modes) {
    (void)format; (void)url;
    if (modes) *modes = NULL;
    return GOFFMPEG_DEVICE_MODES_NO_AVDEVICE;
}
int device_modes_count(void *modes) { (void)modes; return 0; }
int device_modes_width(void *modes, int i) { (void)modes; (void)i; return 0; }
int device_modes_height(void *modes, int i) { (void)modes; (void)i; return 0; }
double device_modes_min_fps(void *modes, int i) { (void)modes; (void)i; return 0; }
double device_modes_max_fps(void *modes, int i) { (void)modes; (void)i; return 0; }
const char *device_modes_pixel_format(void *modes, int i) { (void)modes; (void)i; return NULL; }
int device_modes_is_codec(void *modes, int i) { (void)modes; (void)i; return 0; }
void device_modes_free(void *modes) { (void)modes; }

#else

// DeviceList pairs libav's list with the media mask computed per device. The
// mask cannot live in the AVDeviceInfoList — that is libav's allocation, and
// for v4l2 the mask is something we work out ourselves (see v4l2_media_mask).
typedef struct {
    AVDeviceInfoList *list;
    int *media; // one entry per device; GOFFMPEG_DEVICE_MEDIA_UNKNOWN if unreported
} DeviceList;

// device_at bounds-checks an index against the list. Every accessor goes
// through it, so an out-of-range index from Go is an empty answer rather than
// a walk off the end of the array.
static AVDeviceInfo *device_at(void *list, int i) {
    DeviceList *dl = (DeviceList *)list;
    if (!dl || !dl->list || i < 0 || i >= dl->list->nb_devices) return NULL;
    return dl->list->devices[i];
}

// libav_media_mask reduces what the device itself reported to bit flags, so
// the Go side needs no libav enum of its own. dshow fills this in; v4l2 does
// not, which is what v4l2_media_mask exists for.
static int libav_media_mask(const AVDeviceInfo *d) {
    if (!d || d->nb_media_types <= 0) return GOFFMPEG_DEVICE_MEDIA_UNKNOWN;
    int mask = 0;
    for (int k = 0; k < d->nb_media_types; k++) {
        if (d->media_types[k] == AVMEDIA_TYPE_VIDEO) mask |= 1 << 0;
        else if (d->media_types[k] == AVMEDIA_TYPE_AUDIO) mask |= 1 << 1;
    }
    return mask;
}

#ifdef __linux__
#include <fcntl.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include <linux/videodev2.h>

// v4l2_enumerates_formats asks whether this node can produce frames of the
// given buffer type. A capture node answers with its first pixel format; a
// metadata node has none and fails with EINVAL.
//
// This, rather than the capability bits, is the decisive test. `capabilities`
// is the union across every node the physical device owns, so a metadata node
// claims VIDEO_CAPTURE there too; `device_caps` narrows it to this node but is
// only meaningful when V4L2_CAP_DEVICE_CAPS is set, and drivers that leave it
// unset (a VM's virtual camera, for one) send us straight back to the union.
// The format list belongs to the node either way.
static int v4l2_enumerates_formats(int fd, unsigned int type) {
    struct v4l2_fmtdesc fmt;
    memset(&fmt, 0, sizeof(fmt));
    fmt.index = 0;
    fmt.type = type;
    return ioctl(fd, VIDIOC_ENUM_FMT, &fmt) == 0;
}

// v4l2_media_mask asks a /dev/video node what it can actually do.
//
// v4l2's enumeration reports every node and no media types, but the nodes are
// not interchangeable: one UVC camera commonly claims two, of which the second
// carries only metadata. Opening that one does not fail — it *blocks
// indefinitely* inside avformat_open_input, so a caller walking the listing
// hangs rather than moving on. Distinguishing them here is what keeps that
// node out of a caller's hands.
static int v4l2_media_mask(const char *node) {
    if (!node) return GOFFMPEG_DEVICE_MEDIA_UNKNOWN;
    int fd = open(node, O_RDWR | O_NONBLOCK);
    if (fd < 0) return GOFFMPEG_DEVICE_MEDIA_UNKNOWN; // busy or not ours to judge

    struct v4l2_capability cap;
    memset(&cap, 0, sizeof(cap));
    int mask = GOFFMPEG_DEVICE_MEDIA_UNKNOWN;
    if (ioctl(fd, VIDIOC_QUERYCAP, &cap) == 0) {
        unsigned int caps = (cap.capabilities & V4L2_CAP_DEVICE_CAPS)
                                ? cap.device_caps
                                : cap.capabilities;
        mask = 0;
        // Both halves must agree: the node must claim capture *and* offer a
        // format for it. The claim alone is not trustworthy (see above), and
        // the format list alone would misread a node whose capture side is
        // real but disabled.
        if ((caps & V4L2_CAP_VIDEO_CAPTURE) &&
            v4l2_enumerates_formats(fd, V4L2_BUF_TYPE_VIDEO_CAPTURE)) {
            mask |= 1 << 0;
        } else if ((caps & V4L2_CAP_VIDEO_CAPTURE_MPLANE) &&
                   v4l2_enumerates_formats(fd, V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE)) {
            mask |= 1 << 0;
        }
    }
    close(fd);
    return mask;
}

// v4l2_pixfmt_name maps a V4L2 fourcc to the name v4l2's pixel_format option
// takes, mirroring ff_fmt_conversion_table in FFmpeg's v4l2-common.c.
//
// Mirroring rather than inventing is the whole point: the option is resolved
// against that table, so a name outside it is one OpenDevice would reject, and
// DeviceMode.Options() would then hand back a mode that cannot be opened.
//
// Compressed formats come back as codec names ("mjpeg", "h264") rather than
// pixel formats. That is not a special case for the caller: v4l2 declares
// pixel_format and input_format at the same struct offset and resolves either
// as a codec descriptor first and a pixel format second, so one option name
// carries both.
//
// The #ifdefs mirror the table's own, since these fourccs appeared in
// videodev2.h at different times and the build's kernel headers set the floor.
static const char *v4l2_pixfmt_name(uint32_t f) {
    switch (f) {
        case V4L2_PIX_FMT_YUV420:  return "yuv420p";
        case V4L2_PIX_FMT_YVU420:  return "yuv420p";
        case V4L2_PIX_FMT_YUV422P: return "yuv422p";
        case V4L2_PIX_FMT_YUYV:    return "yuyv422";
        case V4L2_PIX_FMT_UYVY:    return "uyvy422";
        case V4L2_PIX_FMT_YUV411P: return "yuv411p";
        case V4L2_PIX_FMT_YUV410:  return "yuv410p";
        case V4L2_PIX_FMT_YVU410:  return "yuv410p";
        case V4L2_PIX_FMT_RGB555:  return "rgb555le";
        case V4L2_PIX_FMT_RGB555X: return "rgb555be";
        case V4L2_PIX_FMT_RGB565:  return "rgb565le";
        case V4L2_PIX_FMT_RGB565X: return "rgb565be";
        case V4L2_PIX_FMT_BGR24:   return "bgr24";
        case V4L2_PIX_FMT_RGB24:   return "rgb24";
#ifdef V4L2_PIX_FMT_XBGR32
        case V4L2_PIX_FMT_XBGR32:  return "bgr0";
        case V4L2_PIX_FMT_XRGB32:  return "0rgb";
        case V4L2_PIX_FMT_ABGR32:  return "bgra";
        case V4L2_PIX_FMT_ARGB32:  return "argb";
#endif
        case V4L2_PIX_FMT_BGR32:   return "bgr0";
        case V4L2_PIX_FMT_RGB32:   return "0rgb";
        case V4L2_PIX_FMT_GREY:    return "gray";
#ifdef V4L2_PIX_FMT_Y16
        case V4L2_PIX_FMT_Y16:     return "gray16le";
#endif
        case V4L2_PIX_FMT_NV12:    return "nv12";
#ifdef V4L2_PIX_FMT_NV16
        case V4L2_PIX_FMT_NV16:    return "nv16";
#endif
#ifdef V4L2_PIX_FMT_NV24
        case V4L2_PIX_FMT_NV24:    return "nv24";
#endif
        case V4L2_PIX_FMT_MJPEG:   return "mjpeg";
        case V4L2_PIX_FMT_JPEG:    return "mjpeg";
#ifdef V4L2_PIX_FMT_H264
        case V4L2_PIX_FMT_H264:    return "h264";
#endif
#ifdef V4L2_PIX_FMT_MPEG4
        case V4L2_PIX_FMT_MPEG4:   return "mpeg4";
#endif
        default:                   return NULL;
    }
}

// v4l2_pixfmt_is_codec says whether v4l2_pixfmt_name returned a decoder name
// rather than a pixel layout. The rows of ff_fmt_conversion_table carrying a
// codec_id are exactly these, and v4l2 resolves pixel_format as a codec
// descriptor before a pixel format, so one option name still carries both.
static int v4l2_pixfmt_is_codec(uint32_t f) {
    switch (f) {
        case V4L2_PIX_FMT_MJPEG:
        case V4L2_PIX_FMT_JPEG:
#ifdef V4L2_PIX_FMT_H264
        case V4L2_PIX_FMT_H264:
#endif
#ifdef V4L2_PIX_FMT_MPEG4
        case V4L2_PIX_FMT_MPEG4:
#endif
            return 1;
        default:
            return 0;
    }
}

// v4l2_collect_intervals adds one entry per framerate the node offers for this
// size, or a single rate-less entry when it offers none.
//
// A v4l2 interval is time per frame, so the framerate is its reciprocal and
// the *smallest* interval is the *highest* rate — which is why the stepwise
// bounds cross over on the way out.
static void v4l2_collect_intervals(int fd, void *modes, uint32_t pixfmt,
                                   int w, int h, const char *name, int is_codec) {
    struct v4l2_frmivalenum fi;
    unsigned int found = 0;

    for (unsigned int i = 0;; i++) {
        memset(&fi, 0, sizeof(fi));
        fi.index = i;
        fi.pixel_format = pixfmt;
        fi.width = (unsigned int)w;
        fi.height = (unsigned int)h;
        if (ioctl(fd, VIDIOC_ENUM_FRAMEINTERVALS, &fi) != 0) break;
        found++;

        if (fi.type == V4L2_FRMIVAL_TYPE_DISCRETE) {
            if (fi.discrete.numerator == 0) continue;
            double fps = (double)fi.discrete.denominator / (double)fi.discrete.numerator;
            device_modes_append(modes, w, h, fps, fps, name, is_codec);
            continue;
        }

        // Stepwise and continuous are one entry describing a whole range.
        double lo = 0, hi = 0;
        if (fi.stepwise.max.denominator && fi.stepwise.max.numerator)
            lo = (double)fi.stepwise.max.denominator / (double)fi.stepwise.max.numerator;
        if (fi.stepwise.min.denominator && fi.stepwise.min.numerator)
            hi = (double)fi.stepwise.min.denominator / (double)fi.stepwise.min.numerator;
        device_modes_append(modes, w, h, lo, hi, name, is_codec);
        break;
    }

    // Some drivers implement neither interval ioctl — a VM's virtual camera is
    // the case seen here. The size is still real and worth reporting; 0fps is
    // documented as "not reported" rather than "zero".
    if (found == 0) device_modes_append(modes, w, h, 0, 0, name, is_codec);
}

// v4l2_collect_modes walks the node's formats, then each format's frame sizes,
// then each size's intervals — the three-level enumeration v4l2 requires.
static int v4l2_collect_modes(const char *node, void *modes) {
    if (!node) return GOFFMPEG_DEVICE_MODES_NO_DEVICE;

    // Same open as v4l2_media_mask: non-blocking so a metadata node cannot
    // wedge the caller, and read/write because some drivers refuse O_RDONLY.
    int fd = open(node, O_RDWR | O_NONBLOCK);
    if (fd < 0) return GOFFMPEG_DEVICE_MODES_NO_DEVICE;

    for (unsigned int fi = 0;; fi++) {
        struct v4l2_fmtdesc fmt;
        memset(&fmt, 0, sizeof(fmt));
        fmt.index = fi;
        fmt.type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
        if (ioctl(fd, VIDIOC_ENUM_FMT, &fmt) != 0) break;

        const char *name = v4l2_pixfmt_name(fmt.pixelformat);
        int is_codec = v4l2_pixfmt_is_codec(fmt.pixelformat);

        for (unsigned int si = 0;; si++) {
            struct v4l2_frmsizeenum fs;
            memset(&fs, 0, sizeof(fs));
            fs.index = si;
            fs.pixel_format = fmt.pixelformat;
            if (ioctl(fd, VIDIOC_ENUM_FRAMESIZES, &fs) != 0) break;

            if (fs.type == V4L2_FRMSIZE_TYPE_DISCRETE) {
                v4l2_collect_intervals(fd, modes, fmt.pixelformat,
                                       (int)fs.discrete.width,
                                       (int)fs.discrete.height, name, is_codec);
                continue;
            }

            // Stepwise and continuous describe a range in one entry, so there
            // is no list to walk. Both endpoints are reported: any size
            // between them is valid, and the endpoints are what a caller
            // picking a mode can actually name.
            v4l2_collect_intervals(fd, modes, fmt.pixelformat,
                                   (int)fs.stepwise.max_width,
                                   (int)fs.stepwise.max_height, name, is_codec);
            v4l2_collect_intervals(fd, modes, fmt.pixelformat,
                                   (int)fs.stepwise.min_width,
                                   (int)fs.stepwise.min_height, name, is_codec);
            break;
        }
    }

    close(fd);
    return 0;
}
#endif

int device_list_open(const char *format, void **list) {
    if (!list) return GOFFMPEG_DEVICE_LIST_FAILED;
    *list = NULL;

    avdevice_register_all();
    const AVInputFormat *ifmt = av_find_input_format(format);
    if (!ifmt) return GOFFMPEG_DEVICE_LIST_NO_FORMAT;

    AVDeviceInfoList *inner = NULL;
    // NULL device name: enumerate everything the format can see, with no
    // device-specific options.
    int rc = avdevice_list_input_sources(ifmt, NULL, NULL, &inner);
    if (rc < 0) {
        if (inner) avdevice_free_list_devices(&inner);
        // A format with no get_device_list callback (avfoundation) reports
        // ENOSYS; that is a capability answer, not an error, so it keeps its
        // own code. EIO is dshow's "or none found" — a fact about the host
        // rather than a fault, and likewise distinguishable.
        if (rc == AVERROR(ENOSYS)) return GOFFMPEG_DEVICE_LIST_UNSUPPORTED;
        if (rc == AVERROR(EIO))    return GOFFMPEG_DEVICE_LIST_EMPTY;
        return GOFFMPEG_DEVICE_LIST_FAILED;
    }

    DeviceList *dl = av_mallocz(sizeof(DeviceList));
    if (!dl) {
        avdevice_free_list_devices(&inner);
        return GOFFMPEG_DEVICE_LIST_FAILED;
    }
    dl->list = inner;

    if (inner->nb_devices > 0) {
        dl->media = av_malloc_array(inner->nb_devices, sizeof(int));
        if (!dl->media) {
            avdevice_free_list_devices(&inner);
            av_free(dl);
            return GOFFMPEG_DEVICE_LIST_FAILED;
        }
#ifdef __linux__
        int is_v4l2 = format && !strcmp(format, "v4l2");
#endif
        for (int i = 0; i < inner->nb_devices; i++) {
            const AVDeviceInfo *d = inner->devices[i];
            dl->media[i] = libav_media_mask(d);
#ifdef __linux__
            if (is_v4l2 && dl->media[i] == GOFFMPEG_DEVICE_MEDIA_UNKNOWN && d) {
                dl->media[i] = v4l2_media_mask(d->device_name);
            }
#endif
        }
    }

    *list = dl;
    return rc; // device count
}

int device_list_count(void *list) {
    DeviceList *dl = (DeviceList *)list;
    return (dl && dl->list) ? dl->list->nb_devices : 0;
}

const char *device_list_name(void *list, int i) {
    AVDeviceInfo *d = device_at(list, i);
    return d ? d->device_name : NULL;
}

const char *device_list_description(void *list, int i) {
    AVDeviceInfo *d = device_at(list, i);
    return d ? d->device_description : NULL;
}

int device_list_media_types(void *list, int i) {
    DeviceList *dl = (DeviceList *)list;
    if (!device_at(list, i) || !dl->media) return GOFFMPEG_DEVICE_MEDIA_UNKNOWN;
    return dl->media[i];
}

int device_list_default(void *list) {
    DeviceList *dl = (DeviceList *)list;
    return (dl && dl->list) ? dl->list->default_device : -1;
}

void device_list_free(void *list) {
    DeviceList *dl = (DeviceList *)list;
    if (!dl) return;
    if (dl->list) avdevice_free_list_devices(&dl->list);
    av_free(dl->media);
    av_free(dl);
}

#ifdef _WIN32
// DirectShow capture-mode enumeration.
//
// COBJMACROS before dshow.h is what gives the IFoo_Method(p, ...) spellings;
// without it these interfaces are only usable from C++. FFmpeg's own dshow.c
// does the same, and the GUIDs come from -lstrmiids, which the Windows link
// already pulls in for libavdevice.
#define COBJMACROS
#include <dshow.h>
// VIDEOINFOHEADER2 and FORMAT_VideoInfo2 are declared here rather than in
// dshow.h under mingw, and the interlaced/aspect-aware header is the one a
// modern camera reports its formats through.
#include <dvdmedia.h>
#include <math.h>

// dshow_subtype_name maps a media subtype to the name dshow's pixel_format or
// vcodec option takes.
//
// The subtype GUIDs are FOURCCs in Data1 for everything except the RGB
// entries, which have their own MEDIASUBTYPE_RGBxx constants, so the switch is
// on Data1 with the RGB cases spelled out. As with the other two backends the
// names mirror what the demuxer will accept rather than what the hardware
// calls the layout, because a name outside that set is one OpenDevice refuses.
static const char *dshow_subtype_name(const GUID *sub) {
    if (IsEqualGUID(sub, &MEDIASUBTYPE_RGB24))   return "bgr24";
    if (IsEqualGUID(sub, &MEDIASUBTYPE_RGB32))   return "bgr0";
    if (IsEqualGUID(sub, &MEDIASUBTYPE_RGB555))  return "rgb555le";
    if (IsEqualGUID(sub, &MEDIASUBTYPE_RGB565))  return "rgb565le";
    if (IsEqualGUID(sub, &MEDIASUBTYPE_RGB8))    return "gray";

    switch (sub->Data1) {
        case MKTAG('Y','U','Y','2'): return "yuyv422";
        case MKTAG('U','Y','V','Y'): return "uyvy422";
        case MKTAG('Y','V','1','2'): return "yuv420p";
        case MKTAG('I','4','2','0'): return "yuv420p";
        case MKTAG('I','Y','U','V'): return "yuv420p";
        case MKTAG('N','V','1','2'): return "nv12";
        case MKTAG('Y','4','1','P'): return "yuv411p";
        case MKTAG('Y','V','U','9'): return "yuv410p";
        case MKTAG('Y','8','0','0'): return "gray";
        case MKTAG('G','R','E','Y'): return "gray";
        // Compressed subtypes name a decoder rather than a layout. dshow takes
        // these through vcodec= instead of pixel_format=, which is why
        // DeviceMode.Options() has to know which kind it is holding.
        case MKTAG('M','J','P','G'): return "mjpeg";
        case MKTAG('H','2','6','4'): return "h264";
        case MKTAG('h','2','6','4'): return "h264";
        case MKTAG('H','E','V','C'): return "hevc";
        default:                     return NULL;
    }
}

// dshow_subtype_is_codec says whether dshow_subtype_name returned a decoder
// name. It matters more here than on v4l2: dshow keeps pixel_format and vcodec
// as separate options and validates pixel_format strictly as a pixel layout,
// so offering "mjpeg" there is rejected outright rather than reinterpreted.
static int dshow_subtype_is_codec(const GUID *sub) {
    switch (sub->Data1) {
        case MKTAG('M','J','P','G'):
        case MKTAG('H','2','6','4'):
        case MKTAG('h','2','6','4'):
        case MKTAG('H','E','V','C'):
            return 1;
        default:
            return 0;
    }
}

// dshow_moniker_matches compares one enumerated device against the selector.
//
// The caller's url is a dshow name with its "video=" prefix already stripped
// by the Go side. Both the friendly name and the device path are accepted,
// because device_list_name reports the path — a camera's unique identity —
// while a human-written selector is usually the friendly name.
static int dshow_moniker_matches(IMoniker *m, const char *want) {
    IPropertyBag *bag = NULL;
    int matched = 0;

    // The moniker display name first, because that is what device_list_name
    // reports — libavdevice lists dshow devices by it, so a caller feeding a
    // DeviceInfo.Name straight back in arrives with an "@device_pnp_..."
    // string that matches neither of the property-bag keys below.
    //
    // The colon substitution is not cosmetic. The moniker's own display name
    // is "@device:pnp:\\?\usb#...", and libavdevice rewrites every ':' to '_'
    // because it uses ':' to delimit its own source syntax. Comparing against
    // the raw name therefore never matches the name it handed the caller.
    {
        LPOLESTR display = NULL;
        if (IMoniker_GetDisplayName(m, NULL, NULL, &display) == S_OK && display) {
            char buf[1024];
            int n = WideCharToMultiByte(CP_UTF8, 0, display, -1, buf, sizeof(buf), NULL, NULL);
            if (n > 0) {
                for (char *p = buf; *p; p++) {
                    if (*p == ':') *p = '_';
                }
                if (!strcmp(buf, want)) matched = 1;
            }
            CoTaskMemFree(display);
        }
    }
    if (matched) return 1;

    if (IMoniker_BindToStorage(m, NULL, NULL, &IID_IPropertyBag, (void **)&bag) != S_OK)
        return 0;

    const wchar_t *keys[] = { L"FriendlyName", L"DevicePath" };
    for (int k = 0; k < 2 && !matched; k++) {
        VARIANT v;
        VariantInit(&v);
        if (IPropertyBag_Read(bag, keys[k], &v, NULL) != S_OK) continue;
        if (v.vt == VT_BSTR && v.bstrVal) {
            char buf[512];
            int n = WideCharToMultiByte(CP_UTF8, 0, v.bstrVal, -1, buf, sizeof(buf), NULL, NULL);
            if (n > 0 && !strcmp(buf, want)) matched = 1;
        }
        VariantClear(&v);
    }
    IPropertyBag_Release(bag);
    return matched;
}

// dshow_collect_from_pin reads one output pin's capability list.
static void dshow_collect_from_pin(IPin *pin, void *modes) {
    IAMStreamConfig *config = NULL;
    int count = 0, size = 0;

    if (IPin_QueryInterface(pin, &IID_IAMStreamConfig, (void **)&config) != S_OK) return;
    if (IAMStreamConfig_GetNumberOfCapabilities(config, &count, &size) != S_OK ||
        size != (int)sizeof(VIDEO_STREAM_CONFIG_CAPS)) {
        // A non-video pin reports a differently sized caps struct; reading it
        // as VIDEO_STREAM_CONFIG_CAPS would be a type confusion, so skip.
        IAMStreamConfig_Release(config);
        return;
    }

    for (int i = 0; i < count; i++) {
        AM_MEDIA_TYPE *mt = NULL;
        VIDEO_STREAM_CONFIG_CAPS caps;
        if (IAMStreamConfig_GetStreamCaps(config, i, &mt, (BYTE *)&caps) != S_OK) continue;
        if (!mt) continue;

        BITMAPINFOHEADER *bih = NULL;
        if (IsEqualGUID(&mt->formattype, &FORMAT_VideoInfo) && mt->pbFormat) {
            bih = &((VIDEOINFOHEADER *)mt->pbFormat)->bmiHeader;
        } else if (IsEqualGUID(&mt->formattype, &FORMAT_VideoInfo2) && mt->pbFormat) {
            bih = &((VIDEOINFOHEADER2 *)mt->pbFormat)->bmiHeader;
        }

        if (bih) {
            // A frame interval is 100ns units per frame, so the framerate is
            // its reciprocal and the bounds cross over: the *minimum* interval
            // is the *maximum* rate.
            //
            // Rounded because the unit quantises: 60fps is stored as 166666,
            // which inverts to 60.000240000960005 and would be reported — and
            // put into an options string — in that form. v4l2 and avfoundation
            // report exact rationals and need no such correction. Three
            // decimals is enough to keep 29.97 and 23.976 intact while
            // absorbing the quantisation.
            double min_fps = 0, max_fps = 0;
            if (caps.MaxFrameInterval > 0)
                min_fps = round(10000000.0 / (double)caps.MaxFrameInterval * 1000.0) / 1000.0;
            if (caps.MinFrameInterval > 0)
                max_fps = round(10000000.0 / (double)caps.MinFrameInterval * 1000.0) / 1000.0;

            int h = bih->biHeight < 0 ? -bih->biHeight : bih->biHeight; // may be bottom-up
            device_modes_append(modes, bih->biWidth, h, min_fps, max_fps,
                                dshow_subtype_name(&mt->subtype),
                                dshow_subtype_is_codec(&mt->subtype));
        }

        // AM_MEDIA_TYPE is caller-owned and holds two nested allocations.
        if (mt->pbFormat && mt->cbFormat) CoTaskMemFree(mt->pbFormat);
        if (mt->pUnk) IUnknown_Release(mt->pUnk);
        CoTaskMemFree(mt);
    }
    IAMStreamConfig_Release(config);
}

static int dshow_collect_modes(const char *url, void *modes) {
    // The Go side hands the same selector OpenDevice takes, "video=<name>";
    // the prefix is dshow's own url syntax and means nothing to the enumerator.
    const char *want = url;
    if (!strncmp(want, "video=", 6)) want += 6;
    else if (!strncmp(want, "audio=", 6)) return GOFFMPEG_DEVICE_MODES_NO_DEVICE;
    if (!want[0]) return GOFFMPEG_DEVICE_MODES_NO_DEVICE;

    // Balanced by CoUninitialize below. RPC_E_CHANGED_MODE means the thread is
    // already apartment-initialised by someone else, which is fine to work in
    // but must not be undone here.
    HRESULT init = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    int owns_com = (init == S_OK || init == S_FALSE);

    ICreateDevEnum *devenum = NULL;
    IEnumMoniker *classenum = NULL;
    IMoniker *m = NULL;
    int rc = GOFFMPEG_DEVICE_MODES_NO_DEVICE;

    if (CoCreateInstance(&CLSID_SystemDeviceEnum, NULL, CLSCTX_INPROC_SERVER,
                         &IID_ICreateDevEnum, (void **)&devenum) != S_OK) {
        rc = GOFFMPEG_DEVICE_MODES_FAILED;
        goto done;
    }
    // S_FALSE means the category exists but is empty, and leaves classenum
    // NULL — a host with no camera rather than a fault.
    if (ICreateDevEnum_CreateClassEnumerator(devenum, &CLSID_VideoInputDeviceCategory,
                                             &classenum, 0) != S_OK || !classenum) {
        goto done;
    }

    while (IEnumMoniker_Next(classenum, 1, &m, NULL) == S_OK) {
        if (!dshow_moniker_matches(m, want)) {
            IMoniker_Release(m);
            m = NULL;
            continue;
        }

        IBaseFilter *filter = NULL;
        if (IMoniker_BindToObject(m, NULL, NULL, &IID_IBaseFilter, (void **)&filter) == S_OK) {
            IEnumPins *pins = NULL;
            if (IBaseFilter_EnumPins(filter, &pins) == S_OK) {
                IPin *pin = NULL;
                while (IEnumPins_Next(pins, 1, &pin, NULL) == S_OK) {
                    PIN_DIRECTION dir;
                    if (IPin_QueryDirection(pin, &dir) == S_OK && dir == PINDIR_OUTPUT) {
                        dshow_collect_from_pin(pin, modes);
                    }
                    IPin_Release(pin);
                }
                IEnumPins_Release(pins);
            }
            IBaseFilter_Release(filter);
            rc = 0; // the device was found and bound, even if it listed nothing
        }
        IMoniker_Release(m);
        m = NULL;
        break;
    }

done:
    if (classenum) IEnumMoniker_Release(classenum);
    if (devenum) ICreateDevEnum_Release(devenum);
    if (owns_com) CoUninitialize();
    return rc;
}
#endif // _WIN32

// ---------------------------------------------------------------------------
// Capture-mode enumeration (see bridge.h).
//
// The list handling lives here and the per-backend queries fill it through
// device_modes_append, so the ObjC translation unit and the ioctl and COM code
// share one allocator and one deduplication rule rather than three.
// ---------------------------------------------------------------------------

typedef struct {
    int width, height;
    double min_fps, max_fps;
    char pixfmt[24]; // libav's name where known; see device_modes_pixel_format
    int is_codec;    // pixfmt names a decoder, not a layout
} ModeEntry;

typedef struct {
    ModeEntry *items;
    int count, cap;
} ModeList;

int device_modes_append(void *modes, int width, int height,
                        double min_fps, double max_fps, const char *pixfmt,
                        int is_codec) {
    ModeList *ml = (ModeList *)modes;
    if (!ml || width <= 0 || height <= 0) return -1;

    // Backends repeat themselves: v4l2 reports the same frame size once per
    // pixel format and once per interval, and avfoundation lists a format per
    // range. Identical entries are noise to a caller choosing a mode, so they
    // are dropped here rather than in each backend. The lists are tens of
    // entries, so a linear scan costs nothing worth avoiding.
    for (int i = 0; i < ml->count; i++) {
        ModeEntry *e = &ml->items[i];
        if (e->width == width && e->height == height &&
            e->min_fps == min_fps && e->max_fps == max_fps &&
            e->is_codec == is_codec &&
            !strcmp(e->pixfmt, pixfmt ? pixfmt : "")) {
            return 0;
        }
    }

    if (ml->count == ml->cap) {
        int cap = ml->cap ? ml->cap * 2 : 16;
        ModeEntry *grown = av_realloc_array(ml->items, (size_t)cap, sizeof(ModeEntry));
        if (!grown) return -1;
        ml->items = grown;
        ml->cap = cap;
    }

    ModeEntry *e = &ml->items[ml->count];
    memset(e, 0, sizeof(*e));
    e->width = width;
    e->height = height;
    e->min_fps = min_fps;
    e->max_fps = max_fps;
    e->is_codec = is_codec;
    if (pixfmt) snprintf(e->pixfmt, sizeof(e->pixfmt), "%s", pixfmt);
    ml->count++;
    return 0;
}

static ModeEntry *mode_at(void *modes, int i) {
    ModeList *ml = (ModeList *)modes;
    if (!ml || !ml->items || i < 0 || i >= ml->count) return NULL;
    return &ml->items[i];
}

int device_modes_count(void *modes) {
    ModeList *ml = (ModeList *)modes;
    return ml ? ml->count : 0;
}

int device_modes_width(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    return e ? e->width : 0;
}

int device_modes_height(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    return e ? e->height : 0;
}

double device_modes_min_fps(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    return e ? e->min_fps : 0;
}

double device_modes_max_fps(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    return e ? e->max_fps : 0;
}

const char *device_modes_pixel_format(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    if (!e || !e->pixfmt[0]) return NULL;
    return e->pixfmt;
}

int device_modes_is_codec(void *modes, int i) {
    ModeEntry *e = mode_at(modes, i);
    return (e && e->is_codec) ? 1 : 0;
}

void device_modes_free(void *modes) {
    ModeList *ml = (ModeList *)modes;
    if (!ml) return;
    av_free(ml->items);
    av_free(ml);
}

int device_modes_open(const char *format, const char *url, void **modes) {
    if (!modes) return GOFFMPEG_DEVICE_MODES_FAILED;
    *modes = NULL;
    if (!format || !url) return GOFFMPEG_DEVICE_MODES_FAILED;

    // The format still has to exist, so an unknown one is the same answer here
    // as it is from device_list_open rather than a bare "unsupported".
    avdevice_register_all();
    if (!av_find_input_format(format)) return GOFFMPEG_DEVICE_MODES_NO_FORMAT;

    ModeList *ml = av_mallocz(sizeof(ModeList));
    if (!ml) return GOFFMPEG_DEVICE_MODES_FAILED;

    int rc = GOFFMPEG_DEVICE_MODES_UNSUPPORTED;
#ifdef __linux__
    if (!strcmp(format, "v4l2")) rc = v4l2_collect_modes(url, ml);
#endif
#ifdef __APPLE__
    if (!strcmp(format, "avfoundation")) rc = goffmpeg_avf_collect_modes(url, ml);
#endif
#ifdef _WIN32
    if (!strcmp(format, "dshow")) rc = dshow_collect_modes(url, ml);
#endif

    if (rc < 0) {
        device_modes_free(ml);
        return rc;
    }
    *modes = ml;
    return ml->count;
}

#endif
