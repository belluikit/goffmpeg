//go:build cgo

// cgo only: OpenReader needs cgo, because FFmpeg reads an io.Reader by calling
// back into Go.

package goffmpeg

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateReader is a seekable reader whose Read blocks while gated, until
// released, and counts the reads that start after the release.
type gateReader struct {
	*bytes.Reader
	mu       sync.Mutex
	gated    bool
	released chan struct{}
	after    atomic.Int32
}

func (r *gateReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	gated := r.gated
	r.mu.Unlock()
	if gated {
		select {
		case <-r.released:
			r.after.Add(1)
		default:
			<-r.released
		}
	}
	return r.Reader.Read(p)
}

// TestInterruptReader covers OpenReader. Interrupt cannot end a Read already
// in progress, so the caller ends it, here by releasing the gate; the read
// then delivers data as if nothing happened. Custom IO never consults FFmpeg's
// interrupt callback, so without the reader-side check FFmpeg would carry on
// reading through the file. It must stop instead, without calling r again.
func TestInterruptReader(t *testing.T) {
	r := &gateReader{Reader: bytes.NewReader(longMP3(t)), released: make(chan struct{})}
	d, err := OpenReader(r)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	r.mu.Lock()
	r.gated = true
	r.mu.Unlock()
	took, err := interruptAfter(t, d, func() error {
		if err := d.Seek(d.Duration() * 0.75); err != nil {
			return err
		}
		for {
			mt, err := d.Next()
			if err != nil || mt == MediaTypeNone {
				return err
			}
		}
	}, func() { close(r.released) }, func() {})
	t.Logf("returned %v after Interrupt: %v; %d reads after the release", took, err, r.after.Load())
	if took > time.Second {
		t.Errorf("returned %v after Interrupt, want promptly", took)
	}
	if err == nil {
		t.Error("an interrupted Seek reported success")
	}
	if n := r.after.Load(); n > 0 {
		t.Errorf("%d reads started after Interrupt; want none", n)
	}
}
