// Custom-IO entry point: feeds libavformat from a Go io.Reader instead of a
// URL, so callers can decode from any stream (an HTTP body, an embedded asset,
// an Android content:// descriptor) without staging it on disk first.
//
// This file is compiled ONLY into the cgo build. It calls back into Go through
// _cgo_export.h, which exists only when cgo generates it, so the purego
// shared-library target (which compiles bridge.c alone) never sees it.

#include "bridge.h"
#include "_cgo_export.h"

#include <libavutil/mem.h>

// Probing needs a reasonable window before libavformat gives up identifying
// the container; 32 KiB matches what ffmpeg itself uses for custom IO.
#define GOFFMPEG_AVIO_BUFSIZE 32768

static int goffmpeg_avio_read(void *opaque, uint8_t *buf, int buf_size) {
    int n = goffmpegReaderRead((int64_t)(intptr_t)opaque, buf, buf_size);
    if (n == 0) return AVERROR_EOF;
    if (n < 0) return AVERROR(EIO);
    return n;
}

static int64_t goffmpeg_avio_seek(void *opaque, int64_t offset, int whence) {
    int64_t r = goffmpegReaderSeek((int64_t)(intptr_t)opaque, offset, whence);
    // The Go side reports "cannot do that" as -1; translate it into an errno
    // AVERROR so libavformat falls back to forward-only reading instead of
    // treating it as a hard failure.
    if (r < 0) return AVERROR(EINVAL);
    return r;
}

int decoder_open_reader_opts(Decoder *d, int64_t handle, int seekable, const char *options) {
    memset(d, 0, sizeof(Decoder));
    d->v_stream_idx = -1;
    d->a_stream_idx = -1;
    d->reader_handle = handle;

    d->avio_buf = av_malloc(GOFFMPEG_AVIO_BUFSIZE);
    if (!d->avio_buf) return -4;

    d->avio_ctx = avio_alloc_context(
        d->avio_buf, GOFFMPEG_AVIO_BUFSIZE,
        0,                            // write_flag: read-only
        (void *)(intptr_t)handle,     // opaque: an integer handle, never a Go pointer
        goffmpeg_avio_read,
        NULL,                         // no write callback
        seekable ? goffmpeg_avio_seek : NULL);
    if (!d->avio_ctx) {
        av_freep(&d->avio_buf);
        return -5;
    }
    if (!seekable) d->avio_ctx->seekable = 0;

    d->fmt_ctx = avformat_alloc_context();
    if (!d->fmt_ctx) {
        av_freep(&d->avio_ctx->buffer);
        avio_context_free(&d->avio_ctx);
        d->avio_buf = NULL;
        return -6;
    }
    d->fmt_ctx->pb = d->avio_ctx;
    d->fmt_ctx->flags |= AVFMT_FLAG_CUSTOM_IO;

    AVDictionary *opts = NULL;
    if (options && options[0] &&
        av_dict_parse_string(&opts, options, "=", ",", 0) < 0) {
        av_dict_free(&opts);
        return -8;
    }
    // url=NULL: the format is probed through pb rather than inferred from a
    // filename. On failure avformat_open_input frees fmt_ctx and NULLs it, but
    // leaves the AVIOContext for decoder_close to reclaim.
    int rc = avformat_open_input(&d->fmt_ctx, NULL, NULL, &opts);
    av_dict_free(&opts);
    if (rc < 0) return -1;
    return decoder_setup_streams(d);
}

int decoder_open_reader(Decoder *d, int64_t handle, int seekable) {
    return decoder_open_reader_opts(d, handle, seekable, NULL);
}
