package goffmpeg

import "errors"

// LogLevel is how much of FFmpeg's own logging reaches the process's stderr.
// The values are FFmpeg's AV_LOG_* levels.
type LogLevel int

const (
	LogQuiet   LogLevel = -8 // nothing
	LogError   LogLevel = 16 // errors only: the default
	LogWarning LogLevel = 24 // also warnings, such as a missing SIMD conversion path
	LogInfo    LogLevel = 32 // FFmpeg's own default
	LogDebug   LogLevel = 48
)

// ErrLogLevelUnavailable reports SetLogLevel against a shared libgoffmpeg that
// does not export av_log_set_level (the purego backend only).
var ErrLogLevelUnavailable = errors.New("goffmpeg: the loaded libgoffmpeg cannot set FFmpeg's log level")

// SetLogLevel sets how much of FFmpeg's logging is printed to stderr.
//
// The package sets LogError when it loads (#19). FFmpeg's own default prints
// its warnings, and those reached the stderr of every application decoding
// through this package with no way to quiet them; most are notes that do not
// mean the media decoded wrong, such as libswscale having no SIMD path for a
// conversion, or the AAC decoder dropping its priming samples after a seek.
// Raise the level to see them while debugging. The setting is process-wide,
// as FFmpeg's is.
func SetLogLevel(level LogLevel) error {
	return setLogLevel(level)
}
