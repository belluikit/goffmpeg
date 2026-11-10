//go:build cgo

package goffmpeg

/*
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/log.h>

// is_input_device reports a demuxer that is an input device: av_demuxer_iterate
// lists libavdevice's devices too, once they are registered.
static int is_input_device(const AVInputFormat *f) {
	return f->priv_class && AV_IS_INPUT_DEVICE(f->priv_class->category);
}
*/
import "C"

import "unsafe"

// builtComponents lists the decoders and demuxers of the FFmpeg this binary
// loaded, by FFmpeg's names. The tests hold them to the codec set the build
// links (#41): configure can bring in a codec nobody listed, so the set is
// checked against what the library actually holds, not against the flags.
// Input devices (avfoundation, v4l2, dshow) are left out of the demuxers,
// though av_demuxer_iterate lists them once libavdevice is registered;
// check-components holds them to the set at configure time.
func builtComponents() (decoders, demuxers []string) {
	var it unsafe.Pointer
	for {
		c := C.av_codec_iterate(&it)
		if c == nil {
			break
		}
		if C.av_codec_is_decoder(c) != 0 {
			decoders = append(decoders, C.GoString(c.name))
		}
	}
	it = nil
	for {
		f := C.av_demuxer_iterate(&it)
		if f == nil {
			break
		}
		if C.is_input_device(f) != 0 {
			continue
		}
		demuxers = append(demuxers, C.GoString(f.name))
	}
	return decoders, demuxers
}
