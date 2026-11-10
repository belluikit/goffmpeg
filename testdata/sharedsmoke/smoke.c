// smoke.c: decodes the start of a media file with the shared FFmpeg build
// alone (Makefile: smoke-shared). It calls into every library so the
// executable needs each one, and checks that the libavutil it loaded is the
// shared build, so a run against some other FFmpeg fails instead of passing.
//
// It decodes video, or with SMOKE_AUDIO audio: the audio-only codec set (#41)
// has no video decoder and no libswscale.
#include <stdio.h>
#include <string.h>

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
#include <libswresample/swresample.h>
#ifndef SMOKE_AUDIO
#include <libswscale/swscale.h>
#endif
#ifdef SMOKE_AVDEVICE
#include <libavdevice/avdevice.h>
#endif

int main(int argc, char **argv) {
	if (argc != 2) {
		fprintf(stderr, "usage: %s FILE\n", argv[0]);
		return 2;
	}
	const char *conf = avutil_configuration();
	if (!strstr(conf, "--enable-shared")) {
		fprintf(stderr, "libavutil is not the shared build: %s\n", conf);
		return 1;
	}
	printf("avutil %u, swresample %u, avcodec %u, avformat %u",
	       avutil_version(), swresample_version(), avcodec_version(), avformat_version());
#ifndef SMOKE_AUDIO
	printf(", swscale %u", swscale_version());
#endif
#ifdef SMOKE_AVDEVICE
	avdevice_register_all();
	printf(", avdevice %u", avdevice_version());
#endif
	printf("\n");

	AVFormatContext *fmt = NULL;
	int err = avformat_open_input(&fmt, argv[1], NULL, NULL);
	if (err < 0) {
		fprintf(stderr, "open %s: %s\n", argv[1], av_err2str(err));
		return 1;
	}
	if ((err = avformat_find_stream_info(fmt, NULL)) < 0) {
		fprintf(stderr, "stream info: %s\n", av_err2str(err));
		return 1;
	}
#ifdef SMOKE_AUDIO
	const enum AVMediaType want = AVMEDIA_TYPE_AUDIO;
#else
	const enum AVMediaType want = AVMEDIA_TYPE_VIDEO;
#endif
	const AVCodec *codec = NULL;
	int si = av_find_best_stream(fmt, want, -1, -1, &codec, 0);
	if (si < 0) {
		fprintf(stderr, "no %s stream: %s\n", av_get_media_type_string(want), av_err2str(si));
		return 1;
	}
	AVCodecContext *dec = avcodec_alloc_context3(codec);
	avcodec_parameters_to_context(dec, fmt->streams[si]->codecpar);
	if ((err = avcodec_open2(dec, codec, NULL)) < 0) {
		fprintf(stderr, "open %s: %s\n", codec->name, av_err2str(err));
		return 1;
	}

	AVPacket *pkt = av_packet_alloc();
	AVFrame *frame = av_frame_alloc();
	int frames = 0;
	while (frames < 5 && av_read_frame(fmt, pkt) >= 0) {
		if (pkt->stream_index == si && avcodec_send_packet(dec, pkt) >= 0)
			while (avcodec_receive_frame(dec, frame) >= 0)
				frames++;
		av_packet_unref(pkt);
	}
	printf("decoded %d %s frames", frames, codec->name);
	if (want == AVMEDIA_TYPE_VIDEO)
		printf(", %dx%d", dec->width, dec->height);
	printf("\n");

	av_frame_free(&frame);
	av_packet_free(&pkt);
	avcodec_free_context(&dec);
	avformat_close_input(&fmt);
	return frames > 0 ? 0 : 1;
}
