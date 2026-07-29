//go:build cgo

package goffmpeg

/*
#include "bridge.h"

#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"unsafe"
)

// avseekSize is FFmpeg's AVSEEK_SIZE: a seek request that asks for the total
// stream length instead of repositioning.
const avseekSize = 0x10000

// readerState is one open io.Reader, reachable from C by integer handle.
//
// The handle indirection is deliberate: cgo forbids C code from storing Go
// pointers, so the AVIOContext's opaque field carries an integer that indexes
// this registry rather than the reader itself.
type readerState struct {
	r      io.Reader
	seeker io.Seeker
	size   int64 // -1 when unknown
}

var (
	readerMu   sync.Mutex
	readers    = make(map[int64]*readerState)
	readerNext int64
)

func registerReader(rs *readerState) int64 {
	readerMu.Lock()
	defer readerMu.Unlock()
	readerNext++
	h := readerNext
	readers[h] = rs
	return h
}

func lookupReader(h int64) *readerState {
	readerMu.Lock()
	defer readerMu.Unlock()
	return readers[h]
}

func unregisterReader(h int64) {
	readerMu.Lock()
	defer readerMu.Unlock()
	delete(readers, h)
}

// registrySize reports how many readers are currently held. Used by the tests
// to assert that Close releases them.
func registrySize() int {
	readerMu.Lock()
	defer readerMu.Unlock()
	return len(readers)
}

//export goffmpegReaderRead
func goffmpegReaderRead(handle C.int64_t, buf *C.uint8_t, bufSize C.int) C.int {
	rs := lookupReader(int64(handle))
	if rs == nil || bufSize <= 0 {
		return -1
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(bufSize))

	// A reader is allowed to return (0, nil) without being at EOF, but C sees
	// 0 as end-of-stream — so keep pulling until there is data or a verdict.
	for {
		n, err := rs.r.Read(dst)
		if n > 0 {
			return C.int(n)
		}
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			return -1
		}
	}
}

//export goffmpegReaderSeek
func goffmpegReaderSeek(handle C.int64_t, offset C.int64_t, whence C.int) C.int64_t {
	rs := lookupReader(int64(handle))
	if rs == nil {
		return -1
	}

	// AVSEEK_SIZE is a query, not a move: report the total length and leave the
	// read position alone.
	if int(whence)&avseekSize != 0 {
		return C.int64_t(rs.size)
	}
	if rs.seeker == nil {
		return -1
	}

	pos, err := rs.seeker.Seek(int64(offset), int(whence))
	if err != nil {
		return -1
	}
	return C.int64_t(pos)
}

// openReaderDecoder decodes from r without staging it on disk. If r also
// implements io.Seeker the decoder gets full seek support; otherwise playback
// is forward-only and Seek reports an error.
func openReaderDecoder(r io.Reader, options string) (IDecoder, error) {
	if r == nil {
		return nil, errors.New("goffmpeg: nil reader")
	}

	rs := &readerState{r: r, size: -1}
	seekable := 0
	if sk, ok := r.(io.Seeker); ok {
		// Measure the stream once so AVSEEK_SIZE can be answered, then restore
		// the original position — probing starts from wherever the caller left it.
		if start, err := sk.Seek(0, io.SeekCurrent); err == nil {
			if end, err := sk.Seek(0, io.SeekEnd); err == nil {
				if _, err := sk.Seek(start, io.SeekStart); err == nil {
					rs.seeker = sk
					rs.size = end
					seekable = 1
				}
			}
		}
	}

	cOpts := C.CString(options)
	defer C.free(unsafe.Pointer(cOpts))

	handle := registerReader(rs)
	d := &cgoDecoder{handle: handle}
	if res := C.decoder_open_reader_opts(&d.d, C.int64_t(handle), C.int(seekable), cOpts); res != 0 {
		// decoder_open_reader_opts leaves the AVIOContext allocated on failure.
		C.decoder_close(&d.d)
		unregisterReader(handle)
		if res == -8 {
			return nil, fmt.Errorf("goffmpeg: malformed open options %q", options)
		}
		return nil, fmt.Errorf("goffmpeg: failed to open reader: %d", int(res))
	}
	return d, nil
}
