#include "bridge.h"
#include <libavutil/mem.h>
#include <libavutil/channel_layout.h>
#include <libavutil/log.h>
#include <libavutil/hwcontext.h>
#include <libavutil/pixdesc.h>
#include <libavutil/opt.h>
#include <stdlib.h>
#include <string.h>
#ifdef __APPLE__
#include <TargetConditionals.h>
#include <dlfcn.h>
#endif
#ifdef __ANDROID__
#include <pthread.h>
#include <stdarg.h>
#include <dlfcn.h>
#include <jni.h>
#include <libavcodec/jni.h>
#endif

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

static int receive(AVCodecContext *ctx, AVFrame *f);

#ifdef __ANDROID__
// Android's MediaCodec, in both sets: the media set opens it for video (below)
// and AAC, the audio set for AAC (#77).
//
// FFmpeg asks for a Java VM on every MediaCodec open, to look the decoder up
// by profile, and logs "No Java virtual machine has been registered" at error
// level when none was set with av_jni_set_java_vm. It then finds the decoder
// through the NDK, which needs no VM, and decodes. So the message is no error
// here, and at goffmpeg's default level (LogError, #19) it would print twice
// for every video an app opens. This demotes that one message to verbose; the
// callback is installed once, the first time MediaCodec is opened, and passes
// everything else to FFmpeg's own.
//
// It also watches the open for the lookup's failure (#85). mediacodec_opening
// is the context open_mediacodec is opening on this thread; FFmpeg logs the
// failure against that context, from inside avcodec_open2, so on the same
// thread.
static __thread const void *mediacodec_opening;
static __thread int mediacodec_no_lookup;
static void mediacodec_log(void *avcl, int level, const char *fmt, va_list vl) {
    if (fmt && strcmp(fmt, "No Java virtual machine has been registered\n") == 0) level = AV_LOG_VERBOSE;
    if (avcl && avcl == mediacodec_opening && fmt && strncmp(fmt, "Failed to getCodecNameByType(", 29) == 0)
        mediacodec_no_lookup = 1;
    av_log_default_callback(avcl, level, fmt, vl);
}
static void mediacodec_log_install(void) { av_log_set_callback(mediacodec_log); }
static pthread_once_t mediacodec_log_once = PTHREAD_ONCE_INIT;

// mediacodec_find_java_vm hands FFmpeg the app's Java VM, when the process has
// one and nothing has set it yet (#69). FFmpeg looks up a MediaCodec decoder
// for the stream's profile and level through Java, and without a VM it falls
// back to any decoder of the type, through the NDK. JNI_GetCreatedJavaVMs is
// public NDK API from Android 12 (API 31), in libnativehelper. It is looked up
// at run time, so the library still loads on older Android, where this does
// nothing. A process with no VM, a test binary, finds none and is unchanged.
static void mediacodec_find_java_vm(void) {
    if (av_jni_get_java_vm(NULL)) return;
    void *lib = dlopen("libnativehelper.so", RTLD_NOW);
    if (!lib) return;
    jint (*created)(JavaVM **, jsize, jsize *) =
        (jint (*)(JavaVM **, jsize, jsize *))dlsym(lib, "JNI_GetCreatedJavaVMs");
    JavaVM *vm = NULL;
    jsize n = 0;
    if (created && created(&vm, 1, &n) == JNI_OK && n > 0 && vm)
        av_jni_set_java_vm(vm, NULL);
    // libnativehelper stays loaded: FFmpeg keeps using the VM it returned.
}
static pthread_once_t mediacodec_vm_once = PTHREAD_ONCE_INIT;

// mediacodec_binder_pool starts a binder thread pool in a process with no Java
// VM, which has none unless it started one itself: a test binary, a
// command-line tool, a daemon (#86). MediaCodec's decoder runs in a media
// process and hands its results back over binder, as calls into this process.
// Where the Codec2 service is the AIDL one (Pixel 9a, Android 17), nothing
// receives them without a pool: MediaCodec takes two packets, then offers no
// input buffer and gives no frame, and FFmpeg's mediacodec_receive_frame polls
// both with no timeout, forever, without returning to the bridge, so the
// HW_PENDING_MAX fallback (#81) never runs (measured: 14,782 polls in a
// 5-second run, one core busy). The OnePlus Pad 2 and the moto g power 2024
// decode without a pool.
//
// An app's process always has a pool, started before any app code runs, so a
// process with a VM is left alone. On Android 10 and 11, where
// mediacodec_find_java_vm finds no VM, an app makes the call too, and it does
// nothing there: ABinderProcess_startThreadPool does nothing in a process
// whose pool has started. It is in libbinder_ndk from Android 10 (API 29) and
// is looked up at run time, so the library still loads before that, where
// this does nothing.
//
// GOFFMPEG_TEST_NO_BINDER_POOL=1 skips this, for TestHardwareDecodeStall: it
// brings back #86's stall, which patches/0001 must end with a fallback (#88).
static void mediacodec_binder_pool(void) {
    const char *off = getenv("GOFFMPEG_TEST_NO_BINDER_POOL");
    if (off && strcmp(off, "1") == 0) return;
    if (av_jni_get_java_vm(NULL)) return;
    void *lib = dlopen("libbinder_ndk.so", RTLD_NOW);
    if (!lib) return;
    void (*start)(void) = (void (*)(void))dlsym(lib, "ABinderProcess_startThreadPool");
    if (start) start();
    // libbinder_ndk stays loaded: its threads run in it.
}
static pthread_once_t mediacodec_pool_once = PTHREAD_ONCE_INIT;

// open_mediacodec_aac opens Android's own AAC decoder, through FFmpeg's
// aac_mediacodec, so the phones carry no FFmpeg AAC decoder (#77). On every
// device measured that is c2.android.aac.decoder, a software decoder in
// Android's media process. #85's refusal of a decoder with no hardware does
// not apply: FFmpeg's audio decoders do no lookup by profile, and create
// theirs by type (mediacodecdec_common.c, mediacodec_dec_get_audio_codec), so
// there is no failed lookup to watch for. The setup is open_mediacodec's: the
// log hook, the binder pool (#86) and the NDK API, which ndk_codec keeps even
// once a video open has given FFmpeg the app's Java VM. With the VM, FFmpeg
// would use MediaCodec's Java API instead, which was not measured.
static void mediacodec_setup(void) {
    pthread_once(&mediacodec_log_once, mediacodec_log_install);
    pthread_once(&mediacodec_vm_once, mediacodec_find_java_vm);
    pthread_once(&mediacodec_pool_once, mediacodec_binder_pool);
}
static AVCodecContext *open_mediacodec_aac(AVStream *st) {
    const AVCodec *codec = avcodec_find_decoder_by_name("aac_mediacodec");
    if (!codec) return NULL;
    mediacodec_setup();
    AVCodecContext *ctx = avcodec_alloc_context3(codec);
    if (!ctx) return NULL;
    avcodec_parameters_to_context(ctx, st->codecpar);
    AVDictionary *opts = NULL;
    av_dict_set(&opts, "ndk_codec", "1", 0);
    if (avcodec_open2(ctx, codec, &opts) < 0) avcodec_free_context(&ctx);
    av_dict_free(&opts);
    return ctx;
}

// mediacodec_probe_opts gives FFmpeg's stream probing the same setup for
// AAC. avformat_find_stream_info decodes a little of each stream to learn
// what the container does not say, such as the sample format, and HE-AAC's
// real rate where the ADTS header gives only the core's. With no FFmpeg AAC
// decoder (#77), it opens aac_mediacodec to do that. Without the binder pool a
// Pixel 9a never returned from it, and without ndk_codec it would use the Java
// API once a VM is set. Returns the per-stream options for the call, or NULL
// with no AAC stream; mediacodec_probe_opts_free frees them.
static AVDictionary **mediacodec_probe_opts(AVFormatContext *fmt) {
    AVDictionary **opts = NULL;
    for (unsigned i = 0; i < fmt->nb_streams; i++) {
        if (fmt->streams[i]->codecpar->codec_id != AV_CODEC_ID_AAC) continue;
        if (!opts) {
            if (!avcodec_find_decoder_by_name("aac_mediacodec")) return NULL;
            opts = av_calloc(fmt->nb_streams, sizeof(*opts));
            if (!opts) return NULL;
            mediacodec_setup();
        }
        av_dict_set(&opts[i], "ndk_codec", "1", 0);
    }
    return opts;
}
static void mediacodec_probe_opts_free(AVDictionary **opts, unsigned n) {
    if (!opts) return;
    for (unsigned i = 0; i < n; i++) av_dict_free(&opts[i]);
    av_free(opts);
}
#endif

#ifndef GOFFMPEG_NO_VIDEO
// The phone's hardware decoder (#65). bridge.h's decoder_video_decoder says
// when each is used. Two kinds, opened differently:
//
//  - MediaCodec (Android) is a decoder of its own, h264_mediacodec, opened by
//    name. It opens for streams it then cannot decode (High 4:4:4 opens and
//    fails on the first frame with AVERROR_EXTERNAL, measured on the
//    OPD2403), so it is offered only what hw_suits passes. In case it fails anyway
//    before its first frame, the video packets sent to it are kept and are
//    sent again to the software decoder that replaces it, so no frame is lost.
//    Some fail silently instead: MediaTek's takes High 4:4:4 and returns
//    nothing, without an error, to the end of the file (#81). So a MediaCodec
//    that has given no frame after HW_PENDING_MAX packets, or by the end of
//    the input, is replaced the same way.
//    Its frames are nv12 in system memory.
//
//  - VideoToolbox (Apple) is a hwaccel of the software h264 decoder: the same
//    decoder, given a hardware device and a get_format that picks it. When
//    VideoToolbox will not start, FFmpeg asks get_format again without it and
//    the decoder carries on in software. Its frames are CVPixelBuffers, copied
//    to nv12 in system memory by av_hwframe_transfer_data.
//
// And one with nothing to fall back to: on iOS, H.264 decodes only through
// Apple's decoder (#77), h264_videotoolbox, a decoder of its own (an FFmpeg
// patch, patches/0006), which the build has in place of the software h264.
// It is opened by name and its frames are nv12 in system memory, like
// MediaCodec's. Its packets are counted, not kept: a failure before its first
// frame, or HW_PENDING_MAX packets without one, ends decoder_next with
// GOFFMPEG_NO_VIDEO_FRAME (#77 decision A).
enum { HW_NONE, HW_MEDIACODEC, HW_VIDEOTOOLBOX, HW_PLATFORM };
// The most packets kept for a fallback, and the most MediaCodec is sent
// without giving a frame before it is replaced (#81). A working MediaCodec
// gives its first frame within 4 to 16 packets (OnePlus Pad 2, H.264 up to
// 1080p, VP9, AV1; the count varies with timing).
#define HW_PENDING_MAX 64

struct DecoderHW {
    int kind;
    // MediaCodec: a frame has come out, so the kept packets are dropped and no
    // more are kept. VideoToolbox: the last frame came out of the hardware.
    int confirmed;
    // VideoToolbox: the device, and the frame a hardware frame lands in
    // before it is copied to d->v_frame.
    AVBufferRef *device;
    AVFrame *frame;
    // MediaCodec: the video packets sent since the open or the last seek,
    // until the first frame. After a fallback, the ones still to send again;
    // next_replay is the next of those.
    AVPacket *pending[HW_PENDING_MAX];
    int npending, next_replay;
    // VideoToolbox: a frame has come out (from the hardware or not). Until
    // then a VideoToolbox in FFmpeg's av1 decoder can still fail and fall back
    // (hw_fragile). After a fallback: the kind that failed.
    int delivered;
    int failed_kind;
    // MediaCodec with no software decoder behind it: H.264 on Android, whose
    // set has no FFmpeg h264 (#77 decision B). Its packets are counted, not
    // kept, and a failure before its first frame ends decoder_next with
    // GOFFMPEG_NO_VIDEO_FRAME, as HW_PLATFORM's does.
    int no_fallback;
    // What decoder_video_decoder reports when MediaCodec was opened by name:
    // "h264_mediacodec (c2.android.avc.decoder)".
    char label[64];
};

// hw_mode reads GOFFMPEG_HWDECODE: 0 software only, 1 the hardware where the
// platform has it, 2 the hardware with no profile check.
static int hw_mode(void) {
    const char *v = getenv("GOFFMPEG_HWDECODE");
    if (v && strcmp(v, "0") == 0) return 0;
    if (v && strcmp(v, "1") == 0) return 1;
    if (v && strcmp(v, "force") == 0) return 2;
#if defined(__ANDROID__) || (defined(__APPLE__) && TARGET_OS_IPHONE)
    return 1;
#else
    return 0;
#endif
}

static void hw_drop_pending(struct DecoderHW *hw) {
    for (int i = 0; i < hw->npending; i++) av_packet_free(&hw->pending[i]);
    hw->npending = 0;
    hw->next_replay = 0;
}

static void hw_free(Decoder *d) {
    if (!d->hw) return;
    hw_drop_pending(d->hw);
    av_frame_free(&d->hw->frame);
    av_buffer_unref(&d->hw->device);
    av_freep(&d->hw);
}

static void hw_seeked(Decoder *d) {
    if (d->hw) hw_drop_pending(d->hw);
}

// hw_suits is the profile check: the streams the phones' hardware decoders
// decode, 8-bit 4:2:0 H.264 in the Baseline, Main and High profiles. Others
// are not offered to it. MediaCodec opens High 4:4:4 and then fails, and
// VideoToolbox refuses it at the first frame with an error in the log; the
// software decoder decodes either way, but this keeps the log clean and the
// fallback for what nobody predicted.
static int hw_suits(const AVCodecParameters *par) {
    if (par->format != AV_PIX_FMT_YUV420P && par->format != AV_PIX_FMT_YUVJ420P) return 0;
    // VP9 and AV1 (#78): their 8-bit 4:2:0 profiles, which phones decode.
    if (par->codec_id == AV_CODEC_ID_VP9) return par->profile == AV_PROFILE_VP9_0;
    if (par->codec_id == AV_CODEC_ID_AV1) return par->profile == AV_PROFILE_AV1_MAIN;
    if (par->codec_id != AV_CODEC_ID_H264) return 0;
    switch (par->profile) {
    case AV_PROFILE_H264_BASELINE:
    case AV_PROFILE_H264_CONSTRAINED_BASELINE:
    case AV_PROFILE_H264_MAIN:
    case AV_PROFILE_H264_HIGH:
        return 1;
    }
    return 0;
}

static AVCodecContext *open_software_video(AVStream *st) {
    const AVCodec *codec = avcodec_find_decoder(st->codecpar->codec_id);
    if (!codec) return NULL;
    AVCodecContext *ctx = avcodec_alloc_context3(codec);
    if (!ctx) return NULL;
    avcodec_parameters_to_context(ctx, st->codecpar);
    ctx->thread_count = 0;
    if (avcodec_open2(ctx, codec, NULL) < 0) avcodec_free_context(&ctx);
    return ctx;
}

#ifdef __ANDROID__
// MEDIACODEC_MIN_PIXELS: MediaCodec is used only for frames larger than
// 1280x720 (owner's decision, #65). Measured on the OPD2403 in a real player
// (PR #70): at 1080p30 it took the device from about 110% of a core to 96%,
// MediaCodec's own process included; at 720x720 from 71% to 77%, and it
// dropped 6 frames in 1,188 where software dropped none. MediaCodec's process
// costs about 11% of a core per stream whatever the size, so below 1080p
// that cost is more than the software decoding it replaces.
#define MEDIACODEC_MIN_PIXELS (1280 * 720)


// open_mediacodec opens MediaCodec's decoder for the stream: the one the codec
// list offers for its type, or with name the decoder of that name (patches/0007),
// as Android's own software decoder for what the hardware one refuses (#77).
static AVCodecContext *open_mediacodec(Decoder *d, AVStream *st, const char *name) {
    pthread_once(&mediacodec_log_once, mediacodec_log_install);
    pthread_once(&mediacodec_vm_once, mediacodec_find_java_vm);
    pthread_once(&mediacodec_pool_once, mediacodec_binder_pool);
    enum AVCodecID id = st->codecpar->codec_id;
    const AVCodec *codec = avcodec_find_decoder_by_name(
        id == AV_CODEC_ID_VP9 ? "vp9_mediacodec" : id == AV_CODEC_ID_AV1 ? "av1_mediacodec" : "h264_mediacodec");
    if (!codec) return NULL;
    struct DecoderHW *hw = av_mallocz(sizeof(*hw));
    AVCodecContext *ctx = hw ? avcodec_alloc_context3(codec) : NULL;
    if (!ctx) {
        av_free(hw);
        return NULL;
    }
    avcodec_parameters_to_context(ctx, st->codecpar);
    // With a VM, FFmpeg would otherwise switch to MediaCodec's Java API
    // (mediacodecdec.c: ndk_codec defaults to "no VM"). The NDK API is the one
    // measured on the OPD2403 (#65); the VM is only for the lookup.
    AVDictionary *opts = NULL;
    av_dict_set(&opts, "ndk_codec", "1", 0);
    if (name) av_dict_set(&opts, "codec_name", name, 0);
    mediacodec_opening = ctx;
    mediacodec_no_lookup = 0;
    int opened = avcodec_open2(ctx, codec, &opts);
    mediacodec_opening = NULL;
    av_dict_free(&opts);
    // With a Java VM, FFmpeg asks Android's codec list for a decoder of the
    // type and profile that is not software-only. When it finds none, it
    // still creates one by type through the NDK, because ndk_codec is set
    // (mediacodecdec_common.c, ff_mediacodec_dec_init), and Android hands it
    // a software decoder in its media process: for AV1 on a phone without
    // AV1 hardware, c2.android.av1-dav1d, which cost the moto g power 2024
    // 182-189% of a core at 1080p30 against 140-156% for goffmpeg's own dav1d
    // (#85). So a failed lookup with a VM means "no hardware": refuse, and
    // the caller opens the software decoder. Without a VM (a process with no
    // Java, Android before 12) there is no lookup to fail, and the NDK's
    // choice stands, as before.
    if (opened >= 0 && !name && mediacodec_no_lookup && av_jni_get_java_vm(NULL)) {
        av_log(ctx, AV_LOG_VERBOSE, "goffmpeg: no hardware %s decoder; decoding in software\n", avcodec_get_name(id));
        opened = AVERROR(ENOSYS);
    }
    if (opened < 0) {
        avcodec_free_context(&ctx);
        av_free(hw);
        return NULL;
    }
    hw->kind = HW_MEDIACODEC;
    if (name) snprintf(hw->label, sizeof(hw->label), "%s (%s)", codec->name, name);
    d->hw = hw;
    return ctx;
}

// open_android_h264 opens H.264 for a build with no FFmpeg h264, the Android
// media set (#77 decision B): MediaCodec's hardware decoder, and when it
// refuses the stream at open, Android's own software decoder, which takes
// what the hardware refuses by size (the OnePlus Pad 2 refuses 64x64, measured
// 2026-11-14). Neither has software behind it. NULL when both refuse, and the
// stream is left out (#41).
static AVCodecContext *open_android_h264(Decoder *d, AVStream *st) {
    AVCodecContext *ctx = open_mediacodec(d, st, NULL);
    if (!ctx) ctx = open_mediacodec(d, st, "c2.android.avc.decoder");
    if (ctx) d->hw->no_fallback = 1;
    return ctx;
}

#endif

#ifdef __APPLE__
static enum AVPixelFormat vt_get_format(AVCodecContext *ctx, const enum AVPixelFormat *fmts) {
    (void)ctx;
    for (const enum AVPixelFormat *p = fmts; *p != AV_PIX_FMT_NONE; p++)
        if (*p == AV_PIX_FMT_VIDEOTOOLBOX) return *p;
    // Offered without VideoToolbox, which is FFmpeg's fallback when it would
    // not start: the first software format, as the default get_format picks.
    for (const enum AVPixelFormat *p = fmts; *p != AV_PIX_FMT_NONE; p++) {
        const AVPixFmtDescriptor *desc = av_pix_fmt_desc_get(*p);
        if (desc && !(desc->flags & AV_PIX_FMT_FLAG_HWACCEL)) return *p;
    }
    return AV_PIX_FMT_NONE;
}

// vt_hardware reports whether VideoToolbox decodes a codec in hardware on this
// device. FFmpeg's av1 decoder has no software path, so VideoToolbox is offered
// AV1 only where it decodes it; VP9 the same, since a software VP9 there is
// FFmpeg's own vp9 again. The iPad (10th generation, A14) has neither, measured
// on #77. VTIsHardwareDecodeSupported is looked up rather than linked:
// VideoToolbox is in the process through libavcodec.
static int vt_hardware(enum AVCodecID id) {
    unsigned char (*supported)(uint32_t) =
        (unsigned char (*)(uint32_t))dlsym(RTLD_DEFAULT, "VTIsHardwareDecodeSupported");
    uint32_t type = id == AV_CODEC_ID_VP9 ? 0x76703039 /* 'vp09' */ : 0x61763031; /* 'av01' */
    return supported && supported(type);
}

// open_platform_h264 opens h264_videotoolbox, Apple's decoder as an FFmpeg
// decoder of its own (#77), for a build with no software h264: the iOS media
// set. It refuses what VideoToolbox doesn't decode, such as High 4:4:4, at
// the open, and the stream is then left out (#41).
static AVCodecContext *open_platform_h264(Decoder *d, AVStream *st) {
    const AVCodec *codec = avcodec_find_decoder_by_name("h264_videotoolbox");
    if (!codec) return NULL;
    struct DecoderHW *hw = av_mallocz(sizeof(*hw));
    AVCodecContext *ctx = hw ? avcodec_alloc_context3(codec) : NULL;
    if (!ctx) {
        av_free(hw);
        return NULL;
    }
    avcodec_parameters_to_context(ctx, st->codecpar);
    if (avcodec_open2(ctx, codec, NULL) < 0) {
        avcodec_free_context(&ctx);
        av_free(hw);
        return NULL;
    }
    hw->kind = HW_PLATFORM;
    d->hw = hw;
    return ctx;
}

static AVCodecContext *open_videotoolbox(Decoder *d, AVStream *st) {
    enum AVCodecID id = st->codecpar->codec_id;
    if ((id == AV_CODEC_ID_VP9 || id == AV_CODEC_ID_AV1) && !vt_hardware(id)) return NULL;
    // The codec's own FFmpeg decoder, which VideoToolbox's hwaccel runs in:
    // av1, not dav1d, which a lookup by codec ID returns.
    const AVCodec *codec = avcodec_find_decoder_by_name(
        id == AV_CODEC_ID_VP9 ? "vp9" : id == AV_CODEC_ID_AV1 ? "av1" : "h264");
    if (!codec) return NULL;
    AVBufferRef *device = NULL;
    if (av_hwdevice_ctx_create(&device, AV_HWDEVICE_TYPE_VIDEOTOOLBOX, NULL, NULL, 0) < 0) return NULL;
    struct DecoderHW *hw = av_mallocz(sizeof(*hw));
    AVCodecContext *ctx = hw ? avcodec_alloc_context3(codec) : NULL;
    if (hw) hw->frame = av_frame_alloc();
    if (!ctx || !hw->frame) {
        avcodec_free_context(&ctx);
        if (hw) av_frame_free(&hw->frame);
        av_free(hw);
        av_buffer_unref(&device);
        return NULL;
    }
    avcodec_parameters_to_context(ctx, st->codecpar);
    ctx->thread_count = 0;
    ctx->hw_device_ctx = av_buffer_ref(device);
    ctx->get_format = vt_get_format;
    if (avcodec_open2(ctx, codec, NULL) < 0) {
        avcodec_free_context(&ctx);
        av_frame_free(&hw->frame);
        av_free(hw);
        av_buffer_unref(&device);
        return NULL;
    }
    hw->kind = HW_VIDEOTOOLBOX;
    hw->device = device;
    d->hw = hw;
    return ctx;
}
#endif

// open_video opens the stream's decoder: the hardware one when the platform
// has it and the stream suits it, else the software one. NULL when neither
// opens.
static AVCodecContext *open_video(Decoder *d, AVStream *st) {
    int mode = hw_mode();
    AVCodecContext *ctx = NULL;
    enum AVCodecID id = st->codecpar->codec_id;
    if ((id == AV_CODEC_ID_H264 || id == AV_CODEC_ID_VP9 || id == AV_CODEC_ID_AV1) &&
        (mode == 2 || (mode == 1 && hw_suits(st->codecpar)))) {
#ifdef __ANDROID__
        if (id != AV_CODEC_ID_H264 || avcodec_find_decoder_by_name("h264"))
            if (mode == 2 || (int64_t)st->codecpar->width * st->codecpar->height > MEDIACODEC_MIN_PIXELS)
                ctx = open_mediacodec(d, st, NULL);
#endif
#ifdef __APPLE__
        if (id != AV_CODEC_ID_H264 || avcodec_find_decoder_by_name("h264"))
            ctx = open_videotoolbox(d, st);
#endif
    }
#ifdef __ANDROID__
    // With no software h264 (the Android media set), H.264 has only
    // MediaCodec, at every size and whatever GOFFMPEG_HWDECODE says.
    if (!ctx && id == AV_CODEC_ID_H264 && !avcodec_find_decoder_by_name("h264"))
        return open_android_h264(d, st);
#endif
#ifdef __APPLE__
    // With no software h264 (the iOS media set), H.264 has only Apple's
    // decoder, whatever GOFFMPEG_HWDECODE says.
    if (!ctx && id == AV_CODEC_ID_H264 && !avcodec_find_decoder_by_name("h264"))
        return open_platform_h264(d, st);
#endif
    return ctx ? ctx : open_software_video(st);
}

// hw_keep keeps a copy of a video packet sent to MediaCodec before its first
// frame, for hw_fallback. Past HW_PENDING_MAX it keeps no more: a decoder
// that has not produced a frame by then is not starting late. MediaCodec is
// replaced at that point (hw_silent), so it never gets past it; a
// VideoToolbox in av1 could, and a fallback after it loses the frames of the
// packets that were not kept.
// hw_fragile reports a hardware decoder that has produced no frame yet and has
// no software path of its own to fall back on: MediaCodec, and VideoToolbox
// running in FFmpeg's av1 decoder, which only drives hardware. A VideoToolbox
// that reports AV1 hardware and then fails to start leaves that decoder with
// nothing to decode with (measured in GitHub's macOS VM: "function not
// implemented"), so it falls back to dav1d like MediaCodec does (#78).
static int hw_fragile(Decoder *d) {
    struct DecoderHW *hw = d->hw;
    if (!hw) return 0;
    if (hw->kind == HW_MEDIACODEC) return !hw->confirmed && !hw->no_fallback;
    if (hw->kind == HW_VIDEOTOOLBOX) return !hw->delivered && d->v_ctx->codec_id == AV_CODEC_ID_AV1;
    return 0;
}

static void hw_keep(Decoder *d, const AVPacket *pkt) {
    struct DecoderHW *hw = d->hw;
    if (hw && (hw->kind == HW_PLATFORM || hw->no_fallback)) {
        if (!hw->confirmed) hw->npending++;
        return;
    }
    if (!hw_fragile(d)) return;
    if (hw->npending < HW_PENDING_MAX) {
        AVPacket *p = av_packet_clone(pkt);
        if (p) hw->pending[hw->npending++] = p;
    }
}

// hw_fallback replaces MediaCodec, which failed before its first frame, with
// the software decoder, and leaves the kept packets to be sent again
// (hw_replay). It returns 0, or a negative code when the software decoder
// will not open either.
static int hw_fallback(Decoder *d) {
    AVCodecContext *sw = open_software_video(d->fmt_ctx->streams[d->v_stream_idx]);
    if (!sw) return -1;
    av_log(d->v_ctx, AV_LOG_WARNING, "goffmpeg: the hardware decoder failed before its first frame (%d packets sent); decoding in software\n", d->hw->npending);
    avcodec_free_context(&d->v_ctx);
    d->v_ctx = sw;
    d->hw->failed_kind = d->hw->kind;
    d->hw->kind = HW_NONE;
    d->hw->next_replay = 0;
    return 0;
}

// hw_refused handles MediaCodec refusing a packet before its first frame:
// it falls back and returns 1, or returns 0 when that is not the case or the
// software decoder will not open.
// hw_no_fallback_failed is whether the video decoder is one with nothing
// behind it (MediaCodec for H.264 on Android, h264_videotoolbox on iOS) that
// has not given its first frame: a failure now ends the stream (#77).
static int hw_no_fallback_failed(Decoder *d) {
    struct DecoderHW *hw = d->hw;
    return hw && !hw->confirmed && (hw->kind == HW_PLATFORM || hw->no_fallback);
}
static int hw_refused(Decoder *d) {
    if (!hw_fragile(d)) return 0;
    return hw_fallback(d) == 0;
}

// hw_replay moves the next kept packet into pkt, for decoder_next to send in
// place of a read. It returns 1 when it did, 0 when none is left.
static int hw_replay(Decoder *d, AVPacket *pkt) {
    struct DecoderHW *hw = d->hw;
    if (!hw || hw->kind != HW_NONE || hw->next_replay >= hw->npending) return 0;
    av_packet_move_ref(pkt, hw->pending[hw->next_replay++]);
    if (hw->next_replay == hw->npending) hw_drop_pending(hw);
    return 1;
}

// hw_silent reports a MediaCodec that has failed without saying so (#81): it
// has given no frame and either has been sent HW_PENDING_MAX packets
// (receive's "no frame yet", 0) or has drained to the end (AVERROR_EOF). At
// the end it counts only when it was sent something to replay.
static int hw_silent(const struct DecoderHW *hw, int ret) {
    if (ret == 0) return hw->npending >= HW_PENDING_MAX;
    return ret == AVERROR_EOF && hw->npending > 0;
}

// receive_video is receive for the video decoder, with the hardware's part:
// a VideoToolbox frame is copied to d->v_frame in system memory, and a
// MediaCodec failure before the first frame becomes a fallback, reported as
// "no frame yet" so decoder_next goes on to the kept packets.
static int receive_video(Decoder *d) {
    struct DecoderHW *hw = d->hw;
    if (hw && hw->kind == HW_PLATFORM) {
        int ret = receive(d->v_ctx, d->v_frame);
        if (ret == 1) {
            hw->confirmed = 1;
            return 1;
        }
        if (!hw->confirmed && ((ret < 0 && ret != AVERROR_EOF) || hw_silent(hw, ret))) {
            av_log(d->v_ctx, AV_LOG_ERROR, "%s returned no frame from %d packets (%s)\n", d->v_ctx->codec->name,
                   hw->npending, ret < 0 ? av_err2str(ret) : "no error");
            return GOFFMPEG_NO_VIDEO_FRAME;
        }
        return ret;
    }
    if (hw && hw->kind == HW_VIDEOTOOLBOX) {
        int ret = receive(d->v_ctx, hw->frame);
        if (ret < 0 && ret != AVERROR_EOF && hw_fragile(d) && hw_fallback(d) == 0) return 0;
        if (ret != 1) return ret;
        if (!hw->delivered) {
            hw->delivered = 1;
            hw_drop_pending(hw);
        }
        av_frame_unref(d->v_frame);
        hw->confirmed = hw->frame->format == AV_PIX_FMT_VIDEOTOOLBOX;
        if (!hw->confirmed) {
            av_frame_move_ref(d->v_frame, hw->frame);
            return 1;
        }
        int rc = av_hwframe_transfer_data(d->v_frame, hw->frame, 0);
        if (rc >= 0) rc = av_frame_copy_props(d->v_frame, hw->frame);
        av_frame_unref(hw->frame);
        return rc < 0 ? rc : 1;
    }
    int ret = receive(d->v_ctx, d->v_frame);
    if (ret == 1 && hw && hw->kind == HW_MEDIACODEC) {
        // MediaCodec fills in a colour description the stream may not give:
        // an untagged stream comes out tagged with the matrix MediaCodec
        // guessed, so GetYCbCr would report a guess as stated (measured on the
        // OPD2403). The stream's own tags, which the software decoder
        // reports, are put back.
        const AVCodecParameters *par = d->fmt_ctx->streams[d->v_stream_idx]->codecpar;
        d->v_frame->colorspace = par->color_space;
        d->v_frame->color_range = par->color_range;
        d->v_frame->color_primaries = par->color_primaries;
        d->v_frame->color_trc = par->color_trc;
    }
    if (hw && hw->kind == HW_MEDIACODEC && !hw->confirmed) {
        if (ret == 1) {
            hw->confirmed = 1;
            hw_drop_pending(hw);
        } else if ((ret < 0 && ret != AVERROR_EOF) || hw_silent(hw, ret)) {
            if (hw->no_fallback) {
                av_log(d->v_ctx, AV_LOG_ERROR, "%s returned no frame from %d packets (%s)\n",
                       hw->label[0] ? hw->label : d->v_ctx->codec->name, hw->npending,
                       ret < 0 ? av_err2str(ret) : "no error");
                return GOFFMPEG_NO_VIDEO_FRAME;
            }
            if (hw_fallback(d) == 0) return 0;
        }
    }
    return ret;
}
#else
// An audio-only build opens no video decoder, so there is no hardware state.
static void hw_free(Decoder *d) { (void)d; }
static void hw_seeked(Decoder *d) { (void)d; }
static void hw_keep(Decoder *d, const AVPacket *pkt) { (void)d; (void)pkt; }
static int hw_refused(Decoder *d) { (void)d; return 0; }
static int hw_no_fallback_failed(Decoder *d) { (void)d; return 0; }
static int hw_replay(Decoder *d, AVPacket *pkt) { (void)d; (void)pkt; return 0; }
static int receive_video(Decoder *d) { return receive(d->v_ctx, d->v_frame); }
#endif

const char *decoder_video_decoder(Decoder *d) {
    if (!d || !d->v_ctx || !d->v_ctx->codec) return NULL;
#ifndef GOFFMPEG_NO_VIDEO
    enum AVCodecID id = d->v_ctx->codec_id;
    if (d->hw && d->hw->kind == HW_MEDIACODEC && d->hw->label[0])
        return d->hw->label;
    if (d->hw && d->hw->kind == HW_VIDEOTOOLBOX && d->hw->confirmed)
        return id == AV_CODEC_ID_VP9 ? "vp9 (videotoolbox)" : id == AV_CODEC_ID_AV1 ? "av1 (videotoolbox)" : "h264 (videotoolbox)";
    // After hw_fallback the state stays, with no kind.
    if (d->hw && d->hw->kind == HW_NONE && d->hw->failed_kind == HW_VIDEOTOOLBOX)
        return "libdav1d (after videotoolbox failed)";
    if (d->hw && d->hw->kind == HW_NONE)
        return id == AV_CODEC_ID_VP9 ? "vp9 (after vp9_mediacodec failed)" :
               id == AV_CODEC_ID_AV1 ? "libdav1d (after av1_mediacodec failed)" : "h264 (after h264_mediacodec failed)";
#endif
    return d->v_ctx->codec->name;
}

// MEDIACODEC_AUDIO_NO_FRAME_PACKETS is how many packets the phone's own audio
// decoder is sent without returning a frame before it is taken to have failed
// (#95). Fed data it cannot decode, c2.android.aac takes every packet and
// returns nothing, and so does Apple's aac_at, so without a bound the audio
// fails only at the end of the input, however long that is. Measured on a Pixel 9a, a moto g power 2024 and
// a OnePlus Pad 2, the AAC clips in testdata (LC, HE, HEv2 and 5.1; M4A, MP4,
// ADTS and HLS) returned their first frame after 5 to 8 packets. 64 packets is
// about 1.5 s of AAC-LC at 44.1 kHz.
#define MEDIACODEC_AUDIO_NO_FRAME_PACKETS 64

// platform_audio is whether ctx is the phone's own audio decoder, Android's
// through MediaCodec or Apple's through AudioToolbox, which the phones have no
// FFmpeg decoder to fall back from (#77).
static int platform_audio(const AVCodecContext *ctx) {
#if defined(__ANDROID__)
    return ctx->codec && ctx->codec->name && strstr(ctx->codec->name, "_mediacodec") != NULL;
#elif defined(__APPLE__) && TARGET_OS_IPHONE
    const char *n = ctx->codec ? ctx->codec->name : NULL;
    size_t len = n ? strlen(n) : 0;
    return len > 3 && strcmp(n + len - 3, "_at") == 0;
#else
    (void)ctx;
    return 0;
#endif
}

// audio_no_frame ends decoding for the phone's audio decoder returning no frame
// (#95): ret is what it failed with, 0 for packets it took and never answered.
static int audio_no_frame(Decoder *d, int ret) {
    av_log(d->a_ctx, AV_LOG_ERROR, "%s returned no frame from %d packets (%s)\n",
           d->a_ctx->codec->name, d->a_unanswered, ret ? av_err2str(ret) : "no error");
    return GOFFMPEG_NO_AUDIO_FRAME;
}

const char *decoder_audio_decoder(Decoder *d) {
    if (!d || !d->a_ctx || !d->a_ctx->codec) return NULL;
    return d->a_ctx->codec->name;
}

// decoder_setup_streams finds the best video/audio streams, opens their
// decoders, and prepares the swscale/swresample converters. Shared by
// decoder_open (files/URLs), decoder_open_device (capture devices) and
// decoder_open_reader (custom IO) so every entry point produces identical
// decoding behaviour.
int decoder_setup_streams(Decoder *d) {
    AVDictionary **probe_opts = NULL;
    unsigned probe_streams = d->fmt_ctx->nb_streams;
#ifdef __ANDROID__
    probe_opts = mediacodec_probe_opts(d->fmt_ctx);
#endif
    int probed = avformat_find_stream_info(d->fmt_ctx, probe_opts);
#ifdef __ANDROID__
    mediacodec_probe_opts_free(probe_opts, probe_streams);
#else
    (void)probe_streams;
#endif
    if (probed < 0) return -2;

    // Video stream: none in an audio-only build, whose video packets are
    // then read and dropped (decoder_next sends a packet only to an open
    // decoder).
#ifdef GOFFMPEG_NO_VIDEO
    d->v_stream_idx = -1;
#else
    d->v_stream_idx = find_video_stream(d->fmt_ctx);
    if (d->v_stream_idx >= 0) {
        AVStream *st = d->fmt_ctx->streams[d->v_stream_idx];
        d->v_ctx = open_video(d, st);
        if (d->v_ctx) {
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
#endif

    // Audio stream
    d->a_stream_idx = av_find_best_stream(d->fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, NULL, 0);
    if (d->a_stream_idx >= 0) {
        AVStream *st = d->fmt_ctx->streams[d->a_stream_idx];
        const AVCodec *codec = NULL;
#ifdef __ANDROID__
        // AAC goes to Android's decoder (#77).
        if (st->codecpar->codec_id == AV_CODEC_ID_AAC) d->a_ctx = open_mediacodec_aac(st);
#endif
        if (!d->a_ctx && (codec = avcodec_find_decoder(st->codecpar->codec_id))) {
            d->a_ctx = avcodec_alloc_context3(codec);
            avcodec_parameters_to_context(d->a_ctx, st->codecpar);
            if (avcodec_open2(d->a_ctx, codec, NULL) < 0) avcodec_free_context(&d->a_ctx);
        }
        if (d->a_ctx) {
            d->a_unanswered = platform_audio(d->a_ctx) ? 0 : -1;
            d->sample_rate = d->a_ctx->sample_rate;
            d->a_frame = av_frame_alloc();
            // 0 = keep the stream's own layout. Callers that need a specific
            // count (because the audio device is already open at one) ask for
            // it via decoder_set_audio_channels.
            decoder_set_audio_channels(d, 0);
        }
    }

    // A stream with no decoder in this build is left out, and the input still
    // opens with what it can decode (#41): the AAC audio of an MP4 whose video
    // a build cannot decode still plays. An input it can decode nothing of
    // fails here, rather than opening and then yielding nothing.
    // decoder_missing says which codecs those were.
    if (!d->v_frame && !d->a_frame) return GOFFMPEG_NO_DECODER;

    if (d->fmt_ctx->duration != AV_NOPTS_VALUE) {
        d->duration = (double)d->fmt_ctx->duration / AV_TIME_BASE;
    }

    d->pkt = av_packet_alloc();
    return 0;
}

const char *decoder_missing(Decoder *d, int media) {
    if (!d || !d->fmt_ctx) return NULL;
    int idx;
    if (media == 1) {
        if (d->v_frame) return NULL;
        idx = find_video_stream(d->fmt_ctx);
    } else if (media == 2) {
        if (d->a_frame) return NULL;
        idx = av_find_best_stream(d->fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, NULL, 0);
    } else {
        return NULL;
    }
    if (idx < 0) return NULL;
    return avcodec_get_name(d->fmt_ctx->streams[idx]->codecpar->codec_id);
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
    d->swr_flushed = 0;
    av_channel_layout_uninit(&out_layout);
    return 0;
}

static int decoder_interrupt_cb(void *opaque) {
    return __atomic_load_n((int *)opaque, __ATOMIC_ACQUIRE);
}

int decoder_alloc_context(Decoder *d) {
    d->interrupt = av_mallocz(sizeof(int));
    if (!d->interrupt) return -4;
    d->fmt_ctx = avformat_alloc_context();
    if (!d->fmt_ctx) return -6;
    d->fmt_ctx->interrupt_callback.callback = decoder_interrupt_cb;
    d->fmt_ctx->interrupt_callback.opaque = d->interrupt;
    return 0;
}

void decoder_interrupt(Decoder *d) {
    if (d->interrupt) __atomic_store_n(d->interrupt, 1, __ATOMIC_RELEASE);
}

// FFmpeg consults the interrupt callback only on some I/O paths: a local file
// never does, and a seek interrupted mid-read can fall back to an index entry
// it already holds and succeed. So the calls that read check the flag
// themselves, and an interrupted decoder fails them all (#37).
static int decoder_interrupted(const Decoder *d) {
    return d->interrupt && __atomic_load_n(d->interrupt, __ATOMIC_ACQUIRE);
}

int decoder_open_opts(Decoder *d, const char *url, const char *options) {
    return decoder_open_opts_err(d, url, options, NULL);
}

int decoder_open_opts_err(Decoder *d, const char *url, const char *options, int *averr) {
    if (averr) *averr = 0;
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
    int rc = decoder_alloc_context(d);
    if (rc < 0) {
        av_dict_free(&opts);
        return rc;
    }
    rc = avformat_open_input(&d->fmt_ctx, url, NULL, &opts);
    av_dict_free(&opts);
    if (rc < 0) {
        if (averr) *averr = rc;
        return -1;
    }

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

    int rc = decoder_alloc_context(d);
    if (rc < 0) return rc;
    AVDictionary *opts = NULL;
    if (options && options[0]) av_dict_parse_string(&opts, options, "=", ",", 0);
    rc = avformat_open_input(&d->fmt_ctx, url, ifmt, &opts);
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
    if (decoder_interrupted(d)) return AVERROR_EXIT;
    if (seconds < 0) seconds = 0;
    int64_t ts = (int64_t)(seconds * AV_TIME_BASE);
    int ret = av_seek_frame(d->fmt_ctx, -1, ts, AVSEEK_FLAG_BACKWARD);
    if (ret < 0) return ret;
    if (decoder_interrupted(d)) return AVERROR_EXIT;
    if (d->v_ctx) avcodec_flush_buffers(d->v_ctx);
    if (d->a_ctx) avcodec_flush_buffers(d->a_ctx);
    // Packets kept for a MediaCodec fallback, or still to send after one, are
    // from before the seek.
    hw_seeked(d);
    // swresample still holds samples from before the seek, which would come
    // out after it; reinitialising discards them (#24).
    if (d->swr_ctx) swr_init(d->swr_ctx);
    d->swr_flushed = 0;
    d->position = seconds;
    return 0;
}

int decoder_struct_size(void) {
    return (int)sizeof(Decoder);
}

// frame_position sets d->position from a decoded frame of stream idx.
static void frame_position(Decoder *d, AVFrame *f, int idx) {
    int64_t pts = f->best_effort_timestamp;
    if (pts == AV_NOPTS_VALUE) pts = f->pts;
    if (pts == AV_NOPTS_VALUE) pts = 0;
    d->position = (double)pts * av_q2d(d->fmt_ctx->streams[idx]->time_base);
}

// receive takes a frame the decoder already holds: 1 if one is ready, 0 if it
// needs more input, AVERROR_EOF once it is drained, another negative on error.
static int receive(AVCodecContext *ctx, AVFrame *f) {
    int ret = avcodec_receive_frame(ctx, f);
    if (ret == AVERROR(EAGAIN)) return 0;
    return ret == 0 ? 1 : ret;
}

// decoder_next takes frames the decoders already hold before reading another
// packet, so a packet that decodes to several frames loses none. At the end of
// the input it drains them: frame threading holds back about a frame per
// thread, and without the drain those were dropped, every video losing its
// last frames and a clip shorter than the thread count losing all of them
// (#22). v_frame and a_frame mark a decoder that opened.
int decoder_next(Decoder *d) {
    if (decoder_interrupted(d)) return AVERROR_EXIT;
    for (;;) {
        int v_done = 1, a_done = 1;
        if (d->v_frame) {
            int ret = receive_video(d);
            if (ret == 1) {
                // Adopt the real geometry once a frame arrives; probing may
                // not have resolved it, and callers size their buffers from
                // the values reported here.
                if (d->width <= 0 || d->height <= 0) {
                    d->width = d->v_frame->width;
                    d->height = d->v_frame->height;
                }
                frame_position(d, d->v_frame, d->v_stream_idx);
                return 1;
            }
            if (ret == GOFFMPEG_NO_VIDEO_FRAME) return ret;
            if (ret < 0 && ret != AVERROR_EOF) return ret;
            v_done = ret == AVERROR_EOF;
        }
        if (d->a_frame) {
            int ret = receive(d->a_ctx, d->a_frame);
            if (ret == 1) {
                if (d->a_unanswered >= 0)
                    av_log(d->a_ctx, AV_LOG_VERBOSE, "first frame after %d packets\n", d->a_unanswered);
                d->a_unanswered = -1;
                frame_position(d, d->a_frame, d->a_stream_idx);
                return 2;
            }
            // The phone's decoder failing, or drained, before its first frame
            // (#95). A drain is a failure only if it was sent something: an
            // audio stream with no packets just ends.
            if (d->a_unanswered >= 0 && ret < 0 && (ret != AVERROR_EOF || d->a_unanswered > 0))
                return audio_no_frame(d, ret);
            if (ret < 0 && ret != AVERROR_EOF) return ret;
            a_done = ret == AVERROR_EOF;
        }
        if (v_done && a_done) {
            // A rate conversion leaves swresample holding a few samples that
            // only a flush releases, so they come out once more as a last
            // audio frame with no samples of its own, which decoder_get_audio
            // turns into the flush (#24). The flag is set here rather than in
            // decoder_get_audio, so a caller that never asks for audio still
            // reaches the end.
            if (d->a_frame && d->swr_ctx && !d->swr_flushed) {
                d->swr_flushed = 1;
                if (swr_get_delay(d->swr_ctx, d->sample_rate) > 0) {
                    av_frame_unref(d->a_frame);
                    return 2;
                }
            }
            return 0;
        }

        // After a fallback from MediaCodec, the packets it was sent go to the
        // software decoder before anything new is read.
        if (hw_replay(d, d->pkt)) {
            int sent = avcodec_send_packet(d->v_ctx, d->pkt);
            av_packet_unref(d->pkt);
            if (sent < 0) return sent;
            continue;
        }

        int ret = av_read_frame(d->fmt_ctx, d->pkt);
        // EAGAIN is a non-blocking device saying "not yet" and must not be
        // confused with either an end or a failure (#4).
        if (ret == AVERROR(EAGAIN)) return GOFFMPEG_AGAIN;
        if (ret == AVERROR_EOF) {
            // An end that is really a refused reconnect (see GOFFMPEG_TRUNCATED).
            AVIOContext *pb = d->fmt_ctx->pb;
            if (pb) {
                int64_t size = avio_size(pb);
                if (size > 0 && avio_tell(pb) > size) return GOFFMPEG_TRUNCATED;
            }
            // A NULL packet starts the drain. Sending it again once draining
            // returns AVERROR_EOF, which is harmless, so no state is kept.
            if (d->v_frame) avcodec_send_packet(d->v_ctx, NULL);
            if (d->a_frame) avcodec_send_packet(d->a_ctx, NULL);
            continue;
        }
        if (ret < 0) return ret;

        AVCodecContext *ctx = NULL;
        if (d->v_frame && d->pkt->stream_index == d->v_stream_idx) ctx = d->v_ctx;
        if (d->a_frame && d->pkt->stream_index == d->a_stream_idx) ctx = d->a_ctx;
        if (ctx && ctx == d->v_ctx) hw_keep(d, d->pkt);
        ret = ctx ? avcodec_send_packet(ctx, d->pkt) : 0;
        av_packet_unref(d->pkt);
        if (ctx && ctx == d->a_ctx && d->a_unanswered >= 0) {
            if (ret < 0 && ret != AVERROR(EAGAIN)) return audio_no_frame(d, ret);
            if (++d->a_unanswered >= MEDIACODEC_AUDIO_NO_FRAME_PACKETS) return audio_no_frame(d, 0);
        }
        // MediaCodec refusing a packet before its first frame is a fallback:
        // the packet was kept, and goes to the software decoder with the rest.
        // With no software decoder (H.264 on Android, #77) it is the end.
        if (ret < 0 && ret != AVERROR(EAGAIN) && ctx == d->v_ctx && hw_no_fallback_failed(d)) return GOFFMPEG_NO_VIDEO_FRAME;
        if (ret < 0 && ctx == d->v_ctx && hw_refused(d)) ret = 0;
        if (ret < 0) return ret;
    }
}

int decoder_get_rgba(Decoder *d, uint8_t *buffer) {
#ifdef GOFFMPEG_NO_VIDEO
    (void)d; (void)buffer;
    return -8;
#else
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
#endif
}

// decoder_get_yuv copies the current video frame's planes into the caller's,
// unconverted, for a caller that converts to RGB itself, as a GPU shader does
// (#64). Only 8-bit 4:2:0 planar frames are copied (yuv420p and yuvj420p),
// which is what the H.264, HEVC and VP9 software decoders produce for nearly
// all video. The caller sizes its planes for the decoder's frame: y holds
// height rows of y_stride bytes, u and v (height+1)/2 rows of c_stride bytes,
// and each row at least as wide as its plane.
//
// Returns 0; -1 with no frame; -2 when the frame's size is not the size the
// decoder opened with, as in decoder_get_rgba; -3 for any other pixel format,
// which decoder_frame_pix_fmt names; -8 in a build without video.
int decoder_get_yuv(Decoder *d, uint8_t *y, int y_stride, uint8_t *u, uint8_t *v, int c_stride) {
#ifdef GOFFMPEG_NO_VIDEO
    (void)d; (void)y; (void)y_stride; (void)u; (void)v; (void)c_stride;
    return -8;
#else
    if (!d->v_frame) return -1;
    AVFrame *f = d->v_frame;
    if (f->width <= 0 || f->height <= 0 || f->format == AV_PIX_FMT_NONE) return -1;
    if (f->width != d->width || f->height != d->height) return -2;
    int nv12 = f->format == AV_PIX_FMT_NV12;
    if (f->format != AV_PIX_FMT_YUV420P && f->format != AV_PIX_FMT_YUVJ420P && !nv12) return -3;

    int cw = (f->width + 1) / 2, ch = (f->height + 1) / 2;
    av_image_copy_plane(y, y_stride, f->data[0], f->linesize[0], f->width, f->height);
    if (nv12) {
        // What the phones' hardware decoders deliver (#65): the same 4:2:0
        // samples, with Cb and Cr interleaved in one plane. They are split
        // into the caller's two planes as they are copied.
        for (int r = 0; r < ch; r++) {
            const uint8_t *src = f->data[1] + (ptrdiff_t)r * f->linesize[1];
            uint8_t *du = u + (ptrdiff_t)r * c_stride, *dv = v + (ptrdiff_t)r * c_stride;
            for (int c = 0; c < cw; c++) {
                du[c] = src[2 * c];
                dv[c] = src[2 * c + 1];
            }
        }
        return 0;
    }
    av_image_copy_plane(u, c_stride, f->data[1], f->linesize[1], cw, ch);
    av_image_copy_plane(v, c_stride, f->data[2], f->linesize[2], cw, ch);
    return 0;
#endif
}

// decoder_frame_colorspace is the current video frame's AVColorSpace, the
// matrix that turns its YUV into RGB, or AVCOL_SPC_UNSPECIFIED when the stream
// does not say, which most files do not.
int decoder_frame_colorspace(Decoder *d) {
    if (!d->v_frame) return AVCOL_SPC_UNSPECIFIED;
    return d->v_frame->colorspace;
}

// decoder_frame_full_range reports whether the current video frame uses the
// full 0-255 range rather than the limited 16-235 one: its color_range says
// so, or its pixel format is one of the JPEG ("yuvj") formats, which imply it.
int decoder_frame_full_range(Decoder *d) {
    if (!d->v_frame) return 0;
    if (d->v_frame->color_range == AVCOL_RANGE_JPEG) return 1;
    return d->v_frame->format == AV_PIX_FMT_YUVJ420P || d->v_frame->format == AV_PIX_FMT_YUVJ422P ||
           d->v_frame->format == AV_PIX_FMT_YUVJ444P;
}

// decoder_frame_pix_fmt names the current video frame's pixel format
// ("yuv420p"), or returns NULL with no frame.
const char *decoder_frame_pix_fmt(Decoder *d) {
    if (!d->v_frame || d->v_frame->format == AV_PIX_FMT_NONE) return NULL;
    return av_get_pix_fmt_name((enum AVPixelFormat)d->v_frame->format);
}

// swr_follow_frame rebuilds the resampler when the frame's rate, sample
// format or channel layout is not what it was built for. It is built at open
// from what the stream says, and a decoder can learn some of that only as it
// decodes: Android's MediaCodec gives its output format with its first frame
// (FFmpeg's aac_mediacodec, #77). The output stays what AudioInfo reported.
// A rebuild drops what the old resampler held, which at the first frame is
// nothing.
static int swr_follow_frame(Decoder *d) {
    const AVFrame *f = d->a_frame;
    if (f->nb_samples <= 0) return 0;
    int64_t rate = 0;
    enum AVSampleFormat fmt = AV_SAMPLE_FMT_NONE;
    AVChannelLayout in = {0}, out = {0};
    av_opt_get_int(d->swr_ctx, "in_sample_rate", 0, &rate);
    av_opt_get_sample_fmt(d->swr_ctx, "in_sample_fmt", 0, &fmt);
    av_opt_get_chlayout(d->swr_ctx, "in_chlayout", 0, &in);
    int same = rate == f->sample_rate && fmt == f->format && av_channel_layout_compare(&in, &f->ch_layout) == 0;
    av_channel_layout_uninit(&in);
    if (same) return 0;
    int ret = av_opt_get_chlayout(d->swr_ctx, "out_chlayout", 0, &out);
    struct SwrContext *swr = NULL;
    if (ret >= 0)
        ret = swr_alloc_set_opts2(&swr, &out, AV_SAMPLE_FMT_S16, d->sample_rate,
                                  &f->ch_layout, (enum AVSampleFormat)f->format, f->sample_rate, 0, NULL);
    av_channel_layout_uninit(&out);
    if (ret >= 0) ret = swr_init(swr);
    if (ret < 0) {
        swr_free(&swr);
        return ret;
    }
    swr_free(&d->swr_ctx);
    d->swr_ctx = swr;
    return 0;
}

int decoder_get_audio(Decoder *d, uint8_t **buffer) {
    if (!d->swr_ctx || !d->a_frame) return -1;
    if (d->channels <= 0) return -1;
    int ret = swr_follow_frame(d);
    if (ret < 0) return ret;
    int64_t in_rate = 0;
    av_opt_get_int(d->swr_ctx, "in_sample_rate", 0, &in_rate);
    if (in_rate <= 0) return -1;
    int out_samples = av_rescale_rnd(swr_get_delay(d->swr_ctx, in_rate) + d->a_frame->nb_samples,
                                     d->sample_rate, in_rate, AV_ROUND_UP);
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
    // A frame with no samples is the end-of-stream one decoder_next hands out
    // (#24). swresample flushes only on a NULL input, not on a count of 0.
    const uint8_t **in = d->a_frame->nb_samples > 0 ? (const uint8_t **)d->a_frame->data : NULL;
    ret = swr_convert(d->swr_ctx, &d->audio_buf, out_samples, in, d->a_frame->nb_samples);
    if (ret < 0) return ret;
    *buffer = d->audio_buf;
    // Bytes produced: samples x channels x 2 (S16). This was hardcoded to
    // stereo, which would have truncated every multichannel buffer.
    return ret * d->channels * 2;
}

// FFmpeg's log level, exported by the bridge itself for the purego backend,
// which looks every symbol up in libgoffmpeg's handle. With FFmpeg linked in,
// av_log_set_level was among the bridge's own exports. Linked as a shared
// library it is an import, and Windows' GetProcAddress searches only what the
// DLL itself exports (#40).
void decoder_log_set_level(int level) { av_log_set_level(level); }
int decoder_log_get_level(void) { return av_log_get_level(); }

void decoder_close(Decoder *d) {
#ifndef GOFFMPEG_NO_VIDEO
    if (d->sws_ctx) sws_freeContext(d->sws_ctx);
#endif
    if (d->swr_ctx) swr_free(&d->swr_ctx);
    if (d->v_frame) av_frame_free(&d->v_frame);
    if (d->a_frame) av_frame_free(&d->a_frame);
    if (d->audio_buf) av_freep(&d->audio_buf);
    if (d->v_ctx) avcodec_free_context(&d->v_ctx);
    if (d->a_ctx) avcodec_free_context(&d->a_ctx);
    hw_free(d);
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
    av_freep(&d->interrupt);
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
