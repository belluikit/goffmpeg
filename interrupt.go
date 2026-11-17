package goffmpeg

import (
	"fmt"
	"sync/atomic"
)

// interruptState keeps Interrupt's promise on the Go side: once it is set,
// Seek and Next fail whatever the bridge returned. FFmpeg consults its
// interrupt callback only on some I/O paths, and a bridge from before #37
// does not check the flag itself, so the result of a call that ran (or
// finished) after Interrupt cannot be trusted to say so.
type interruptState struct{ set atomic.Bool }

// seekResult turns decoder_seek's return code into Seek's error. The code is
// wrapped as an AVError, as Next reports one, so a caller can tell what failed
// with errors.As: a stream that cannot seek fails with ENOSYS, which a caller
// may want to explain rather than print as a number.
func (s *interruptState) seekResult(res int) error {
	if s.set.Load() {
		res = int(avErrorExit)
	}
	if res < 0 {
		return fmt.Errorf("seek failed: %w", AVError(res))
	}
	return nil
}

// nextResult turns decoder_next's return code into Next's result.
func (s *interruptState) nextResult(res int) (MediaType, error) {
	if s.set.Load() {
		return MediaTypeNone, avErrorExit
	}
	return nextResult(res)
}
