#include "bridge.h"
#include <libavutil/mem.h>
#include <libavutil/channel_layout.h>

// decoder_setup_streams finds the best video/audio streams, opens their
// decoders, and prepares the swscale/swresample converters. Shared by
// decoder_open (files/URLs), decoder_open_device (capture devices) and
// decoder_open_reader (custom IO) so every entry point produces identical
// decoding behaviour.
int decoder_setup_streams(Decoder *d) {
    if (avformat_find_stream_info(d->fmt_ctx, NULL) < 0) return -2;

    // Video stream
    d->v_stream_idx = av_find_best_stream(d->fmt_ctx, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
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

int decoder_open_device(Decoder *d, const char *format, const char *url, const char *options) {
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
    if (rc < 0) return -1;

    return decoder_setup_streams(d);
#endif
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
    return (ret == AVERROR_EOF) ? 0 : ret;
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
        // own code.
        return rc == AVERROR(ENOSYS) ? GOFFMPEG_DEVICE_LIST_UNSUPPORTED
                                     : GOFFMPEG_DEVICE_LIST_FAILED;
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

#endif
