//go:build cgo

package goffmpeg

/*
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
*/
import "C"

import "unsafe"

// builtComponents lists the decoders and demuxers of the FFmpeg this binary
// loaded, by FFmpeg's names. The tests hold it to the codec set the build
// links (#41): configure can bring in a codec nobody listed, so the set is
// checked against what the library actually holds, not against the flags.
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
		demuxers = append(demuxers, C.GoString(f.name))
	}
	return decoders, demuxers
}
