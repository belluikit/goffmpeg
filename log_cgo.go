//go:build cgo

package goffmpeg

/*
#include <libavutil/log.h>
*/
import "C"

func init() { _ = setLogLevel(LogError) }

func setLogLevel(level LogLevel) error {
	C.av_log_set_level(C.int(level))
	return nil
}
